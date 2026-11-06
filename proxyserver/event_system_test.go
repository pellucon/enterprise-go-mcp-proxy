// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

const (
	EVENT_AUTH_PORT       = ":8097"
	EVENT_AGGREGATOR_PORT = ":8098" // deliberately not :8099 - that's claude-bridge's own hardcoded default port (see claude-bridge/main.go), which commonly runs persistently on a dev machine
	EVENT_DEMO_PORT       = ":8100"
	EVENT_TEST_PROXY_PORT = ":8101"
	EVENT_TEST_PROXY_URL  = "http://localhost:8101/mcp"
)

// startTestNATSServer starts an embedded, in-process NATS server on a
// random free port for the duration of the test - mirrors
// events/client_test.go's helper of the same shape (duplicated since
// package main can't import that _test.go file, and it's not worth
// exporting a test-only helper across packages for one use).
func startTestNATSServer(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create embedded NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(4 * time.Second) {
		t.Fatal("embedded NATS server not ready in time")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// eventCallRequestName extracts the "name" field from a marshaled
// *mcp.CallToolParamsRaw (what triggered_tools_runtime.Runtime.Guard sends
// as CallRequest) - duplicated from authorization-mcp-server's identical
// helper, same cross-package-main constraint.
func eventCallRequestName(callRequest any) string {
	m, ok := callRequest.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["name"].(string)
	return name
}

// createEventTestAuthorizationServer is an in-process stand-in for
// sample-mcp-servers/authorization-mcp-server's call-authorization tool,
// publishing the same "alert"/"tool-call-blocked" event on rejection.
func createEventTestAuthorizationServer(eventsClient *events.Client) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "event-auth-mcp", Version: "1.0.0"}, nil)
	platformMeta := mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true}

	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization", Meta: platformMeta},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			name := eventCallRequestName(in.CallRequest)
			authorized := !strings.Contains(name, "bad")
			if !authorized {
				err := eventsClient.Publish(&events.Event{
					Type:    "alert",
					Subtype: "tool-call-blocked",
					Time:    time.Now().UTC(),
					Title:   "Tool call rejected",
					Text:    `tool name contains "bad"`,
					Metadata: map[string]any{
						"user":       in.User,
						"roles":      in.Roles,
						"tool":       name,
						"reason":     `tool name contains "bad"`,
						"toolCallId": in.ToolCallID,
					},
				})
				if err != nil {
					log.Printf("test: failed to publish alert event: %v", err)
				}
			}
			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: authorized}, nil
		})

	return server
}

// testEventBuffer is a minimal in-process stand-in for
// platform-mcp-servers/event-aggregator's ringBuffer, duplicated (not
// cross-imported) for the same reason as everything else in this file.
type testEventBuffer struct {
	mu     sync.Mutex
	events []*events.Event
}

func (b *testEventBuffer) add(e *events.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, e)
}

func (b *testEventBuffer) list(typeFilter string) []*events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []*events.Event
	for _, e := range b.events {
		if typeFilter == "" || e.Type == typeFilter {
			out = append(out, e)
		}
	}
	return out
}

// createEventTestAggregatorServer is an in-process stand-in for
// platform-mcp-servers/event-aggregator: an ordinary (non-triggered) tool
// reading from a buffer that a NATS subscription feeds in the background.
func createEventTestAggregatorServer(buffer *testEventBuffer) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "event-aggregator", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_events",
		Description: "List received events, optionally filtered by type.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Type string `json:"type,omitempty"`
	}) (*mcp.CallToolResult, any, error) {
		data, err := json.Marshal(buffer.list(args.Type))
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal events: %w", err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})

	return server
}

// createEventDemoServer is an in-process ordinary backend hosting the
// tools the test orchestration guards: good_tool (clean name, always
// succeeds) and bad_tool (name contains "bad", rejected by
// call-authorization before the handler ever runs).
func createEventDemoServer(badToolInvoked *bool) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "event-demo", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "good_tool", Description: "Always succeeds"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "bad_tool", Description: "Rejected by call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			*badToolInvoked = true
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "should never get here"}}}, nil, nil
		})

	return server
}

