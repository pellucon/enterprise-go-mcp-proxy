// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func newFakePlatformServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-platform-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "some-tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestMatchOrchestration(t *testing.T) {
	cfg := &OrchestrationConfig{
		Orchestrations: map[string]Orchestration{
			"standard-guardrails": {},
		},
		ToolMappings: []ToolMapping{
			{Pattern: "orchestration-demo.*", Orchestration: "standard-guardrails"},
		},
	}
	r := NewRuntime(cfg)

	name, _, ok := r.MatchOrchestration("orchestration-demo.say_hello")
	if !ok || name != "standard-guardrails" {
		t.Errorf("expected match on standard-guardrails, got name=%q ok=%v", name, ok)
	}

	if _, _, ok := r.MatchOrchestration("customer-service.get_customer_name"); ok {
		t.Error("expected no match for an unmapped tool name")
	}
}

func TestOnAddedRefusesReservedServerName(t *testing.T) {
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"_admin": &discover_mcp_servers.ServerAttributes{ServerName: "_admin", ServerUrl: "http://example.invalid"},
	})
	if _, err := r.session(context.Background(), "_admin"); err == nil {
		t.Error("expected session() to fail for a reserved server name that OnAdded should have refused")
	}
}

func TestSessionLazyDialAndCache(t *testing.T) {
	httpServer := newFakePlatformServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"fake-server": &discover_mcp_servers.ServerAttributes{ServerName: "fake-server", ServerUrl: httpServer.URL},
	})

	sess1, err := r.session(context.Background(), "fake-server")
	if err != nil {
		t.Fatalf("session() returned error: %v", err)
	}
	sess2, err := r.session(context.Background(), "fake-server")
	if err != nil {
		t.Fatalf("session() (second call) returned error: %v", err)
	}
	if sess1 != sess2 {
		t.Error("expected session() to return the same cached *mcp.ClientSession on a second call")
	}
}

func TestSessionUnknownServer(t *testing.T) {
	r := NewRuntime(nil)
	if _, err := r.session(context.Background(), "never-discovered"); err == nil {
		t.Error("expected session() to fail for a server that was never OnAdded")
	}
}

// TestSessionAttachesRelayHeadersFromContext proves platformHTTPClientFor
// gives a triggered-tool invocation the same relay capability an ordinary
// backend call already has: the relay headers the calling ctx's minter
// (relayheaders.WithMinter - which Guard's own ctx already carries by the
// time it reaches session/SessionFor, since createMiddleware puts it there
// upstream of the dispatch to Guard) produces for this server must arrive
// on the wire as real X-Go-Proxy-* headers, so a discovered triggered tool
// can read them via relayheaders.Extract on its own incoming request
// exactly like an ordinary backend tool does. The minter must be asked for
// this server by name, since tokens are minted per server.
func TestSessionAttachesRelayHeadersFromContext(t *testing.T) {
	var gotHeader http.Header
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-platform-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "some-tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, struct{}, error) {
			gotHeader = req.GetExtra().Header
			return &mcp.CallToolResult{}, struct{}{}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"fake-server": &discover_mcp_servers.ServerAttributes{ServerName: "fake-server", ServerUrl: httpServer.URL},
	})

	var mintedFor []string
	ctx := relayheaders.WithMinter(context.Background(), func(server string) (relayheaders.Set, error) {
		mintedFor = append(mintedFor, server)
		return relayheaders.Set{
			Assertion:   "Bearer test-assertion-token",
			RelayToken:  "Bearer test-relay-token",
			CallbackURL: "http://localhost:9999/mcp",
			ToolCallID:  "test-tool-call-id",
		}, nil
	})

	sess, err := r.session(ctx, "fake-server")
	if err != nil {
		t.Fatalf("session() returned error: %v", err)
	}
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "some-tool"}); err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}

	for _, server := range mintedFor {
		if server != "fake-server" {
			t.Errorf("headers were minted for server %q, want %q", server, "fake-server")
		}
	}
	incoming := relayheaders.Extract(gotHeader)
	if incoming.RelayToken != "Bearer test-relay-token" {
		t.Errorf("expected relayed relay token %q, got %q", "Bearer test-relay-token", incoming.RelayToken)
	}
	if incoming.Assertion != "Bearer test-assertion-token" {
		t.Errorf("expected relayed assertion %q, got %q", "Bearer test-assertion-token", incoming.Assertion)
	}
	if incoming.CallbackURL != "http://localhost:9999/mcp" {
		t.Errorf("expected relayed callback URL %q, got %q", "http://localhost:9999/mcp", incoming.CallbackURL)
	}
	if incoming.ToolCallID != "test-tool-call-id" {
		t.Errorf("expected relayed tool call ID %q, got %q", "test-tool-call-id", incoming.ToolCallID)
	}
}

