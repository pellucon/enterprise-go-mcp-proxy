// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newAuthorizationServer starts an in-process platform server whose
// call-authorization/result-authorization tools always return authorized.
func newAuthorizationServer(t *testing.T, authorized bool) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-authorization-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in CallAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			return nil, AuthorizationOutput{Authorized: authorized}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "result-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in ResultAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			return nil, AuthorizationOutput{Authorized: authorized}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

// eventRecorder captures every ToolRequestResultEvent published through a
// Runtime's EventPublisher seam, for the test to poll (publishing is
// fire-and-forget/async, same as the old tool-call-audit invocation was).
type eventRecorder struct {
	mu      sync.Mutex
	entries []ToolRequestResultEvent
}

func (r *eventRecorder) record(ctx context.Context, e ToolRequestResultEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *eventRecorder) snapshot() []ToolRequestResultEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolRequestResultEvent, len(r.entries))
	copy(out, r.entries)
	return out
}

// requestEventRecorder mirrors eventRecorder for the sibling "tool-request"
// event (ToolRequestEvent), captured via RequestEventPublisher.
type requestEventRecorder struct {
	mu      sync.Mutex
	entries []ToolRequestEvent
}

func (r *requestEventRecorder) record(ctx context.Context, e ToolRequestEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries = append(r.entries, e)
	return nil
}

func (r *requestEventRecorder) snapshot() []ToolRequestEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ToolRequestEvent, len(r.entries))
	copy(out, r.entries)
	return out
}

func waitForRequestEvents(t *testing.T, rec *requestEventRecorder, n int) []ToolRequestEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entries := rec.snapshot(); len(entries) >= n {
			return entries
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d published request events, got %d", n, len(rec.snapshot()))
	return nil
}

func waitForEvents(t *testing.T, rec *eventRecorder, n int) []ToolRequestResultEvent {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if entries := rec.snapshot(); len(entries) >= n {
			return entries
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d published events, got %d", n, len(rec.snapshot()))
	return nil
}

func newTestGuardRuntime(t *testing.T, authorized bool) (*Runtime, *eventRecorder, Orchestration) {
	t.Helper()
	authServer := newAuthorizationServer(t, authorized)
	rec := &eventRecorder{}

	r := NewRuntime(nil)
	r.SetEventPublisher(rec.record)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: authServer.URL},
	})

	orch := Orchestration{
		CallAuthorization:   []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}},
		ResultAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "result-authorization"}},
		ToolCallAudit:       []string{ToolCallAuditSentinel},
	}
	return r, rec, orch
}

func TestGuardFullSuccess(t *testing.T) {
	r, rec, orch := newTestGuardRuntime(t, true)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
	}

	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	result, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "alice", Roles: []string{"admin"}}, "test-tool-call-id", reqParams, doCall)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if !called {
		t.Error("expected doCall to be invoked")
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected a non-empty result")
	}

	entries := waitForEvents(t, rec, 1)
	if entries[0].Subtype != ToolRequestResultCallSucceeded {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestResultCallSucceeded, entries[0])
	}
	if entries[0].Reason != "" {
		t.Errorf("expected empty Reason on success, got %+v", entries[0])
	}
	if entries[0].CallResult == nil {
		t.Error("expected event to include a non-nil CallResult")
	}
	if entries[0].Tool != "orchestration-demo.say_hello" {
		t.Errorf("expected Tool=%q, got %+v", "orchestration-demo.say_hello", entries[0])
	}
	if entries[0].Caller != "alice" || len(entries[0].Roles) != 1 || entries[0].Roles[0] != "admin" {
		t.Errorf("expected caller=alice roles=[admin], got %+v", entries[0])
	}
}

func TestGuardCallAuthorizationReject(t *testing.T) {
	r, rec, orch := newTestGuardRuntime(t, false)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}

	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.bad_tool"}
	_, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "bob", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall)
	if err == nil {
		t.Fatal("expected an error from call-authorization rejection")
	}
	if called {
		t.Error("expected doCall to NOT be invoked when call-authorization rejects")
	}

	entries := waitForEvents(t, rec, 1)
	if entries[0].Subtype != ToolRequestResultRequestBlocked {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestResultRequestBlocked, entries[0])
	}
	if entries[0].Reason == "" {
		t.Error("expected a non-empty Reason on rejection")
	}
	if entries[0].CallResult != nil {
		t.Errorf("expected event to have a nil CallResult (no backend call was made), got %v", entries[0].CallResult)
	}
}

