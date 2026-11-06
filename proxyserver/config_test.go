// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "proxy-server.cfg")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

// minimalRoleMapping is the required "roleMapping" section (see
// access_control.ParseRoleMapping — there is no default), included in
// every fixture below except the ones testing roleMapping itself, so each
// negative test fails for the reason it's about rather than for a missing
// mapping.
const minimalRoleMapping = `,
		"roleMapping": { "administrator": "admin" }`

// minimalEventingSections is the smallest valid "eventDictionary"/
// "declarativeGuardrails" pair plus minimalRoleMapping, appended to every
// fixture below that expects loadProxyConfig to succeed — all three
// sections are always required (not pointer-nil-optional, see config.go),
// so every prior fixture that predates their addition needs them to still
// parse.
const minimalEventingSections = minimalRoleMapping + `,
		"eventDictionary": { "eventTypes": {} },
		"declarativeGuardrails": { "actions": ["log"], "declarations": [] }`

func TestLoadProxyConfigValid(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": [
				{ "serverName": "mcp1", "serverUrl": "http://localhost:8082/mcp" }
			]
		},
		"accessRules": {
			"clientAccessRules": [
				{ "role": "admin", "allowed_tools": ["*"] }
			]
		},
		"auth": {
			"publicKeyPath": "/tmp/public.pem",
			"jwksUrl": "",
			"issuer": "",
			"audience": ""
		}`+minimalEventingSections+`
	}`)

	cfg, err := loadProxyConfig(path)
	if err != nil {
		t.Fatalf("loadProxyConfig returned error: %v", err)
	}
	if cfg.Discovery == nil {
		t.Fatal("expected non-nil Discovery")
	}
	if cfg.AccessRules == nil || len(cfg.AccessRules.ClientAccessRules) != 1 {
		t.Fatalf("expected 1 access rule, got %+v", cfg.AccessRules)
	}
	if cfg.Auth == nil {
		t.Fatal("expected non-nil Auth when \"auth\" section is present")
	}
	if cfg.Auth.PublicKeyPath != "/tmp/public.pem" {
		t.Errorf("expected PublicKeyPath %q, got %q", "/tmp/public.pem", cfg.Auth.PublicKeyPath)
	}
	if cfg.EventDictionary == nil {
		t.Fatal("expected non-nil EventDictionary")
	}
	if cfg.DeclarativeGuardrails == nil {
		t.Fatal("expected non-nil DeclarativeGuardrails")
	}
}

func TestLoadProxyConfigMissingAuthSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": []
		},
		"accessRules": {
			"clientAccessRules": []
		}`+minimalEventingSections+`
	}`)

	cfg, err := loadProxyConfig(path)
	if err != nil {
		t.Fatalf("loadProxyConfig returned error: %v", err)
	}
	if cfg.Auth != nil {
		t.Errorf("expected nil Auth when \"auth\" section is absent, got %+v", cfg.Auth)
	}
}

func TestLoadProxyConfigMalformedJSON(t *testing.T) {
	path := writeTempConfig(t, `{ "discovery": { `)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error for malformed top-level JSON, got nil")
	}
}

func TestLoadProxyConfigMalformedAccessRulesSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": []
		},
		"accessRules": "not-an-object"`+minimalEventingSections+`
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error for malformed \"accessRules\" section, got nil")
	}
}

func TestLoadProxyConfigMissingFile(t *testing.T) {
	if _, err := loadProxyConfig(filepath.Join(t.TempDir(), "does-not-exist.cfg")); err == nil {
		t.Fatal("expected error for missing config file, got nil")
	}
}

func TestLoadProxyConfigWithOrchestrationSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": []
		},
		"accessRules": {
			"clientAccessRules": []
		},
		"governanceOrchestration": {
			"orchestrations": {
				"standard-guardrails": {
					"callAuthorization": [
						{ "server": "authorization-mcp-server", "tool": "call-authorization" }
					],
					"toolCallAudit": ["publish-tool-request-result"]
				}
			},
			"toolMappings": [
				{ "pattern": "orchestration-demo.*", "orchestration": "standard-guardrails" }
			]
		}`+minimalEventingSections+`
	}`)

	cfg, err := loadProxyConfig(path)
	if err != nil {
		t.Fatalf("loadProxyConfig returned error: %v", err)
	}
	if cfg.Orchestration == nil {
		t.Fatal("expected non-nil Orchestration when \"orchestration\" section is present")
	}
	if len(cfg.Orchestration.Orchestrations) != 1 {
		t.Fatalf("expected 1 orchestration, got %d", len(cfg.Orchestration.Orchestrations))
	}
}

func TestLoadProxyConfigMissingOrchestrationSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": []
		},
		"accessRules": {
			"clientAccessRules": []
		}`+minimalEventingSections+`
	}`)

	cfg, err := loadProxyConfig(path)
	if err != nil {
		t.Fatalf("loadProxyConfig returned error: %v", err)
	}
	if cfg.Orchestration != nil {
		t.Errorf("expected nil Orchestration when \"orchestration\" section is absent, got %+v", cfg.Orchestration)
	}
}

