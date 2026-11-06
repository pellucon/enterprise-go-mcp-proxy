// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"fmt"
	"strings"
)

// liveSections maps each configsync section to its key in the cfg and the
// CLI command that updates it live (_admin.update_<name>).
var liveSections = map[string]struct{ cfgKey, cliUpdate string }{
	"access-rules":           {"accessRules", "update-role-permissions"},
	"orchestration":          {"governanceOrchestration", "update-orchestration"},
	"discovery":              {"discovery", "update-discovery"},
	"event-dictionary":       {"eventDictionary", "update-event-dictionary"},
	"declarative-guardrails": {"declarativeGuardrails", "update-declarative-guardrails"},
	"event-aggregation":      {"eventAggregation", "update-event-aggregation"},
	"rate-limiting":          {"rateLimiting", "update-rate-limiting"},
	"log-level":              {"logLevel", "update-log-level"},
}

// driftError is startup's refusal when the deployed cfg changed sections
// (configsync.Writer.Reconcile) so that they differ from the live config:
// it says which, and the two ways out.
func driftError(sections []string) error {
	var keys, updates, configMaps []string
	for _, section := range sections {
		s := liveSections[section]
		keys = append(keys, s.cfgKey)
		updates = append(updates, "cli "+s.cliUpdate)
		configMaps = append(configMaps, section+"-override")
	}
	return fmt.Errorf("the deployed cfg changes %s since it last matched the live config, and now differs from it. "+
		"Once a section is live the cfg never overrides it, so this change would be silently ignored; go-proxy won't start with it. "+
		"Either (1) keep the live config: regenerate the cfg from the running go-proxy (cli export-full-config > proxy-server.cfg) and redeploy it; "+
		"to make a change, apply it live first (%s), then regenerate the cfg; "+
		"or (2) replace the live config with the cfg's: kubectl delete configmap %s in go-proxy's namespace, then restart go-proxy, which re-seeds it from the cfg. "+
		"See docs/architecture/configsync.md",
		strings.Join(keys, ", "), strings.Join(updates, ", "), strings.Join(configMaps, " "))
}
