// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import (
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func claimsForRole(role string) jwt.MapClaims {
	return jwt.MapClaims{"role": role}
}

func TestMatcherToolRules(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			{
				Role:         "admin",
				AllowedTools: []string{"*"},
			},
			{
				Role:         "finance",
				AllowedTools: []string{"analytics_transactions", "audit_transactions"},
			},
			{
				Role:         "development.*",
				AllowedTools: []string{"analytics_*", "audit_*"},
			},
			{
				Role:         "development.manager",
				AllowedTools: []string{"meta_*"},
			},
		},
	}

	engine := NewAccessPolicyEngine(mockConfig, "admin")

	tests := []struct {
		name          string
		role          string
		toolName      string
		expectedMatch bool
	}{
		// Admin wildcard checks
		{name: "Admin accesses any tool", role: "admin", toolName: "drop_database", expectedMatch: true},
		{name: "Admin accesses wildcard tool", role: "admin", toolName: "analytics_transactions", expectedMatch: true},

		// Finance explicit checks
		{name: "Finance accesses allowed tool 1", role: "finance", toolName: "analytics_transactions", expectedMatch: true},
		{name: "Finance accesses allowed tool 2", role: "finance", toolName: "audit_transactions", expectedMatch: true},
		{name: "Finance denied unauthorized tool", role: "finance", toolName: "meta_configs", expectedMatch: false},

		// Development wildcard suffix checks (e.g., development.engineer)
		{name: "Dev sub-role matches analytics pattern", role: "development.engineer", toolName: "analytics_logs", expectedMatch: true},
		{name: "Dev sub-role matches audit pattern", role: "development.qa", toolName: "audit_logs", expectedMatch: true},
		{name: "Dev sub-role denied unpatterned tool", role: "development.engineer", toolName: "meta_configs", expectedMatch: false},

		// Development Manager specific checks
		// Note: development.manager will catch the "development.*" rules AND its specific rule
		{name: "Dev Manager matches its specific rule", role: "development.manager", toolName: "meta_schemas", expectedMatch: true},
		{name: "Dev Manager also inherits development sub-role patterns", role: "development.manager", toolName: "analytics_users", expectedMatch: true},
		{name: "Dev Manager denied random tool", role: "development.manager", toolName: "finance_payouts", expectedMatch: false},

		// Complete misses
		{name: "Unknown role completely denied", role: "guest", toolName: "analytics_transactions", expectedMatch: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualMatch := engine.IsToolAuthorized(claimsForRole(tc.role), tc.toolName)
			if actualMatch != tc.expectedMatch {
				t.Errorf("FAIL: [%s] IsToolAuthorized(%q, %q) = %v; want %v",
					tc.name, tc.role, tc.toolName, actualMatch, tc.expectedMatch)
			}
		})
	}
}

func TestMatcherPromptRules(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			{
				Role:           "admin",
				AllowedPrompts: []string{"*"},
			},
			{
				Role:           "finance",
				AllowedPrompts: []string{"quarterly_report"},
			},
			{
				Role:           "development.*",
				AllowedPrompts: []string{"dev_*"},
			},
		},
	}

	engine := NewAccessPolicyEngine(mockConfig, "admin")

	tests := []struct {
		name          string
		role          string
		promptName    string
		expectedMatch bool
	}{
		{name: "Admin accesses any prompt", role: "admin", promptName: "onboarding", expectedMatch: true},
		{name: "Finance accesses allowed prompt", role: "finance", promptName: "quarterly_report", expectedMatch: true},
		{name: "Finance denied unauthorized prompt", role: "finance", promptName: "dev_debug", expectedMatch: false},
		{name: "Dev sub-role matches dev pattern", role: "development.engineer", promptName: "dev_debug", expectedMatch: true},
		{name: "Dev sub-role denied unpatterned prompt", role: "development.engineer", promptName: "quarterly_report", expectedMatch: false},
		{name: "Unknown role completely denied", role: "guest", promptName: "onboarding", expectedMatch: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualMatch := engine.IsPromptAuthorized(claimsForRole(tc.role), tc.promptName)
			if actualMatch != tc.expectedMatch {
				t.Errorf("FAIL: [%s] IsPromptAuthorized(%q, %q) = %v; want %v",
					tc.name, tc.role, tc.promptName, actualMatch, tc.expectedMatch)
			}
		})
	}
}

func TestMatcherResourceRules(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			{
				Role:             "admin",
				AllowedResources: []string{"*"},
			},
			{
				Role:             "finance",
				AllowedResources: []string{"mcp1:file:///quarterly_report"},
			},
			{
				Role:             "development.*",
				AllowedResources: []string{"mcp1:dev_*"},
			},
		},
	}

	engine := NewAccessPolicyEngine(mockConfig, "admin")

	tests := []struct {
		name          string
		role          string
		uri           string
		expectedMatch bool
	}{
		{name: "Admin accesses any resource", role: "admin", uri: "mcp1:file:///onboarding", expectedMatch: true},
		{name: "Finance accesses allowed resource", role: "finance", uri: "mcp1:file:///quarterly_report", expectedMatch: true},
		{name: "Finance denied unauthorized resource", role: "finance", uri: "mcp1:dev_debug", expectedMatch: false},
		{name: "Dev sub-role matches dev pattern", role: "development.engineer", uri: "mcp1:dev_debug", expectedMatch: true},
		{name: "Dev sub-role denied unpatterned resource", role: "development.engineer", uri: "mcp1:file:///quarterly_report", expectedMatch: false},
		{name: "Unknown role completely denied", role: "guest", uri: "mcp1:file:///onboarding", expectedMatch: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualMatch := engine.IsResourceAuthorized(claimsForRole(tc.role), tc.uri)
			if actualMatch != tc.expectedMatch {
				t.Errorf("FAIL: [%s] IsResourceAuthorized(%q, %q) = %v; want %v",
					tc.name, tc.role, tc.uri, actualMatch, tc.expectedMatch)
			}
		})
	}
}

