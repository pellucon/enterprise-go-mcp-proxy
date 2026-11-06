// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package guardrailcfg

import (
	"os"
	"strings"
	"testing"
	"time"
)

func mustParseUTC(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("failed to parse %q: %v", s, err)
	}
	return parsed
}

func TestCompileConfig_HappyPath(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block", "alert", "log"},
		Declarations: []RawDeclaration{
			{
				Name:           "no-dollar-sums",
				Constraint:     "dollar sums are not allowed",
				RoleExpression: "manager & developer",
				Actions:        []string{"block", "alert"},
			},
			{
				Name:           "scoped",
				Constraint:     "scoped declaration",
				RoleExpression: "_any_role",
				Actions:        []string{"log"},
				EffectiveDate:  &RawEffectiveDate{Start: mustDate("2026-01-01")},
				EffectiveTime: []RawEffectiveTimeWindow{
					{Days: []string{"Monday"}, StartTime: "09:00", EndTime: "17:00"},
				},
			},
		},
	}
	g, err := compileConfig(raw)
	if err != nil {
		t.Fatalf("compileConfig failed: %v", err)
	}
	if len(g.declarations) != 2 {
		t.Fatalf("expected 2 declarations, got %d", len(g.declarations))
	}
	if _, ok := g.declarations["no-dollar-sums"]; !ok {
		t.Fatal("expected declaration \"no-dollar-sums\" to be present")
	}
	if !g.actions["block"] || !g.actions["alert"] || !g.actions["log"] {
		t.Fatal("expected all three actions in the catalog")
	}
}

func TestCompileConfig_EmptyConstraintIsValid(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{Name: "x", RoleExpression: "_any_role", Actions: []string{"block"}},
		},
	}
	g, err := compileConfig(raw)
	if err != nil {
		t.Fatalf("compileConfig failed: %v", err)
	}
	decl, ok := g.declarations["x"]
	if !ok {
		t.Fatal("expected declaration \"x\" to be present")
	}
	if decl.constraint != "" {
		t.Fatalf("expected empty constraint, got %q", decl.constraint)
	}
}

func TestCompileConfig_NoActions(t *testing.T) {
	raw := &RawConfig{
		Declarations: []RawDeclaration{
			{Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"}},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: no actions catalog defined")
	}
}

func TestCompileConfig_NoDeclarationsIsValid(t *testing.T) {
	raw := &RawConfig{Actions: []string{"block"}}
	g, err := compileConfig(raw)
	if err != nil {
		t.Fatalf("compileConfig failed: %v", err)
	}
	if len(g.declarations) != 0 {
		t.Fatalf("expected 0 declarations, got %d", len(g.declarations))
	}
	if !g.actions["block"] {
		t.Fatal("expected the actions catalog to still be populated")
	}
}

func TestCompileConfig_MissingMandatoryField(t *testing.T) {
	base := RawDeclaration{Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"}}

	cases := []struct {
		name string
		mut  func(d RawDeclaration) RawDeclaration
	}{
		{"missing name", func(d RawDeclaration) RawDeclaration { d.Name = ""; return d }},
		{"missing roleExpression", func(d RawDeclaration) RawDeclaration { d.RoleExpression = ""; return d }},
		{"missing actions", func(d RawDeclaration) RawDeclaration { d.Actions = nil; return d }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := &RawConfig{Actions: []string{"block"}, Declarations: []RawDeclaration{c.mut(base)}}
			if _, err := compileConfig(raw); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

func TestCompileConfig_UnknownAction(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"quarantine"}},
		},
	}
	_, err := compileConfig(raw)
	if err == nil {
		t.Fatal("expected error: action not in catalog")
	}
	if !strings.Contains(err.Error(), "quarantine") {
		t.Fatalf("expected error to mention the offending action, got: %v", err)
	}
}

func TestCompileConfig_MalformedRoleExpression(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{Name: "x", Constraint: "x", RoleExpression: "manager &", Actions: []string{"block"}},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: malformed role expression")
	}
}

func TestCompileConfig_MalformedEffectiveDate(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{
				Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"},
				EffectiveDate: &RawEffectiveDate{Start: mustDate("not-a-date")},
			},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: malformed effectiveDate")
	}
}

func TestCompileConfig_TimezoneAppliesToEffectiveDateAndTime(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"alert"},
		Declarations: []RawDeclaration{
			{
				Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"alert"},
				Timezone:      "America/New_York",
				EffectiveDate: &RawEffectiveDate{Start: mustDate("2026-01-01")},
				EffectiveTime: []RawEffectiveTimeWindow{{StartTime: "09:00", EndTime: "17:00"}},
			},
		},
	}
	g, err := compileConfig(raw)
	if err != nil {
		t.Fatalf("compileConfig failed: %v", err)
	}
	decl := g.declarations["x"]

	// Midnight Jan 1 in America/New_York (EST, UTC-5) is 05:00 UTC.
	if decl.effectiveDate.Active(mustParseUTC(t, "2026-01-01T04:00:00Z")) {
		t.Fatal("expected inactive: still Dec 31 in America/New_York")
	}
	if !decl.effectiveDate.Active(mustParseUTC(t, "2026-01-01T05:00:00Z")) {
		t.Fatal("expected active: midnight Jan 1 in America/New_York")
	}

	// 14:00 UTC is 09:00 EST, the start of the effectiveTime window.
	if !decl.effectiveTime.Active(mustParseUTC(t, "2026-01-05T14:00:00Z")) {
		t.Fatal("expected active: 14:00 UTC is 09:00 EST")
	}
	if decl.effectiveTime.Active(mustParseUTC(t, "2026-01-05T13:00:00Z")) {
		t.Fatal("expected inactive: 13:00 UTC is 08:00 EST, before the window")
	}
}

func TestCompileConfig_InvalidTimezone(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{
				Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"},
				Timezone:      "Not/A_Real_Zone",
				EffectiveDate: &RawEffectiveDate{Start: mustDate("2026-01-01")},
			},
		},
	}
	_, err := compileConfig(raw)
	if err == nil {
		t.Fatal("expected error: invalid timezone")
	}
	if !strings.Contains(err.Error(), "Not/A_Real_Zone") {
		t.Fatalf("expected error to mention the offending timezone, got: %v", err)
	}
}

func TestCompileConfig_MalformedEffectiveTime(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{
				Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"},
				EffectiveTime: []RawEffectiveTimeWindow{{StartTime: "not-a-time", EndTime: "17:00"}},
			},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: malformed effectiveTime")
	}
}

func TestCompileConfig_DuplicateDeclarationName(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block"},
		Declarations: []RawDeclaration{
			{Name: "dup", Constraint: "a", RoleExpression: "_any_role", Actions: []string{"block"}},
			{Name: "dup", Constraint: "b", RoleExpression: "_any_role", Actions: []string{"block"}},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: duplicate declaration name")
	}
}

func TestCompileConfig_DuplicateActionName(t *testing.T) {
	raw := &RawConfig{
		Actions: []string{"block", "block"},
		Declarations: []RawDeclaration{
			{Name: "x", Constraint: "x", RoleExpression: "_any_role", Actions: []string{"block"}},
		},
	}
	if _, err := compileConfig(raw); err == nil {
		t.Fatal("expected error: duplicate action name")
	}
}

func TestLoadConfig_MissingFile(t *testing.T) {
	if _, err := LoadConfig("/nonexistent/path/guardrails.json"); err == nil {
		t.Fatal("expected error for missing config file")
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/guardrails.json"
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
