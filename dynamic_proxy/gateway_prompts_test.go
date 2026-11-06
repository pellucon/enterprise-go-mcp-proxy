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

// addTestPrompts registers the given prompt names on server, so tests don't
// depend on any externally-started process.
func addTestPrompts(server *mcp.Server, promptNames ...string) {
	for _, name := range promptNames {
		server.AddPrompt(&mcp.Prompt{
			Name:        name,
			Description: "test prompt " + name,
		}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{
				Messages: []*mcp.PromptMessage{
					{Role: "user", Content: &mcp.TextContent{Text: "ok"}},
				},
			}, nil
		})
	}
}

func TestDynamicProxyPromptsAddition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend1 := newTestBackendServer("mcp1")
	addTestPrompts(backend1, "greet", "farewell")
	backend2 := newTestBackendServer("mcp2")
	addTestPrompts(backend2, "summarize")

	backendHTTP1 := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend1
	}, nil))
	defer backendHTTP1.Close()

	backendHTTP2 := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend2
	}, nil))
	defer backendHTTP2.Close()

	serverMap := discover_mcp_servers.ServerMap{
		"mcp1": {ServerName: "mcp1", ServerUrl: backendHTTP1.URL},
		"mcp2": {ServerName: "mcp2", ServerUrl: backendHTTP2.URL},
	}

	gateway := NewGateway(func(_ context.Context, sessionId string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{
			Name:    "test-client-" + sessionId,
			Version: "1.0.0",
		}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: server.ServerUrl,
		}, nil)
	}, nil, 1, 1, 1, 1)
	defer gateway.OnRemoved(serverMap)

	monitor := discover_mcp_servers.NewServerMonitor(
		1,
		func() (discover_mcp_servers.ServerMap, error) {
			return serverMap, nil
		},
		func(added discover_mcp_servers.ServerMap) {
			gateway.OnAdded(ctx, added)
		},
		func(removed discover_mcp_servers.ServerMap) {
			gateway.OnRemoved(removed)
		},
	)
	go monitor.Start(ctx)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return gateway.Server()
	}, &mcp.StreamableHTTPOptions{Stateless: true})

	gatewayHTTP := httptest.NewServer(mcpHandler)
	defer gatewayHTTP.Close()

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer clientCancel()

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "test-integration-client",
		Version: "1.0.0",
	}, nil)

	clientSession, err := client.Connect(clientCtx, &mcp.StreamableClientTransport{
		Endpoint: gatewayHTTP.URL,
	}, nil)
	if err != nil {
		t.Fatalf("Failed to connect client to dynamic gateway: %v", err)
	}
	defer clientSession.Close()

	wantPrompts := map[string]bool{
		"mcp1.greet":     false,
		"mcp1.farewell":  false,
		"mcp2.summarize": false,
	}

	var promptsList []*mcp.Prompt
	for i := 0; i < 10; i++ {
		result, err := clientSession.ListPrompts(clientCtx, &mcp.ListPromptsParams{})
		if err == nil && len(result.Prompts) >= len(wantPrompts) {
			promptsList = result.Prompts
			break
		}
		t.Logf("Attempt %d: Waiting for prompts to be registered...", i+1)
		time.Sleep(300 * time.Millisecond)
	}

	if len(promptsList) == 0 {
		t.Fatalf("Test failed: No prompts were registered in the dynamic gateway.")
	}

	for _, prompt := range promptsList {
		if _, ok := wantPrompts[prompt.Name]; ok {
			wantPrompts[prompt.Name] = true
		}
	}
	for name, found := range wantPrompts {
		if !found {
			t.Errorf("expected prompt %q to be registered in the gateway, got: %v", name, promptNames(promptsList))
		}
	}
}

// waitForGatewayPrompts polls the gateway client until its prompt list
// exactly matches `want`, or fails the test after a timeout.
func waitForGatewayPrompts(t *testing.T, ctx context.Context, clientSession *mcp.ClientSession, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		result, err := clientSession.ListPrompts(ctx, &mcp.ListPromptsParams{})
		if err == nil {
			got := promptNames(result.Prompts)
			if sameElements(got, want) {
				return
			}
			t.Logf("gateway prompts: %v (want %v)", got, want)
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for gateway prompts to match %v", want)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestDynamicProxyPromptsLiveUpdate verifies that the gateway keeps its
// registered prompts in sync with a backend server's prompt list *after*
// the initial discovery, mirroring TestDynamicProxyToolsLiveUpdate.
func TestDynamicProxyPromptsLiveUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := newTestBackendServer("mcp1")
	addTestPrompts(backend, "greet")
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend
	}, nil))
	defer backendHTTP.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "mcp1", ServerUrl: backendHTTP.URL}
	serverMap := discover_mcp_servers.ServerMap{"mcp1": attrs}

	gateway := NewGateway(func(_ context.Context, sessionId string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{Name: "test-client-" + sessionId, Version: "1.0.0"}, nil)
		return client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.ServerUrl}, nil)
	}, nil, 1, 1, 1, 1)
	defer gateway.OnRemoved(serverMap)

	gateway.OnAdded(ctx, serverMap)

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return gateway.Server()
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer gatewayHTTP.Close()

	clientCtx, clientCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer clientCancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-integration-client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(clientCtx, &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("Failed to connect client to dynamic gateway: %v", err)
	}
	defer clientSession.Close()

	// 1. Initial discovery registered the one prompt the backend started with.
	waitForGatewayPrompts(t, clientCtx, clientSession, []string{"mcp1.greet"})

	// 2. Adding a prompt directly to the backend (no re-discovery call) must
	// propagate through the poller's next tick.
	addTestPrompts(backend, "farewell")
	waitForGatewayPrompts(t, clientCtx, clientSession, []string{"mcp1.greet", "mcp1.farewell"})

	// 3. Removing a prompt on the backend must also propagate.
	backend.RemovePrompts("greet")
	waitForGatewayPrompts(t, clientCtx, clientSession, []string{"mcp1.farewell"})

	// 4. Removing the server must close the discovery session and clear its
	// prompts from the gateway.
	gateway.OnRemoved(serverMap)
	waitForGatewayPrompts(t, clientCtx, clientSession, nil)
}