func TestGuardResultAuthorizationReject(t *testing.T) {
	// call-authorization passes (separate server always-authorized), but
	// result-authorization is wired to a server that always rejects.
	callAuthServer := newAuthorizationServer(t, true)
	resultAuthServer := newAuthorizationServer(t, false)
	rec := &eventRecorder{}

	r := NewRuntime(nil)
	r.SetEventPublisher(rec.record)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"call-auth-server":   &discover_mcp_servers.ServerAttributes{ServerName: "call-auth-server", ServerUrl: callAuthServer.URL},
		"result-auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "result-auth-server", ServerUrl: resultAuthServer.URL},
	})
	orch := Orchestration{
		CallAuthorization:   []OrchestrationStep{{Server: "call-auth-server", Tool: "call-authorization"}},
		ResultAuthorization: []OrchestrationStep{{Server: "result-auth-server", Tool: "result-authorization"}},
		ToolCallAudit:       []string{ToolCallAuditSentinel},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "this result contains bad content"}}}, nil
	}

	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.return_bad_result"}
	_, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "carol", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall)
	if err == nil {
		t.Fatal("expected an error from result-authorization rejection")
	}
	if !called {
		t.Error("expected doCall to have been invoked (the real backend call happens before result-authorization runs)")
	}

	entries := waitForEvents(t, rec, 1)
	if entries[0].Subtype != ToolRequestResultRequestBlocked {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestResultRequestBlocked, entries[0])
	}
	if entries[0].CallResult == nil {
		t.Error("expected event to include the actual (rejected) CallResult")
	}
}

func TestGuardBackendCallError(t *testing.T) {
	r, rec, orch := newTestGuardRuntime(t, true)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return nil, errors.New("backend exploded")
	}

	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.flaky_tool"}
	_, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "dave", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall)
	if err == nil || err.Error() != "backend exploded" {
		t.Fatalf("expected the original backend error to propagate unchanged, got: %v", err)
	}

	entries := waitForEvents(t, rec, 1)
	if entries[0].Subtype != ToolRequestResultCallFailed {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestResultCallFailed, entries[0])
	}
	if entries[0].Reason != "backend exploded" {
		t.Errorf("expected Reason=%q, got %+v", "backend exploded", entries[0])
	}
	if entries[0].CallResult != nil {
		t.Errorf("expected event to have a nil CallResult, got %v", entries[0].CallResult)
	}
}

func TestGuardNoOrchestrationStepsSkipsAudit(t *testing.T) {
	r := NewRuntime(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"empty-orch": {}}})
	rec := &eventRecorder{}
	r.SetEventPublisher(rec.record)
	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	_, err := r.Guard(context.Background(), "empty-orch", CallerIdentity{User: "eve", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall)
	if err != nil {
		t.Fatalf("expected an empty orchestration to be a no-op pass-through, got error: %v", err)
	}
	if !called {
		t.Error("expected doCall to be invoked")
	}
	// A true no-op: give any (incorrectly) fired goroutine time to land,
	// then confirm nothing was published.
	time.Sleep(50 * time.Millisecond)
	if entries := rec.snapshot(); len(entries) != 0 {
		t.Errorf("expected no published events for an orchestration with an empty toolCallAudit, got %+v", entries)
	}
}

func TestGuardSentinelWithoutPublisherDoesNotPanic(t *testing.T) {
	r, _, orch := newTestGuardRuntime(t, true)
	r.SetEventPublisher(nil) // undo newTestGuardRuntime's SetEventPublisher
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	result, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "frank", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall)
	if err != nil {
		t.Fatalf("expected success even with no publisher set, got error: %v", err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("expected a non-empty result")
	}
}

