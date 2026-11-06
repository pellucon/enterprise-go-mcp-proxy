// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package llmclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeClaudeCLI writes a stand-in shell script for the real `claude`
// binary, and points claudeCLICommand at it for the duration of the test —
// so these tests are hermetic and don't require a real, locally-
// authenticated Claude Code install. The script echoes back the prompt it
// received (the argument right after "-p") inside a canned JSON result, so
// tests can assert the CLI was invoked with the expected argument.
func writeFakeClaudeCLI(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("failed to write fake claude CLI: %v", err)
	}
	original := claudeCLICommand
	claudeCLICommand = path
	t.Cleanup(func() { claudeCLICommand = original })
}

func TestClaudeCodePromptHappyPath(t *testing.T) {
	writeFakeClaudeCLI(t, `
while [ "$#" -gt 0 ]; do
  case "$1" in
    -p) prompt="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '{"result": "echo: %s", "is_error": false}' "$prompt"
`)

	got, err := ClaudeCodePrompt(context.Background(), "what tools do I have?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "echo: what tools do I have?"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestClaudeCodePromptPassesExpectedFlags(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	writeFakeClaudeCLI(t, fmt.Sprintf(`
echo "$@" > %s
printf '{"result": "ok", "is_error": false}'
`, argsFile))

	if _, err := ClaudeCodePrompt(context.Background(), "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read captured args: %v", err)
	}
	args := strings.TrimSpace(string(argsBytes))
	for _, want := range []string{"-p hello", "--output-format json", "--permission-mode dontAsk"} {
		if !strings.Contains(args, want) {
			t.Errorf("expected invocation to contain %q, got: %s", want, args)
		}
	}
}

func TestClaudeCodePromptReturnsErrorOnIsErrorResult(t *testing.T) {
	writeFakeClaudeCLI(t, `printf '{"result": "something went wrong", "is_error": true}'`)

	if _, err := ClaudeCodePrompt(context.Background(), "task"); err == nil {
		t.Error("expected an error when the CLI reports is_error: true")
	}
}

func TestClaudeCodePromptReturnsErrorOnNonZeroExit(t *testing.T) {
	writeFakeClaudeCLI(t, `echo "boom" >&2; exit 1`)

	if _, err := ClaudeCodePrompt(context.Background(), "task"); err == nil {
		t.Error("expected an error when the CLI exits non-zero")
	}
}

func TestClaudeCodePromptReturnsErrorOnInvalidJSON(t *testing.T) {
	writeFakeClaudeCLI(t, `printf 'not json at all'`)

	if _, err := ClaudeCodePrompt(context.Background(), "task"); err == nil {
		t.Error("expected an error when the CLI output isn't valid JSON")
	}
}

func TestClaudeCodePromptReturnsErrorWhenBinaryMissing(t *testing.T) {
	original := claudeCLICommand
	claudeCLICommand = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { claudeCLICommand = original })

	if _, err := ClaudeCodePrompt(context.Background(), "task"); err == nil {
		t.Error("expected an error when the claude CLI binary isn't found on PATH")
	}
}

func TestClaudeCodePromptWithSystemAppendsSystemPrompt(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args.txt")
	writeFakeClaudeCLI(t, fmt.Sprintf(`
echo "$@" > %s
printf '{"result": "ok", "is_error": false}'
`, argsFile))

	if _, err := ClaudeCodePromptWithSystem(context.Background(), "the-rules", "the-data"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	argsBytes, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("failed to read captured args: %v", err)
	}
	if args := string(argsBytes); !strings.Contains(args, "--append-system-prompt the-rules") || !strings.Contains(args, "-p the-data") {
		t.Errorf("expected the system prompt appended and the data as the prompt, got: %s", args)
	}
}
