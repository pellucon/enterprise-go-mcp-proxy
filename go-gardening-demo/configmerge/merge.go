// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package configmerge holds the merge algorithms shared by go-gardening-demo's
// two ways of applying go-gardening-demo/initial-data's generated/hand-authored
// accessRules/declarativeGuardrails/governanceOrchestration content: go-gardening-demo/cli's
// "admin update_*" commands (a live merge against a running proxy, via its
// _admin.export_*/update_* tools) and go-gardening-demo/patchcfg (an offline
// merge against a committed *.cfg file's own baked-in section, with no proxy
// involved at all). Extracted into its own importable package — rather than
// living in cli/admin.go, where it originated — specifically so patchcfg (a
// separate binary, also package main) can call the exact same merge logic
// without duplicating it: go-proxy's own package main can't import another
// package main (see CLAUDE.md's "AI-powered tool discovery" section
// for the same constraint elsewhere), so a shared algorithm used by two
// package-main binaries has to live in an importable subpackage like this one
// — the same reason platform-mcp-servers/declarative-guardrail/guardrailcfg
// exists.
package configmerge

import (
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

// applicationServerPrefix is the namespace prefix every go-gardening-demo
// server's tools share — matching cli/admin.go's own constant of the same
// name and purpose (isGardenOnlyRule below is the only thing that needs it).
const applicationServerPrefix = "garden-"

// MergeAccessRules drops any rule from current whose AllowedTools entries
// all start with "garden-" (exactly how the old single blanket gardening
// wildcard rule was shaped, in both proxy-server.cfg and
// gardening_demo_test.cfg) — or whose Role exactly matches one of
// newRules' own roles — then appends newRules. A merge, not a wholesale
// replace: the go-proxy-deployment and gardening-demo-test cfgs
// intentionally carry different "other" rules (the former also has a
// proxy_utility.call_tool wildcard the latter doesn't), so a static file
// can't safely represent "the whole final array" for either — but
// stripping exactly the garden-prefixed rules (the old wildcard) and
// same-named roles (this file's own rules, on a repeat run) while leaving
// everything else alone works for both. The role-match check matters
// specifically for admin: newRules' own admin/["*"] entry isn't
// garden-prefixed, so without it a pre-existing admin/["*"] rule — which
// every committed *.cfg already has — would never be dropped, and
// re-running this would append a duplicate admin rule every time instead
// of replacing it once.
func MergeAccessRules(current, newRules []access_control.AccessibilityRule) []access_control.AccessibilityRule {
	newRoles := make(map[string]bool, len(newRules))
	for _, r := range newRules {
		newRoles[r.Role] = true
	}
	merged := make([]access_control.AccessibilityRule, 0, len(current)+len(newRules))
	for _, r := range current {
		if isGardenOnlyRule(r) || newRoles[r.Role] {
			continue
		}
		merged = append(merged, r)
	}
	return append(merged, newRules...)
}

// isGardenOnlyRule reports whether every one of r's AllowedTools entries
// is namespaced under a go-gardening-demo server (applicationServerPrefix)
// — an empty AllowedTools list doesn't count (nothing to identify it by).
func isGardenOnlyRule(r access_control.AccessibilityRule) bool {
	if len(r.AllowedTools) == 0 {
		return false
	}
	for _, t := range r.AllowedTools {
		if !strings.HasPrefix(t, applicationServerPrefix) {
			return false
		}
	}
	return true
}

// MergeDeclarativeGuardrails replaces any declaration in current whose Name
// matches one of newDeclarations (a repeat run updates in place rather than
// duplicating), then appends the rest of newDeclarations, and unions
// current's action catalog with every action name newDeclarations itself
// references (so a repeat run — or a fresh seed file naming an action not
// yet in the catalog — never fails config validation for a missing action
// name).
func MergeDeclarativeGuardrails(current guardrailcfg.RawConfig, newDeclarations []guardrailcfg.RawDeclaration) (actions []string, declarations []guardrailcfg.RawDeclaration) {
	newNames := make(map[string]bool, len(newDeclarations))
	actionSet := make(map[string]bool, len(current.Actions))
	for _, a := range current.Actions {
		actionSet[a] = true
	}
	for _, d := range newDeclarations {
		newNames[d.Name] = true
		for _, a := range d.Actions {
			actionSet[a] = true
		}
	}

	declarations = make([]guardrailcfg.RawDeclaration, 0, len(current.Declarations)+len(newDeclarations))
	for _, d := range current.Declarations {
		if newNames[d.Name] {
			continue
		}
		declarations = append(declarations, d)
	}
	declarations = append(declarations, newDeclarations...)

	actions = make([]string, 0, len(actionSet))
	for _, a := range current.Actions {
		actions = append(actions, a)
	}
	for a := range actionSet {
		if !contains(actions, a) {
			actions = append(actions, a)
		}
	}
	return actions, declarations
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// MergeOrchestration upserts every orchestration in newConfig into
// current.Orchestrations by name (overwriting a same-named existing one -
// a repeat run converges rather than erroring), then rebuilds ToolMappings
// as newConfig's own mappings (in their given order) followed by every
// current mapping whose Pattern isn't also named by newConfig - so a
// repeat run doesn't duplicate a mapping, and newConfig's own mappings
// always precede current's (in particular any pre-existing catch-all "*"
// pattern), matching toolMappings' first-match-wins evaluation order.
func MergeOrchestration(current, newConfig triggered_tools_runtime.OrchestrationConfig) triggered_tools_runtime.OrchestrationConfig {
	orchestrations := make(map[string]triggered_tools_runtime.Orchestration, len(current.Orchestrations)+len(newConfig.Orchestrations))
	for name, o := range current.Orchestrations {
		orchestrations[name] = o
	}
	for name, o := range newConfig.Orchestrations {
		orchestrations[name] = o
	}

	newPatterns := make(map[string]bool, len(newConfig.ToolMappings))
	for _, m := range newConfig.ToolMappings {
		newPatterns[m.Pattern] = true
	}
	toolMappings := make([]triggered_tools_runtime.ToolMapping, 0, len(current.ToolMappings)+len(newConfig.ToolMappings))
	toolMappings = append(toolMappings, newConfig.ToolMappings...)
	for _, m := range current.ToolMappings {
		if newPatterns[m.Pattern] {
			continue
		}
		toolMappings = append(toolMappings, m)
	}

	return triggered_tools_runtime.OrchestrationConfig{Orchestrations: orchestrations, ToolMappings: toolMappings}
}