func TestGuardPassesStepParametersToAuthorizationTools(t *testing.T) {
	var gotCallParameters, gotResultParameters string

	server := mcp.NewServer(&mcp.Implementation{Name: "test-authorization-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in CallAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			gotCallParameters = in.Parameters
			return nil, AuthorizationOutput{Authorized: true}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "result-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in ResultAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			gotResultParameters = in.Parameters
			return nil, AuthorizationOutput{Authorized: true}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: httpServer.URL},
	})
	orch := Orchestration{
		CallAuthorization:   []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization", Parameters: `{"team":"platform-security"}`}},
		ResultAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "result-authorization", Parameters: `{"maxLength":50}`}},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "gina", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotCallParameters != `{"team":"platform-security"}` {
		t.Errorf("expected call-authorization to receive the step's Parameters, got %q", gotCallParameters)
	}
	if gotResultParameters != `{"maxLength":50}` {
		t.Errorf("expected result-authorization to receive the step's Parameters, got %q", gotResultParameters)
	}
}

// TestGuardPopulatesTargetServerAndTargetTool covers the
// CallAuthorizationInput.TargetServer/TargetTool extension: Guard must
// split reqParams.Name (the namespaced tool name being guarded) and pass
// both halves to every call-authorization step, since
// proxy_utility.business_logic_constraints_validation relies on TargetServer to
// know which server to dial for a business validation tool.
func TestGuardPopulatesTargetServerAndTargetTool(t *testing.T) {
	var gotTargetServer, gotTargetTool string

	server := mcp.NewServer(&mcp.Implementation{Name: "test-authorization-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, in CallAuthorizationInput) (*mcp.CallToolResult, AuthorizationOutput, error) {
			gotTargetServer = in.TargetServer
			gotTargetTool = in.TargetTool
			return nil, AuthorizationOutput{Authorized: true}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	r := NewRuntime(nil)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: httpServer.URL},
	})
	orch := Orchestration{
		CallAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "garden-online-store.purchase_products"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "gina", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotTargetServer != "garden-online-store" {
		t.Errorf("expected TargetServer %q, got %q", "garden-online-store", gotTargetServer)
	}
	if gotTargetTool != "purchase_products" {
		t.Errorf("expected TargetTool %q, got %q", "purchase_products", gotTargetTool)
	}
}

func TestGuardMarshalsRequestParams(t *testing.T) {
	r, rec, orch := newTestGuardRuntime(t, true)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello", Arguments: json.RawMessage(`{"language":"english"}`)}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "frank", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entries := waitForEvents(t, rec, 1)
	raw, ok := entries[0].CallRequest.(json.RawMessage)
	if !ok {
		t.Fatalf("expected CallRequest to be json.RawMessage, got %T", entries[0].CallRequest)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("failed to unmarshal CallRequest: %v", err)
	}
	if decoded["name"] != "orchestration-demo.say_hello" {
		t.Errorf("expected marshaled request to include the tool name, got %+v", decoded)
	}
}

func TestGuardFiresToolRequestEventOnSuccess(t *testing.T) {
	authServer := newAuthorizationServer(t, true)
	resultRec := &eventRecorder{}
	requestRec := &requestEventRecorder{}

	r := NewRuntime(nil)
	r.SetEventPublisher(resultRec.record)
	r.SetRequestEventPublisher(requestRec.record)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: authServer.URL},
	})
	orch := Orchestration{
		CallAuthorization:   []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}},
		ResultAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "result-authorization"}},
		ToolCallAudit:       []string{ToolCallAuditSentinel, ToolRequestAuditSentinel},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "henry", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	reqEntries := waitForRequestEvents(t, requestRec, 1)
	if reqEntries[0].Subtype != ToolRequestOK {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestOK, reqEntries[0])
	}
	if reqEntries[0].Reason != "" {
		t.Errorf("expected empty Reason on request-ok, got %+v", reqEntries[0])
	}

	// Both sentinels were configured together, so the sibling
	// tool-request-result event must also still fire.
	resultEntries := waitForEvents(t, resultRec, 1)
	if resultEntries[0].Subtype != ToolRequestResultCallSucceeded {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestResultCallSucceeded, resultEntries[0])
	}
}

