// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIsTriggeredToolMarked(t *testing.T) {
	tool := &mcp.Tool{Name: "call-authorization", Meta: mcp.Meta{TriggeredToolMetaKey: true}}
	if !IsTriggeredTool(tool) {
		t.Error("expected a tool with the triggered-tool meta key set to true to be recognized as a triggered tool")
	}
}

func TestIsTriggeredToolUnmarked(t *testing.T) {
	tool := &mcp.Tool{Name: "say_hello"}
	if IsTriggeredTool(tool) {
		t.Error("expected a tool with no _meta to NOT be recognized as a triggered tool")
	}
}

func TestIsTriggeredToolFalseValue(t *testing.T) {
	tool := &mcp.Tool{Name: "say_hello", Meta: mcp.Meta{TriggeredToolMetaKey: false}}
	if IsTriggeredTool(tool) {
		t.Error("expected a tool with the triggered-tool meta key explicitly set to false to NOT be recognized as a triggered tool")
	}
}

func TestIsTriggeredToolNil(t *testing.T) {
	if IsTriggeredTool(nil) {
		t.Error("expected a nil tool to NOT be recognized as a triggered tool")
	}
}

// TestIsTriggeredToolAfterWireRoundTrip confirms IsTriggeredTool works on a
// tool that went through a real JSON marshal/unmarshal (i.e. as it would
// arrive from a client-side ListTools call), not just one constructed
// directly in-process.
func TestIsTriggeredToolAfterWireRoundTrip(t *testing.T) {
	original := &mcp.Tool{Name: "tool-call-audit", Meta: mcp.Meta{TriggeredToolMetaKey: true}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("failed to marshal tool: %v", err)
	}
	var roundTripped mcp.Tool
	if err := json.Unmarshal(raw, &roundTripped); err != nil {
		t.Fatalf("failed to unmarshal tool: %v", err)
	}
	if !IsTriggeredTool(&roundTripped) {
		t.Errorf("expected round-tripped tool to still be recognized as a triggered tool, got Meta=%+v", roundTripped.Meta)
	}
}