func TestMatcherResourceTemplateRules(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			{
				Role:                     "admin",
				AllowedResourceTemplates: []string{"*"},
			},
			{
				Role:                     "finance",
				AllowedResourceTemplates: []string{"mcp1:file:///reports/{name}"},
			},
			{
				Role:                     "development.*",
				AllowedResourceTemplates: []string{"mcp1:dev_*"},
			},
		},
	}

	engine := NewAccessPolicyEngine(mockConfig, "admin")

	tests := []struct {
		name          string
		role          string
		uriTemplate   string
		expectedMatch bool
	}{
		{name: "Admin accesses any resource template", role: "admin", uriTemplate: "mcp1:file:///{path}", expectedMatch: true},
		{name: "Finance accesses allowed resource template", role: "finance", uriTemplate: "mcp1:file:///reports/{name}", expectedMatch: true},
		{name: "Finance denied unauthorized resource template", role: "finance", uriTemplate: "mcp1:dev_debug/{id}", expectedMatch: false},
		{name: "Dev sub-role matches dev pattern", role: "development.engineer", uriTemplate: "mcp1:dev_debug/{id}", expectedMatch: true},
		{name: "Dev sub-role denied unpatterned resource template", role: "development.engineer", uriTemplate: "mcp1:file:///reports/{name}", expectedMatch: false},
		{name: "Unknown role completely denied", role: "guest", uriTemplate: "mcp1:file:///{path}", expectedMatch: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualMatch := engine.IsResourceTemplateAuthorized(claimsForRole(tc.role), tc.uriTemplate)
			if actualMatch != tc.expectedMatch {
				t.Errorf("FAIL: [%s] IsResourceTemplateAuthorized(%q, %q) = %v; want %v",
					tc.name, tc.role, tc.uriTemplate, actualMatch, tc.expectedMatch)
			}
		})
	}
}

func TestMultiRoleClaim(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			{Role: "finance", AllowedTools: []string{"analytics_transactions"}},
		},
	}
	engine := NewAccessPolicyEngine(mockConfig, "admin")

	claims := jwt.MapClaims{"role": []interface{}{"guest", "finance"}}
	if !engine.IsToolAuthorized(claims, "analytics_transactions") {
		t.Error("expected multi-role claim including 'finance' to be authorized for analytics_transactions")
	}
	if engine.IsToolAuthorized(claims, "meta_configs") {
		t.Error("expected multi-role claim to still be denied for a tool none of its roles allow")
	}
}

// TestAdminToolAuthorizationIsConfigIndependent proves the admin-only check
// for _admin.* tools never consults the configured rules at all — not just
// that it's consistent with today's admin: allowed_tools: ["*"] rule, but
// that it holds even when the config grants admin nothing.
func TestAdminToolAuthorizationIsConfigIndependent(t *testing.T) {
	mockConfig := &AccessConfig{
		ClientAccessRules: []AccessibilityRule{
			// No rule grants "admin" anything, and "guest" gets a wildcard —
			// the opposite of what config-driven matching would need to
			// produce the expected results below.
			{Role: "guest", AllowedTools: []string{"*"}},
		},
	}
	engine := NewAccessPolicyEngine(mockConfig, "admin")

	if !engine.IsToolAuthorized(claimsForRole("admin"), "_admin.update_role_permissions") {
		t.Error("expected admin to be authorized for a _admin.* tool even with no config-granted access")
	}
	if engine.IsToolAuthorized(claimsForRole("guest"), "_admin.update_role_permissions") {
		t.Error("expected guest to be denied for a _admin.* tool despite its config wildcard grant")
	}
}

func TestAccessPolicyEngineUpdateAndSnapshot(t *testing.T) {
	engine := NewAccessPolicyEngine(&AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "admin", AllowedTools: []string{"*"}}}}, "admin")

	if engine.IsToolAuthorized(claimsForRole("finance"), "analytics_transactions") {
		t.Fatal("sanity check failed: finance should not initially be authorized")
	}

	engine.Update(&AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "finance", AllowedTools: []string{"analytics_transactions"}}}})

	if !engine.IsToolAuthorized(claimsForRole("finance"), "analytics_transactions") {
		t.Error("expected finance to be authorized after Update granted it access")
	}
	if engine.IsToolAuthorized(claimsForRole("admin"), "drop_database") {
		t.Error("expected admin's old wildcard grant to be gone after Update replaced the whole config")
	}

	snap := engine.Snapshot()
	if len(snap.ClientAccessRules) != 1 || snap.ClientAccessRules[0].Role != "finance" {
		t.Errorf("expected Snapshot to reflect the updated config, got %+v", snap)
	}
}

func TestAccessPolicyEngineConcurrentUpdateAndRead(t *testing.T) {
	engine := NewAccessPolicyEngine(&AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "admin", AllowedTools: []string{"*"}}}}, "admin")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			engine.Update(&AccessConfig{ClientAccessRules: []AccessibilityRule{{Role: "admin", AllowedTools: []string{"*"}}}})
		}()
		go func() {
			defer wg.Done()
			engine.IsToolAuthorized(claimsForRole("admin"), "anything")
		}()
	}
	wg.Wait()
}
