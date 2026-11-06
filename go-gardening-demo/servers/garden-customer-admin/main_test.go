// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/authserver"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
)

// fakeAuthServer is a minimal in-process stand-in for demo-authorization-
// server's POST /admin/users, recording every batch it receives (and
// optionally failing, to exercise the "auth-server failure leaves in-memory
// state untouched" atomicity guarantee).
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

func TestCustomerAdminLoadAndUpsert(t *testing.T) {
	fake := &fakeAuthServer{}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	ca := newCustomerAdmin(authserver.New(srv.URL, "admin", "admin"))

	_, res, err := ca.loadCustomers(ctx, nil, LoadCustomersArgs{Customers: []entities.OnlineCustomer{
		{ID: "cust-0001", Details: entities.ContactDetails{Name: "Alice"}, Type: "vip", Sentiment: "happy"},
		{ID: "cust-0002", Details: entities.ContactDetails{Name: "Bob"}, Type: "one-time-customer", Sentiment: "neutral"},
	}})
	if err != nil {
		t.Fatalf("loadCustomers: %v", err)
	}
	if res.Loaded != 2 {
		t.Errorf("expected 2 loaded, got %d", res.Loaded)
	}
	if len(ca.customers) != 2 {
		t.Fatalf("expected 2 customers committed to memory, got %d", len(ca.customers))
	}
	got := rolesOf(fake.lastBatch(), "cust-0001")
	want := []string{"customer", "vip"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("expected roles %v for cust-0001, got %v", want, got)
	}

	// add_or_update_customer preserves an existing Deactivated flag.
	ca.customers["cust-0001"] = CustomerRecord{ID: "cust-0001", Type: "vip", Deactivated: true}
	_, rec, err := ca.addOrUpdateCustomer(ctx, nil, AddOrUpdateCustomerArgs{ID: "cust-0001", Name: "Alice B.", Type: "vip", Sentiment: "happy"})
	if err != nil {
		t.Fatalf("addOrUpdateCustomer: %v", err)
	}
	if !rec.Deactivated {
		t.Error("expected addOrUpdateCustomer to preserve the existing Deactivated=true flag")
	}
	got = rolesOf(fake.lastBatch(), "cust-0001")
	if len(got) != 3 || got[2] != "deactivated" {
		t.Errorf("expected roles to include deactivated after update, got %v", got)
	}
}

func TestCustomerAdminDeactivateAndSentimentAndType(t *testing.T) {
	fake := &fakeAuthServer{}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	ca := newCustomerAdmin(authserver.New(srv.URL, "admin", "admin"))
	ca.customers["cust-0001"] = CustomerRecord{ID: "cust-0001", Type: "vip", Sentiment: "neutral"}

	// mark_customer_deactivated adds "deactivated" without losing the
	// existing functional role.
	_, rec, err := ca.markCustomerDeactivated(ctx, nil, MarkCustomerDeactivatedArgs{ID: "cust-0001", Deactivated: true})
	if err != nil {
		t.Fatalf("markCustomerDeactivated: %v", err)
	}
	if !rec.Deactivated {
		t.Error("expected Deactivated=true")
	}
	got := rolesOf(fake.lastBatch(), "cust-0001")
	if len(got) != 3 || got[0] != "customer" || got[1] != "vip" || got[2] != "deactivated" {
		t.Errorf("expected [customer vip deactivated], got %v", got)
	}

	// Re-activating removes it again.
	_, rec, err = ca.markCustomerDeactivated(ctx, nil, MarkCustomerDeactivatedArgs{ID: "cust-0001", Deactivated: false})
	if err != nil {
		t.Fatalf("markCustomerDeactivated (reactivate): %v", err)
	}
	if rec.Deactivated {
		t.Error("expected Deactivated=false after reactivation")
	}
	got = rolesOf(fake.lastBatch(), "cust-0001")
	if len(got) != 2 {
		t.Errorf("expected deactivated role removed, got %v", got)
	}

	// modify_customer_sentiment must NOT call the auth server at all.
	batchesBefore := len(fake.batches)
	_, rec, err = ca.modifyCustomerSentiment(ctx, nil, ModifyCustomerSentimentArgs{ID: "cust-0001", Sentiment: "unhappy"})
	if err != nil {
		t.Fatalf("modifyCustomerSentiment: %v", err)
	}
	if rec.Sentiment != "unhappy" {
		t.Errorf("expected sentiment updated locally, got %q", rec.Sentiment)
	}
	if len(fake.batches) != batchesBefore {
		t.Errorf("expected modifyCustomerSentiment to make no auth-server call, but batch count changed %d -> %d", batchesBefore, len(fake.batches))
	}

	// modify_customer_type DOES call the auth server (it's the relay target for
	// garden-business-ops).
	_, rec, err = ca.modifyCustomerType(ctx, nil, ModifyCustomerTypeArgs{ID: "cust-0001", Type: "returning-customer"})
	if err != nil {
		t.Fatalf("modifyCustomerType: %v", err)
	}
	if rec.Type != "returning-customer" {
		t.Errorf("expected type updated, got %q", rec.Type)
	}
	got = rolesOf(fake.lastBatch(), "cust-0001")
	if len(got) != 2 || got[1] != "returning-customer" {
		t.Errorf("expected role to reflect new type, got %v", got)
	}
}

