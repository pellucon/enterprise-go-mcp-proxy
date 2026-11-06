// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package access_control

import (
	"path/filepath"
	"strings"
	"sync"

	"github.com/golang-jwt/jwt/v5"
)

// adminToolPrefix marks a tool name as a proxy-native admin tool (e.g.
// _admin.update_role_permissions) rather than a backend-discovered or
// ordinary tool. Any tool name with this prefix is authorized solely by
// hasAdministratorRole below, never by the configured rules — see
// IsToolAuthorized.
//
// This is intentionally an independent literal, not an import of
// dynamic_proxy.ReservedServerName (access_control stays decoupled from the
// gateway/routing package, preferring small independent duplication over
// cross-package coupling) — but it MUST stay equal to
// dynamic_proxy.ReservedServerName + "." That
// constant is what dynamic_proxy.Gateway.OnAdded/registerTool refuse to let
// a discovered backend server collide with; if the two ever diverge, a
// backend could be discovered under a name that produces tool names this
// prefix check treats as reserved-admin-only without the gateway actually
// having reserved that name, or vice versa.
const adminToolPrefix = "_admin."

// AccessPolicyEngine holds the current AccessConfig and is safe for concurrent use: reads
// (the IsXAuthorized methods, Snapshot) take an RLock, and Update takes a
// Lock to swap in a wholly new config. Update never mutates an existing
// *AccessConfig in place — it always replaces s.config with a new one — so
// Snapshot can safely hand out the live pointer without a deep copy: nothing
// will ever write through a *AccessConfig (or its rules' slices) after it's
// been published. Do not break this invariant by mutating a Snapshot's
// result or a pointer passed to Update after the call.
//
// administratorRole is set once by NewAccessPolicyEngine and never written
// again — Update only replaces config — so it needs no lock. See
// RoleMapping for why it must not be live-updatable.
type AccessPolicyEngine struct {
	mu                sync.RWMutex
	config            *AccessConfig
	administratorRole string
}

// NewAccessPolicyEngine creates an AccessPolicyEngine holding initial as
// the current config. administratorRole is the role name that authorizes
// the _admin.* tools (RoleMapping.Administrator); it is fixed for the
// engine's lifetime. An empty administratorRole means no caller is ever
// an administrator — it never matches an empty role claim.
func NewAccessPolicyEngine(initial *AccessConfig, administratorRole string) *AccessPolicyEngine {
	return &AccessPolicyEngine{config: initial, administratorRole: administratorRole}
}

// AdministratorRole returns the role name that authorizes the _admin.*
// tools, as fixed at construction.
func (e *AccessPolicyEngine) AdministratorRole() string {
	return e.administratorRole
}

// Update replaces the entire rule set. There is no per-rule patch/merge
// semantics — callers (e.g. the _admin.update_role_permissions tool) always
// replace the whole config, matching the same shape LoadConfigFromFile/
// Snapshot produce.
func (e *AccessPolicyEngine) Update(cfg *AccessConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.config = cfg
}

// Snapshot returns the current config. The returned value (and everything
// reachable from it) must be treated as read-only — see the AccessPolicyEngine doc
// comment for why that's safe without a defensive copy.
func (e *AccessPolicyEngine) Snapshot() *AccessConfig {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.config
}

// IsToolAuthorized reports whether claims authorize calling the tool named
// toolName. Tool names prefixed with adminToolPrefix are a special case:
// their authorization is fixed to "caller has the administrator role" (the
// role name RoleMapping mapped it to at startup) and never consults the
// configured rules at all. This is deliberate: since these
// tools are exactly what can change the configured rules, their own
// authorization must never depend on that same config, or a bad edit could
// permanently lock admin out of fixing it (or open the tools to non-admins).
func (e *AccessPolicyEngine) IsToolAuthorized(claims jwt.MapClaims, toolName string) bool {
	if isAdminTool(toolName) {
		return hasAdministratorRole(claims, e.administratorRole)
	}
	return e.matchAny(claims, toolName, func(r AccessibilityRule) []string { return r.AllowedTools })
}

