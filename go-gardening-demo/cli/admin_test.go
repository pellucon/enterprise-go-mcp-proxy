// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"reflect"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
)

func TestParseFileFlag(t *testing.T) {
	got, err := parseFileFlag([]string{"-file", "some/path.json"})
	if err != nil {
		t.Fatalf("parseFileFlag: %v", err)
	}
	if got != "some/path.json" {
		t.Errorf("got %q, want %q", got, "some/path.json")
	}

	if _, err := parseFileFlag(nil); err == nil {
		t.Error("expected an error when -file is missing")
	}
	if _, err := parseFileFlag([]string{"-not-a-real-flag", "x"}); err == nil {
		t.Error("expected an error for an unknown flag")
	}
}

// isGardenOnlyRule/mergeAccessRules (and mergeDeclarativeGuardrails/
// mergeOrchestration) now live in go-gardening-demo/configmerge, along with
// their own tests — extracted so go-gardening-demo/patchcfg (a separate
// package main) can reuse the exact same merge logic offline, against a
// committed *.cfg file directly, without duplicating it (see
// configmerge/merge.go's own doc comment).

func TestRunAdminCommandUpdateAccessRulesUnknownFile(t *testing.T) {
	if err := runAdminCommand(nil, nil, "update_access_rules", []string{"-file", "/does/not/exist.json"}); err == nil {
		t.Error("expected an error for a nonexistent -file")
	}
	if err := runAdminCommand(nil, nil, "update_access_rules", nil); err == nil {
		t.Error("expected an error when -file is missing")
	}
}

func TestRunAdminCommandLoadPromptsUnknownFile(t *testing.T) {
	if err := runAdminCommand(nil, nil, "load_prompts", []string{"-file", "/does/not/exist.json"}); err == nil {
		t.Error("expected an error for a nonexistent -file")
	}
	if err := runAdminCommand(nil, nil, "load_prompts", nil); err == nil {
		t.Error("expected an error when -file is missing")
	}
}

func TestGroupPromptsByServer(t *testing.T) {
	entries := []promptsFileEntry{
		{Server: "garden-business-ops", Prompt: promptstore.Prompt{Name: "p1"}},
		{Server: "garden-online-store", Prompt: promptstore.Prompt{Name: "p2"}},
		{Server: "garden-business-ops", Prompt: promptstore.Prompt{Name: "p3"}},
	}
	servers, byServer, err := groupPromptsByServer(entries)
	if err != nil {
		t.Fatalf("groupPromptsByServer: %v", err)
	}
	if !reflect.DeepEqual(servers, []string{"garden-business-ops", "garden-online-store"}) {
		t.Errorf("expected servers in first-seen order, got %v", servers)
	}
	if len(byServer["garden-business-ops"]) != 2 || byServer["garden-business-ops"][0].Name != "p1" || byServer["garden-business-ops"][1].Name != "p3" {
		t.Errorf("expected garden-business-ops to have [p1, p3], got %+v", byServer["garden-business-ops"])
	}
	if len(byServer["garden-online-store"]) != 1 || byServer["garden-online-store"][0].Name != "p2" {
		t.Errorf("expected garden-online-store to have [p2], got %+v", byServer["garden-online-store"])
	}
}

func TestGroupPromptsByServerMissingServer(t *testing.T) {
	_, _, err := groupPromptsByServer([]promptsFileEntry{{Prompt: promptstore.Prompt{Name: "p1"}}})
	if err == nil {
		t.Error("expected an error for an entry with no \"server\"")
	}
}
