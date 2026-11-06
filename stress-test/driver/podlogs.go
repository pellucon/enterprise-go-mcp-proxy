// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// logTarget is one `kubectl logs` invocation to run and save. Verified
// directly: go-proxy/nats/demo-authorization-server don't carry a common
// selector label, so they're fetched by Deployment name; every
// platform-mcp-servers/* and go-gardening-demo/servers/* Deployment
// carries mcp.io/role=backend-mcp-server, so each of those two groups is
// fetched in one shot via that label selector.
type logTarget struct {
	name      string // output file base name (<name>.log)
	namespace string
	args      []string
}

var logTargets = []logTarget{
	{"go-proxy", "mcp-proxy", []string{"deployment/go-proxy"}},
	{"nats", "mcp-proxy", []string{"deployment/nats"}},
	{"demo-authorization-server", "demo-auth", []string{"deployment/demo-authorization-server"}},
	{"platform-servers", "mcp-proxy", []string{"-l", "mcp.io/role=backend-mcp-server", "--all-containers", "--prefix"}},
	{"gardening-demo-backends", "gardening-demo", []string{"-l", "mcp.io/role=backend-mcp-server", "--all-containers", "--prefix"}},
}

// LogAnomaly is one level=ERROR or level=WARN line found in a collected
// log, per the exact slog.TextHandler wire format logging/logging.go
// produces (verified directly) — an unambiguous grep pattern.
//
// Two genuinely different things both produce level=ERROR lines, and the
// report must not conflate them (verified directly against
// middleware.go, not assumed): an ordinary IN-TOOL business-rule
// rejection (e.g. insufficient stock) logs nothing server-side at all
// (see README.md's "flagged code gaps" section) — but a REJECTION BY THE
// ORCHESTRATION/GUARDRAIL ENGINE (a declarative or business-logic
// constraint blocking a call before it ever reaches the backend) DOES:
// middleware.go's createMiddleware calls logging.Errorf("tool call
// rejected by orchestration: ...") on exactly this path. That's expected,
// even reassuring, behavior — proof the guardrail machinery is actually
// firing — not a bug, so it's categorized separately from a genuine
// anomaly (e.g. a transient session/discovery error) that deserves real
// scrutiny.
type LogAnomaly struct {
	Source   string `json:"source"`   // the logTarget name this came from
	Level    string `json:"level"`    // "ERROR" or "WARN"
	Line     string `json:"line"`
	Expected bool   `json:"expected"` // true for a known-benign category (currently: orchestration/guardrail rejections)
}

// guardrailRejectionPattern matches middleware.go's own
// logging.Errorf("tool call rejected by orchestration: ...") call —
// the one level=ERROR message this driver knows to be an expected,
// working-as-intended guardrail rejection rather than a genuine anomaly.
var guardrailRejectionPattern = regexp.MustCompile(`tool call rejected by orchestration`)

// collectLogs fetches every logTarget's output (scoped to [since, now) via
// --since-time), saves each to <outDir>/logs/<name>.log, and scans every
// saved file for level=ERROR/level=WARN lines. A single target's kubectl
// invocation failing (e.g. no matching pods) is recorded as a warning in
// the returned slice rather than aborting the whole collection — the
// other targets' logs are still worth having.
func collectLogs(outDir string, since time.Time) ([]LogAnomaly, []string, error) {
	logsDir := filepath.Join(outDir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("creating logs dir: %w", err)
	}

	var anomalies []LogAnomaly
	var collectionWarnings []string
	sinceArg := "--since-time=" + since.UTC().Format(time.RFC3339)

	for _, t := range logTargets {
		args := append([]string{"logs", "-n", t.namespace}, t.args...)
		args = append(args, sinceArg)
		out, err := exec.Command("kubectl", args...).CombinedOutput()
		outPath := filepath.Join(logsDir, t.name+".log")
		if writeErr := os.WriteFile(outPath, out, 0o644); writeErr != nil {
			return nil, nil, fmt.Errorf("writing %s: %w", outPath, writeErr)
		}
		if err != nil {
			collectionWarnings = append(collectionWarnings, fmt.Sprintf("kubectl logs for %q (ns %s) failed: %v (output saved to %s, may be empty or contain kubectl's own error text)", t.name, t.namespace, err, outPath))
			continue
		}
		found, err := scanForAnomalies(t.name, out)
		if err != nil {
			return nil, nil, err
		}
		anomalies = append(anomalies, found...)
	}
	return anomalies, collectionWarnings, nil
}

func scanForAnomalies(source string, log []byte) ([]LogAnomaly, error) {
	var found []LogAnomaly
	scanner := bufio.NewScanner(strings.NewReader(string(log)))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.Contains(line, "level=ERROR"):
			found = append(found, LogAnomaly{Source: source, Level: "ERROR", Line: line, Expected: guardrailRejectionPattern.MatchString(line)})
		case strings.Contains(line, "level=WARN"):
			found = append(found, LogAnomaly{Source: source, Level: "WARN", Line: line})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s log for anomalies: %w", source, err)
	}
	return found, nil
}
