// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestToolArgumentsForwardedVerbatim checks that a tool call's arguments
// reach the backend exactly as the client sent them. Decoding them into
// map[string]any rounds numbers to float64, so a backend could receive a
// value (0 for 1e-400) other than the one a guardrail constraint checked.
func TestToolArgumentsForwardedVerbatim(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	backend := mcp.NewServer(&mcp.Implementation{Name: "echo-backend", Version: "1.0.0"}, nil)
	backend.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
	})
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer backendHTTP.Close()

	gateway := NewGateway(func(_ context.Context, _ string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		return mcp.NewClient(&mcp.Implementation{Name: "gw-backend-client", Version: "1.0.0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.ServerUrl}, nil)
	}, nil, 1, 1, 1, 1)
	servers := discover_mcp_servers.ServerMap{"eb": {ServerName: "eb", ServerUrl: backendHTTP.URL}}
	gateway.OnAdded(ctx, servers)
	defer gateway.OnRemoved(servers)

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gateway.Server() }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer gatewayHTTP.Close()

	session, err := mcp.NewClient(&mcp.Implementation{Name: "args-test-client", Version: "1.0.0"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()
	waitForTool(t, ctx, session, "eb.echo")

	const sent = `{"id":9007199254740993,"tiny":1e-400,"price":99.99999999999999999}`
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "eb.echo", Arguments: json.RawMessage(sent)})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if got := res.Content[0].(*mcp.TextContent).Text; got != sent {
		t.Errorf("the backend received %s; want exactly %s", got, sent)
	}
}
