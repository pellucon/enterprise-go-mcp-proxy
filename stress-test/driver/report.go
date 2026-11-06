// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// RunConfig is written verbatim to <runDir>/config.json.
type RunConfig struct {
	ClientCount      int       `json:"clientCount"`
	CallsPerSecond   int       `json:"callsPerSecond"`
	DurationMinutes  int       `json:"durationMinutes"`
	StartedAt        time.Time `json:"startedAt"`
	SetupCompletedAt time.Time `json:"setupCompletedAt"`
	EndedAt          time.Time `json:"endedAt"`
}

// correlationWindow is how close (in either direction) a call record's
// CompletedAt must be to a log anomaly's own timestamp to be listed as
// possible context for it in the report — approximate root-cause context
// (correlation, not a confirmed causal link), but a real one: every log
// line from this repo's shared logging package embeds its own `time=...`
// field (see logging/logging.go's slog.TextHandler default format), which
// logLineTime below parses directly, rather than relying on when this
// driver happened to collect the log (which can lag the actual event by
// as long as the whole client-run phase — an early version of this
// report used collection time and, as a result, correlated a genuine
// setup-phase error against an unrelated call from minutes later).
const correlationWindow = 2 * time.Second

// loadAllRecords reads every client<N>.jsonl file already written under
// runDir by the client-run phase.
func loadAllRecords(runDir string, clientCount int) ([]CallRecord, error) {
	var all []CallRecord
	for i := 1; i <= clientCount; i++ {
		path := filepath.Join(runDir, fmt.Sprintf("client%d.jsonl", i))
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening %s: %w", path, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.TrimSpace(line) == "" {
				continue
			}
			var rec CallRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				f.Close()
				return nil, fmt.Errorf("parsing %s: %w", path, err)
			}
			all = append(all, rec)
		}
		scanErr := scanner.Err()
		f.Close()
		if scanErr != nil {
			return nil, fmt.Errorf("reading %s: %w", path, scanErr)
		}
	}
	return all, nil
}

// writeReport builds runDir/report.md from every artifact the setup/run/
// teardown phases produced.
func writeReport(runDir string, cfg RunConfig, records []CallRecord, findings []HealthFinding, anomalies []LogAnomaly, logWarnings []string) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Stress-test report\n\n")
	fmt.Fprintf(&b, "- client-count: %d\n", cfg.ClientCount)
	fmt.Fprintf(&b, "- calls-per-second (per client): %d\n", cfg.CallsPerSecond)
	fmt.Fprintf(&b, "- duration: %d minutes\n", cfg.DurationMinutes)
	fmt.Fprintf(&b, "- setup completed: %s\n", cfg.SetupCompletedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- run window: %s -> %s\n", cfg.StartedAt.Format(time.RFC3339), cfg.EndedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "- total calls: %d\n\n", len(records))

	writeOutcomeBreakdown(&b, records)
	writeBusinessErrorSection(&b, records)
	writeGuardrailRejectedSection(&b, records)
	writeAuthzDeniedSection(&b, records)
	writeTransportErrorSection(&b, records)
	writePodHealthSection(&b, findings)
	writeLogScanSection(&b, anomalies, logWarnings, records)
	writeFlaggedGapsSection(&b)

	return os.WriteFile(filepath.Join(runDir, "report.md"), []byte(b.String()), 0o644)
}

