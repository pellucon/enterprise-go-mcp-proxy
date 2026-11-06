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

// addTestResources registers the given resource URIs on server, so tests
// don't depend on any externally-started process.
func addTestResources(server *mcp.Server, uris ...string) {
	for _, uri := range uris {
		server.AddResource(&mcp.Resource{
			URI:         uri,
			Name:        uri,
			Description: "test resource " + uri,
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{URI: req.Params.URI, Text: "ok"},
				},
			}, nil
		})
	}
}

func TestDynamicProxyResourcesAddition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend1 := newTestBackendServer("mcp1")
	addTestResources(backend1, "file:///greeting", "file:///farewell")
	backend2 := newTestBackendServer("mcp2")
	addTestResources(backend2, "file:///summary")

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

	wantResources := map[string]bool{
		"mcp1:file:///greeting": false,
		"mcp1:file:///farewell": false,
		"mcp2:file:///summary":  false,
	}

	var resourcesList []*mcp.Resource
	for i := 0; i < 10; i++ {
		result, err := clientSession.ListResources(clientCtx, &mcp.ListResourcesParams{})
		if err == nil && len(result.Resources) >= len(wantResources) {
			resourcesList = result.Resources
			break
		}
		t.Logf("Attempt %d: Waiting for resources to be registered...", i+1)
		time.Sleep(300 * time.Millisecond)
	}

	if len(resourcesList) == 0 {
		t.Fatalf("Test failed: No resources were registered in the dynamic gateway.")
	}

	for _, resource := range resourcesList {
		if _, ok := wantResources[resource.URI]; ok {
			wantResources[resource.URI] = true
		}
	}
	for uri, found := range wantResources {
		if !found {
			t.Errorf("expected resource %q to be registered in the gateway, got: %v", uri, resourceURIs(resourcesList))
		}
	}
}

// waitForGatewayResources polls the gateway client until its resource list
// exactly matches `want`, or fails the test after a timeout.
func waitForGatewayResources(t *testing.T, ctx context.Context, clientSession *mcp.ClientSession, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		result, err := clientSession.ListResources(ctx, &mcp.ListResourcesParams{})
		if err == nil {
			got := resourceURIs(result.Resources)
			if sameElements(got, want) {
				return
			}
			t.Logf("gateway resources: %v (want %v)", got, want)
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for gateway resources to match %v", want)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestDynamicProxyResourcesLiveUpdate verifies that the gateway keeps its
// registered resources in sync with a backend server's resource list *after*
// the initial discovery, mirroring TestDynamicProxyPromptsLiveUpdate.
func TestDynamicProxyResourcesLiveUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := newTestBackendServer("mcp1")
	addTestResources(backend, "file:///greeting")
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

	// 1. Initial discovery registered the one resource the backend started with.
	waitForGatewayResources(t, clientCtx, clientSession, []string{"mcp1:file:///greeting"})

	// 2. Adding a resource directly to the backend (no re-discovery call) must
	// propagate through the poller's next tick.
	addTestResources(backend, "file:///farewell")
	waitForGatewayResources(t, clientCtx, clientSession, []string{"mcp1:file:///greeting", "mcp1:file:///farewell"})

	// 3. Removing a resource on the backend must also propagate.
	backend.RemoveResources("file:///greeting")
	waitForGatewayResources(t, clientCtx, clientSession, []string{"mcp1:file:///farewell"})

	// 4. Removing the server must close the discovery session and clear its
	// resources from the gateway.
	gateway.OnRemoved(serverMap)
	waitForGatewayResources(t, clientCtx, clientSession, nil)
}
