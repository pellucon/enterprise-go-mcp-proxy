// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import (
	"encoding/json"
	"testing"
)

func TestParseRoleMapping(t *testing.T) {
	t.Run("absent section is rejected", func(t *testing.T) {
		if _, err := ParseRoleMapping(nil); err == nil {
			t.Error("expected an error for an absent roleMapping section")
		}
	})

	t.Run("present section is used verbatim", func(t *testing.T) {
		m, err := ParseRoleMapping(json.RawMessage(`{"administrator": "mcp-gateway-admins"}`))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if m.Administrator != "mcp-gateway-admins" {
			t.Errorf("Administrator = %q, want %q", m.Administrator, "mcp-gateway-admins")
		}
	})

	for name, raw := range map[string]string{
		"null section":          `null`,
		"missing administrator": `{}`,
		"empty administrator":   `{"administrator": ""}`,
		"blank administrator":   `{"administrator": "  "}`,
		"malformed json":        `{"administrator": `,
	} {
		t.Run(name+" is rejected", func(t *testing.T) {
			if _, err := ParseRoleMapping(json.RawMessage(raw)); err == nil {
				t.Errorf("expected an error for %s", raw)
			}
		})
	}
}

func TestAdministratorRoleMapping(t *testing.T) {
	// The configured rules grant everyone everything, so any _admin.*
	// decision below comes from the administrator role alone.
	allowAll := &AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "*", AllowedTools: []string{"*"}}}}
	engine := NewAccessPolicyEngine(allowAll, "mcp-gateway-admins")

	if got := engine.AdministratorRole(); got != "mcp-gateway-admins" {
		t.Errorf("AdministratorRole() = %q, want %q", got, "mcp-gateway-admins")
	}
	if !engine.IsToolAuthorized(claimsForRole("mcp-gateway-admins"), "_admin.export_full_config") {
		t.Error("expected the mapped administrator role to be authorized for _admin.* tools")
	}
	if engine.IsToolAuthorized(claimsForRole("admin"), "_admin.export_full_config") {
		t.Error("expected the literal role \"admin\" to have no special meaning once administrator is mapped elsewhere")
	}
	if engine.IsToolAuthorized(claimsForRole("mcp-gateway-*"), "_admin.export_full_config") {
		t.Error("expected the administrator role to be matched literally, not as a glob")
	}

	// Update replaces the rules, never the administrator role.
	engine.Update(&AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "admin", AllowedTools: []string{"*"}}}})
	if !engine.IsToolAuthorized(claimsForRole("mcp-gateway-admins"), "_admin.update_role_permissions") {
		t.Error("expected the administrator role to survive Update")
	}
	if engine.IsToolAuthorized(claimsForRole("admin"), "_admin.update_role_permissions") {
		t.Error("expected an Update granting \"admin\" everything not to make it an administrator")
	}
}

func TestEmptyAdministratorRoleMatchesNothing(t *testing.T) {
	engine := NewAccessPolicyEngine(&AccessConfig{}, "")
	if engine.IsToolAuthorized(claimsForRole(""), "_admin.export_full_config") {
		t.Error("expected an empty administrator role never to match, even an empty role claim")
	}
}
