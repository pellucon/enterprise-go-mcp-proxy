// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"encoding/json"
	"testing"
)

func TestParseConfigValid(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"callAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "call-authorization" },
					{ "server": "authorization-mcp-server", "tool": "call-authorization-role-check" }
				],
				"resultAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "result-authorization" }
				],
				"toolCallAudit": ["publish-tool-request-result"]
			}
		},
		"toolMappings": [
			{ "pattern": "orchestration-demo.*", "orchestration": "standard-guardrails" }
		]
	}`)

	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig returned error for valid JSON: %v", err)
	}
	if len(cfg.Orchestrations) != 1 {
		t.Fatalf("expected 1 orchestration, got %d", len(cfg.Orchestrations))
	}
	orch, ok := cfg.Orchestrations["standard-guardrails"]
	if !ok {
		t.Fatal("expected orchestration \"standard-guardrails\" to be present")
	}
	if len(orch.CallAuthorization) != 2 {
		t.Fatalf("expected 2 callAuthorization steps, got %d", len(orch.CallAuthorization))
	}
	if len(orch.ResultAuthorization) != 1 {
		t.Fatalf("expected 1 resultAuthorization step, got %d", len(orch.ResultAuthorization))
	}
	if len(orch.ToolCallAudit) != 1 || orch.ToolCallAudit[0] != ToolCallAuditSentinel {
		t.Fatalf("expected toolCallAudit to be [%q], got %v", ToolCallAuditSentinel, orch.ToolCallAudit)
	}
	if len(cfg.ToolMappings) != 1 || cfg.ToolMappings[0].Pattern != "orchestration-demo.*" {
		t.Fatalf("unexpected toolMappings: %+v", cfg.ToolMappings)
	}
}

func TestParseConfigMalformedJSON(t *testing.T) {
	data := []byte(`{ "orchestrations": { `)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestParseConfigToolMappingReferencesUnknownOrchestration(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {}
		},
		"toolMappings": [
			{ "pattern": "*.*", "orchestration": "does-not-exist" }
		]
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for toolMapping referencing unknown orchestration, got nil")
	}
}

func TestParseConfigToolCallAuditDuplicateEntry(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"toolCallAudit": ["publish-tool-request-result", "publish-tool-request-result"]
			}
		}
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for toolCallAudit with a duplicate entry, got nil")
	}
}

func TestParseConfigToolCallAuditWrongValue(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"toolCallAudit": ["not-the-sentinel"]
			}
		}
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for toolCallAudit entry that isn't a recognized sentinel, got nil")
	}
}

func TestParseConfigToolCallAuditBothSentinels(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"toolCallAudit": ["publish-tool-request-result", "publish-tool-request"]
			}
		}
	}`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("expected both sentinels together to be valid, got error: %v", err)
	}
	audit := cfg.Orchestrations["standard-guardrails"].ToolCallAudit
	if len(audit) != 2 {
		t.Fatalf("expected 2 toolCallAudit entries, got %v", audit)
	}
}

func TestParseConfigToolCallAuditJustRequest(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"toolCallAudit": ["publish-tool-request"]
			}
		}
	}`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("expected publish-tool-request alone to be valid, got error: %v", err)
	}
	audit := cfg.Orchestrations["standard-guardrails"].ToolCallAudit
	if len(audit) != 1 || audit[0] != ToolRequestAuditSentinel {
		t.Fatalf("expected toolCallAudit=[%q], got %v", ToolRequestAuditSentinel, audit)
	}
}

func TestParseConfigToolCallAuditThreeEntries(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"toolCallAudit": ["publish-tool-request-result", "publish-tool-request", "publish-tool-request-result"]
			}
		}
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for toolCallAudit with more entries than known sentinels, got nil")
	}
}

func TestParseConfigStepParametersValidJSON(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"callAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "call-authorization-role-check", "parameters": "{\"minRole\":\"user\",\"team\":\"platform-security\"}" }
				],
				"resultAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "result-authorization-length-check", "parameters": "{\"maxLength\":50}" }
				]
			}
		}
	}`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig returned error for valid parameters JSON: %v", err)
	}
	orch := cfg.Orchestrations["standard-guardrails"]
	if got := orch.CallAuthorization[0].Parameters; got != `{"minRole":"user","team":"platform-security"}` {
		t.Errorf("unexpected CallAuthorization[0].Parameters: %q", got)
	}
	if got := orch.ResultAuthorization[0].Parameters; got != `{"maxLength":50}` {
		t.Errorf("unexpected ResultAuthorization[0].Parameters: %q", got)
	}
}

func TestParseConfigStepParametersEmptyIsFine(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"callAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "call-authorization" }
				]
			}
		}
	}`)
	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig returned error for a step with no parameters: %v", err)
	}
	if got := cfg.Orchestrations["standard-guardrails"].CallAuthorization[0].Parameters; got != "" {
		t.Errorf("expected empty Parameters, got %q", got)
	}
}

func TestParseConfigStepParametersInvalidJSON(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"callAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "call-authorization", "parameters": "not valid json" }
				]
			}
		}
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for a callAuthorization step with invalid JSON parameters, got nil")
	}
}

func TestParseConfigResultStepParametersInvalidJSON(t *testing.T) {
	data := []byte(`{
		"orchestrations": {
			"standard-guardrails": {
				"resultAuthorization": [
					{ "server": "authorization-mcp-server", "tool": "result-authorization", "parameters": "{not valid" }
				]
			}
		}
	}`)
	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for a resultAuthorization step with invalid JSON parameters, got nil")
	}
}

// TestParseConfigStepParametersNonObjectJSON covers parameters that are
// syntactically valid JSON but not an object (a scalar or an array) — this
// must still be rejected, since every consumer of Parameters expects to
// decode named fields out of it (see authorization-mcp-server's
// logParameters). json.Valid alone would accept these; validateStepParameters
// must not.
func TestParseConfigStepParametersNonObjectJSON(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parameters string
	}{
		{"number", "42"},
		{"string", `"hello"`},
		{"array", "[1,2,3]"},
		{"bool", "true"},
		{"null", "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{
				"orchestrations": {
					"standard-guardrails": {
						"callAuthorization": [
							{ "server": "authorization-mcp-server", "tool": "call-authorization", "parameters": ` + toJSONStringLiteral(tc.parameters) + ` }
						]
					}
				}
			}`)
			if _, err := ParseConfig(data); err == nil {
				t.Fatalf("expected error for non-object parameters %q, got nil", tc.parameters)
			}
		})
	}
}

// toJSONStringLiteral encodes s (itself already a JSON-text fragment, e.g.
// "42" or "[1,2,3]") as a JSON string literal, so it can be embedded as an
// OrchestrationStep.Parameters value in a test fixture without hand-escaping.
func toJSONStringLiteral(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
