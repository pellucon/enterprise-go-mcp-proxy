// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import "testing"

func TestParseConfigValid(t *testing.T) {
	data := []byte(`{
		"clientAccessRules": [
			{ "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] },
			{ "role": "finance", "allowed_tools": ["analytics_transactions"] }
		]
	}`)

	cfg, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig returned error for valid JSON: %v", err)
	}
	if len(cfg.ClientAccessRules) != 2 {
		t.Fatalf("expected 2 rules, got %d", len(cfg.ClientAccessRules))
	}
	if cfg.ClientAccessRules[0].Role != "admin" {
		t.Errorf("expected first rule role %q, got %q", "admin", cfg.ClientAccessRules[0].Role)
	}
	if len(cfg.ClientAccessRules[0].AllowedTools) != 1 || cfg.ClientAccessRules[0].AllowedTools[0] != "*" {
		t.Errorf("unexpected AllowedTools for admin rule: %v", cfg.ClientAccessRules[0].AllowedTools)
	}
	if cfg.ClientAccessRules[1].Role != "finance" {
		t.Errorf("expected second rule role %q, got %q", "finance", cfg.ClientAccessRules[1].Role)
	}
}

func TestParseConfigMalformed(t *testing.T) {
	data := []byte(`{ "clientAccessRules": [ { "role": "admin", `)

	if _, err := ParseConfig(data); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}
