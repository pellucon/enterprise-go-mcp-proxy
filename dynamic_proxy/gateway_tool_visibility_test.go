// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// TestToolVisibilityFilterHidesMarkedTool exercises the injected
// ToolVisibilityFilter mechanism (used in production to hide go-proxy's
// "triggered tools" — see triggered-tools-runtime — without dynamic_proxy
// needing to import that package). A backend server exposes both an
// ordinary tool and one carrying an arbitrary "hide me" marker in its
// _meta; the injected filter hides only the latter.
func TestToolVisibilityFilterHidesMarkedTool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server := mcp.NewServer(&mcp.Implementation{Name: "mixed-server", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "visible_tool",
		Description: "an ordinary, visible tool",
		InputSchema: map[string]any{"type": "object"},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
	})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "hidden_tool",
		Description: "a tool that should never reach the client-facing gateway",
		InputSchema: map[string]any{"type": "object"},
		Meta:        mcp.Meta{"hide-me": true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "should never be called"}}}, nil, nil
	})

	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	defer backendHTTP.Close()

	serverMap := discover_mcp_servers.ServerMap{
		"mixed-server": {ServerName: "mixed-server", ServerUrl: backendHTTP.URL},
	}

	isHidden := func(tool *mcp.Tool) bool {
		v, _ := tool.Meta["hide-me"].(bool)
		return v
	}

	gateway := NewGateway(func(_ context.Context, sessionId string, srv *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client-" + sessionId, Version: "1.0.0"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.ServerUrl}, nil)
	}, isHidden, 1, 1, 1, 1)
	defer gateway.OnRemoved(serverMap)

	monitor := discover_mcp_servers.NewServerMonitor(
		1,
		func() (discover_mcp_servers.ServerMap, error) { return serverMap, nil },
		func(added discover_mcp_servers.ServerMap) { gateway.OnAdded(ctx, added) },
		func(removed discover_mcp_servers.ServerMap) { gateway.OnRemoved(removed) },
	)
	go monitor.Start(ctx)

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return gateway.Server() }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer gatewayHTTP.Close()

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer clientCancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-integration-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(clientCtx, &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("Failed to connect client to dynamic gateway: %v", err)
	}
	defer clientSession.Close()

	var toolsList []*mcp.Tool
	for i := 0; i < 10; i++ {
		result, err := clientSession.ListTools(clientCtx, &mcp.ListToolsParams{})
		if err == nil && len(result.Tools) >= 1 {
			toolsList = result.Tools
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	if len(toolsList) == 0 {
		t.Fatal("Timed out waiting for tools to be registered")
	}

	var sawVisible, sawHidden bool
	for _, tool := range toolsList {
		switch tool.Name {
		case "mixed-server.visible_tool":
			sawVisible = true
		case "mixed-server.hidden_tool":
			sawHidden = true
		}
	}
	if !sawVisible {
		t.Errorf("expected mixed-server.visible_tool to be registered, got tools: %v", toolNames(toolsList))
	}
	if sawHidden {
		t.Errorf("expected mixed-server.hidden_tool to be hidden by the ToolVisibilityFilter, but it was returned: %v", toolNames(toolsList))
	}

	if _, err := clientSession.CallTool(clientCtx, &mcp.CallToolParams{Name: "mixed-server.hidden_tool", Arguments: map[string]any{}}); err == nil {
		t.Error("expected calling the hidden tool by name to fail (it was never registered), but it succeeded")
	}
}