func writeOutcomeBreakdown(b *strings.Builder, records []CallRecord) {
	fmt.Fprintf(b, "## Outcome breakdown\n\n")

	overall := map[Outcome]int{}
	perTool := map[string]map[Outcome]int{}
	for _, r := range records {
		overall[r.Outcome]++
		if perTool[r.Tool] == nil {
			perTool[r.Tool] = map[Outcome]int{}
		}
		perTool[r.Tool][r.Outcome]++
	}

	allOutcomes := []Outcome{OutcomeSuccess, OutcomeBusinessError, OutcomeGuardrailRejected, OutcomeAuthzDenied, OutcomeTransportError}

	total := len(records)
	fmt.Fprintf(b, "Overall (%d total):\n\n", total)
	for _, o := range allOutcomes {
		pct := 0.0
		if total > 0 {
			pct = 100 * float64(overall[o]) / float64(total)
		}
		fmt.Fprintf(b, "- %s: %d (%.1f%%)\n", o, overall[o], pct)
	}
	fmt.Fprintf(b, "\nPer tool:\n\n")
	fmt.Fprintf(b, "| Tool | success | business-error | guardrail-rejected | authz-denied | transport-error |\n|---|---|---|---|---|---|\n")

	tools := make([]string, 0, len(perTool))
	for t := range perTool {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	for _, t := range tools {
		counts := perTool[t]
		fmt.Fprintf(b, "| %s | %d | %d | %d | %d | %d |\n", t,
			counts[OutcomeSuccess], counts[OutcomeBusinessError], counts[OutcomeGuardrailRejected], counts[OutcomeAuthzDenied], counts[OutcomeTransportError])
	}
	fmt.Fprintf(b, "\n")
}

// knownExpectedBusinessErrorSources annotates report output with the
// generation-time-known reasons a given tool is expected to produce
// business errors, so the report reads as "this is anticipated" rather
// than silently looking like an unexplained pile of failures.
var knownExpectedBusinessErrorSources = map[string]string{
	"garden-customer-service.resolve_customer_service_call": "expected: only ~3 seed rows are unresolved; this pool exhausts almost immediately, after which every call legitimately errors as \"already resolved\" (see stress-test/README.md)",
	"garden-online-store.purchase_products":                 "expected in some fraction: insufficient stock / unknown product or customer id are real, intentional business rejections this stress test is designed to exercise",
}

func writeBusinessErrorSection(b *strings.Builder, records []CallRecord) {
	fmt.Fprintf(b, "## Business errors\n\n")

	type example struct {
		tool, message string
	}
	counts := map[string]int{}
	examples := map[string]example{}
	for _, r := range records {
		if r.Outcome != OutcomeBusinessError {
			continue
		}
		counts[r.Tool]++
		if _, ok := examples[r.Tool]; !ok {
			examples[r.Tool] = example{tool: r.Tool, message: firstLine(r.Stdout)}
		}
	}
	if len(counts) == 0 {
		fmt.Fprintf(b, "None observed.\n\n")
		return
	}

	tools := make([]string, 0, len(counts))
	for t := range counts {
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return counts[tools[i]] > counts[tools[j]] })

	for _, t := range tools {
		fmt.Fprintf(b, "- **%s** — %d occurrence(s)\n", t, counts[t])
		fmt.Fprintf(b, "  - example: %s\n", examples[t].message)
		if note, ok := knownExpectedBusinessErrorSources[t]; ok {
			fmt.Fprintf(b, "  - %s\n", note)
		}
	}
	fmt.Fprintf(b, "\n")
}

// writeGuardrailRejectedSection covers calls go-proxy's orchestration
// engine blocked via a call-authorization/result-authorization step
// before the call ever reached its backend (OutcomeGuardrailRejected) —
// expected, common, and proof the guardrail machinery is working, same
// framing as writeBusinessErrorSection above. This used to be lumped into
// "transport errors" (see OutcomeTransportError's own doc comment),
// which was misleading: "blocked by constraint(s)" is not a transport
// failure.
func writeGuardrailRejectedSection(b *strings.Builder, records []CallRecord) {
	fmt.Fprintf(b, "## Guardrail rejections (call-authorization / result-authorization)\n\n")

	type example struct{ tool, message string }
	counts := map[string]int{}
	examples := map[string]example{}
	for _, r := range records {
		if r.Outcome != OutcomeGuardrailRejected {
			continue
		}
		counts[r.Tool]++
		if _, ok := examples[r.Tool]; !ok {
			examples[r.Tool] = example{tool: r.Tool, message: firstLine(r.Stderr)}
		}
	}
	if len(counts) == 0 {
		fmt.Fprintf(b, "None observed.\n\n")
		return
	}

	tools := make([]string, 0, len(counts))
	for t := range counts {
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return counts[tools[i]] > counts[tools[j]] })

	for _, t := range tools {
		fmt.Fprintf(b, "- **%s** — %d occurrence(s)\n", t, counts[t])
		fmt.Fprintf(b, "  - example: %s\n", examples[t].message)
	}
	fmt.Fprintf(b, "\n")
}