func TestCustomerAdminAtomicityOnAuthServerFailure(t *testing.T) {
	fake := &fakeAuthServer{fail: true}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	ca := newCustomerAdmin(authserver.New(srv.URL, "admin", "admin"))

	_, _, err := ca.addOrUpdateCustomer(ctx, nil, AddOrUpdateCustomerArgs{ID: "cust-0001", Name: "Alice", Type: "vip"})
	if err == nil {
		t.Fatal("expected addOrUpdateCustomer to fail when the auth server is unreachable/erroring")
	}
	if _, ok := ca.customers["cust-0001"]; ok {
		t.Error("expected in-memory state to remain untouched after an auth-server failure")
	}
}

func TestCustomerAdminGetCustomersFiltering(t *testing.T) {
	ca := newCustomerAdmin(nil) // no auth server needed for read-only filtering
	ca.customers = map[string]CustomerRecord{
		"cust-0001": {ID: "cust-0001", Details: entities.ContactDetails{Name: "Alice Smith"}, Type: "vip", Sentiment: "happy", Deactivated: false},
		"cust-0002": {ID: "cust-0002", Details: entities.ContactDetails{Name: "Bob Jones"}, Type: "one-time-customer", Sentiment: "neutral", Deactivated: true},
		"cust-0003": {ID: "cust-0003", Details: entities.ContactDetails{Name: "Alicia Keys"}, Type: "returning-customer", Sentiment: "unhappy", Deactivated: false},
	}

	ctx := context.Background()

	_, allRes, _ := ca.getCustomers(ctx, nil, GetCustomersArgs{})
	all := allRes.Customers
	if len(all) != 3 {
		t.Errorf("expected 3 with no filters, got %d", len(all))
	}

	_, byNameRes, _ := ca.getCustomers(ctx, nil, GetCustomersArgs{Name: "ali"})
	byName := byNameRes.Customers
	if len(byName) != 2 {
		t.Errorf("expected 2 case-insensitive substring matches for 'ali', got %d: %+v", len(byName), byName)
	}

	deactivatedTrue := true
	_, byDeactivatedRes, _ := ca.getCustomers(ctx, nil, GetCustomersArgs{Deactivated: &deactivatedTrue})
	byDeactivated := byDeactivatedRes.Customers
	if len(byDeactivated) != 1 || byDeactivated[0].ID != "cust-0002" {
		t.Errorf("expected only cust-0002 deactivated, got %+v", byDeactivated)
	}

	_, byTypeRes, _ := ca.getCustomers(ctx, nil, GetCustomersArgs{Type: "vip"})
	byType := byTypeRes.Customers
	if len(byType) != 1 || byType[0].ID != "cust-0001" {
		t.Errorf("expected only cust-0001 for type=vip, got %+v", byType)
	}
}

// TestCustomerAdminGetCustomerType covers the narrow id->type lookup
// garden-online-store's purchase_products relays to for deal eligibility
// - deliberately not returning name/address/phone the way get_customers
// does (see getCustomerType's own doc comment).
func TestCustomerAdminGetCustomerType(t *testing.T) {
	ca := newCustomerAdmin(nil)
	ca.customers = map[string]CustomerRecord{
		"cust-0001": {ID: "cust-0001", Details: entities.ContactDetails{Name: "Alice Smith"}, Type: "vip"},
	}
	ctx := context.Background()

	_, res, err := ca.getCustomerType(ctx, nil, GetCustomerTypeArgs{ID: "cust-0001"})
	if err != nil {
		t.Fatalf("getCustomerType: %v", err)
	}
	if res.ID != "cust-0001" || res.Type != "vip" {
		t.Errorf("expected {cust-0001, vip}, got %+v", res)
	}

	if _, _, err := ca.getCustomerType(ctx, nil, GetCustomerTypeArgs{ID: "cust-unknown"}); err == nil {
		t.Error("expected an error for an unknown customer id")
	}
}

