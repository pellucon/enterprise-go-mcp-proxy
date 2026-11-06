// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"encoding/json"
	"testing"
)

func TestDoscovery(t *testing.T) {
	t.Skip("Skipping this test - requires running the MCP servers manually")
	serverMap, err := DiscoverMCPs("mcp_discovery_test.json")

	if err != nil {
		t.Errorf("Failed %v", err)
	}
	if serverMap["mcp1"].ServerUrl == "" {
		t.Errorf("Failed %v", err)
	}
	if serverMap["mcp2"].ServerUrl == "" {
		t.Errorf("Failed %v", err)
	}
	if serverMap["mcp1"].ServerName != "mcp1" {
		t.Errorf("Failed %v", err)
	}

	if serverMap["mcp2"].ServerName != "mcp2" {
		t.Errorf("Failed %v", err)
	}

}

func TestParseConfigFile(t *testing.T) {
	// Test parsing scan mode with frequency
	config, err := ParseConfigFile("mcp_discovery_test.json")
	if err != nil {
		t.Fatalf("Failed to parse config file: %v", err)
	}
	if config.Mode != ModeScan {
		t.Errorf("Expected ModeScan, got %v", config.Mode)
	}
	if config.FrequencySeconds != 1 {
		t.Errorf("Expected FrequencySeconds to be 1, got %v", config.FrequencySeconds)
	}
	if config.ToolsPollFrequencySeconds != 5 {
		t.Errorf("Expected ToolsPollFrequencySeconds to be 5, got %v", config.ToolsPollFrequencySeconds)
	}
	if config.PromptsPollFrequencySeconds != 5 {
		t.Errorf("Expected PromptsPollFrequencySeconds to be 5, got %v", config.PromptsPollFrequencySeconds)
	}
	if config.ResourcesPollFrequencySeconds != 5 {
		t.Errorf("Expected ResourcesPollFrequencySeconds to be 5, got %v", config.ResourcesPollFrequencySeconds)
	}
	if config.ResourceTemplatesPollFrequencySeconds != 5 {
		t.Errorf("Expected ResourceTemplatesPollFrequencySeconds to be 5, got %v", config.ResourceTemplatesPollFrequencySeconds)
	}
}

