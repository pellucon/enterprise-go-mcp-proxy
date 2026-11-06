// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package configmerge

import (
	"reflect"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

func TestIsGardenOnlyRule(t *testing.T) {
	cases := []struct {
		name string
		rule access_control.AccessibilityRule
		want bool
	}{
		{"all garden-namespaced", access_control.AccessibilityRule{AllowedTools: []string{"garden-customer-admin.*", "garden-staff-admin.*"}}, true},
		{"admin wildcard", access_control.AccessibilityRule{AllowedTools: []string{"*"}}, false},
		{"mixed", access_control.AccessibilityRule{AllowedTools: []string{"garden-customer-admin.*", "proxy_utility.call_tool"}}, false},
		{"empty", access_control.AccessibilityRule{AllowedTools: nil}, false},
	}
	for _, c := range cases {
		if got := isGardenOnlyRule(c.rule); got != c.want {
			t.Errorf("%s: isGardenOnlyRule(%#v) = %v, want %v", c.name, c.rule, got, c.want)
		}
	}
}

func TestMergeAccessRules(t *testing.T) {
	current := []access_control.AccessibilityRule{
		{Role: "admin", AllowedTools: []string{"*"}},
		{Role: "*", AllowedTools: []string{"proxy_utility.call_tool"}},
		{Role: "*", AllowedTools: []string{"garden-customer-admin.*", "garden-staff-admin.*"}},
	}
	newRules := []access_control.AccessibilityRule{
		{Role: "admin", AllowedTools: []string{"*"}, AllowedPrompts: []string{"*"}},
		{Role: "business-manager", AllowedTools: []string{"garden-customer-admin.*"}},
		{Role: "customer", AllowedTools: []string{"garden-online-store.purchase_products"}},
	}

	want := []access_control.AccessibilityRule{
		{Role: "*", AllowedTools: []string{"proxy_utility.call_tool"}},
		{Role: "admin", AllowedTools: []string{"*"}, AllowedPrompts: []string{"*"}},
		{Role: "business-manager", AllowedTools: []string{"garden-customer-admin.*"}},
		{Role: "customer", AllowedTools: []string{"garden-online-store.purchase_products"}},
	}

	// The old blanket "*" garden-only rule is dropped (isGardenOnlyRule),
	// and the old admin/["*"] rule is replaced (same role as newRules'
	// own admin entry) rather than left duplicated alongside it -
	// proxy_utility.call_tool (role "*", not garden-only, not a role
	// newRules mentions) survives untouched.
	got := MergeAccessRules(current, newRules)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}

	// Applying the same newRules a second time (simulating a repeat run)
	// must be idempotent - no duplicate admin/business-manager/customer
	// rules accumulating.
	again := MergeAccessRules(got, newRules)
	if !reflect.DeepEqual(again, want) {
		t.Errorf("second merge not idempotent: got %#v, want %#v", again, want)
	}
}

