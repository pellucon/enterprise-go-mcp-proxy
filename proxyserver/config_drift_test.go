// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

func TestDriftErrorNamesTheFixes(t *testing.T) {
	msg := driftError([]string{"access-rules", "rate-limiting"}).Error()
	for _, want := range []string{
		"accessRules, rateLimiting",
		"cli update-role-permissions, cli update-rate-limiting",
		"cli export-full-config",
		"kubectl delete configmap access-rules-override rate-limiting-override",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message doesn't mention %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Error("the message must be one line: it's logged through slog, which escapes newlines")
	}
}

// canonicalOf is configsync's normalization (parse, then re-encode).
func canonicalOf[T any](t *testing.T, raw []byte, parse func([]byte) (*T, error)) string {
	t.Helper()
	parsed, err := parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	out, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// checkStable checks that a section compares equal to itself however it
// reached the comparison: as the cfg's parsed value (what go-proxy starts
// with), as the cfg's raw JSON (what an admin update stores, unparsed), and
// re-parsed from its own normalized form (a cfg regenerated with
// export_full_config). If they differed, a regenerated cfg would be refused
// as a contradicting edit.
func checkStable[T any](t *testing.T, section string, value *T, raw json.RawMessage, parse func([]byte) (*T, error)) {
	t.Helper()
	if value == nil {
		return
	}
	marshaled, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	fromValue := canonicalOf(t, marshaled, parse)
	if fromRaw := canonicalOf(t, raw, parse); fromRaw != fromValue {
		t.Errorf("%s: the cfg's raw JSON and its parsed value normalize differently:\n%s\n%s", section, fromRaw, fromValue)
	}
	if again := canonicalOf(t, []byte(fromValue), parse); again != fromValue {
		t.Errorf("%s: normalizing isn't stable:\n%s\n%s", section, again, fromValue)
	}
}

// TestShippedCfgSectionsCompareStably runs checkStable over every live
// section of the shipped cfgs.
func TestShippedCfgSectionsCompareStably(t *testing.T) {
	for _, path := range []string{"../proxy-server.cfg", "../go-gardening-demo/gardening-demo.cfg", "../docs/enterprise-deployment/proxy-server.okta.cfg"} {
		t.Run(path, func(t *testing.T) {
			cfg, err := loadProxyConfig(path)
			if err != nil {
				t.Fatalf("loadProxyConfig: %v", err)
			}
			data, _ := os.ReadFile(path)
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			checkStable(t, "accessRules", cfg.AccessRules, raw["accessRules"], access_control.ParseConfig)
			checkStable(t, "governanceOrchestration", cfg.Orchestration, raw["governanceOrchestration"], triggered_tools_runtime.ParseConfig)
			checkStable(t, "discovery", cfg.Discovery, raw["discovery"], discover_mcp_servers.ParseConfig)
			checkStable(t, "eventDictionary", cfg.EventDictionary, raw["eventDictionary"], events.ParseDictionary)
			checkStable(t, "declarativeGuardrails", cfg.DeclarativeGuardrails, raw["declarativeGuardrails"], guardrailcfg.ParseRawConfig)
			checkStable(t, "eventAggregation", cfg.EventAggregation, raw["eventAggregation"], events.ParseAggregationConfig)
			checkStable(t, "rateLimiting", cfg.RateLimiting, raw["rateLimiting"], ratelimit.ParseConfig)
			checkStable(t, "logLevel", cfg.LogLevel, raw["logLevel"], logging.ParseConfig)
		})
	}
}

// TestRBACCoversEveryLiveSection keeps go-proxy's Role in step with the
// configsync sections: it may get/update only the override ConfigMaps it
// names, so a section missing there would be denied in the cluster.
func TestRBACCoversEveryLiveSection(t *testing.T) {
	data, err := os.ReadFile("../k8s/go-proxy-serviceaccount-rbac.yaml")
	if err != nil {
		t.Fatal(err)
	}
	granted := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s+- ([a-z-]+-override)\s*$`).FindAllStringSubmatch(string(data), -1) {
		granted[m[1]] = true
	}
	want := map[string]bool{}
	for section := range liveSections {
		want[section+"-override"] = true
	}
	if !reflect.DeepEqual(granted, want) {
		t.Errorf("the Role grants %v; the live sections need exactly %v", granted, want)
	}
}