func TestGuardFiresToolRequestEventOnRejection(t *testing.T) {
	authServer := newAuthorizationServer(t, false)
	requestRec := &requestEventRecorder{}

	r := NewRuntime(nil)
	r.SetRequestEventPublisher(requestRec.record)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: authServer.URL},
	})
	orch := Orchestration{
		CallAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}},
		ToolCallAudit:     []string{ToolRequestAuditSentinel},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.bad_tool"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "iris", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err == nil {
		t.Fatal("expected an error from call-authorization rejection")
	}
	// The strongest proof the event fires "before trying to execute" the
	// call: doCall never runs at all on this path, yet the event still
	// arrives.
	if called {
		t.Error("expected doCall to NOT be invoked when call-authorization rejects")
	}

	entries := waitForRequestEvents(t, requestRec, 1)
	if entries[0].Subtype != ToolRequestBlocked {
		t.Errorf("expected Subtype=%q, got %+v", ToolRequestBlocked, entries[0])
	}
	if entries[0].Reason == "" {
		t.Error("expected a non-empty Reason on rejection")
	}
}

func TestGuardToolRequestEventIndependentOfToolCallAudit(t *testing.T) {
	// Only ToolCallAuditSentinel configured (not ToolRequestAuditSentinel):
	// tool-request-result still fires, but tool-request must not - proving
	// the two publish switches are gated independently.
	r, resultRec, orch := newTestGuardRuntime(t, true)
	requestRec := &requestEventRecorder{}
	r.SetRequestEventPublisher(requestRec.record)
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hello"}}}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "jack", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForEvents(t, resultRec, 1)

	// Give any (incorrectly) fired tool-request event time to land, then
	// confirm none did.
	time.Sleep(50 * time.Millisecond)
	if entries := requestRec.snapshot(); len(entries) != 0 {
		t.Errorf("expected no published tool-request events when only ToolCallAuditSentinel is configured, got %+v", entries)
	}
}

func TestGuardToolCallResultAuditIndependentOfToolRequestAudit(t *testing.T) {
	// Only ToolRequestAuditSentinel configured (not ToolCallAuditSentinel):
	// tool-request fires, but tool-request-result must not - the reverse of
	// TestGuardToolRequestEventIndependentOfToolCallAudit.
	authServer := newAuthorizationServer(t, true)
	resultRec := &eventRecorder{}
	requestRec := &requestEventRecorder{}

	r := NewRuntime(nil)
	r.SetEventPublisher(resultRec.record)
	r.SetRequestEventPublisher(requestRec.record)
	r.OnAdded(discover_mcp_servers.ServerMap{
		"authorization-mcp-server": &discover_mcp_servers.ServerAttributes{ServerName: "authorization-mcp-server", ServerUrl: authServer.URL},
	})
	orch := Orchestration{
		CallAuthorization: []OrchestrationStep{{Server: "authorization-mcp-server", Tool: "call-authorization"}},
		ToolCallAudit:     []string{ToolRequestAuditSentinel},
	}
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})

	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "orchestration-demo.say_hello"}
	if _, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "leo", Roles: []string{"user"}}, "test-tool-call-id", reqParams, doCall); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	waitForRequestEvents(t, requestRec, 1)

	time.Sleep(50 * time.Millisecond)
	if entries := resultRec.snapshot(); len(entries) != 0 {
		t.Errorf("expected no published tool-request-result events when only ToolRequestAuditSentinel is configured, got %+v", entries)
	}
}

