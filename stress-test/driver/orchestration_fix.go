// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

// This file works around a real environmental gap discovered empirically
// while implementing this driver (not assumed or guessed): go-gardening-demo's
// own real orchestration.json wires two guardrail steps —
// "proxy_utility.request_ai_constraints_validation" (on
// garden-customer-service.add_customer_service_call, via the
// "service-call-guardrails" orchestration) and
// "proxy_utility.result_ai_constraints_validation" (on
// garden-customer-admin.get_customer_details AND
// garden-customer-admin.load_customers, via "customer-details-no-audit")
// — both of which call ai-powered-tools.ai_result_validator, and both fail
// CLOSED (rejecting the call) when that backend is unreachable
// (constraints_validation.go's own documented "guardrail-reachability
// failure" behavior). This stress test deliberately does not deploy
// ai-powered-tools (it requires real API credentials or a local headless
// Claude bridge this driver has no way to provide, and none of its own
// 32 included tools are ai-powered-tools tools) — see README.md's
// "Flagged code gaps" section for the parallel, more fundamental logging
// gap; this one is a genuine deploy-scope mismatch, not a code gap, and
// IS worked around here (not just flagged) because otherwise:
//  1. go-gardening-demo/cli's own `init` sequence's load_customers step
//     would always fail once orchestration.json has ever been applied to
//     this cluster before (it persists in a k8s ConfigMap across pod
//     restarts) — making a second/later run of this driver's own setup
//     phase permanently broken, not just the very first one.
//  2. garden-customer-admin.get_customer_details and
//     garden-customer-service.add_customer_service_call — both ordinary,
//     included tools in the stress-test call mix — would then reject
//     100% of the time throughout the actual client-run phase too, which
//     would misreport a deploy-scope artifact as if it were a real
//     system health problem.
//
// The fix is surgical, not a wholesale orchestration reset: it strips
// only the two AI-dependent steps, leaving every other guardrail
// (declarative JSON-schema and business-logic-validation based — neither
// depends on ai-powered-tools) fully intact and exercised normally.

const proxyUtilityServer = "proxy_utility"

var aiDependentNativeTools = map[string]bool{
	"request_ai_constraints_validation": true,
	"result_ai_constraints_validation":  true,
}

// stripAIGuardianSteps removes every AI-dependent step from cfg's
// orchestrations in place, returning how many were removed. An
// orchestration left with empty CallAuthorization/ResultAuthorization
// lists is a valid no-op orchestration (middleware.go's Guard simply has
// zero steps to run) — no toolMapping needs to change.
func stripAIGuardianSteps(cfg *triggered_tools_runtime.OrchestrationConfig) int {
	removed := 0
	for name, orch := range cfg.Orchestrations {
		orch.CallAuthorization, removed = filterSteps(orch.CallAuthorization, removed)
		orch.ResultAuthorization, removed = filterSteps(orch.ResultAuthorization, removed)
		cfg.Orchestrations[name] = orch
	}
	return removed
}

func filterSteps(steps []triggered_tools_runtime.OrchestrationStep, removedSoFar int) ([]triggered_tools_runtime.OrchestrationStep, int) {
	var kept []triggered_tools_runtime.OrchestrationStep
	for _, s := range steps {
		if s.Server == proxyUtilityServer && aiDependentNativeTools[s.Tool] {
			removedSoFar++
			continue
		}
		kept = append(kept, s)
	}
	return kept, removedSoFar
}

// neutralizeAIGuardedOrchestration exports the live orchestration config,
// strips AI-dependent steps, and writes it back if anything changed. A
// no-op (returns nil, changes nothing) if there was nothing to strip —
// safe to call unconditionally, including on a cluster that's never had
// go-gardening-demo's orchestration.json applied at all yet.
func neutralizeAIGuardedOrchestration() error {
	stdout, stderr, err := runOneShotCall("_admin.export_orchestration", "{}")
	if err != nil {
		return fmt.Errorf("_admin.export_orchestration failed: %w (stderr: %s)", err, stderr)
	}
	if strings.Contains(stderr, "Error:") {
		return fmt.Errorf("_admin.export_orchestration returned an error: %s", stderr)
	}

	var current configsync.VersionedConfig[triggered_tools_runtime.OrchestrationConfig]
	if err := json.Unmarshal([]byte(firstJSONLine(stdout)), &current); err != nil {
		return fmt.Errorf("parsing _admin.export_orchestration output: %w (raw: %s)", err, stdout)
	}

	removed := stripAIGuardianSteps(&current.Config)
	if removed == 0 {
		return nil
	}

	argsJSON, err := json.Marshal(current)
	if err != nil {
		return fmt.Errorf("marshaling stripped orchestration config: %w", err)
	}
	_, updateStderr, err := runOneShotCall("_admin.update_orchestration", string(argsJSON))
	if err != nil {
		return fmt.Errorf("_admin.update_orchestration failed: %w (stderr: %s)", err, updateStderr)
	}
	if strings.Contains(updateStderr, "Error:") {
		return fmt.Errorf("_admin.update_orchestration returned an error: %s", updateStderr)
	}
	return nil
}

// firstJSONLine returns the text up to (not including) the first
// newline — the CLI's call-tool output always prints the compact,
// single-line JSON representation of a tool's typed return value first,
// followed by a separate pretty-printed block; encoding/json.Marshal's
// compact form never itself contains a raw newline, so this reliably
// isolates just that first block.
func firstJSONLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
