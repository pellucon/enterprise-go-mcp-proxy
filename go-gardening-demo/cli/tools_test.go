// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInferValue(t *testing.T) {
	cases := []struct {
		raw  string
		want any
	}{
		{"cust-0001", "cust-0001"}, // letter-prefixed id must stay a string
		{"true", true},
		{"false", false},
		{"2", int64(2)},
		{"20", int64(20)},
		{"9.99", 9.99},
		{"Alice Smith", "Alice Smith"},
		{"it-operations,business-analyst", []string{"it-operations", "business-analyst"}},
		{"prod-01, prod-02", []string{"prod-01", "prod-02"}}, // whitespace after comma trimmed
	}
	for _, c := range cases {
		got := inferValue(c.raw)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("inferValue(%q) = %#v (%T), want %#v (%T)", c.raw, got, got, c.want, c.want)
		}
	}
}

func TestBuildArgumentsGenericFields(t *testing.T) {
	spec := sections["customer-admin"]["add_or_update_customer"]
	args, err := buildArguments(spec, []string{"-id", "cust-0001", "-name", "Alice Smith", "-type", "vip"})
	if err != nil {
		t.Fatalf("buildArguments: %v", err)
	}
	want := map[string]any{"id": "cust-0001", "name": "Alice Smith", "type": "vip"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("got %#v, want %#v", args, want)
	}
}

func TestBuildArgumentsPurchaseItems(t *testing.T) {
	spec := sections["online-store"]["purchase_products"]
	args, err := buildArguments(spec, []string{"-customerId", "cust-0001", "-item", "prod-01:2", "-item", "prod-05:1"})
	if err != nil {
		t.Fatalf("buildArguments: %v", err)
	}
	if args["customerId"] != "cust-0001" {
		t.Errorf("expected customerId cust-0001, got %v", args["customerId"])
	}
	items, ok := args["items"].([]map[string]any)
	if !ok || len(items) != 2 {
		t.Fatalf("expected 2 items, got %#v", args["items"])
	}
	if items[0]["productId"] != "prod-01" || items[0]["units"] != 2 {
		t.Errorf("unexpected first item: %#v", items[0])
	}
	if items[1]["productId"] != "prod-05" || items[1]["units"] != 1 {
		t.Errorf("unexpected second item: %#v", items[1])
	}

	if _, err := buildArguments(spec, []string{"-item", "bad-format"}); err == nil {
		t.Error("expected an error for a malformed -item value")
	}
}

func TestBuildArgumentsLoadTool(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "customers.json")
	if err := os.WriteFile(path, []byte(`[{"id":"cust-0001","type":"vip"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := sections["customer-admin"]["load_customers"]
	args, err := buildArguments(spec, []string{"-file", path})
	if err != nil {
		t.Fatalf("buildArguments: %v", err)
	}
	customers, ok := args["customers"].([]any)
	if !ok || len(customers) != 1 {
		t.Fatalf("expected 1 wrapped customer entry, got %#v", args["customers"])
	}

	if _, err := buildArguments(spec, nil); err == nil {
		t.Error("expected an error when -file is missing")
	}
	if _, err := buildArguments(spec, []string{"-file", "/does/not/exist.json"}); err == nil {
		t.Error("expected an error for a nonexistent file")
	}
}

func TestTokenizeQuoting(t *testing.T) {
	tokens, err := tokenize(`customer-admin add_or_update_customer -id cust-0001 -name "Alice Smith" -type vip`)
	if err != nil {
		t.Fatalf("tokenize: %v", err)
	}
	want := []string{"customer-admin", "add_or_update_customer", "-id", "cust-0001", "-name", "Alice Smith", "-type", "vip"}
	if !reflect.DeepEqual(tokens, want) {
		t.Errorf("got %#v, want %#v", tokens, want)
	}

	if _, err := tokenize(`-name "unterminated`); err == nil {
		t.Error("expected an error for an unterminated quote")
	}
}

func TestRunCommandUnknownSectionAndTool(t *testing.T) {
	if err := dispatch(nil, nil, []string{"not-a-real-section", "foo"}); err == nil {
		t.Error("expected an error for an unknown section")
	}
	if err := dispatch(nil, nil, []string{"online-store", "not_a_real_tool"}); err == nil {
		t.Error("expected an error for an unknown tool")
	}
}

func TestDispatchHelp(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}, {"--help"}} {
		if err := dispatch(nil, nil, args); err != nil {
			t.Errorf("dispatch(%v) = %v, want nil (no session/ctx needed to print help)", args, err)
		}
	}
}

func TestRunAdminCommandUnknownTool(t *testing.T) {
	if err := runAdminCommand(nil, nil, "not_a_real_admin_tool", nil); err == nil {
		t.Error("expected an error for an unknown admin tool")
	}
}

func TestParseVerboseFlag(t *testing.T) {
	cases := []struct {
		tokens []string
		want   bool
	}{
		{nil, false},
		{[]string{"-verbose"}, true},
		{[]string{"-verbose=true"}, true},
		{[]string{"-verbose=false"}, false},
	}
	for _, c := range cases {
		got, err := parseVerboseFlag(c.tokens)
		if err != nil {
			t.Fatalf("parseVerboseFlag(%v): %v", c.tokens, err)
		}
		if got != c.want {
			t.Errorf("parseVerboseFlag(%v) = %v, want %v", c.tokens, got, c.want)
		}
	}

	if _, err := parseVerboseFlag([]string{"-not-a-real-flag"}); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

func TestParseTriggeredTools(t *testing.T) {
	sample := []any{
		map[string]any{"name": "proxy_utility.request_constraints_validation", "description": "d1"},
		map[string]any{"name": "proxy_utility.result_constraints_validation", "description": "d2"},
	}
	tools, err := parseTriggeredTools(sample)
	if err != nil {
		t.Fatalf("parseTriggeredTools: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "proxy_utility.request_constraints_validation" || tools[1].Name != "proxy_utility.result_constraints_validation" {
		t.Errorf("got %#v", tools)
	}
	if tools[0].Description != "d1" || tools[1].Description != "d2" {
		t.Errorf("descriptions not preserved: %#v", tools)
	}

	if _, err := parseTriggeredTools(func() {}); err == nil {
		t.Error("expected an error for un-marshalable structuredContent")
	}
	if _, err := parseTriggeredTools([]any{"not an object"}); err == nil {
		t.Error("expected an error when array elements aren't Tool-shaped objects")
	}
}
