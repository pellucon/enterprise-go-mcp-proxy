// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"reflect"
	"sort"
	"testing"

	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

func TestTrustedServerNames(t *testing.T) {
	orch := &triggered_tools_runtime.OrchestrationConfig{Orchestrations: map[string]triggered_tools_runtime.Orchestration{
		"remote": {CallAuthorization: []triggered_tools_runtime.OrchestrationStep{{Server: "authorization-server", Tool: "check"}}},
		"native": {
			CallAuthorization: []triggered_tools_runtime.OrchestrationStep{
				{Server: "proxy_utility", Tool: "request_constraints_validation", Parameters: `{"declarationNames":["a"],"guardrailServer":"other-guardrail"}`},
				{Server: "proxy_utility", Tool: "business_logic_constraints_validation", Parameters: `{"declarationNames":["b"]}`},
			},
			ResultAuthorization: []triggered_tools_runtime.OrchestrationStep{
				{Server: "proxy_utility", Tool: "result_ai_constraints_validation", Parameters: `{"declarationNames":["c"],"aiValidatorServer":"other-ai"}`},
			},
		},
	}}
	relay := map[string][]string{"garden-online-store": {"garden-customer-admin.get_customer_type"}}

	got := trustedServerNames(orch, relay)
	var names []string
	for name := range got {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"ai-powered-tools", "authorization-server", "declarative-guardrail", "garden-online-store", "other-ai", "other-guardrail", "rate-limiter"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("trusted names = %v; want %v", names, want)
	}
	// The relay target and proxy_utility itself are never trusted names.
	for _, name := range []string{"garden-customer-admin", "proxy_utility"} {
		if _, ok := got[name]; ok {
			t.Errorf("%q must not need a pin", name)
		}
	}
	if got["garden-online-store"] != "an internalAuth.relay source" {
		t.Errorf("garden-online-store's reason = %q", got["garden-online-store"])
	}
	// Deterministic: the same inputs give the same reasons.
	if again := trustedServerNames(orch, relay); !reflect.DeepEqual(again, got) {
		t.Errorf("not deterministic: %v vs %v", again, got)
	}
}

func TestTrustedServerNamesWithoutConfig(t *testing.T) {
	got := trustedServerNames(nil, nil)
	if len(got) != 3 {
		t.Errorf("with no orchestration or relay, want just the three platform servers; got %v", got)
	}
}
