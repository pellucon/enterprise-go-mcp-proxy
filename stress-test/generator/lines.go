// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// call is one planned tool invocation, kept in this structured form
// through the whole generation pipeline (weighting, add_supply_order/
// mark_supply_order_fulfilled pairing, insertion) — only the very last
// step (renderLine) turns it into the actual text line a cli-callsN.txt
// file holds.
type call struct {
	Tool string
	Args map[string]any
}

// renderLine renders one call as the exact line stress-test/driver's
// client.go will later feed into a `cli` REPL session's stdin:
//
//	call-tool -name <namespaced.tool> -args {"key":"value",...}
//
// CRITICAL: the -args value must be bare JSON with NO wrapping quotes at
// all (never `-args '{...}'`, never `-args "{...}"`) — cli/repl.go's own
// tokenize() only treats '"' as quote-toggling when it opens/closes a
// FRESH token, and a JSON object always starts with '{', so bare JSON
// passes through untouched while a quote-wrapped one would not (the
// wrapping quotes/embedded quotes would be parsed as token delimiters).
// See selfCheckTokenize below, which mechanically verifies this for every
// generated line before it's written to a file.
func renderLine(c call) (string, error) {
	argsJSON, err := json.Marshal(c.Args)
	if err != nil {
		return "", fmt.Errorf("marshaling args for %s: %w", c.Tool, err)
	}
	line := fmt.Sprintf("call-tool -name %s -args %s", c.Tool, argsJSON)

	tokens, err := selfCheckTokenize(line)
	if err != nil {
		return "", fmt.Errorf("generated line for %s failed self-check tokenizing: %w (line: %s)", c.Tool, err, line)
	}
	want := []string{"call-tool", "-name", c.Tool, "-args", string(argsJSON)}
	if !reflect.DeepEqual(tokens, want) {
		return "", fmt.Errorf("generated line for %s did not round-trip through the tokenizer as expected:\n  got:  %#v\n  want: %#v\n  line: %s", c.Tool, tokens, want, line)
	}
	return line, nil
}

// selfCheckTokenize is a private, exact port of cli/repl.go's tokenize()
// — duplicated rather than imported because cli/ is package main, not an
// importable library (the same "package main can't cross-import another
// package main" constraint documented throughout this repo, e.g.
// CLAUDE.md's business-logic-validation section). Any change to the real
// tokenizer's behavior must be mirrored here, or this self-check will
// silently stop catching real quoting regressions.
func selfCheckTokenize(line string) ([]string, error) {
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
