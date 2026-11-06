// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapTextCutsLongStrings(t *testing.T) {
	long := strings.Repeat("é", maxTextBytes) // 2 bytes each: a cut can land mid-rune
	e := &Event{Type: "alert", Title: long, Text: "short", Metadata: map[string]any{"reason": long, "n": 3}}
	capped := capText(e)
	for name, s := range map[string]string{"title": capped.Title, "reason": capped.Metadata["reason"].(string)} {
		if len(s) > maxTextBytes+64 || !strings.Contains(s, "truncated") || !utf8.ValidString(s) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v; want it cut and marked", name, len(s), utf8.ValidString(s))
		}
	}
	if capped.Text != "short" || capped.Metadata["n"] != 3 {
		t.Error("short or non-string values changed")
	}
	if e.Title != long {
		t.Error("the caller's event was modified")
	}
}

func TestParseDictionaryRefusesSubjectUnsafeNames(t *testing.T) {
	for _, raw := range []string{
		`{"eventTypes":{"a.b":{}}}`,
		`{"eventTypes":{"alert":{"subtypes":["x>"]}}}`,
		`{"eventTypes":{"alert":{"subtypes":["has space"]}}}`,
	} {
		if _, err := ParseDictionary([]byte(raw)); err == nil {
			t.Errorf("ParseDictionary(%s) was accepted", raw)
		}
	}
	if _, err := ParseDictionary([]byte(`{"eventTypes":{"tool-request-result":{"subtypes":["call_failed"]}}}`)); err != nil {
		t.Errorf("a valid dictionary was refused: %v", err)
	}
}
