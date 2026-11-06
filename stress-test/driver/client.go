// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// idleWindow is phase 2 of awaitSettledOutput's two-phase wait: once the
// first byte of a call's response has already arrived (phase 1), how
// long BOTH stdout and stderr must then stay silent before the call is
// considered complete. Confirmed empirically against a real bin/cli REPL
// process with all three streams piped (never a TTY): chzyer/readline
// does NOT write its "> " prompt at all when stdin isn't a real
// terminal — there is no prompt byte sequence to frame against, so this
// quiet-gap heuristic is an empirically-necessary substitute. Kept short
// because by the time phase 1 has already seen data, everything left is
// just the CLI's own synchronous, in-memory fmt.Println calls (see
// cli/client.go's printContent/printStructuredContent — no more network
// I/O to wait out), and because it's paid on every single call, eating
// directly into the --calls-per-second budget at the high end (10/sec
// leaves only a 100ms/call window in total). Do NOT use this same short
// duration for the phase-1 "wait for the first byte at all" step — that
// one must bound the real MCP round trip (including any orchestration
// steps), which can easily exceed a short window; awaitSettledOutput
// takes a separate, much longer hardTimeout for that. See
// stress-test/README.md's "REPL framing" section before changing this.
const idleWindow = 20 * time.Millisecond

// perCallTimeout is an absolute per-call cap, measured from when the line
// is written, independent of idleWindow resets — a safety net in case a
// call never settles (e.g. a genuinely hung backend), so one stuck call
// can never hang the whole client indefinitely.
const perCallTimeout = 30 * time.Second

// initialSettleTimeout bounds login + startup (the two banner lines) —
// longer than perCallTimeout since it includes the REPL's own OAuth
// login round trip, not just one call.
const initialSettleTimeout = 20 * time.Second

// clientConfig is one client's assignment, handed in from main.go.
type clientConfig struct {
	clientNumber   int // 1-based; also selects cli-calls<N>.txt
	callsPerSecond int
	deadline       time.Time
	runDir         string
}

// streamChunk is one read from either of a REPL child process's stdout or
// stderr pipes, tagged by which stream it came from — both readers push
// onto one shared channel so completion detection can watch both streams
// as a single merged "has anything arrived recently" signal (see
// awaitSettledOutput).
type streamChunk struct {
	isStderr bool
	data     []byte
	err      error // set (data nil) when the stream's Read returned a non-nil error (EOF or otherwise)
}

