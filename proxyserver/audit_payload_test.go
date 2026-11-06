// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

func TestAuditPayloadCapsSize(t *testing.T) {
	small := map[string]any{"name": "store.buy", "arguments": map[string]any{"qty": 2}}
	if got := auditPayload(small); got == nil || got.(map[string]any)["name"] != "store.buy" {
		t.Errorf("a small payload was changed: %v", got)
	}
	big := map[string]any{"data": strings.Repeat("x", maxAuditPayloadBytes)}
	got, ok := auditPayload(big).(map[string]any)
	if !ok || got["truncated"] != true || got["bytes"].(int) <= maxAuditPayloadBytes || len(got["sha256"].(string)) != 64 {
		t.Errorf("an oversized payload wasn't summarized: %v", got)
	}
	if _, hasData := got["data"]; hasData {
		t.Error("the summary still carries the content")
	}
}

// TestToolRequestResultEventPayloads checks what tool-request-result audit
// events carry: no result when the result was blocked, and summaries
// instead of oversized payloads.
func TestToolRequestResultEventPayloads(t *testing.T) {
	url := startTestNATSServer(t)
	client, err := events.NewClient(url)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cfgRaw, err := os.ReadFile("testdata/orchestration_test.cfg")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		EventDictionary json.RawMessage `json:"eventDictionary"`
	}
	if err := json.Unmarshal(cfgRaw, &cfg); err != nil {
		t.Fatal(err)
	}
	dict, err := events.ParseDictionary(cfg.EventDictionary)
	if err != nil {
		t.Fatal(err)
	}
	client.SetDictionary(dict)
	old := eventsClient
	eventsClient = client
	t.Cleanup(func() { eventsClient = old })

	received := make(chan *events.Event, 4)
	if _, err := client.Subscribe("events.tool-request-result.>", func(e *events.Event) { received <- e }); err != nil {
		t.Fatal(err)
	}
	client.Flush()

	next := func() *events.Event {
		t.Helper()
		select {
		case e := <-received:
			return e
		case <-time.After(2 * time.Second):
			t.Fatal("no event received")
			return nil
		}
	}
	base := triggered_tools_runtime.ToolRequestResultEvent{Caller: "alice", Roles: []string{"user"}, Tool: "crm.get_customer", ToolCallID: "id-1", CallRequest: map[string]any{"name": "crm.get_customer"}}

	blocked := base
	blocked.Subtype = triggered_tools_runtime.ToolRequestResultRequestBlocked
	blocked.Reason = "result rejected"
	blocked.CallResult = map[string]any{"ssn": "123-45-6789"}
	if err := publishToolRequestResult(context.Background(), blocked); err != nil {
		t.Fatal(err)
	}
	if e := next(); e.Metadata["result"] != nil {
		t.Errorf("a blocked result reached the audit event: %v", e.Metadata["result"])
	}

	large := base
	large.Subtype = triggered_tools_runtime.ToolRequestResultCallSucceeded
	large.CallResult = map[string]any{"rows": strings.Repeat("r", 2*maxAuditPayloadBytes)}
	if err := publishToolRequestResult(context.Background(), large); err != nil {
		t.Fatal(err)
	}
	e := next()
	result, _ := e.Metadata["result"].(map[string]any)
	if result["truncated"] != true {
		t.Errorf("an oversized result wasn't summarized: %v", e.Metadata["result"])
	}
	if request, _ := e.Metadata["request"].(map[string]any); request["name"] != "crm.get_customer" {
		t.Errorf("a small request was changed: %v", e.Metadata["request"])
	}
}