func TestParseConfig_StaticMode(t *testing.T) {
	config, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
			"listedTools": ["*"],
		"servers": [
			{"serverName": "mcp1", "serverUrl": "http://localhost:8082/mcp"},
			{"serverName": "mcp2", "serverUrl": "http://localhost:8083/mcp"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if len(config.ServerMap) != 2 {
		t.Fatalf("expected 2 servers, got %d: %+v", len(config.ServerMap), config.ServerMap)
	}
}

// TestMarshalJSON_RoundTripsThroughParseConfig proves MCPDiscoveryConfig's
// MarshalJSON/UnmarshalJSON produce and accept the exact same
// discoveryMode/servers/... wire shape ParseConfig does - not
// encoding/json's default field-name-keyed shape - so a value round-tripped
// through _admin.export_discovery is valid input for _admin.update_discovery
// and for a real proxy-server.cfg's "discovery" section.
func TestMarshalJSON_RoundTripsThroughParseConfig(t *testing.T) {
	original, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
			"listedTools": ["*"],
		"toolsPollFrequencySeconds": 5,
		"servers": [
			{"serverName": "mcp1", "serverUrl": "http://localhost:8082/mcp"},
			{"serverName": "mcp2", "serverUrl": "http://localhost:8083/mcp"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to decode marshaled output as generic JSON: %v", err)
	}
	if _, ok := m["discoveryMode"]; !ok {
		t.Errorf("expected a \"discoveryMode\" key in marshaled output, got %s", data)
	}
	if _, ok := m["servers"]; !ok {
		t.Errorf("expected a \"servers\" key in marshaled output, got %s", data)
	}
	if _, ok := m["Mode"]; ok {
		t.Errorf("expected no \"Mode\" key (encoding/json's default, unwanted, shape) in marshaled output, got %s", data)
	}

	roundTripped, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig on the marshaled output: %v", err)
	}
	if len(roundTripped.ServerMap) != 2 {
		t.Fatalf("expected 2 servers after round-tripping, got %d: %+v", len(roundTripped.ServerMap), roundTripped.ServerMap)
	}
	if roundTripped.ToolsPollFrequencySeconds != 5 {
		t.Errorf("expected toolsPollFrequencySeconds to survive the round trip, got %d", roundTripped.ToolsPollFrequencySeconds)
	}

	var viaUnmarshalJSON MCPDiscoveryConfig
	if err := json.Unmarshal(data, &viaUnmarshalJSON); err != nil {
		t.Fatalf("UnmarshalJSON on the marshaled output: %v", err)
	}
	if len(viaUnmarshalJSON.ServerMap) != 2 {
		t.Fatalf("expected 2 servers via UnmarshalJSON, got %d: %+v", len(viaUnmarshalJSON.ServerMap), viaUnmarshalJSON.ServerMap)
	}
}

// TestMarshalJSON_ScanModeOmitsServersKey proves scan/k8s-mode configs
// don't emit a spurious "servers":null (jsonConfig.Servers has no value
// set for these modes) - matching what a hand-written config for these
// modes actually looks like.
func TestMarshalJSON_ScanModeOmitsServersKey(t *testing.T) {
	original, err := ParseConfig([]byte(`{"discoveryMode": "scan", "frequencySeconds": 2, "listedTools": ["*"]}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to decode marshaled output as generic JSON: %v", err)
	}
	if _, ok := m["servers"]; ok {
		t.Errorf("expected no \"servers\" key for scan mode, got %s", data)
	}
}

func TestParseConfig_StaticMode_RejectsEmptyServerName(t *testing.T) {
	_, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
			"listedTools": ["*"],
		"servers": [{"serverName": "", "serverUrl": "http://localhost:8082/mcp"}]
	}`))
	if err == nil {
		t.Fatal("expected an error for an empty serverName, got nil")
	}
}

func TestParseConfig_StaticMode_RejectsEmptyServerUrl(t *testing.T) {
	_, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
			"listedTools": ["*"],
		"servers": [{"serverName": "mcp1", "serverUrl": ""}]
	}`))
	if err == nil {
		t.Fatal("expected an error for an empty serverUrl, got nil")
	}
}

func TestParseConfig_StaticMode_RejectsDuplicateServerName(t *testing.T) {
	_, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
			"listedTools": ["*"],
		"servers": [
			{"serverName": "mcp1", "serverUrl": "http://localhost:8082/mcp"},
			{"serverName": "mcp1", "serverUrl": "http://localhost:8083/mcp"}
		]
	}`))
	if err == nil {
		t.Fatal("expected an error for a duplicate serverName, got nil")
	}
}

// TestParseConfig_RejectsMissingListedTools covers listedTools' mandatory
// status: a config that never mentions the key at all is rejected, distinct
// from one that gives it as an explicit empty array (see the next test).
func TestParseConfig_RejectsMissingListedTools(t *testing.T) {
	_, err := ParseConfig([]byte(`{"discoveryMode": "scan"}`))
	if err == nil {
		t.Fatal("expected an error for a config with no listedTools key at all, got nil")
	}
}

// TestParseConfig_AcceptsEmptyListedTools covers the other half of
// listedTools' contract: an explicit empty array is a valid (very
// restrictive - "list nothing") value, not a mandatory-field violation.
func TestParseConfig_AcceptsEmptyListedTools(t *testing.T) {
	config, err := ParseConfig([]byte(`{"discoveryMode": "scan", "listedTools": []}`))
	if err != nil {
		t.Fatalf("expected an explicit empty listedTools to be valid, got: %v", err)
	}
	if len(config.ListedTools) != 0 {
		t.Errorf("expected an empty ListedTools, got %v", config.ListedTools)
	}
}

