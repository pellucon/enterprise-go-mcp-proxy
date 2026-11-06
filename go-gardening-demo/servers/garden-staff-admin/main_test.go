// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/authserver"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeAuthServer struct {
	mu      sync.Mutex
	batches [][]authserver.User
	fail    bool
}

func (f *fakeAuthServer) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail {
			http.Error(w, "simulated failure", http.StatusInternalServerError)
			return
		}
		var users []authserver.User
		if err := json.NewDecoder(r.Body).Decode(&users); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.batches = append(f.batches, users)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(users)
	}))
}

func (f *fakeAuthServer) lastBatch() []authserver.User {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return nil
	}
	return f.batches[len(f.batches)-1]
}

func rolesOf(users []authserver.User, username string) []string {
	for _, u := range users {
		if u.Username == username {
			return u.Roles
		}
	}
	return nil
}

func TestStaffAdminLoadRolesAndStaffers(t *testing.T) {
	fake := &fakeAuthServer{}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	sa := newStaffAdmin(authserver.New(srv.URL, "admin", "admin"))

	// load_staff_roles is local-only - no auth-server call.
	_, res, err := sa.loadStaffRoles(ctx, nil, LoadStaffRolesArgs{StaffRoles: []entities.StaffRole{
		{ID: "customer-service-representative", Name: "Customer Service Representative"},
	}})
	if err != nil {
		t.Fatalf("loadStaffRoles: %v", err)
	}
	if res.Loaded != 1 || len(fake.batches) != 0 {
		t.Errorf("expected local-only load (0 auth-server calls), got loaded=%d batches=%d", res.Loaded, len(fake.batches))
	}

	_, res, err = sa.loadStaffers(ctx, nil, LoadStaffersArgs{Staffers: []entities.Staffer{
		{ID: "staff-01", Details: entities.ContactDetails{Name: "Morgan Ellery"}, Roles: []string{"super-user", "it-admin"}},
		{ID: "staff-02", Details: entities.ContactDetails{Name: "Priya Anand"}, Roles: []string{"customer-service-representative", "customer-service-manager"}},
	}})
	if err != nil {
		t.Fatalf("loadStaffers: %v", err)
	}
	if res.Loaded != 2 {
		t.Errorf("expected 2 loaded, got %d", res.Loaded)
	}
	got := rolesOf(fake.lastBatch(), "staff-01")
	if len(got) != 2 || got[0] != "super-user" || got[1] != "it-admin" {
		t.Errorf("expected [super-user it-admin], got %v", got)
	}
}

func TestStaffAdminDeactivatePreservesFunctionalRoles(t *testing.T) {
	fake := &fakeAuthServer{}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	sa := newStaffAdmin(authserver.New(srv.URL, "admin", "admin"))
	sa.staffers["staff-06"] = StafferRecord{ID: "staff-06", Roles: []string{"customer-service-representative"}}

	_, rec, err := sa.markStafferDeactivated(ctx, nil, MarkStafferDeactivatedArgs{ID: "staff-06", Deactivated: true})
	if err != nil {
		t.Fatalf("markStafferDeactivated: %v", err)
	}
	if !rec.Deactivated {
		t.Error("expected Deactivated=true")
	}
	got := rolesOf(fake.lastBatch(), "staff-06")
	if len(got) != 2 || got[0] != "customer-service-representative" || got[1] != "deactivated" {
		t.Errorf("expected [customer-service-representative deactivated], got %v", got)
	}

	// Reactivate: functional role must remain, deactivated must be gone.
	_, rec, err = sa.markStafferDeactivated(ctx, nil, MarkStafferDeactivatedArgs{ID: "staff-06", Deactivated: false})
	if err != nil {
		t.Fatalf("markStafferDeactivated (reactivate): %v", err)
	}
	if rec.Deactivated {
		t.Error("expected Deactivated=false")
	}
	got = rolesOf(fake.lastBatch(), "staff-06")
	if len(got) != 1 || got[0] != "customer-service-representative" {
		t.Errorf("expected [customer-service-representative] after reactivation, got %v", got)
	}

	// addOrUpdateStaffer must preserve an existing Deactivated flag.
	sa.staffers["staff-06"] = StafferRecord{ID: "staff-06", Roles: []string{"customer-service-representative"}, Deactivated: true}
	_, rec, err = sa.addOrUpdateStaffer(ctx, nil, AddOrUpdateStafferArgs{ID: "staff-06", Name: "Devon Ortiz", Roles: []string{"customer-service-representative"}})
	if err != nil {
		t.Fatalf("addOrUpdateStaffer: %v", err)
	}
	if !rec.Deactivated {
		t.Error("expected addOrUpdateStaffer to preserve Deactivated=true")
	}
}

