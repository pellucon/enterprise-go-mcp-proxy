// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package llmclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// claudeCLICommand is the binary invoked by ClaudeCodePrompt. A package
// variable (not a constant) so tests can point it at a stand-in script
// instead of requiring a real, locally-authenticated Claude Code install.
var claudeCLICommand = "claude"

// claudeCodeResult mirrors the subset of `claude -p ... --output-format
// json`'s JSON stdout this package cares about, for a single, non-streaming
// call (see https://code.claude.com/docs/en/headless.md).
type claudeCodeResult struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
}

// ClaudeCodePrompt sends prompt to a local, already-authenticated Claude
// Code CLI (`claude`) running in headless mode, and returns its final text
// response.
//
// Development-time only — never wire this into anything that's actually
// deployed. It shells out to a `claude` binary that must already be on
// PATH and logged in on the machine running this code, so its exact
// behavior depends on whatever's installed and authenticated there — the
// opposite of the reproducible, environment-independent behavior a
// deployed service needs. It exists so a caller (e.g. ai-powered-tools, run
// locally) can be exercised without a real LLM API key, using a
// developer's own Claude subscription instead of API billing. That's
// permitted for personal automation of your own work under Anthropic's
// Consumer Terms, but explicitly NOT for routing other users' requests
// through your subscription — see
// https://code.claude.com/docs/en/legal-and-compliance. A deployed backend
// serving real client traffic (which is exactly what ai-powered-tools is,
// once deployed) must keep using llmclient.ChatCompletion with a real API
// key; this function must never be reached from that path.
//
// Deliberately does not pass --bare: bare mode requires ANTHROPIC_API_KEY
// and skips the subscription-based OAuth login this function exists to use
// instead. To keep this repo's own CLAUDE.md and working files from
// bleeding into the prompt without needing --bare, the command runs in a
// throwaway temp directory instead of the caller's own working directory.
//
// The session has no tools at all, whatever the developer has set up for
// their own use: the prompt carries text other people control (a tool's
// arguments and results, a task, backend tool descriptions), so a prompt
// injection must find nothing to run on the developer's machine.
//   - --tools "" turns off every built-in tool;
//   - --strict-mcp-config, with no --mcp-config, loads no MCP servers;
//   - --setting-sources project loads no user settings (the developer's
//     pre-approved tools, hooks, MCP servers): only project settings,
//     and the throwaway working directory has none;
//   - --permission-mode dontAsk refuses, rather than prompts for, any tool
//     use that's left - headless mode has no user to ask.
//
// This keeps ClaudeCodePrompt a plain text-in/text-out call, the same
// contract as ChatCompletion.
func ClaudeCodePrompt(ctx context.Context, prompt string) (string, error) {
	return ClaudeCodePromptWithSystem(ctx, "", prompt)
}

// ClaudeCodePromptWithSystem is ClaudeCodePrompt with system, if set,
// passed as an addition to the system prompt (--append-system-prompt) -
// where a caller puts its instructions, so that data it passes in prompt
// can't pose as instructions as easily.
func ClaudeCodePromptWithSystem(ctx context.Context, system, prompt string) (string, error) {
	if _, err := exec.LookPath(claudeCLICommand); err != nil {
		return "", fmt.Errorf("claude CLI not found on PATH (%s): %w", claudeCLICommand, err)
	}

	workDir, err := os.MkdirTemp("", "claude-code-prompt-*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp working directory: %w", err)
	}
	defer os.RemoveAll(workDir)

	args := []string{
		"-p", prompt,
		"--output-format", "json",
		"--permission-mode", "dontAsk",
		"--tools", "",
		"--strict-mcp-config",
		"--setting-sources", "project",
	}
	if system != "" {
		args = append(args, "--append-system-prompt", system)
	}
	cmd := exec.CommandContext(ctx, claudeCLICommand, args...)
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("claude CLI failed: %w (stderr: %s)", err, stderr.String())
	}

	var result claudeCodeResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return "", fmt.Errorf("failed to parse claude CLI JSON output: %w (raw: %s)", err, stdout.String())
	}
	if result.IsError {
		return "", fmt.Errorf("claude CLI returned an error result: %s", result.Result)
	}
	return result.Result, nil
}