// TestMarshalJSON_AlwaysEmitsListedTools proves a zero-value/never-loaded
// MCPDiscoveryConfig still marshals listedTools as "[]", never a bare
// "null" - since the field is mandatory, re-importing that output must
// never fail listedTools' own presence check.
func TestMarshalJSON_AlwaysEmitsListedTools(t *testing.T) {
	var zero MCPDiscoveryConfig
	data, err := json.Marshal(zero)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("failed to decode marshaled output as generic JSON: %v", err)
	}
	lt, ok := m["listedTools"]
	if !ok {
		t.Fatalf("expected a \"listedTools\" key in marshaled output, got %s", data)
	}
	if lt == nil {
		t.Errorf("expected listedTools to marshal as [], not null, got %s", data)
	}
}

func TestParseConfig_RejectsUnknownMode(t *testing.T) {
	_, err := ParseConfig([]byte(`{"discoveryMode": "bogus", "listedTools": ["*"]}`))
	if err == nil {
		t.Fatal("expected an error for an unknown discoveryMode, got nil")
	}
}

func TestParseConfig_StaticMode_CredentialsSecretRoundTrips(t *testing.T) {
	config, err := ParseConfig([]byte(`{
		"discoveryMode": "static",
		"listedTools": ["*"],
		"servers": [
			{"serverName": "crm", "serverUrl": "http://crm.internal/mcp", "credentialsSecret": "crm-api-key"},
			{"serverName": "open", "serverUrl": "http://open.internal/mcp"}
		]
	}`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if got := config.ServerMap["crm"].CredentialsSecret; got != "crm-api-key" {
		t.Errorf("crm CredentialsSecret = %q, want %q", got, "crm-api-key")
	}
	if got := config.ServerMap["open"].CredentialsSecret; got != "" {
		t.Errorf("open CredentialsSecret = %q, want empty", got)
	}

	data, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := ParseConfig(data)
	if err != nil {
		t.Fatalf("ParseConfig of marshaled config: %v", err)
	}
	if got := again.ServerMap["crm"].CredentialsSecret; got != "crm-api-key" {
		t.Errorf("after round trip CredentialsSecret = %q, want %q", got, "crm-api-key")
	}
}

func TestParseConfig_StaticMode_RejectsInvalidServerName(t *testing.T) {
	for _, name := range []string{"a.b", "a:b", "a/b", "_admin", "-x", "a b"} {
		_, err := ParseConfig([]byte(`{
			"discoveryMode": "static",
			"listedTools": ["*"],
			"servers": [{"serverName": "` + name + `", "serverUrl": "http://localhost:8082/mcp"}]
		}`))
		if err == nil {
			t.Errorf("serverName %q: expected an error, got nil", name)
		}
	}
}

func TestValidateServerName(t *testing.T) {
	for _, name := range []string{"rate-limiter", "garden_online_store", "mcp1", "A1"} {
		if err := ValidateServerName(name); err != nil {
			t.Errorf("ValidateServerName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "a.b", "a:b", "a/b", "_admin", "-x", "a b", "é"} {
		if err := ValidateServerName(name); err == nil {
			t.Errorf("ValidateServerName(%q) = nil, want an error", name)
		}
	}
}

func TestCheckScanProcess(t *testing.T) {
	tests := []struct {
		name string
		uids []uint32
		url  string
		ok   bool
	}{
		{"own user, localhost", []uint32{501}, "http://localhost:8080/mcp", true},
		{"own user, loopback IP", []uint32{501, 501, 501, 501}, "http://127.0.0.1:8080/mcp", true},
		{"own user, IPv6 loopback", []uint32{501}, "http://[::1]:8080/mcp", true},
		{"another user", []uint32{502}, "http://localhost:8080/mcp", false},
		{"setuid to another user", []uint32{501, 0}, "http://localhost:8080/mcp", false},
		{"owner unknown", nil, "http://localhost:8080/mcp", false},
		{"remote host", []uint32{501}, "http://attacker.example:9/mcp", false},
		{"empty URL", []uint32{501}, "", false},
		{"not http", []uint32{501}, "file:///etc/passwd", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkScanProcess(tt.uids, 501, tt.url); (err == nil) != tt.ok {
				t.Errorf("checkScanProcess(%v, %q) = %v; want ok=%v", tt.uids, tt.url, err, tt.ok)
			}
		})
	}
}