// TestGuardChecksInputRequiredResults checks that an "input required"
// result (protocol 2026-07-28+) - the backend asking the client for user
// input - goes through result-authorization like any result, since its
// questions and content reach the client too: a rejecting check blocks it,
// and an accepting one passes it through unchanged (so elicitation still
// works) without a result audit, which waits for the follow-up call.
func TestGuardChecksInputRequiredResults(t *testing.T) {
	inputRequired := func(t *testing.T) *mcp.CallToolResult {
		t.Helper()
		var res mcp.CallToolResult
		if err := json.Unmarshal([]byte(`{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"mode":"form","message":"Book it for SSN 123-45-6789?"}}},"requestState":"s1"}`), &res); err != nil {
			t.Fatalf("building an input-required result: %v", err)
		}
		if !res.NeedsInput() {
			t.Fatal("test setup: result isn't marked input-required")
		}
		return &res
	}
	guard := func(t *testing.T, authorized bool) (*mcp.CallToolResult, *mcp.CallToolResult, error, *eventRecorder) {
		t.Helper()
		resultAuthServer := newAuthorizationServer(t, authorized)
		rec := &eventRecorder{}
		r := NewRuntime(nil)
		r.SetEventPublisher(rec.record)
		r.OnAdded(discover_mcp_servers.ServerMap{
			"result-auth-server": &discover_mcp_servers.ServerAttributes{ServerName: "result-auth-server", ServerUrl: resultAuthServer.URL},
		})
		orch := Orchestration{
			ResultAuthorization: []OrchestrationStep{{Server: "result-auth-server", Tool: "result-authorization"}},
			ToolCallAudit:       []string{ToolCallAuditSentinel},
		}
		r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{"orch": orch}})
		sent := inputRequired(t)
		got, err := r.Guard(context.Background(), "orch", CallerIdentity{User: "carol", Roles: []string{"user"}}, "id-1",
			&mcp.CallToolParamsRaw{Name: "orchestration-demo.book"},
			func(context.Context) (*mcp.CallToolResult, error) { return sent, nil })
		return sent, got, err, rec
	}

	t.Run("a rejecting result check blocks it", func(t *testing.T) {
		_, got, err, rec := guard(t, false)
		if err == nil || got != nil {
			t.Fatalf("Guard = %v, %v; want the input-required result blocked", got, err)
		}
		events := waitForEvents(t, rec, 1)
		if events[0].Subtype != ToolRequestResultRequestBlocked {
			t.Errorf("audit subtype = %q, want %q", events[0].Subtype, ToolRequestResultRequestBlocked)
		}
	})

	t.Run("an accepting result check passes it through unchanged, unaudited", func(t *testing.T) {
		sent, got, err, rec := guard(t, true)
		if err != nil {
			t.Fatalf("Guard: %v", err)
		}
		if got != sent {
			t.Error("the input-required result wasn't passed through unchanged")
		}
		time.Sleep(50 * time.Millisecond)
		if n := len(rec.snapshot()); n != 0 {
			t.Errorf("%d result audit events for an input-required result, want 0 (the follow-up call is audited)", n)
		}
	})
}

// TestMissingOrchestrationFailsClosed checks that a tool mapped to an
// orchestration that doesn't exist - a bad config, or one removed by an
// update between MatchOrchestration and Guard - is rejected, not run
// unguarded.
func TestMissingOrchestrationFailsClosed(t *testing.T) {
	r := NewRuntime(&OrchestrationConfig{
		Orchestrations: map[string]Orchestration{"real": {}},
		ToolMappings:   []ToolMapping{{Pattern: "store.*", Orchestration: "missing"}, {Pattern: "*", Orchestration: "real"}},
	})
	name, _, matched := r.MatchOrchestration("store.buy")
	if !matched || name != "missing" {
		t.Fatalf("MatchOrchestration = %q, %v; want the first matching mapping (%q) to count as a match", name, matched, "missing")
	}

	called := false
	doCall := func(ctx context.Context) (*mcp.CallToolResult, error) {
		called = true
		return &mcp.CallToolResult{}, nil
	}
	reqParams := &mcp.CallToolParamsRaw{Name: "store.buy"}
	if _, err := r.Guard(context.Background(), name, CallerIdentity{User: "eve"}, "id", reqParams, doCall); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Guard for a missing orchestration: err = %v; want a not-found rejection", err)
	}
	// Removed after matching: "real" matched, then an update dropped it.
	r.Update(&OrchestrationConfig{Orchestrations: map[string]Orchestration{}})
	if _, err := r.Guard(context.Background(), "real", CallerIdentity{User: "eve"}, "id", reqParams, doCall); err == nil {
		t.Error("Guard ran a call whose orchestration was removed after matching")
	}
	if called {
		t.Error("the guarded call ran")
	}
}