// TestEventSystem proves the NATS event system end-to-end: a rejected tool
// call produces both an "alert" event (from the authorization stand-in)
// and a "tool-request-result"/result-blocked event (from go-proxy's own
// orchestration engine), a successful tool call produces a
// "tool-request-result"/call-succeeded event, and all of it is observable
// via an event-aggregator-style subscriber - all through the real
// orchestration/proxy path, not just the events package's own unit tests
// (events/client_test.go).
func TestEventSystem(t *testing.T) {
	natsURL := startTestNATSServer(t)
	const dictionaryPath = "testdata/event_dictionary_test.json" // committed, {"version":N,"config":{"eventTypes":{...}}} envelope

	publisherClient, err := events.NewClient(natsURL)
	if err != nil {
		t.Fatalf("failed to build publisher events client: %v", err)
	}
	defer publisherClient.Close()

	subscriberClient, err := events.NewClient(natsURL)
	if err != nil {
		t.Fatalf("failed to build subscriber events client: %v", err)
	}
	defer subscriberClient.Close()

	// No live NATS-triggered reload needed for this test - just the
	// one-shot envelope-unwrap-and-validate load configsync.Watcher shares
	// with every real caller (see the real servers'/go-proxy's own main.go).
	dictWatcher, err := configsync.NewWatcher("event-dictionary", dictionaryPath, nil, (*events.Dictionary)(nil), events.ParseDictionary, nil)
	if err != nil {
		t.Fatalf("failed to load event dictionary: %v", err)
	}
	publisherClient.SetDictionary(dictWatcher.Current())
	subscriberClient.SetDictionary(dictWatcher.Current())

	buffer := &testEventBuffer{}
	if _, err := subscriberClient.Subscribe("events.>", buffer.add); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	if err := subscriberClient.Flush(); err != nil {
		t.Fatalf("failed to flush subscriber: %v", err)
	}

	var badToolInvoked bool
	authServer := createEventTestAuthorizationServer(publisherClient)
	aggregatorServer := createEventTestAggregatorServer(buffer)
	demoServer := createEventDemoServer(&badToolInvoked)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestBackend(t, ctx, cancel, EVENT_AUTH_PORT, authServer)
	startTestBackend(t, ctx, cancel, EVENT_AGGREGATOR_PORT, aggregatorServer)
	startTestBackend(t, ctx, cancel, EVENT_DEMO_PORT, demoServer)

	// natsUrl points go-proxy's own eventsClient (see main.go's doRunOn) at
	// the same embedded NATS instance the test's own publisher/subscriber
	// clients use, so go-proxy's directly-published tool-request-result
	// events land in the same buffer. eventDictionary must describe both
	// "alert" and "tool-request-result" - go-proxy's own Publish call fails
	// closed against an empty dictionary exactly like any other Client.
	cfg := fmt.Sprintf(`{
  "discovery": {
    "discoveryMode": "static",
    "listedTools": ["*"],
    "servers": [
      { "serverName": "event-auth-mcp", "serverUrl": "http://localhost%s/mcp" },
      { "serverName": "event-aggregator", "serverUrl": "http://localhost%s/mcp" },
      { "serverName": "event-demo", "serverUrl": "http://localhost%s/mcp" }
    ]
  },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] }
    ]
  },
  "auth": {
    "publicKeyPath": "./public.pem",
    "jwksUrl": "",
    "issuer": "",
    "audience": ""
  },
  "governanceOrchestration": {
    "orchestrations": {
      "event-demo-guardrails": {
        "callAuthorization": [
          { "server": "event-auth-mcp", "tool": "call-authorization" }
        ],
        "toolCallAudit": ["publish-tool-request-result"]
      }
    },
    "toolMappings": [
      { "pattern": "event-demo.*", "orchestration": "event-demo-guardrails" }
    ]
  },
  "eventDictionary": {
    "eventTypes": {
      "alert": {
        "subtypes": ["tool-call-blocked", "result-blocked"],
        "metadata": {
          "user": { "type": "string", "required": true },
          "roles": { "type": "json", "required": true },
          "tool": { "type": "string", "required": true },
          "reason": { "type": "string", "required": true },
          "toolCallId": { "type": "string", "required": true }
        }
      },
      "tool-request-result": {
        "subtypes": ["result-blocked", "call-failed", "call-succeeded"],
        "metadata": {
          "user": { "type": "string", "required": true },
          "roles": { "type": "json", "required": true },
          "tool": { "type": "string", "required": true },
          "request": { "type": "json", "required": true },
          "result": { "type": "json", "required": false },
          "reason": { "type": "string", "required": false },
          "toolCallId": { "type": "string", "required": true }
        }
      }
    }
  },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, EVENT_AUTH_PORT, EVENT_AGGREGATOR_PORT, EVENT_DEMO_PORT, natsURL)

	cfgPath := t.TempDir() + "/event-system-test.cfg"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	startTestProxy(t, ctx, cancel, cfgPath, EVENT_TEST_PROXY_PORT)

	// Scenario 1: a rejected tool call produces an alert/tool-call-blocked
	// event (from the authorization stand-in) and a
	// tool-request-result/result-blocked event (from go-proxy's own
	// orchestration engine).
	badParams := mcp.CallToolParams{Name: "event-demo.bad_tool", Arguments: map[string]any{}}
	if _, err := callTool(EVENT_TEST_PROXY_URL, badParams); err == nil {
		t.Error("expected event-demo.bad_tool to be rejected by call-authorization")
	}
	if badToolInvoked {
		t.Error("expected bad_tool's handler to never be invoked")
	}

	// Scenario 2: a successful tool call produces a
	// tool-request-result/call-succeeded event.
	goodParams := mcp.CallToolParams{Name: "event-demo.good_tool", Arguments: map[string]any{}}
	result, err := callTool(EVENT_TEST_PROXY_URL, goodParams)
	if err != nil {
		t.Fatalf("expected event-demo.good_tool to succeed, got: %v", err)
	}
	if result != "ok" {
		t.Errorf("unexpected good_tool result: %q", result)
	}

	alerts := waitForEventCount(t, buffer, "alert", 1)
	if len(alerts) != 1 {
		t.Fatalf("expected exactly 1 alert event, got %d", len(alerts))
	}
	if alerts[0].Subtype != "tool-call-blocked" {
		t.Errorf("expected subtype tool-call-blocked, got %q", alerts[0].Subtype)
	}
	// The tool name a triggered tool sees is the namespaced name
	// ("event-demo.bad_tool"), not the bare name - the namespace is only
	// stripped later, when the backend session actually dispatches the
	// call (see dynamic_proxy.Gateway.registerTool).
	if alerts[0].Metadata["tool"] != "event-demo.bad_tool" {
		t.Errorf("expected alert metadata tool=event-demo.bad_tool, got %v", alerts[0].Metadata["tool"])
	}
	if alerts[0].Metadata["reason"] == nil {
		t.Error("expected alert metadata to include a reason")
	}
	alertToolCallID, _ := alerts[0].Metadata["toolCallId"].(string)
	if alertToolCallID == "" {
		t.Error("expected alert metadata to include a non-empty toolCallId")
	}

	toolRequestResults := waitForEventCount(t, buffer, "tool-request-result", 2)
	if len(toolRequestResults) != 2 {
		t.Fatalf("expected exactly 2 tool-request-result events (bad_tool and good_tool both guarded), got %d", len(toolRequestResults))
	}
	var sawRejected, sawSucceededWithResult bool
	var rejectedToolCallID, succeededToolCallID string
	for _, e := range toolRequestResults {
		switch e.Metadata["tool"] {
		case "event-demo.bad_tool":
			sawRejected = e.Subtype == triggered_tools_runtime.ToolRequestResultRequestBlocked && e.Metadata["result"] == nil
			rejectedToolCallID, _ = e.Metadata["toolCallId"].(string)
		case "event-demo.good_tool":
			sawSucceededWithResult = e.Subtype == triggered_tools_runtime.ToolRequestResultCallSucceeded && e.Metadata["result"] != nil
			succeededToolCallID, _ = e.Metadata["toolCallId"].(string)
		}
	}
	// The alert (published by the call-authorization stand-in) and the
	// tool-request-result event (published by go-proxy's own orchestration
	// engine) for the same rejected call must carry the same toolCallId —
	// proving the ID is threaded through the triggered-tool hop, not
	// re-minted. The two independent calls (bad_tool/good_tool) must get
	// distinct IDs.
	if rejectedToolCallID == "" || rejectedToolCallID != alertToolCallID {
		t.Errorf("expected bad_tool's tool-request-result toolCallId (%q) to match the alert's toolCallId (%q)", rejectedToolCallID, alertToolCallID)
	}
	if succeededToolCallID == "" {
		t.Error("expected good_tool's tool-request-result event to include a non-empty toolCallId")
	}
	if succeededToolCallID == rejectedToolCallID {
		t.Error("expected bad_tool and good_tool calls to get distinct toolCallIds")
	}
	if !sawRejected {
		t.Error("expected a tool-request-result event for bad_tool with subtype=result-blocked and no result")
	}
	if !sawSucceededWithResult {
		t.Error("expected a tool-request-result event for good_tool with subtype=call-succeeded and a result")
	}

	// Prove the aggregator itself (an ordinary, non-triggered tool reachable
	// through the proxy) can see the same events via its own list_events tool.
	listResult, err := callTool(EVENT_TEST_PROXY_URL, mcp.CallToolParams{Name: "event-aggregator.list_events", Arguments: map[string]any{"type": "alert"}})
	if err != nil {
		t.Fatalf("failed to call event-aggregator.list_events: %v", err)
	}
	if !strings.Contains(listResult, "tool-call-blocked") {
		t.Errorf("expected event-aggregator.list_events to show the rejected call, got: %s", listResult)
	}
}

// waitForEventCount polls buffer (up to 3s) until it holds at least n
// events of the given type - publishing is async relative to the tool
// call that triggers it, same rationale as platform_servers_test.go's
// waitForPlatformAuditEntries.
func waitForEventCount(t *testing.T, buffer *testEventBuffer, eventType string, n int) []*events.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := buffer.list(eventType); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %q events, got %d", n, eventType, len(buffer.list(eventType)))
	return nil
}
