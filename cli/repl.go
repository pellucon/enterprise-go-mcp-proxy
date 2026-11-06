// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/chzyer/readline"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tokenize is a small, quote-aware whitespace splitter — enough to let a
// REPL line's values contain spaces (e.g. -name "Alice Smith") without
// pulling in a full shell-parsing dependency. No escape-sequence support
// beyond that; good enough for this CLI's own flag values.
//
// A '"' only opens/closes quoting when it starts a fresh token (i.e. no
// characters have been accumulated for the current token yet) or when
// already inside an open quote (closing it) — it's treated as an ordinary
// literal character anywhere else. This differs from a naive "every quote
// toggles" splitter specifically so a JSON blob typed as a flag value (e.g.
// -args {"limit":10}, -config {"version":1,...}) passes through unscathed:
// such a token starts with '{', not '"', so its embedded quotes are never
// treated as delimiters at all.
func tokenize(line string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	hasToken := false

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"' && (!hasToken || inQuotes):
			inQuotes = !inQuotes
			hasToken = true
		case c == ' ' && !inQuotes:
			if hasToken {
				tokens = append(tokens, cur.String())
				cur.Reset()
				hasToken = false
			}
		default:
			cur.WriteByte(c)
			hasToken = true
		}
	}
	if inQuotes {
		return nil, fmt.Errorf("unterminated quote")
	}
	if hasToken {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

// dispatch handles one already-tokenized command line. Shared by one-shot
// mode and every REPL iteration, so both behave identically: it always
// returns an error rather than exiting the process itself — the caller
// (one-shot main(), or runREPL below) decides whether that's fatal.
func dispatch(ctx context.Context, session *mcp.ClientSession, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command given (type help for full usage)")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Print(topLevelUsage)
		return nil
	case "list-tools":
		return runListTools(ctx, session, args[1:])
	case "list-prompts":
		return runListPrompts(ctx, session, args[1:])
	case "list-resources":
		return runListResources(ctx, session, args[1:])
	case "call-tool":
		return runCallTool(ctx, session, args[1:])
	case "get-prompt":
		return runGetPrompt(ctx, session, args[1:])
	case "read-resource":
		return runReadResource(ctx, session, args[1:])
	case "export-role-permissions":
		return runExportSection(ctx, session, "export-role-permissions", "_admin.export_role_permissions")
	case "update-role-permissions":
		return runUpdateSection(ctx, session, "update-role-permissions", "_admin.update_role_permissions", args[1:])
	case "export-orchestration":
		return runExportSection(ctx, session, "export-orchestration", "_admin.export_orchestration")
	case "update-orchestration":
		return runUpdateSection(ctx, session, "update-orchestration", "_admin.update_orchestration", args[1:])
	case "export-discovery":
		return runExportSection(ctx, session, "export-discovery", "_admin.export_discovery")
	case "update-discovery":
		return runUpdateSection(ctx, session, "update-discovery", "_admin.update_discovery", args[1:])
	case "export-event-dictionary":
		return runExportSection(ctx, session, "export-event-dictionary", "_admin.export_event_dictionary")
	case "update-event-dictionary":
		return runUpdateSection(ctx, session, "update-event-dictionary", "_admin.update_event_dictionary", args[1:])
	case "export-declarative-guardrails":
		return runExportSection(ctx, session, "export-declarative-guardrails", "_admin.export_declarative_guardrails")
	case "update-declarative-guardrails":
		return runUpdateSection(ctx, session, "update-declarative-guardrails", "_admin.update_declarative_guardrails", args[1:])
	case "export-event-aggregation":
		return runExportSection(ctx, session, "export-event-aggregation", "_admin.export_event_aggregation")
	case "update-event-aggregation":
		return runUpdateSection(ctx, session, "update-event-aggregation", "_admin.update_event_aggregation", args[1:])
	case "export-rate-limiting":
		return runExportSection(ctx, session, "export-rate-limiting", "_admin.export_rate_limiting")
	case "update-rate-limiting":
		return runUpdateSection(ctx, session, "update-rate-limiting", "_admin.update_rate_limiting", args[1:])
	case "export-log-level":
		return runExportSection(ctx, session, "export-log-level", "_admin.export_log_level")
	case "update-log-level":
		return runUpdateSection(ctx, session, "update-log-level", "_admin.update_log_level", args[1:])
	case "export-full-config":
		return runExportSection(ctx, session, "export-full-config", "_admin.export_full_config")
	case "find-relevant-tools":
		return runFindRelevantTools(ctx, session, args[1:])
	case "list-events":
		return runListEvents(ctx, session, args[1:])
	case "load-events":
		return runLoadEvents(ctx, session, args[1:])
	default:
		return fmt.Errorf("unknown command: %s (type help for full usage)", args[0])
	}
}

// runREPL is the CLI's interactive mode: authenticate once (already done by
// the time this is called), then read one command per line until
// exit/quit/EOF. Reads via github.com/chzyer/readline rather than a plain
// bufio.Scanner specifically for its line-editing/history: up/down arrow
// recalls previously entered commands within the same session (in-memory
// only — never persisted to disk, so history doesn't survive past
// exit/quit).
func runREPL(ctx context.Context, session *mcp.ClientSession) {
	fmt.Println("cli — logged in.")
	fmt.Println(`Type help for the full usage text, or exit/quit to leave.`)

	rl, err := readline.New("> ")
	if err != nil {
		fatal(fmt.Errorf("failed to initialize the interactive prompt: %w", err))
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err != nil {
			if errors.Is(err, readline.ErrInterrupt) {
				continue
			}
			return // io.EOF (Ctrl-D) or any other read failure
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return
		}
		tokens, err := tokenize(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
			continue
		}
		if err := dispatch(ctx, session, tokens); err != nil {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
	}
}