func TestStaffAdminAtomicityOnDemoAuthFailure(t *testing.T) {
	fake := &fakeAuthServer{fail: true}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	sa := newStaffAdmin(authserver.New(srv.URL, "admin", "admin"))

	_, _, err := sa.addOrUpdateStaffer(ctx, nil, AddOrUpdateStafferArgs{ID: "staff-01", Name: "X", Roles: []string{"it-operations"}})
	if err == nil {
		t.Fatal("expected addOrUpdateStaffer to fail when the auth server is unreachable/erroring")
	}
	if _, ok := sa.staffers["staff-01"]; ok {
		t.Error("expected in-memory state to remain untouched after an auth-server failure")
	}
}

func TestStaffAdminGetStaffersFiltering(t *testing.T) {
	sa := newStaffAdmin(nil)
	sa.staffers = map[string]StafferRecord{
		"staff-01": {ID: "staff-01", Details: entities.ContactDetails{Name: "Morgan Ellery"}, Roles: []string{"super-user", "it-admin"}},
		"staff-02": {ID: "staff-02", Details: entities.ContactDetails{Name: "Priya Anand"}, Roles: []string{"customer-service-representative", "customer-service-manager"}},
		"staff-03": {ID: "staff-03", Details: entities.ContactDetails{Name: "Jordan Lee"}, Roles: []string{"customer-service-representative"}, Deactivated: true},
	}

	ctx := context.Background()

	_, allRes, _ := sa.getStaffers(ctx, nil, GetStaffersArgs{})
	all := allRes.Staffers
	if len(all) != 3 {
		t.Errorf("expected 3 with no filters, got %d", len(all))
	}

	_, byRoleRes, _ := sa.getStaffers(ctx, nil, GetStaffersArgs{Role: "customer-service-representative"})
	byRole := byRoleRes.Staffers
	if len(byRole) != 2 {
		t.Errorf("expected 2 with role customer-service-representative, got %d", len(byRole))
	}

	deactivatedTrue := true
	_, byDeactivatedRes, _ := sa.getStaffers(ctx, nil, GetStaffersArgs{Deactivated: &deactivatedTrue})
	byDeactivated := byDeactivatedRes.Staffers
	if len(byDeactivated) != 1 || byDeactivated[0].ID != "staff-03" {
		t.Errorf("expected only staff-03 deactivated, got %+v", byDeactivated)
	}

	_, byNameRes, _ := sa.getStaffers(ctx, nil, GetStaffersArgs{Name: "morgan"})
	byName := byNameRes.Staffers
	if len(byName) != 1 || byName[0].ID != "staff-01" {
		t.Errorf("expected only staff-01 for name=morgan, got %+v", byName)
	}
}

// staffDeactivationCallRequest builds a CallAuthorizationInput.CallRequest
// for mark_staffer_deactivated the same shape a real MCP wire round-trip
// produces (see garden-online-store's purchaseCallRequest for the same
// convention).
func staffDeactivationCallRequest(t *testing.T, args MarkStafferDeactivatedArgs) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Name      string                     `json:"name"`
		Arguments MarkStafferDeactivatedArgs `json:"arguments"`
	}{
		Name:      "garden-staff-admin.mark_staffer_deactivated",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("failed to build mark_staffer_deactivated CallRequest: %v", err)
	}
	return raw
}

// requestWithRelayHeaders mirrors garden-online-store's own helper of the
// same name - builds a *mcp.CallToolRequest carrying the given callback URL
// as inbound relay headers, matching what a real triggered-tool invocation
// presents (see runtime.go's platformHTTPClient). The assertion value
// itself is never verified by the fake proxy below, so any non-empty
// placeholder is fine.
func requestWithRelayHeaders(callbackURL string) *mcp.CallToolRequest {
	h := http.Header{}
	relayheaders.Set{Assertion: "Bearer test-assertion", CallbackURL: callbackURL}.Apply(h)
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
}