// runClient owns one client's entire lifecycle: spawn a long-lived REPL
// `cli` process, replay its assigned call file at the confirmed
// sleep-after-completion pace until the deadline, classify and record
// every call, then shut the process down cleanly.
func runClient(cfg clientConfig) error {
	fileName := fmt.Sprintf("cli-calls%d.txt", cfg.clientNumber)
	lines, err := readLines(filepath.Join("stress-test", "data", fileName))
	if err != nil {
		return fmt.Errorf("client %d: %w", cfg.clientNumber, err)
	}
	if len(lines) == 0 {
		return fmt.Errorf("client %d: %s is empty", cfg.clientNumber, fileName)
	}

	cmd := exec.Command(cliBinPath,
		"-username", "admin", "-password", "admin",
		"-auth-server", authServerURL, "-endpoint", proxyEndpoint,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("client %d: stdin pipe: %w", cfg.clientNumber, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("client %d: stdout pipe: %w", cfg.clientNumber, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("client %d: stderr pipe: %w", cfg.clientNumber, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("client %d: starting %s: %w", cfg.clientNumber, cliBinPath, err)
	}

	chunks := make(chan streamChunk, 64)
	go readStreamLoop(stdout, false, chunks)
	go readStreamLoop(stderr, true, chunks)

	// Consume the login banner (no command has been sent yet, so
	// whatever arrives before things go quiet is startup noise, not a
	// call result) before the call loop starts.
	if _, _, err := awaitSettledOutput(chunks, initialSettleTimeout); err != nil {
		killAndWait(cmd)
		return fmt.Errorf("client %d: waiting for REPL startup (login) to settle: %w", cfg.clientNumber, err)
	}

	outPath := filepath.Join(cfg.runDir, fmt.Sprintf("client%d.jsonl", cfg.clientNumber))
	outFile, err := os.Create(outPath)
	if err != nil {
		killAndWait(cmd)
		return fmt.Errorf("client %d: creating %s: %w", cfg.clientNumber, outPath, err)
	}
	defer outFile.Close()
	enc := json.NewEncoder(outFile)

	interval := time.Second / time.Duration(cfg.callsPerSecond)
	idx, cycle := 0, 1

	for {
		if time.Now().After(cfg.deadline) {
			break
		}
		if idx == len(lines) {
			idx = 0
			cycle++
		}
		line := lines[idx]
		tool := extractToolName(line)

		dispatchedAt := time.Now()
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			killAndWait(cmd)
			return fmt.Errorf("client %d: writing line %d to REPL stdin: %w", cfg.clientNumber, idx, err)
		}

		stdoutText, stderrText, err := awaitSettledOutput(chunks, perCallTimeout)
		if err != nil {
			killAndWait(cmd)
			return fmt.Errorf("client %d: line %d (%s): %w", cfg.clientNumber, idx, tool, err)
		}
		completedAt := time.Now()

		rec := CallRecord{
			Client:       cfg.clientNumber,
			File:         fileName,
			LineIndex:    idx,
			Cycle:        cycle,
			Tool:         tool,
			Line:         line,
			DispatchedAt: dispatchedAt,
			CompletedAt:  completedAt,
			LatencyMs:    completedAt.Sub(dispatchedAt).Milliseconds(),
			Outcome:      classifyOutcome(stdoutText, stderrText),
			Stdout:       truncateForStorage(stdoutText),
			Stderr:       truncateForStorage(stderrText),
		}
		if err := enc.Encode(rec); err != nil {
			killAndWait(cmd)
			return fmt.Errorf("client %d: writing call record: %w", cfg.clientNumber, err)
		}

		idx++
		time.Sleep(interval)
	}

	return shutdownClient(cmd, stdin)
}

// awaitSettledOutput is a two-phase wait, and the two-phase split is
// load-bearing, not a stylistic choice — an earlier version of this
// function started its idle-window countdown immediately, before any
// byte had arrived at all, and that broke real runs: the underlying MCP
// round trip (especially through an orchestration step, e.g.
// purchase_products) can easily exceed a short idle window, so the timer
// fired with nothing captured yet, the call was wrongly recorded as
// empty, and its real (delayed) output then arrived while the NEXT
// command's line was already being awaited — silently attributing one
// call's result to a different CallRecord. Verified and fixed against a
// real run (see stress-test/README.md's "REPL framing" section).
//
// Phase 1 blocks for the first byte on EITHER stream, with a generous
// hardTimeout (effectively the actual per-call round-trip budget) — this
// alone fixes the bug above. Phase 2 then applies idleWindow, a much
// shorter, purely local, print-flushing-latency margin: by the time
// phase 1 has already seen the RPC's response start arriving, everything
// left is just the CLI's own synchronous, in-memory fmt.Println calls
// (see cli/client.go's printContent/printStructuredContent) — no more
// network I/O to wait out. Handles all three real outcome shapes
// uniformly: stdout-only (success), stderr-only (an authz/transport
// denial — verified empirically to genuinely produce zero stdout bytes),
// and both (a business-error result, whose text is printed to stdout
// before the CLI's own synthetic wrapping error goes to stderr).
func awaitSettledOutput(chunks <-chan streamChunk, hardTimeout time.Duration) (stdout, stderr string, err error) {
	var outBuf, errBuf strings.Builder
	// One absolute deadline covering BOTH phases combined — a call whose
	// output keeps trickling in slowly enough to repeatedly reset the
	// phase-2 idle timer (without ever going fully quiet) must still be
	// bounded, not just the phase-1 "first byte" wait.
	hardDeadline := time.After(hardTimeout)

	write := func(c streamChunk) {
		if c.isStderr {
			errBuf.Write(c.data)
		} else {
			outBuf.Write(c.data)
		}
	}

	select {
	case c := <-chunks:
		if c.err != nil {
			return "", "", fmt.Errorf("output stream closed unexpectedly before any data arrived: %w", c.err)
		}
		write(c)
	case <-hardDeadline:
		return "", "", fmt.Errorf("timed out after %s waiting for the first byte of output", hardTimeout)
	}

	timer := time.NewTimer(idleWindow)
	defer timer.Stop()
	for {
		select {
		case c := <-chunks:
			if c.err != nil {
				return outBuf.String(), errBuf.String(), fmt.Errorf("output stream closed unexpectedly: %w", c.err)
			}
			write(c)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(idleWindow)
		case <-timer.C:
			return outBuf.String(), errBuf.String(), nil
		case <-hardDeadline:
			return outBuf.String(), errBuf.String(), fmt.Errorf("timed out after %s waiting for output to settle (accumulated so far: stdout=%q stderr=%q)", hardTimeout, outBuf.String(), errBuf.String())
		}
	}
}

// shutdownClient asks the REPL to exit cleanly, falling back to killing
// the process if it doesn't within a short grace period — a single hung
// REPL must never block the rest of the driver from finishing.
func shutdownClient(cmd *exec.Cmd, stdin io.WriteCloser) error {
	io.WriteString(stdin, "exit\n")
	stdin.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil
	}
}

func killAndWait(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	_ = cmd.Wait()
}

// readStreamLoop continuously reads raw bytes from r (either the REPL's
// stdout or stderr) and pushes each nonempty read onto chunks, tagged by
// isStderr — raw byte reads, not a bufio.Scanner split on newlines,
// since completion is detected by inter-read timing (see
// awaitSettledOutput), not by line boundaries.
func readStreamLoop(r io.Reader, isStderr bool, chunks chan<- streamChunk) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			cp := make([]byte, n)
			copy(cp, buf[:n])
			chunks <- streamChunk{isStderr: isStderr, data: cp}
		}
		if err != nil {
			chunks <- streamChunk{isStderr: isStderr, err: err}
			return
		}
	}
}

// extractToolName pulls the namespaced tool name back out of a rendered
// "call-tool -name <tool> -args {...}" line, for per-call/per-tool
// reporting — a plain substring parse, not a re-run of the REPL's own
// tokenizer, since the generator already guarantees these lines are
// well-formed (see generator/lines.go's self-check).
func extractToolName(line string) string {
	const marker = " -name "
	i := strings.Index(line, marker)
	if i < 0 {
		return ""
	}
	rest := line[i+len(marker):]
	if j := strings.Index(rest, " -args"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r\n")
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return lines, nil
}
