// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	cliBinPath    = "bin/cli"
	authServerURL = "http://localhost:8102"
	proxyEndpoint = "http://localhost/mcp"
)

// runSetup performs every precondition the confirmed design requires
// before any client starts issuing traffic: a live cluster, a full
// build+redeploy (which — verified directly against scripts/deploy.sh's
// deploy_folder — already force-restarts every pre-existing Deployment
// and reseeds gardening data via its own embedded `cli init` step), a
// positive confirmation that seeding actually succeeded (deploy.sh's own
// exit code is NOT sufficient — see below), a post-setup baseline pod
// snapshot for the teardown health check to diff against, and a
// reset_events call so the run starts with a clean event-aggregator
// buffer.
func runSetup(runDir string) ([]PodStatus, error) {
	if err := checkClusterReachable(); err != nil {
		return nil, err
	}

	if err := runLoggedCommand(runDir, "setup-build.log", "./scripts/build.sh"); err != nil {
		return nil, fmt.Errorf("./scripts/build.sh failed: %w", err)
	}

	// Deployed in two steps, not one combined --deploy-all-minus-ai
	// invocation, specifically so neutralizeAIGuardedOrchestration can run
	// in between — see orchestration_fix.go's own doc comment for why
	// this is necessary (not optional) for a second/later run of this
	// driver against a cluster that's ever had go-gardening-demo's real
	// orchestration.json applied before.
	if _, err := runLoggedCommandCapture(runDir, "setup-deploy-core.log", "./scripts/deploy.sh",
		"--deploy-proxy", "--deploy-platform-servers", "--deploy-auth-server",
		// The demo's own configuration, as the docs deploy it: a cluster
		// set up with it refuses a proxy deploy with any other file.
		"--config", "go-gardening-demo/gardening-demo.cfg"); err != nil {
		return nil, fmt.Errorf("./scripts/deploy.sh (core) failed: %w", err)
	}

	if err := neutralizeAIGuardedOrchestration(); err != nil {
		return nil, fmt.Errorf("pre-seed orchestration neutralization failed: %w", err)
	}

	deployOutput, err := runLoggedCommandCapture(runDir, "setup-deploy-gardening.log", "./scripts/deploy.sh", "--deploy-gardening-demo")
	if err != nil {
		return nil, fmt.Errorf("./scripts/deploy.sh (gardening-demo) failed: %w", err)
	}
	// deploy.sh wraps its own `cli init` reseed step so a failure there
	// only prints "WARNING: 'cli init' failed" and does NOT make the
	// overall script exit nonzero (verified directly: the call is inside
	// an `if ! ...; then echo WARNING ...; fi` specifically so a seeding
	// hiccup doesn't abort the whole deploy under `set -euo pipefail`).
	// A clean deploy.sh exit code is therefore not sufficient proof that
	// gardening data was actually reseeded.
	if strings.Contains(deployOutput, "WARNING: 'cli init' failed") {
		return nil, fmt.Errorf("./scripts/deploy.sh reported that 'cli init' (gardening data reseed) failed — see %s for details", filepath.Join(runDir, "setup-deploy-gardening.log"))
	}

	// cli init's own last-but-one step re-applies go-gardening-demo's
	// real orchestration.json wholesale, reintroducing the AI-dependent
	// steps this driver just stripped — strip them again so the affected
	// tools stay callable throughout the actual client-run phase, not
	// just during seeding.
	if err := neutralizeAIGuardedOrchestration(); err != nil {
		return nil, fmt.Errorf("post-seed orchestration neutralization failed: %w", err)
	}

	if err := verifySeeded(); err != nil {
		return nil, fmt.Errorf("post-deploy reseed verification failed: %w", err)
	}

	baseline, err := snapshotPods()
	if err != nil {
		return nil, fmt.Errorf("taking baseline pod snapshot: %w", err)
	}

	if stdout, stderr, err := runOneShotCall("event-aggregator.reset_events", "{}"); err != nil || strings.Contains(stderr, "Error:") {
		return nil, fmt.Errorf("event-aggregator.reset_events failed: err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}

	return baseline, nil
}

// checkClusterReachable fails fast, with a clear message, if the local
// Colima/k3s cluster isn't up — nothing existing in this repo checks this
// before running kubectl commands.
func checkClusterReachable() error {
	if err := exec.Command("kubectl", "cluster-info").Run(); err != nil {
		return fmt.Errorf("kubectl cluster-info failed — is Colima/k3s running? (try `colima start --kubernetes`): %w", err)
	}
	return nil
}

// verifySeeded makes one real call against a gardening backend and checks
// its result is non-empty (the seed data has 500 orders), as a positive
// confirmation that reseeding actually landed — independent of, and in
// addition to, the deploy-output string check above.
func verifySeeded() error {
	stdout, stderr, err := runOneShotCall("garden-online-store.get_customer_orders", "{}")
	if err != nil {
		return fmt.Errorf("garden-online-store.get_customer_orders call failed: %w (stderr: %s)", err, stderr)
	}
	if strings.Contains(stderr, "Error:") {
		return fmt.Errorf("garden-online-store.get_customer_orders returned an error: %s", stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		return fmt.Errorf("garden-online-store.get_customer_orders returned an empty result — gardening data does not appear to be seeded")
	}
	return nil
}

// runOneShotCall invokes bin/cli in one-shot (non-REPL) mode for a single
// call-tool invocation — used only during setup, where each call is rare
// and simple enough that a fresh login per call is a non-issue (unlike
// the client-run phase, which uses one long-lived REPL process per
// client specifically to avoid this overhead — see client.go).
func runOneShotCall(tool, argsJSON string) (stdout, stderr string, err error) {
	cmd := exec.Command(cliBinPath,
		"-username", "admin", "-password", "admin",
		"-auth-server", authServerURL, "-endpoint", proxyEndpoint,
		"call-tool", "-name", tool, "-args", argsJSON,
	)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	err = cmd.Run()
	return stdoutBuf.String(), stderrBuf.String(), err
}

// runLoggedCommand runs cmd, saves its combined output to
// <runDir>/<logName>, and returns an error if it exited nonzero.
func runLoggedCommand(runDir, logName, name string, args ...string) error {
	_, err := runLoggedCommandCapture(runDir, logName, name, args...)
	return err
}

// runLoggedCommandCapture is runLoggedCommand's sibling for callers (like
// the deploy.sh soft-fail check above) that also need the captured output
// text, not just pass/fail.
func runLoggedCommandCapture(runDir, logName, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()
	output := buf.String()
	if writeErr := os.WriteFile(filepath.Join(runDir, logName), []byte(output), 0o644); writeErr != nil {
		return output, fmt.Errorf("writing %s: %w", logName, writeErr)
	}
	return output, runErr
}
