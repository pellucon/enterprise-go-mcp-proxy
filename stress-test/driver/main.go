// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Command driver is stress-test's 3-flag entry point: it builds+redeploys
// the local Colima/k3s stack, reseeds gardening data, runs --client-count
// concurrent REPL `cli` clients replaying stress-test/data/cli-callsN.txt
// at --calls-per-second (per client) for --duration minutes, then checks
// pod health and scans pod logs for anomalies, writing everything to
// stress-test/runs/<timestamp>/.
//
// Normally launched via stress-test/run.sh, not directly — see
// stress-test/README.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "stress-test driver:", err)
		os.Exit(1)
	}
}

func run() error {
	clientCount := flag.Int("client-count", 1, "number of concurrent clients, 1-5")
	callsPerSecond := flag.Int("calls-per-second", 1, "per-client call rate, 1-10")
	duration := flag.Int("duration", 2, "run duration in minutes, 1-60")
	flag.Parse()

	if err := validateFlags(*clientCount, *callsPerSecond, *duration); err != nil {
		return err
	}

	runDir, err := createRunDir()
	if err != nil {
		return fmt.Errorf("creating run directory: %w", err)
	}
	fmt.Println("stress-test run directory:", runDir)

	startedAt := time.Now()

	fmt.Println("==> setup: build, deploy, reseed, reset_events")
	baseline, err := runSetup(runDir)
	if err != nil {
		return fmt.Errorf("setup failed: %w", err)
	}
	if err := writeJSON(filepath.Join(runDir, "pods-baseline.json"), baseline); err != nil {
		return err
	}
	setupCompletedAt := time.Now()
	fmt.Println("==> setup complete")

	deadline := setupCompletedAt.Add(time.Duration(*duration) * time.Minute)
	fmt.Printf("==> running %d client(s) at %d call(s)/sec each until %s\n", *clientCount, *callsPerSecond, deadline.Format(time.RFC3339))

	var wg sync.WaitGroup
	errs := make([]error, *clientCount)
	for i := 0; i < *clientCount; i++ {
		wg.Add(1)
		go func(clientIndex int) {
			defer wg.Done()
			errs[clientIndex] = runClient(clientConfig{
				clientNumber:   clientIndex + 1,
				callsPerSecond: *callsPerSecond,
				deadline:       deadline,
				runDir:         runDir,
			})
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			fmt.Fprintf(os.Stderr, "client %d error: %v\n", i+1, e)
		}
	}

	endedAt := time.Now()
	fmt.Println("==> client run complete, starting teardown/analysis")

	final, err := snapshotPods()
	if err != nil {
		return fmt.Errorf("taking final pod snapshot: %w", err)
	}
	if err := writeJSON(filepath.Join(runDir, "pods-final.json"), final); err != nil {
		return err
	}
	findings := checkPodHealth(baseline, final)

	anomalies, logWarnings, err := collectLogs(runDir, setupCompletedAt)
	if err != nil {
		return fmt.Errorf("collecting pod logs: %w", err)
	}

	records, err := loadAllRecords(runDir, *clientCount)
	if err != nil {
		return fmt.Errorf("loading call records: %w", err)
	}

	cfg := RunConfig{
		ClientCount:      *clientCount,
		CallsPerSecond:   *callsPerSecond,
		DurationMinutes:  *duration,
		StartedAt:        startedAt,
		SetupCompletedAt: setupCompletedAt,
		EndedAt:          endedAt,
	}
	if err := writeJSON(filepath.Join(runDir, "config.json"), cfg); err != nil {
		return err
	}
	if err := writeReport(runDir, cfg, records, findings, anomalies, logWarnings); err != nil {
		return fmt.Errorf("writing report: %w", err)
	}

	fmt.Println("==> report:", filepath.Join(runDir, "report.md"))

	genuineAnomalies := 0
	for _, a := range anomalies {
		if !a.Expected {
			genuineAnomalies++
		}
	}
	if len(findings) > 0 || genuineAnomalies > 0 {
		return fmt.Errorf("run completed with %d pod-health finding(s) and %d genuine log anomaly/anomalies — see report.md", len(findings), genuineAnomalies)
	}
	return nil
}

func validateFlags(clientCount, callsPerSecond, duration int) error {
	var problems []string
	if clientCount < 1 || clientCount > 5 {
		problems = append(problems, fmt.Sprintf("--client-count must be between 1 and 5, got %d", clientCount))
	}
	if callsPerSecond < 1 || callsPerSecond > 10 {
		problems = append(problems, fmt.Sprintf("--calls-per-second must be between 1 and 10, got %d", callsPerSecond))
	}
	if duration < 1 || duration > 60 {
		problems = append(problems, fmt.Sprintf("--duration must be between 1 and 60, got %d", duration))
	}
	if len(problems) > 0 {
		msg := "invalid flags:"
		for _, p := range problems {
			msg += "\n  - " + p
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func createRunDir() (string, error) {
	dir := filepath.Join("stress-test", "runs", time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling %s: %w", path, err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
