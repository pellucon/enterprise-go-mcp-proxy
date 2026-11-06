// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"fmt"
	"sort"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

// trustedServerNames returns the servers go-proxy trusts, by name alone,
// for a security decision, each with why - the names k8s discovery accepts
// only from their pinned Service (see discover_mcp_servers.MustPin):
//
//   - the platform servers go-proxy calls by a fixed name: rate-limiter
//     (rate limiting), declarative-guardrail (guardrail declarations) and
//     ai-powered-tools (the AI guardrails, and proxy_utility.
//     find_relevant_tools, which is always available). Always, whether or
//     not those features are configured, so enabling one later never
//     depends on someone remembering to pin its server;
//   - every server an orchestration step runs on, including the servers a
//     native step's parameters name (guardrailServer, aiValidatorServer);
//   - every internalAuth.relay source, which gets relay tokens that let it
//     call other backends as each user.
//
// A guarded tool's own server isn't here, though a business-logic step
// asks it for the verdict: a Service that claimed that name would receive
// the call itself anyway, so the verdict gains it nothing.
//
// Deterministic (the first reason found wins, in a fixed order), since
// discovery logs the reason and repeats a message only when it changes.
func trustedServerNames(orch *triggered_tools_runtime.OrchestrationConfig, relay map[string][]string) discover_mcp_servers.MustPin {
	names := discover_mcp_servers.MustPin{}
	add := func(name, why string) {
		if _, ok := names[name]; !ok && name != dynamic_proxy.UtilityServerName {
			names[name] = why
		}
	}
	add(rateLimiterServerName, "rate limiting")
	add(defaultDeclarativeGuardrailServer, "guardrail declarations")
	add(defaultAIValidatorServer, "AI guardrails and proxy_utility.find_relevant_tools")

	if orch != nil {
		orchNames := make([]string, 0, len(orch.Orchestrations))
		for name := range orch.Orchestrations {
			orchNames = append(orchNames, name)
		}
		sort.Strings(orchNames)
		for _, orchName := range orchNames {
			o := orch.Orchestrations[orchName]
			why := fmt.Sprintf("a step of orchestration %q", orchName)
			for _, steps := range [][]triggered_tools_runtime.OrchestrationStep{o.CallAuthorization, o.ResultAuthorization} {
				for _, step := range steps {
					for _, server := range stepServers(step) {
						add(server, why)
					}
				}
			}
		}
	}

	relaySources := make([]string, 0, len(relay))
	for name := range relay {
		relaySources = append(relaySources, name)
	}
	sort.Strings(relaySources)
	for _, name := range relaySources {
		add(name, "an internalAuth.relay source")
	}
	return names
}

// stepServers returns the servers whose answers decide step: its own
// server, or, for a native proxy_utility step, the servers its parameters
// send it to (their defaults if unset). Parameters that don't decode name
// no servers: the step then fails closed when it runs.
func stepServers(step triggered_tools_runtime.OrchestrationStep) []string {
	if step.Server != dynamic_proxy.UtilityServerName {
		return []string{step.Server}
	}
	switch step.Tool {
	case "request_constraints_validation", "result_constraints_validation", "business_logic_constraints_validation":
		if p, err := decodeConstraintsValidationParameters(step.Parameters); err == nil {
			return []string{p.GuardrailServer}
		}
	case "request_ai_constraints_validation", "result_ai_constraints_validation":
		if p, err := decodeAIConstraintsValidationParameters(step.Parameters); err == nil {
			return []string{p.GuardrailServer, p.AIValidatorServer}
		}
	}
	return nil
}

// relayOf returns internalAuth's relay sources, or nil without internalAuth.
func relayOf(cfg *auth.SignerConfig) map[string][]string {
	if cfg == nil {
		return nil
	}
	return cfg.Relay
}

// currentMustPin is trustedServerNames for the live orchestration config
// and internalAuth (startup-only).
func currentMustPin() discover_mcp_servers.MustPin {
	return trustedServerNames(triggeredRuntime.Snapshot(), relayOf(internalAuthConfig))
}

// validateOrchestrationUpdate is _admin.update_orchestration's validate: the
// orchestration must parse, and in k8s mode every server its steps trust
// must already be pinned by the live discovery config - otherwise discovery
// would refuse that server, and the new steps would fail closed.
func validateOrchestrationUpdate(raw []byte) error {
	orch, err := triggered_tools_runtime.ParseConfig(raw)
	if err != nil {
		return err
	}
	return discover_mcp_servers.UnpinnedNames(discoveryWatcher.Current(), trustedServerNames(orch, relayOf(internalAuthConfig)))
}