func TestLoadProxyConfigMalformedOrchestrationSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": {
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": []
		},
		"accessRules": {
			"clientAccessRules": []
		},
		"governanceOrchestration": {
			"orchestrations": {
				"bad-orch": {}
			},
			"toolMappings": [
				{ "pattern": "*.*", "orchestration": "does-not-exist" }
			]
		}`+minimalEventingSections+`
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error for malformed \"orchestration\" section, got nil")
	}
}

func TestLoadProxyConfigMissingEventDictionarySection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
		"accessRules": { "clientAccessRules": [] },
		"roleMapping": { "administrator": "admin" },
		"declarativeGuardrails": { "actions": ["log"], "declarations": [] }
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error when \"eventDictionary\" section is absent (it's always required, not pointer-nil-optional)")
	}
}

func TestLoadProxyConfigMalformedEventDictionarySection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
		"accessRules": { "clientAccessRules": [] },
		"roleMapping": { "administrator": "admin" },
		"eventDictionary": { "eventTypes": { "alert": { "metadata": { "x": { "type": "not-a-real-type" } } } } },
		"declarativeGuardrails": { "actions": ["log"], "declarations": [] }
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error for an eventDictionary field type outside the known set")
	}
}

func TestLoadProxyConfigMissingDeclarativeGuardrailsSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
		"accessRules": { "clientAccessRules": [] },
		"roleMapping": { "administrator": "admin" },
		"eventDictionary": { "eventTypes": {} }
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error when \"declarativeGuardrails\" section is absent (it's always required, not pointer-nil-optional)")
	}
}

func TestLoadProxyConfigMalformedDeclarativeGuardrailsSection(t *testing.T) {
	path := writeTempConfig(t, `{
		"discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
		"accessRules": { "clientAccessRules": [] },
		"roleMapping": { "administrator": "admin" },
		"eventDictionary": { "eventTypes": {} },
		"declarativeGuardrails": { "actions": [], "declarations": [] }
	}`)

	if _, err := loadProxyConfig(path); err == nil {
		t.Fatal("expected error for declarativeGuardrails with an empty actions catalog")
	}
}

func TestLoadProxyConfigRoleMapping(t *testing.T) {
	// Spelled out instead of using minimalEventingSections, which already
	// carries a roleMapping.
	base := `{
		"discovery": { "discoveryMode": "static", "listedTools": ["*"], "servers": [] },
		"accessRules": { "clientAccessRules": [] },
		"eventDictionary": { "eventTypes": {} },
		"declarativeGuardrails": { "actions": ["log"], "declarations": [] }%s
	}`

	t.Run("absent section fails startup", func(t *testing.T) {
		if _, err := loadProxyConfig(writeTempConfig(t, fmt.Sprintf(base, ""))); err == nil {
			t.Fatal("expected an error when the \"roleMapping\" section is absent (it has no default)")
		}
	})

	t.Run("present section is honored", func(t *testing.T) {
		cfg, err := loadProxyConfig(writeTempConfig(t, fmt.Sprintf(base, `,
		"roleMapping": { "administrator": "mcp-gateway-admins" }`)))
		if err != nil {
			t.Fatalf("loadProxyConfig returned error: %v", err)
		}
		if cfg.RoleMapping.Administrator != "mcp-gateway-admins" {
			t.Errorf("Administrator = %q, want %q", cfg.RoleMapping.Administrator, "mcp-gateway-admins")
		}
	})

	t.Run("empty administrator fails startup", func(t *testing.T) {
		if _, err := loadProxyConfig(writeTempConfig(t, fmt.Sprintf(base, `,
		"roleMapping": { "administrator": "" }`))); err == nil {
			t.Fatal("expected an error for an empty roleMapping.administrator, got nil")
		}
	})
}
