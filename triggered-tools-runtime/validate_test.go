// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newValidationTestServer registers a call-authorization-shaped tool
// (matching) and, optionally, a mismatched tool for the caller to exercise.
func newValidationTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "validation-test-server"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "good-call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in CallAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			return nil, AuthorizationOutput{Authorized: true}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "mismatched-tool"},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct {
			Foo string `json:"foo"`
		}) (*mcp.CallToolResult, struct{}, error) {
			return &mcp.CallToolResult{}, struct{}{}, nil
		})

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

func TestEnsureValidatedMatchingSchemaPasses(t *testing.T) {
	httpServer := newValidationTestServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	orch := Orchestration{CallAuthorization: []OrchestrationStep{
		{Server: "auth-server", Tool: "good-call-authorization"},
	}}
	if err := r.ensureValidated(context.Background(), "orch", orch); err != nil {
		t.Fatalf("expected validation to pass for matching schema, got: %v", err)
	}
}

func TestEnsureValidatedMismatchedSchemaFails(t *testing.T) {
	httpServer := newValidationTestServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	orch := Orchestration{CallAuthorization: []OrchestrationStep{
		{Server: "auth-server", Tool: "mismatched-tool"},
	}}
	if err := r.ensureValidated(context.Background(), "orch", orch); err == nil {
		t.Fatal("expected validation to fail for a mismatched schema")
	}
}

func TestEnsureValidatedMissingToolFails(t *testing.T) {
	httpServer := newValidationTestServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	orch := Orchestration{CallAuthorization: []OrchestrationStep{
		{Server: "auth-server", Tool: "does-not-exist"},
	}}
	if err := r.ensureValidated(context.Background(), "orch", orch); err == nil {
		t.Fatal("expected validation to fail for a tool that doesn't exist on the server")
	}
}

func TestEnsureValidatedCachesResult(t *testing.T) {
	httpServer := newValidationTestServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	orch := Orchestration{CallAuthorization: []OrchestrationStep{
		{Server: "auth-server", Tool: "good-call-authorization"},
	}}
	if err := r.ensureValidated(context.Background(), "orch", orch); err != nil {
		t.Fatalf("unexpected error on first validation: %v", err)
	}

	// Evict the session so a second, uncached validation attempt would fail
	// to even dial — proving the second ensureValidated call below is
	// served from cache rather than re-validating over the network.
	r.OnRemoved(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	if err := r.ensureValidated(context.Background(), "orch", orch); err != nil {
		t.Fatalf("expected cached validation result to be reused without error, got: %v", err)
	}
}

func TestUpdateInvalidatesValidationCache(t *testing.T) {
	httpServer := newValidationTestServer(t)
	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	orch := Orchestration{CallAuthorization: []OrchestrationStep{
		{Server: "auth-server", Tool: "good-call-authorization"},
	}}
	if err := r.ensureValidated(context.Background(), "orch", orch); err != nil {
		t.Fatalf("unexpected error on first validation: %v", err)
	}

	r.Update(&OrchestrationConfig{})

	r.OnRemoved(discover_mcp_servers.ServerMap{
		"auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "auth-server", ServerUrl: httpServer.URL},
	})

	if err := r.ensureValidated(context.Background(), "orch", orch); err == nil {
		t.Fatal("expected Update to invalidate the cache, forcing re-validation (which should now fail since the server was evicted)")
	}
}

func guardOnce(ctx context.Context, r *Runtime) error {
	_, err := r.Guard(ctx, "orch", CallerIdentity{User: "alice", Roles: []string{"admin"}}, "test-tool-call-id",
		&mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"},
		func(context.Context) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
		})
	return err
}

// TestValidationNotSpoiledByCancelledCaller is the regression test for a
// caller cancelling the very first guarded call: that used to cache the
// cancellation as the orchestration's validation result, refusing every
// later call until the next config update or restart.
func TestValidationNotSpoiledByCancelledCaller(t *testing.T) {
	r, _, orch := newTestGuardRuntime(t, true)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := guardOnce(cancelled, r); err == nil {
		t.Fatal("a cancelled call succeeded")
	}
	if err := guardOnce(context.Background(), r); err != nil {
		t.Fatalf("the next call failed: %v", err)
	}
}

// TestValidationRetriesUnreachableServer checks that a step server that
// can't be reached fails that call closed but isn't cached: once the
// server is there, the next call is checked again and succeeds.
func TestValidationRetriesUnreachableServer(t *testing.T) {
	authServer := newAuthorizationServer(t, true)
	r := NewRuntime(nil)
	orch := Orchestration{CallAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}}}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	if err := guardOnce(context.Background(), r); err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("Guard with the step server missing = %v; want a fail-closed error", err)
	}

	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: authServer.URL},
	})
	if err := guardOnce(context.Background(), r); err != nil {
		t.Fatalf("Guard after the step server appeared: %v", err)
	}
}

// TestValidationCachesSchemaProblem checks that a definitive problem - the
// step's tool doesn't exist - is cached.
func TestValidationCachesSchemaProblem(t *testing.T) {
	r, _, _ := newTestGuardRuntime(t, true)
	orch := Orchestration{CallAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "no-such-tool"}}}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	if err := guardOnce(context.Background(), r); err == nil || !strings.Contains(err.Error(), "tool not found") {
		t.Fatalf("Guard = %v; want a tool-not-found error", err)
	}
	r.validationMu.Lock()
	cached, ok := r.validated["orch"]
	r.validationMu.Unlock()
	if !ok || cached == nil {
		t.Errorf("the tool-not-found result wasn't cached (ok=%v, err=%v)", ok, cached)
	}
}
