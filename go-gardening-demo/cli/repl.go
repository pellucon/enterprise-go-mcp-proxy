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
func tokenize(line string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	inQuotes := false
	hasToken := false

	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '"':
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

// dispatch handles one already-tokenized command line, either the special
// "init" command or an ordinary "<section> <tool> [-flag value...]" one.
// Shared by one-shot mode and every REPL iteration, so both behave
// identically.
func dispatch(ctx context.Context, session *mcp.ClientSession, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("no command given (type help for full usage)")
	}
	if args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(topLevelUsage)
		return nil
	}
	if args[0] == "init" {
		return runInit(ctx, session, args[1:])
	}
	if len(args) < 2 {
		return fmt.Errorf("usage: <section> <tool> [-flag value...] (or: init [-data-dir DIR]) (type help for full usage)")
	}
	if args[0] == "admin" {
		return runAdminCommand(ctx, session, args[1], args[2:])
	}
	return runCommand(ctx, session, args[0], args[1], args[2:])
}

// runREPL is the CLI's default mode per the gardening-demo spec: "called
// once and then run in a loop to allow multiple call[s]". One line, one
// command, printed result, loop — until EOF or a literal exit/quit line.
//
// Reads via github.com/chzyer/readline rather than a plain bufio.Scanner
// specifically for its line-editing/history: up/down arrow recalls
// previously entered commands within the same session (in-memory only —
// never persisted to disk, so history doesn't survive past exit/quit).
func runREPL(ctx context.Context, session *mcp.ClientSession) {
	fmt.Println("go-gardening-demo cli — logged in.")
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