func TestMergeDeclarativeGuardrails(t *testing.T) {
	current := guardrailcfg.RawConfig{
		Actions: []string{"block", "log"},
		Declarations: []guardrailcfg.RawDeclaration{
			{Name: "existing-only", Actions: []string{"log"}},
			{Name: "will-be-replaced", Constraint: "old", Actions: []string{"log"}},
		},
	}
	newDeclarations := []guardrailcfg.RawDeclaration{
		{Name: "will-be-replaced", Constraint: "new", Actions: []string{"block", "alert"}},
		{Name: "brand-new", Actions: []string{"alert"}},
	}

	gotActions, gotDecls := MergeDeclarativeGuardrails(current, newDeclarations)

	wantDecls := []guardrailcfg.RawDeclaration{
		{Name: "existing-only", Actions: []string{"log"}},
		{Name: "will-be-replaced", Constraint: "new", Actions: []string{"block", "alert"}},
		{Name: "brand-new", Actions: []string{"alert"}},
	}
	if !reflect.DeepEqual(gotDecls, wantDecls) {
		t.Errorf("declarations: got %#v, want %#v", gotDecls, wantDecls)
	}

	// "block"/"log" already present; "alert" newly referenced by
	// newDeclarations must be unioned in.
	wantActionSet := map[string]bool{"block": true, "log": true, "alert": true}
	if len(gotActions) != len(wantActionSet) {
		t.Errorf("actions: got %v, want set %v", gotActions, wantActionSet)
	}
	for _, a := range gotActions {
		if !wantActionSet[a] {
			t.Errorf("unexpected action %q in %v", a, gotActions)
		}
	}

	// Idempotent: merging the same newDeclarations again must not
	// duplicate "brand-new"/"will-be-replaced" or re-grow the action set.
	againActions, againDecls := MergeDeclarativeGuardrails(guardrailcfg.RawConfig{Actions: gotActions, Declarations: gotDecls}, newDeclarations)
	if !reflect.DeepEqual(againDecls, wantDecls) {
		t.Errorf("second merge not idempotent: got %#v, want %#v", againDecls, wantDecls)
	}
	if len(againActions) != len(wantActionSet) {
		t.Errorf("second merge action set grew: got %v", againActions)
	}
}

func TestMergeOrchestration(t *testing.T) {
	current := triggered_tools_runtime.OrchestrationConfig{
		Orchestrations: map[string]triggered_tools_runtime.Orchestration{
			"existing-only":    {ToolCallAudit: []string{"publish-tool-request-result"}},
			"will-be-replaced": {ToolCallAudit: []string{"publish-tool-request"}},
		},
		ToolMappings: []triggered_tools_runtime.ToolMapping{
			{Pattern: "*", Orchestration: "existing-only"},
			{Pattern: "server.will-be-replaced-pattern", Orchestration: "will-be-replaced"},
		},
	}
	newConfig := triggered_tools_runtime.OrchestrationConfig{
		Orchestrations: map[string]triggered_tools_runtime.Orchestration{
			"will-be-replaced": {ToolCallAudit: []string{"publish-tool-request-result", "publish-tool-request"}},
			"brand-new":        {},
		},
		ToolMappings: []triggered_tools_runtime.ToolMapping{
			{Pattern: "server.will-be-replaced-pattern", Orchestration: "will-be-replaced"},
			{Pattern: "server.brand-new-pattern", Orchestration: "brand-new"},
		},
	}

	got := MergeOrchestration(current, newConfig)

	if len(got.Orchestrations) != 3 {
		t.Fatalf("expected 3 orchestrations, got %d: %#v", len(got.Orchestrations), got.Orchestrations)
	}
	if !reflect.DeepEqual(got.Orchestrations["will-be-replaced"], newConfig.Orchestrations["will-be-replaced"]) {
		t.Errorf("will-be-replaced not overwritten: got %#v", got.Orchestrations["will-be-replaced"])
	}
	if _, ok := got.Orchestrations["existing-only"]; !ok {
		t.Error("existing-only orchestration was dropped")
	}

	// newConfig's own mappings must precede current's untouched mapping
	// (the pre-existing catch-all "*"), matching first-match-wins.
	wantMappings := []triggered_tools_runtime.ToolMapping{
		{Pattern: "server.will-be-replaced-pattern", Orchestration: "will-be-replaced"},
		{Pattern: "server.brand-new-pattern", Orchestration: "brand-new"},
		{Pattern: "*", Orchestration: "existing-only"},
	}
	if !reflect.DeepEqual(got.ToolMappings, wantMappings) {
		t.Errorf("toolMappings: got %#v, want %#v", got.ToolMappings, wantMappings)
	}

	// Idempotent: merging newConfig again into the result must not
	// duplicate the "will-be-replaced"/"brand-new" mappings.
	again := MergeOrchestration(got, newConfig)
	if !reflect.DeepEqual(again.ToolMappings, wantMappings) {
		t.Errorf("second merge not idempotent: got %#v, want %#v", again.ToolMappings, wantMappings)
	}
}