// newFakeCustomerServiceRelayProxy is an in-process stand-in for go-proxy
// itself, exposing the one namespaced tool validate_staff_handling relays
// to (garden-customer-service.get_customer_service_calls) - mirrors
// garden-online-store's newFakeGardeningRelayProxy. Ignores the "resolved"
// filter argument and always returns calls verbatim, since the real
// filtering happens on garden-customer-service's own side, not something
// this test needs to re-verify.
func newFakeCustomerServiceRelayProxy(t *testing.T, calls []entities.CustomerServiceCall) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-gardening-proxy", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "garden-customer-service.get_customer_service_calls"},
		func(ctx context.Context, req *mcp.CallToolRequest, args struct {
			Resolved *bool `json:"resolved,omitempty"`
		}) (*mcp.CallToolResult, struct {
			Calls []entities.CustomerServiceCall `json:"calls"`
		}, error) {
			return nil, struct {
				Calls []entities.CustomerServiceCall `json:"calls"`
			}{Calls: calls}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

// TestValidateStaffHandlingAuthorizesReactivationWithoutRelay covers that
// reactivating (Deactivated:false) is never restricted - no relay is even
// attempted, so this must succeed with no relay headers present at all.
func TestValidateStaffHandlingAuthorizesReactivationWithoutRelay(t *testing.T) {
	ctx := context.Background()
	callRequest := staffDeactivationCallRequest(t, MarkStafferDeactivatedArgs{ID: "staff-06", Deactivated: false})

	_, out, err := validateStaffHandling(ctx, &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateStaffHandling: unexpected Go error %v", err)
	}
	if !out.Authorized {
		t.Errorf("expected reactivation to be authorized without needing a relay, got Reason=%q", out.Reason)
	}
}

// TestValidateStaffHandlingFailsClosedWithoutRelayHeaders covers
// validate_staff_handling's fail-closed behavior when deactivating with no
// identity assertion/callback URL present - same convention as
// garden-online-store's TestOnlineStoreValidateStoreRequestsFailsClosedWithoutRelayHeaders.
func TestValidateStaffHandlingFailsClosedWithoutRelayHeaders(t *testing.T) {
	ctx := context.Background()
	callRequest := staffDeactivationCallRequest(t, MarkStafferDeactivatedArgs{ID: "staff-06", Deactivated: true})

	res, _, err := validateStaffHandling(ctx, &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateStaffHandling: unexpected Go error %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an IsError result when no relay headers are present, got %+v", res)
	}
}

// TestValidateStaffHandlingRelaySuccess is the full end-to-end proof that
// validate_staff_handling relays back through the proxy to
// garden-customer-service.get_customer_service_calls and correctly blocks
// deactivating a staffer who's still the handler or escalation target of
// an unresolved call, while allowing one who isn't.
func TestValidateStaffHandlingRelaySuccess(t *testing.T) {
	ctx := context.Background()
	escalatedTo := "staff-02"
	calls := []entities.CustomerServiceCall{
		{ID: "csc-01", HandledByStafferID: "staff-06", IssueText: "unresolved, handled by staff-06"},
		{ID: "csc-02", HandledByStafferID: "staff-05", EscalatedToStafferID: &escalatedTo, IssueText: "unresolved, escalated to staff-02"},
	}
	proxy := newFakeCustomerServiceRelayProxy(t, calls)

	// staff-06 is the handler of csc-01 - deactivation must be blocked.
	callRequest := staffDeactivationCallRequest(t, MarkStafferDeactivatedArgs{ID: "staff-06", Deactivated: true})
	_, out, err := validateStaffHandling(ctx, requestWithRelayHeaders(proxy.URL+"/mcp"), triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateStaffHandling: %v", err)
	}
	if out.Authorized {
		t.Error("expected deactivating staff-06 (still handling an unresolved call) to be rejected")
	}
	if out.Reason == "" {
		t.Error("expected a non-empty Reason explaining the rejection")
	}

	// staff-02 is the escalation target of csc-02 - deactivation must also
	// be blocked.
	callRequest = staffDeactivationCallRequest(t, MarkStafferDeactivatedArgs{ID: "staff-02", Deactivated: true})
	_, out, err = validateStaffHandling(ctx, requestWithRelayHeaders(proxy.URL+"/mcp"), triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateStaffHandling: %v", err)
	}
	if out.Authorized {
		t.Error("expected deactivating staff-02 (still the escalation target of an unresolved call) to be rejected")
	}

	// staff-99 isn't referenced by any call - deactivation must be allowed.
	callRequest = staffDeactivationCallRequest(t, MarkStafferDeactivatedArgs{ID: "staff-99", Deactivated: true})
	_, out, err = validateStaffHandling(ctx, requestWithRelayHeaders(proxy.URL+"/mcp"), triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateStaffHandling: %v", err)
	}
	if !out.Authorized {
		t.Errorf("expected deactivating staff-99 (not assigned to any unresolved call) to be authorized, got Reason=%q", out.Reason)
	}
}