// writeAuthzDeniedSection covers calls rejected purely because the
// caller's role wasn't granted the tool (OutcomeAuthzDenied). Should be
// empty in a normal run: this test always authenticates as "admin",
// granted everything in every committed *.cfg — any occurrence here is
// worth individual scrutiny, same framing as writeTransportErrorSection.
func writeAuthzDeniedSection(b *strings.Builder, records []CallRecord) {
	fmt.Fprintf(b, "## Authorization denials (rare/unexpected — each deserves individual scrutiny)\n\n")
	var found []CallRecord
	for _, r := range records {
		if r.Outcome == OutcomeAuthzDenied {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		fmt.Fprintf(b, "None observed.\n\n")
		return
	}
	for _, r := range found {
		fmt.Fprintf(b, "- client %d, line %d, cycle %d, tool `%s` at %s:\n  - line: `%s`\n  - stderr: %s\n",
			r.Client, r.LineIndex, r.Cycle, r.Tool, r.CompletedAt.Format(time.RFC3339), r.Line, firstLine(r.Stderr))
	}
	fmt.Fprintf(b, "\n")
}

func writeTransportErrorSection(b *strings.Builder, records []CallRecord) {
	fmt.Fprintf(b, "## Transport errors (rare/unexpected — each deserves individual scrutiny)\n\n")
	var found []CallRecord
	for _, r := range records {
		if r.Outcome == OutcomeTransportError {
			found = append(found, r)
		}
	}
	if len(found) == 0 {
		fmt.Fprintf(b, "None observed.\n\n")
		return
	}
	for _, r := range found {
		fmt.Fprintf(b, "- client %d, line %d, cycle %d, tool `%s` at %s:\n  - line: `%s`\n  - stderr: %s\n",
			r.Client, r.LineIndex, r.Cycle, r.Tool, r.CompletedAt.Format(time.RFC3339), r.Line, firstLine(r.Stderr))
	}
	fmt.Fprintf(b, "\n")
}

func writePodHealthSection(b *strings.Builder, findings []HealthFinding) {
	fmt.Fprintf(b, "## Pod health\n\n")
	if len(findings) == 0 {
		fmt.Fprintf(b, "PASS — every pod present at the post-setup baseline is still present, Running/Ready, with no restart-count increase, and no unexpected pod appeared.\n\n")
		return
	}
	fmt.Fprintf(b, "FAIL — %d finding(s):\n\n", len(findings))
	for _, f := range findings {
		fmt.Fprintf(b, "- %s/%s: %s\n", f.Namespace, f.Pod, f.Problem)
	}
	fmt.Fprintf(b, "\n")
}

func writeLogScanSection(b *strings.Builder, anomalies []LogAnomaly, logWarnings []string, records []CallRecord) {
	fmt.Fprintf(b, "## Pod log scan (level=ERROR / level=WARN)\n\n")
	for _, w := range logWarnings {
		fmt.Fprintf(b, "> collection warning: %s\n\n", w)
	}

	var expected, genuine []LogAnomaly
	for _, a := range anomalies {
		if a.Expected {
			expected = append(expected, a)
		} else {
			genuine = append(genuine, a)
		}
	}

	if len(expected) > 0 {
		fmt.Fprintf(b, "**%d expected guardrail rejection(s)** — middleware.go logs a level=ERROR line whenever the orchestration/guardrail engine blocks a call before it reaches its backend (verified directly); this is proof the guardrail machinery is working, not a bug, and does NOT count toward the pass/fail verdict below:\n\n", len(expected))
		for _, a := range expected {
			fmt.Fprintf(b, "- `%s`\n", a.Line)
		}
		fmt.Fprintf(b, "\n")
	}

	if len(genuine) == 0 {
		fmt.Fprintf(b, "PASS — zero genuine anomalies found (beyond the expected guardrail rejections listed above, if any). Ordinary IN-TOOL business-rule rejections are NOT expected to appear here at all (see \"flagged code gaps\" below) — this is a real anomaly-free result, not a blind spot.\n\n")
		return
	}
	fmt.Fprintf(b, "FAIL — %d genuine anomalous line(s) found (crash, panic, or dispatch/transport failure — NOT an ordinary business rejection or guardrail block):\n\n", len(genuine))
	for _, a := range genuine {
		fmt.Fprintf(b, "- **[%s] %s**: `%s`\n", a.Level, a.Source, a.Line)
		nearby := nearbyRecords(records, a, correlationWindow)
		if len(nearby) == 0 {
			if _, ok := logLineTime(a.Line); !ok {
				fmt.Fprintf(b, "  - no parseable time=... field on this line — cannot correlate\n")
			} else {
				fmt.Fprintf(b, "  - no call found within +/-%s of this line's own timestamp\n", correlationWindow)
			}
			continue
		}
		fmt.Fprintf(b, "  - calls in flight around this time (root-cause context, not a confirmed causal link):\n")
		for _, r := range nearby {
			fmt.Fprintf(b, "    - client %d, tool `%s`, outcome %s, at %s\n", r.Client, r.Tool, r.Outcome, r.CompletedAt.Format(time.RFC3339))
		}
	}
	fmt.Fprintf(b, "\n")
}

// logLineTimePattern extracts a slog TextHandler line's own "time=..."
// field — the value runs up to the next unescaped space; a log message
// itself could theoretically contain a literal space-free "time=" token,
// but slog always emits the timestamp as the very first key=value pair,
// so anchoring at the start of the line (rather than searching anywhere
// within it) avoids that false-positive risk entirely.
var logLineTimePattern = regexp.MustCompile(`^time=(\S+)`)

// logLineTime parses the timestamp a log anomaly's own line carries,
// per logLineTimePattern — every line from this repo's shared logging
// package has one (see logging/logging.go). Returns ok=false if the line
// doesn't match (e.g. a non-Go component's log line slipped through, or
// the format ever changes), so the caller can fall back gracefully rather
// than mis-correlating.
func logLineTime(line string) (time.Time, bool) {
	m := logLineTimePattern.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// nearbyRecords finds every call record whose CompletedAt falls within
// window of the anomaly's OWN embedded timestamp (see logLineTime) — a
// real time-based correlation, not collection-time guesswork. Falls back
// to an empty result (rather than a misleading guess) if the anomaly's
// own line has no parseable timestamp.
func nearbyRecords(records []CallRecord, a LogAnomaly, window time.Duration) []CallRecord {
	at, ok := logLineTime(a.Line)
	if !ok {
		return nil
	}
	var out []CallRecord
	for _, r := range records {
		if at.Sub(r.CompletedAt) <= window && r.CompletedAt.Sub(at) <= window {
			out = append(out, r)
		}
	}
	return out
}

func writeFlaggedGapsSection(b *strings.Builder) {
	fmt.Fprintf(b, "## Flagged code gaps (not implemented, per instruction)\n\n")
	fmt.Fprintf(b, "1. **Business-error server-side logging gap**: garden-online-store's (and the other gardening servers') business-rule rejection paths emit no `logging.*` call at all — the rejection is only observable in the tool's own IsError:true response, never in a pod log line. This is why this report classifies business errors from the CLI's own captured output (above), not from pod logs, and why the log-scan section above expects zero matches for ordinary rejections. Not implemented, per instruction — the fix would be adding logging.Errorf/logging.Infof calls on each rejection path in go-gardening-demo/servers/*/main.go, an application-code change outside this tool's scope.\n")
	fmt.Fprintf(b, "2. No other code-change gaps were identified during design/implementation.\n\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		return "(empty)"
	}
	return s
}