// IsPromptAuthorized reports whether claims authorize fetching the prompt
// named promptName, per the configured rules.
func (e *AccessPolicyEngine) IsPromptAuthorized(claims jwt.MapClaims, promptName string) bool {
	return e.matchAny(claims, promptName, func(r AccessibilityRule) []string { return r.AllowedPrompts })
}

// IsResourceAuthorized reports whether claims authorize reading the
// resource at the given (namespaced) uri, per the configured rules.
func (e *AccessPolicyEngine) IsResourceAuthorized(claims jwt.MapClaims, uri string) bool {
	return e.matchAny(claims, uri, func(r AccessibilityRule) []string { return r.AllowedResources })
}

// IsResourceTemplateAuthorized reports whether claims authorize seeing the
// resource template with the given (namespaced) uriTemplate, per the
// configured rules.
func (e *AccessPolicyEngine) IsResourceTemplateAuthorized(claims jwt.MapClaims, uriTemplate string) bool {
	return e.matchAny(claims, uriTemplate, func(r AccessibilityRule) []string { return r.AllowedResourceTemplates })
}

func isAdminTool(name string) bool {
	return strings.HasPrefix(name, adminToolPrefix)
}

// hasAdministratorRole reports whether claims include administratorRole
// literally. This is an exact string check, not a glob match, so a claim
// value can't pattern its way into satisfying it. An empty
// administratorRole matches nothing.
func hasAdministratorRole(claims jwt.MapClaims, administratorRole string) bool {
	if administratorRole == "" {
		return false
	}
	for _, role := range RolesFromClaims(claims) {
		if role == administratorRole {
			return true
		}
	}
	return false
}

// RolesFromClaims normalizes a JWT's "role" claim, which may be a plain
// string or a []interface{} of strings, into a slice of role strings.
// Exported for reuse by triggered_tools_runtime, which needs the same
// normalization to build a single-string "role" field for its audit
// contract (see triggered-tools-runtime/orchestrate.go).
func RolesFromClaims(claims jwt.MapClaims) []string {
	switch v := claims["role"].(type) {
	case string:
		return []string{v}
	case []interface{}:
		roles := make([]string, 0, len(v))
		for _, r := range v {
			if roleStr, ok := r.(string); ok {
				roles = append(roles, roleStr)
			}
		}
		return roles
	default:
		return nil
	}
}

// matchAny finds any configured rule whose Role pattern matches one of
// claims' roles (via filepath.Match, supporting wildcards like
// "development.*"), then checks name against that rule's patterns as
// selected by patternsOf (e.g. AllowedTools, AllowedPrompts) via globMatch.
func (e *AccessPolicyEngine) matchAny(claims jwt.MapClaims, name string, patternsOf func(AccessibilityRule) []string) bool {
	e.mu.RLock()
	cfg := e.config
	e.mu.RUnlock()

	if cfg == nil {
		return false
	}

	for _, role := range RolesFromClaims(claims) {
		for _, rule := range cfg.ClientAccessRules {
			roleMatched, err := filepath.Match(rule.Role, role)
			if err != nil || !roleMatched {
				continue
			}
			for _, pattern := range patternsOf(rule) {
				if GlobMatch(pattern, name) {
					return true
				}
			}
		}
	}
	return false
}

// GlobMatch reports whether name matches pattern, where "*" means "match
// any sequence of characters". Unlike filepath.Match (used above for role
// matching, where "/" never appears), this is deliberately not path-aware:
// resource URIs routinely contain "/" (e.g. "mcp1:file:///a.txt"), and
// filepath.Match's "*" refuses to cross a path separator, which would make
// even a bare "*" pattern silently fail to match almost any real resource
// URI. Exported for reuse by triggered_tools_runtime's tool-name-pattern
// matching (orchestration toolMappings), which needs the same semantics.
func GlobMatch(pattern, name string) bool {
	if pattern == name {
		return true
	}

	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return false // no wildcard and not an exact match
	}

	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]

	for _, part := range parts[1 : len(parts)-1] {
		idx := strings.Index(name, part)
		if idx < 0 {
			return false
		}
		name = name[idx+len(part):]
	}

	return strings.HasSuffix(name, parts[len(parts)-1])
}