// TestCustomerAdminGetCustomersStatus covers the type+sentiment bulk
// listing granted to the analyst role in place of garden-customer-service.
// get_customer_service_calls - narrow like get_customer_type, but covering
// every customer at once, never name/address/phone.
func TestCustomerAdminGetCustomersStatus(t *testing.T) {
	ca := newCustomerAdmin(nil)
	ca.customers = map[string]CustomerRecord{
		"cust-0001": {ID: "cust-0001", Details: entities.ContactDetails{Name: "Alice Smith"}, Type: "vip", Sentiment: "happy"},
		"cust-0002": {ID: "cust-0002", Details: entities.ContactDetails{Name: "Bob Jones"}, Type: "one-time-customer", Sentiment: "neutral"},
	}
	ctx := context.Background()

	_, res, err := ca.getCustomersStatus(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("getCustomersStatus: %v", err)
	}
	if len(res.Customers) != 2 {
		t.Fatalf("expected 2 customers, got %d: %+v", len(res.Customers), res.Customers)
	}
	// sorted by id
	if res.Customers[0] != (CustomerStatus{ID: "cust-0001", Type: "vip", Sentiment: "happy"}) {
		t.Errorf("expected cust-0001 first with {vip, happy}, got %+v", res.Customers[0])
	}
	if res.Customers[1] != (CustomerStatus{ID: "cust-0002", Type: "one-time-customer", Sentiment: "neutral"}) {
		t.Errorf("expected cust-0002 second with {one-time-customer, neutral}, got %+v", res.Customers[1])
	}
}

// TestCustomerAdminGetCustomerDetails covers the one tool that returns a
// customer's contact details - see its own doc comment on why every other
// tool in this file returns a CustomerSummary instead.
func TestCustomerAdminGetCustomerDetails(t *testing.T) {
	ca := newCustomerAdmin(nil)
	ca.customers = map[string]CustomerRecord{
		"cust-0001": {ID: "cust-0001", Details: entities.ContactDetails{Name: "Alice Smith", Address: "1 Main St", Phone: "555-201-1001"}, Type: "vip"},
	}
	ctx := context.Background()

	_, res, err := ca.getCustomerDetails(ctx, nil, GetCustomerDetailsArgs{ID: "cust-0001"})
	if err != nil {
		t.Fatalf("getCustomerDetails: %v", err)
	}
	if res.ID != "cust-0001" || res.Details.Name != "Alice Smith" || res.Details.Address != "1 Main St" || res.Details.Phone != "555-201-1001" {
		t.Errorf("expected full contact details for cust-0001, got %+v", res)
	}

	if _, _, err := ca.getCustomerDetails(ctx, nil, GetCustomerDetailsArgs{ID: "cust-unknown"}); err == nil {
		t.Error("expected an error for an unknown customer id")
	}
}

// TestCustomerAdminSummaryToolsNeverExposeContactDetails covers that every
// tool other than get_customer_details/load_customers is structurally
// incapable of returning a customer's name/address/phone: CustomerSummary
// simply has no field for it, so this asserts the actual wire-level
// guarantee (no "name"/"address"/"phone" substring anywhere in the
// marshaled JSON) rather than a Go-level check the compiler already makes
// impossible to get wrong.
func TestCustomerAdminSummaryToolsNeverExposeContactDetails(t *testing.T) {
	fake := &fakeAuthServer{}
	srv := fake.server()
	defer srv.Close()

	ctx := context.Background()
	ca := newCustomerAdmin(authserver.New(srv.URL, "admin", "admin"))

	assertNoContactDetails := func(t *testing.T, label string, v any) {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", label, err)
		}
		for _, needle := range []string{"Alice", "555-201-1001", "1 Main St", `"details"`, `"name"`, `"address"`, `"phone"`} {
			if strings.Contains(string(raw), needle) {
				t.Errorf("%s: expected no contact-details leakage, but %q appeared in %s", label, needle, raw)
			}
		}
	}

	_, addRes, err := ca.addOrUpdateCustomer(ctx, nil, AddOrUpdateCustomerArgs{ID: "cust-0001", Name: "Alice", Address: "1 Main St", Phone: "555-201-1001", Type: "vip"})
	if err != nil {
		t.Fatalf("addOrUpdateCustomer: %v", err)
	}
	assertNoContactDetails(t, "addOrUpdateCustomer", addRes)

	_, deactRes, err := ca.markCustomerDeactivated(ctx, nil, MarkCustomerDeactivatedArgs{ID: "cust-0001", Deactivated: true})
	if err != nil {
		t.Fatalf("markCustomerDeactivated: %v", err)
	}
	assertNoContactDetails(t, "markCustomerDeactivated", deactRes)

	_, sentRes, err := ca.modifyCustomerSentiment(ctx, nil, ModifyCustomerSentimentArgs{ID: "cust-0001", Sentiment: "unhappy"})
	if err != nil {
		t.Fatalf("modifyCustomerSentiment: %v", err)
	}
	assertNoContactDetails(t, "modifyCustomerSentiment", sentRes)

	_, typeRes, err := ca.modifyCustomerType(ctx, nil, ModifyCustomerTypeArgs{ID: "cust-0001", Type: "returning-customer"})
	if err != nil {
		t.Fatalf("modifyCustomerType: %v", err)
	}
	assertNoContactDetails(t, "modifyCustomerType", typeRes)

	_, listRes, err := ca.getCustomers(ctx, nil, GetCustomersArgs{})
	if err != nil {
		t.Fatalf("getCustomers: %v", err)
	}
	assertNoContactDetails(t, "getCustomers", listRes)
}