func TestOnRemovedEvictsSession(t *testing.T) {
	httpServer := newFakePlatformServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"fake-server": &discover_mcp_servers.ServerAttributes{ServerName: "fake-server", ServerUrl: httpServer.URL},
	})
	if _, err := r.session(context.Background(), "fake-server"); err != nil {
		t.Fatalf("session() returned error: %v", err)
	}

	r.OnRemoved(discover_mcp_servers.ServerMap{
		"fake-server": &discover_mcp_servers.ServerAttributes{ServerName: "fake-server", ServerUrl: httpServer.URL},
	})

	if _, err := r.session(context.Background(), "fake-server"); err == nil {
		t.Error("expected session() to fail after OnRemoved evicted the server")
	}
}

func TestUpdateClearsValidationCache(t *testing.T) {
	r := NewRuntime(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": {}}})
	r.validationMu.Lock()
	r.validated["orch"] = nil
	r.validationMu.Unlock()

	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": {}}})

	r.validationMu.Lock()
	_, stillCached := r.validated["orch"]
	r.validationMu.Unlock()
	if stillCached {
		t.Error("expected Update to clear the validation cache")
	}
}

func TestSnapshotReturnsCurrentConfig(t *testing.T) {
	cfg := &OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": {}}}
	r := NewRuntime(cfg)
	if r.Snapshot() != cfg {
		t.Error("expected Snapshot to return the initial config pointer")
	}
	newCfg := &OrchestrationConfig{Orchestrations: map[string]Orchestration{"other": {}}}
	r.Update(newCfg)
	if r.Snapshot() != newCfg {
		t.Error("expected Snapshot to return the updated config pointer after Update")
	}
}

// swappableHandler serves whatever handler was set last, standing in for a
// platform server that restarts at the same address.
type swappableHandler struct{ h atomic.Value }

func (s *swappableHandler) set(h http.Handler) { s.h.Store(&h) }
func (s *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load().(*http.Handler)).ServeHTTP(w, r)
}

func statefulPingHandler() http.Handler {
	server := mcp.NewServer(&mcp.Implementation{Name: "platform"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ping"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, struct{}, error) {
		return &mcp.CallToolResult{}, struct{}{}, nil
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
}

// TestSessionDroppedWhenConnectionEnds checks that a cached platform-server
// session is dropped once the SDK ends its connection for good - here a
// stateful server that restarted and no longer knows the session - so the
// next call reconnects instead of failing until rediscovery.
func TestSessionDroppedWhenConnectionEnds(t *testing.T) {
	backend := &swappableHandler{}
	backend.set(statefulPingHandler())
	srv := httptest.NewServer(backend)
	defer srv.Close()

	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{"platform": {ServerName: "platform", ServerUrl: srv.URL}})
	defer r.OnRemoved(discover_mcp_servers.ServerMap{"platform": {ServerName: "platform"}})
	ctx := context.Background()

	first, err := r.session(ctx, "platform")
	if err != nil {
		t.Fatal(err)
	}
	backend.set(statefulPingHandler()) // restarted
	if _, err := first.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err == nil {
		t.Fatal("a call on the old session succeeded after the restart")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		cached, _ := r.sessions.Cached("", "platform")
		if cached != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the dead session was never dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second, err := r.session(ctx, "platform")
	if err != nil {
		t.Fatalf("reconnecting: %v", err)
	}
	if _, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err != nil {
		t.Errorf("call after reconnecting: %v", err)
	}
}
