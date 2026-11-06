// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// TestElicitationPassesThroughTheGateway checks the protocol 2026-07-28
// multi-round-trip flow end to end: a backend tool asks for user input by
// returning an "input required" result; the gateway passes it to the client
// unchanged; the client answers and calls again; the gateway forwards the
// answers and the request state; the backend completes.
func TestElicitationPassesThroughTheGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	backend := mcp.NewServer(&mcp.Implementation{Name: "elicit-backend", Version: "1.0.0"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "book", Description: "books after the user confirms"}, func(_ context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		if len(req.Params.InputResponses) == 0 {
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{
					Mode:    "form",
					Message: "Book the 10:00 slot?",
					RequestedSchema: map[string]any{
						"type":       "object",
						"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
						"required":   []string{"ok"},
					},
				}},
				RequestState: "slot-10",
			}, nil, nil
		}
		answer, ok := req.Params.InputResponses["confirm"].(*mcp.ElicitResult)
		switch {
		case !ok:
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "answer missing or of the wrong type"}}}, nil, nil
		case req.Params.RequestState != "slot-10":
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "request state not echoed back: " + req.Params.RequestState}}}, nil, nil
		case answer.Action != "accept" || answer.Content["ok"] != true:
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "not booked"}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "booked 10:00"}}}, nil, nil
	})
	// Stateless: a stateful Go SDK server negotiates protocol 2025-11-25 and
	// asks for input with an old-style server-to-client request, which the
	// gateway can't relay. Pass-through needs the backend on 2026-07-28.
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer backendHTTP.Close()

	// The gateway's backend connection is configured as in go-proxy: it
	// passes "input required" results through instead of answering them.
	var sessMu sync.Mutex
	var shared *mcp.ClientSession
	gateway := NewGateway(func(_ context.Context, _ string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		sessMu.Lock()
		defer sessMu.Unlock()
		if shared != nil {
			return shared, nil
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "gw-backend-client", Version: "1.0.0"}, &mcp.ClientOptions{
			MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
		})
		s, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.ServerUrl}, nil)
		if err == nil {
			shared = s
		}
		return s, err
	}, nil, 1, 1, 1, 1)
	defer func() {
		sessMu.Lock()
		defer sessMu.Unlock()
		if shared != nil {
			shared.Close()
		}
	}()
	servers := discover_mcp_servers.ServerMap{"eb": {ServerName: "eb", ServerUrl: backendHTTP.URL}}
	gateway.OnAdded(ctx, servers)
	defer gateway.OnRemoved(servers)

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gateway.Server() }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer gatewayHTTP.Close()

	// The client answers through the SDK's normal elicitation handler; the
	// SDK sends the follow-up call with the answers automatically.
	var asked atomic.Int32
	var question string
	client := mcp.NewClient(&mcp.Implementation{Name: "elicit-test-client", Version: "1.0.0"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			asked.Add(1)
			question = req.Params.Message
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"ok": true}}, nil
		},
	})
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	waitForTool(t, ctx, session, "eb.book")

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "eb.book"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if n := asked.Load(); n != 1 {
		t.Errorf("the user was asked %d times, want 1", n)
	}
	if question != "Book the 10:00 slot?" {
		t.Errorf("question shown to the user = %q, want the backend's", question)
	}
	if res.IsError || len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != "booked 10:00" {
		t.Errorf("final result = %q (isError=%v), want \"booked 10:00\"", res.Content[0].(*mcp.TextContent).Text, res.IsError)
	}
}
