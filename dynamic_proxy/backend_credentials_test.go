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
	"github.com/pellucon/enterprise-go-mcp-proxy/backendauth"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// TestFetchUsesBackendCredentials runs a real MCP backend that rejects any
// request without the right API key, and checks that the gateway's tool
// fetch succeeds only when the server is configured with a credentials
// secret holding that key.
func TestFetchUsesBackendCredentials(t *testing.T) {
	backend := mcp.NewServer(&mcp.Implementation{Name: "keyed-backend", Version: "1.0.0"}, nil)
	mcp.AddTool(backend, newTestTool("secret_tool"), noopToolHandler)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k-123" {
			http.Error(w, "missing or wrong API key", http.StatusUnauthorized)
			return
		}
		mcpHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	old := backendauth.Default
	backendauth.Default = backendauth.NewStore(func(_ context.Context, name string) (*backendauth.Secret, error) {
		return &backendauth.Secret{
			Data:        map[string][]byte{"header": []byte("X-API-Key"), "value": []byte("k-123")},
			Annotations: map[string]string{backendauth.AllowedHostsAnnotation: "127.0.0.1"},
		}, nil
	}, time.Minute)
	defer func() { backendauth.Default = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := fetchTools(ctx, &discover_mcp_servers.ServerAttributes{ServerName: "keyed", ServerUrl: srv.URL}); err == nil {
		t.Error("fetch without a credentials secret succeeded; the backend should have refused it")
	}

	tools, err := fetchTools(ctx, &discover_mcp_servers.ServerAttributes{ServerName: "keyed", ServerUrl: srv.URL, CredentialsSecret: "keyed-api-key"})
	if err != nil {
		t.Fatalf("fetch with a credentials secret: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "secret_tool" {
		t.Errorf("tools = %v, want [secret_tool]", tools)
	}
}
