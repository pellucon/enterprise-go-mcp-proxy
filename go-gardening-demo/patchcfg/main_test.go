// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", path, err)
	}
}

func TestPatchConfig_MergesPresentSectionsAndSkipsAbsent(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "client_access_rules.json"), `[
		{"role": "customer", "allowed_tools": ["garden-online-store.purchase_products"]}
	]`)
	writeTestFile(t, filepath.Join(dataDir, "declarative_guardrails.json"), `[
		{"name": "store-purchase-cost-limit", "actions": ["block"]}
	]`)
	writeTestFile(t, filepath.Join(dataDir, "orchestration.json"), `{
		"orchestrations": {"store-purchase-guardrails": {"toolCallAudit": ["publish-tool-request-result"]}},
		"toolMappings": [{"pattern": "garden-online-store.purchase_products", "orchestration": "store-purchase-guardrails"}]
	}`)

	// Deliberately mirrors gardening_demo_test.cfg's own shape: has
	// accessRules/declarativeGuardrails but no governanceOrchestration key
	// at all, plus an unrelated key ("selfUrl") that must survive untouched.
	cfgPath := filepath.Join(t.TempDir(), "test.cfg")
	writeTestFile(t, cfgPath, `{
  "accessRules": {
    "clientAccessRules": [
      {"role": "admin", "allowed_tools": ["*"]}
    ]
  },
  "declarativeGuardrails": {
    "actions": ["log"],
    "declarations": []
  },
  "selfUrl": "http://example.invalid/mcp"
}
`)

	if err := patchConfig(cfgPath, dataDir); err != nil {
		t.Fatalf("patchConfig: %v", err)
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read patched file: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("patched file is not valid JSON: %v", err)
	}

	if _, ok := doc["governanceOrchestration"]; ok {
		t.Error("governanceOrchestration should not have been added — the original cfg had no such key")
	}

	var accessRules struct {
		ClientAccessRules []map[string]any `json:"clientAccessRules"`
	}
	if err := json.Unmarshal(doc["accessRules"], &accessRules); err != nil {
		t.Fatalf("failed to parse patched accessRules: %v", err)
	}
	if len(accessRules.ClientAccessRules) != 2 {
		t.Errorf("expected admin (kept) + customer (merged in) = 2 rules, got %d: %#v", len(accessRules.ClientAccessRules), accessRules.ClientAccessRules)
	}

	var guardrails struct {
		Actions      []string         `json:"actions"`
		Declarations []map[string]any `json:"declarations"`
	}
	if err := json.Unmarshal(doc["declarativeGuardrails"], &guardrails); err != nil {
		t.Fatalf("failed to parse patched declarativeGuardrails: %v", err)
	}
	if len(guardrails.Declarations) != 1 || guardrails.Declarations[0]["name"] != "store-purchase-cost-limit" {
		t.Errorf("expected exactly the merged-in declaration, got %#v", guardrails.Declarations)
	}
	wantActions := map[string]bool{"log": true, "block": true}
	if len(guardrails.Actions) != len(wantActions) {
		t.Errorf("actions: got %v, want set %v", guardrails.Actions, wantActions)
	}

	var selfURL string
	if err := json.Unmarshal(doc["selfUrl"], &selfURL); err != nil || selfURL != "http://example.invalid/mcp" {
		t.Errorf("unrelated key selfUrl was not preserved untouched: %q, err=%v", selfURL, err)
	}
}

func TestPatchConfig_PreservesTopLevelKeyOrder(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(dataDir, "client_access_rules.json"), `[]`)

	cfgPath := filepath.Join(t.TempDir(), "test.cfg")
	// Deliberately non-alphabetical order, mirroring gardening_demo_test.cfg's
	// own hand-authored ordering (e.g. "_comment" then "discovery" before
	// "accessRules") - proves this isn't silently re-sorted to alphabetical,
	// which a naive map[string]json.RawMessage round-trip would do.
	writeTestFile(t, cfgPath, `{
  "zzz_last": 1,
  "accessRules": {"clientAccessRules": []},
  "aaa_first": 2
}
`)

	if err := patchConfig(cfgPath, dataDir); err != nil {
		t.Fatalf("patchConfig: %v", err)
	}

	keys, _, err := orderedDocument(mustRead(t, cfgPath))
	if err != nil {
		t.Fatalf("failed to parse patched file: %v", err)
	}
	want := []string{"zzz_last", "accessRules", "aaa_first"}
	if len(keys) != len(want) {
		t.Fatalf("got keys %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key order: got %v, want %v", keys, want)
			break
		}
	}
}

func TestPatchConfig_MissingDataFileIsAnError(t *testing.T) {
	dataDir := t.TempDir() // no client_access_rules.json in here

	cfgPath := filepath.Join(t.TempDir(), "test.cfg")
	writeTestFile(t, cfgPath, `{"accessRules": {"clientAccessRules": []}}`)

	if err := patchConfig(cfgPath, dataDir); err == nil {
		t.Fatal("expected an error when the backing initial-data file is missing")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	return raw
}
