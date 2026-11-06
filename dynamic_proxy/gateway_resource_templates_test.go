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

// addTestResourceTemplates registers the given URI templates on server, so
// tests don't depend on any externally-started process. The handler echoes
// the concrete requested URI back, so a test can prove which URI a template
// match actually resolved to.
func addTestResourceTemplates(server *mcp.Server, uriTemplates ...string) {
	for _, uriTemplate := range uriTemplates {
		server.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: uriTemplate,
			Name:        uriTemplate,
			Description: "test resource template " + uriTemplate,
		}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{
				Contents: []*mcp.ResourceContents{
					{URI: req.Params.URI, Text: "contents of " + req.Params.URI},
				},
			}, nil
		})
	}
}

func TestDynamicProxyResourceTemplatesAddition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend1 := newTestBackendServer("mcp1")
	addTestResourceTemplates(backend1, "file:///{path}", "git:///{ref}")
	backend2 := newTestBackendServer("mcp2")
	addTestResourceTemplates(backend2, "issue:///{id}")

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

	wantTemplates := map[string]bool{
		"mcp1:file:///{path}": false,
		"mcp1:git:///{ref}":   false,
		"mcp2:issue:///{id}":  false,
	}

	var templatesList []*mcp.ResourceTemplate
	for i := 0; i < 10; i++ {
		result, err := clientSession.ListResourceTemplates(clientCtx, &mcp.ListResourceTemplatesParams{})
		if err == nil && len(result.ResourceTemplates) >= len(wantTemplates) {
			templatesList = result.ResourceTemplates
			break
		}
		t.Logf("Attempt %d: Waiting for resource templates to be registered...", i+1)
		time.Sleep(300 * time.Millisecond)
	}

	if len(templatesList) == 0 {
		t.Fatalf("Test failed: No resource templates were registered in the dynamic gateway.")
	}

	for _, tmpl := range templatesList {
		if _, ok := wantTemplates[tmpl.URITemplate]; ok {
			wantTemplates[tmpl.URITemplate] = true
		}
	}
	for uriTemplate, found := range wantTemplates {
		if !found {
			t.Errorf("expected resource template %q to be registered in the gateway, got: %v", uriTemplate, resourceTemplateURIs(templatesList))
		}
	}
}

// waitForGatewayResourceTemplates polls the gateway client until its
// resource template list exactly matches `want`, or fails the test after a
// timeout.
func waitForGatewayResourceTemplates(t *testing.T, ctx context.Context, clientSession *mcp.ClientSession, want []string) {
	t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		result, err := clientSession.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
		if err == nil {
			got := resourceTemplateURIs(result.ResourceTemplates)
			if sameElements(got, want) {
				return
			}
			t.Logf("gateway resource templates: %v (want %v)", got, want)
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for gateway resource templates to match %v", want)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// TestDynamicProxyResourceTemplatesLiveUpdate verifies that the gateway
// keeps its registered resource templates in sync with a backend server's
// template list *after* the initial discovery, mirroring
// TestDynamicProxyResourcesLiveUpdate.
func TestDynamicProxyResourceTemplatesLiveUpdate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := newTestBackendServer("mcp1")
	addTestResourceTemplates(backend, "file:///{path}")
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

	// 1. Initial discovery registered the one template the backend started with.
	waitForGatewayResourceTemplates(t, clientCtx, clientSession, []string{"mcp1:file:///{path}"})

	// 2. Adding a template directly to the backend (no re-discovery call) must
	// propagate through the poller's next tick.
	addTestResourceTemplates(backend, "git:///{ref}")
	waitForGatewayResourceTemplates(t, clientCtx, clientSession, []string{"mcp1:file:///{path}", "mcp1:git:///{ref}"})

	// 3. Removing a template on the backend must also propagate.
	backend.RemoveResourceTemplates("file:///{path}")
	waitForGatewayResourceTemplates(t, clientCtx, clientSession, []string{"mcp1:git:///{ref}"})

	// 4. Removing the server must close the discovery session and clear its
	// templates from the gateway.
	gateway.OnRemoved(serverMap)
	waitForGatewayResourceTemplates(t, clientCtx, clientSession, nil)
}

// TestDynamicProxyResourceTemplateReadRoutesToBackend proves the read-side
// dispatch nuance: a concrete URI that was never registered as an exact
// Resource, but matches a registered ResourceTemplate's pattern, must still
// route through to the correct backend when the client calls
// ReadResource — because the SDK's own server dispatches an unmatched exact
// URI to whichever registered template pattern matches it (see
// registerResourceTemplate's comment in gateway.go).
func TestDynamicProxyResourceTemplateReadRoutesToBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := newTestBackendServer("mcp1")
	addTestResourceTemplates(backend, "file:///{path}")
	// Unlike the Addition/LiveUpdate tests above (which only ever poll
	// ListResourceTemplates), this test performs a real, successful
	// ReadResource round-trip through to this backend — which requires
	// Stateless: true to match every other backend server in this repo
	// (production code and mcp_server_test.go's shared test helper): the
	// newest MCP protocol version is only served over StreamableHTTP when
	// Stateless is set, otherwise the backend rejects it with 400 Bad
	// Request.
	backendHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer backendHTTP.Close()

	attrs := &discover_mcp_servers.ServerAttributes{ServerName: "mcp1", ServerUrl: backendHTTP.URL}
	serverMap := discover_mcp_servers.ServerMap{"mcp1": attrs}

	// Unlike the Addition/LiveUpdate tests above (which only ever call
	// ListResourceTemplates and so never actually invoke getSession), this
	// test performs a real ReadResource call, which does invoke it. Cache and
	// explicitly close a single backend session here (mirroring how
	// production's sessionMap caches/closes sessions) rather than opening a
	// fresh, never-closed connection per call — an uncached retriever would
	// leak a connection that keeps backendHTTP.Close() blocked forever below.
	backendClient := mcp.NewClient(&mcp.Implementation{Name: "test-backend-client", Version: "1.0.0"}, nil)
	backendSession, err := backendClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: backendHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("Failed to connect to backend: %v", err)
	}
	defer backendSession.Close()

	gateway := NewGateway(func(_ context.Context, sessionId string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		return backendSession, nil
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

	waitForGatewayResourceTemplates(t, clientCtx, clientSession, []string{"mcp1:file:///{path}"})

	// "mcp1:file:///notes.txt" was never registered as an exact resource —
	// it should only be reachable via the template match.
	result, err := clientSession.ReadResource(clientCtx, &mcp.ReadResourceParams{URI: "mcp1:file:///notes.txt"})
	if err != nil {
		t.Fatalf("Failed to read resource via template match: %v", err)
	}
	if len(result.Contents) != 1 || result.Contents[0].Text != "contents of file:///notes.txt" {
		t.Fatalf("unexpected contents from template-matched read: %+v", result.Contents)
	}
}
