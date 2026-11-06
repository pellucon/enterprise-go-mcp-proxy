// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	STATEFUL_BACKEND_PORT    = ":8104"
	PROTOCOL_TEST_PROXY_PORT = ":8105"
	PROTOCOL_TEST_PROXY_URL  = "http://localhost:8105/mcp"
)

// startStatefulTestBackend starts server on port with Stateless: false —
// deliberately the opposite of runBackendServer (mcp_server_test.go), which
// every other root-package test backend goes through and which hardcodes
// Stateless: true. This exists specifically to catch a regression of the
// "over-propagated protocol version" bug: without middleware.go's
// detachContextValues, a client negotiating the newest MCP protocol with
// go-proxy's own (stateless) gateway would cause go-proxy to request that
// same version against every backend it forwards calls to — including one
// that, left to negotiate for itself, would settle on an older version —
// and a non-stateless backend rejects the newest version outright (400 Bad
// Request). See CLAUDE.md's "Backend protocol version isolation" section.
func startStatefulTestBackend(t *testing.T, ctx context.Context, cancel context.CancelFunc, port string, server *mcp.Server) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
		srv := &http.Server{Addr: port, Handler: handler}
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			srv.Shutdown(shutdownCtx)
		}()
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("stateful backend server on %s stopped with error: %v", port, err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

// TestProtocolVersionNotPropagatedToStatefulBackend proves go-proxy can
// still successfully forward a call to a backend that isn't configured
// Stateless: true, even when the calling client negotiates the newest MCP
// protocol with go-proxy itself. Before middleware.go's detachContextValues
// fix, the client's negotiated protocol version silently rode along (via
// the shared context.Context) onto go-proxy's own outgoing call to this
// backend, which then rejected it with 400 Bad Request regardless of what
// protocol version the backend would have negotiated for itself.
func TestProtocolVersionNotPropagatedToStatefulBackend(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	backend := mcp.NewServer(&mcp.Implementation{Name: "stateful-backend", Version: "1.0.0"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "ping", Description: "returns pong"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})

	startStatefulTestBackend(t, ctx, cancel, STATEFUL_BACKEND_PORT, backend)

	natsURL := startTestNATSServer(t)

	cfg := fmt.Sprintf(`{
  "discovery": {
    "discoveryMode": "static",
    "listedTools": ["*"],
    "servers": [
      { "serverName": "stateful-backend", "serverUrl": "http://localhost%s/mcp" }
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
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, STATEFUL_BACKEND_PORT, natsURL)

	cfgPath := t.TempDir() + "/protocol-version-test.cfg"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	startTestProxy(t, ctx, cancel, cfgPath, PROTOCOL_TEST_PROXY_PORT)

	session, err := connectAsRole(ctx, PROTOCOL_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer session.Close()

	// The client here negotiates the newest MCP protocol with go-proxy's
	// own (stateless) gateway by default, same as any other test in this
	// suite.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "stateful-backend.ping"})
	if err != nil {
		t.Fatalf("CallTool against a non-stateless backend failed (protocol version may be over-propagated): %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %+v", result.Content)
	}
	textContent, ok := result.Content[0].(*mcp.TextContent)
	if !ok || textContent.Text != "pong" {
		t.Fatalf("unexpected tool result: %+v", result.Content)
	}
}
