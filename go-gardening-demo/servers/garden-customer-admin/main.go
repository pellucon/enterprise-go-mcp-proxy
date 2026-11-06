// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// garden-customer-admin is a go-gardening-demo MCP backend server: it
// holds the gardening store's customer list in memory and keeps the auth
// server's role assignments for each customer in sync with it. Per the
// gardening-demo spec: "A customer should have the 'customer' role and
// a role that corresponds to their customer type."
//
// -auth-server-url's default (http://host.docker.internal:8102) targets
// demo-authorization-server, which has no k8s manifest and is documented as
// host-only (see CLAUDE.md's "AI-powered tool discovery" section for the
// closely-analogous host.docker.internal precedent used by
// ai-powered-tools' headless-Claude-bridge mode) — but the flag itself is
// generic (this server doesn't hardcode which auth server it talks to),
// overridden at deploy time via scripts/deploy.sh's own --auth-server-url
// flag (see garden-customer-admin/k8s/deployment.yaml).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/authserver"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// CustomerRecord is this server's own record for a customer: the same
// shape as entities.OnlineCustomer plus Deactivated, which
// entities.OnlineCustomer has no field for (deactivation is a live-server
// concept, not part of the generated seed data).
type CustomerRecord struct {
	ID          string                  `json:"id"`
	Details     entities.ContactDetails `json:"details"`
	Type        string                  `json:"type"`
	Sentiment   string                  `json:"sentiment"`
	Deactivated bool                    `json:"deactivated"`
}

// CustomerSummary is what every garden-customer-admin tool returns except
// get_customer_details below - id/type/sentiment/deactivated, deliberately
// never a customer's name/address/phone (entities.ContactDetails, stored
// internally as CustomerRecord.Details). Contact details are readable
// through exactly one tool now; every other tool that used to echo back a
// full CustomerRecord returns this narrower view instead.
type CustomerSummary struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Sentiment   string `json:"sentiment"`
	Deactivated bool   `json:"deactivated"`
}

func summarize(rec CustomerRecord) CustomerSummary {
	return CustomerSummary{ID: rec.ID, Type: rec.Type, Sentiment: rec.Sentiment, Deactivated: rec.Deactivated}
}

// rolesFor computes the full auth server role list for rec: the base
// "customer" role, its customer-type id (already a valid role-name string,
// e.g. "one-time-customer"), and "deactivated" if flagged. Recomputed fresh
// from rec every time rather than merged against whatever the auth server
// currently has — garden-customer-admin is the exclusive writer of
// "cust-*" usernames there, so there's nothing to merge against.
func rolesFor(rec CustomerRecord) []string {
	roles := []string{"customer", rec.Type}
	if rec.Deactivated {
		roles = append(roles, "deactivated")
	}
	return roles
}

type customerAdmin struct {
	mu         sync.Mutex
	customers  map[string]CustomerRecord
	authServer *authserver.Client
}

func newCustomerAdmin(authServer *authserver.Client) *customerAdmin {
	return &customerAdmin{customers: make(map[string]CustomerRecord), authServer: authServer}
}

type LoadCustomersArgs struct {
	Customers []entities.OnlineCustomer `json:"customers" jsonschema:"The full online_customers.json list to load."`
}

// LoadPromptsArgs is load_prompts' own bulk-load shape - see
// go-gardening-demo/promptstore's own doc comment for why this
// server's whole prompt set lives in data (prompts.json) rather than
// hand-written Go prompt functions.
type LoadPromptsArgs struct {
	Prompts []promptstore.Prompt `json:"prompts" jsonschema:"This server's entries from prompts.json (server field stripped - already implied by which server's load_prompts you're calling)."`
}

type LoadResult struct {
	Loaded int `json:"loaded"`
}

// loadCustomers commits to memory only if the single bulk auth-server POST
// for all N customers succeeds — atomic, no drift between the two stores.
func (ca *customerAdmin) loadCustomers(ctx context.Context, req *mcp.CallToolRequest, args LoadCustomersArgs) (*mcp.CallToolResult, LoadResult, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	candidates := make([]CustomerRecord, len(args.Customers))
	authUsers := make([]authserver.User, len(args.Customers))
	for i, c := range args.Customers {
		rec := CustomerRecord{ID: c.ID, Details: c.Details, Type: c.Type, Sentiment: c.Sentiment}
		candidates[i] = rec
		authUsers[i] = authserver.User{Username: rec.ID, Roles: rolesFor(rec)}
	}

	if err := ca.authServer.UpsertUsers(ctx, authUsers); err != nil {
		return nil, LoadResult{}, fmt.Errorf("load_customers: %w", err)
	}
	for _, rec := range candidates {
		ca.customers[rec.ID] = rec
	}
	return nil, LoadResult{Loaded: len(args.Customers)}, nil
}

type AddOrUpdateCustomerArgs struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Address   string `json:"address,omitempty"`
	Phone     string `json:"phone,omitempty"`
	Type      string `json:"type"`
	Sentiment string `json:"sentiment,omitempty"`
}

// addOrUpdateCustomer upserts by id. An existing customer's Deactivated
// flag is preserved (updating a customer's contact/type/sentiment
// shouldn't silently reactivate them).
func (ca *customerAdmin) addOrUpdateCustomer(ctx context.Context, req *mcp.CallToolRequest, args AddOrUpdateCustomerArgs) (*mcp.CallToolResult, CustomerSummary, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec := CustomerRecord{
		ID:        args.ID,
		Details:   entities.ContactDetails{Name: args.Name, Address: args.Address, Phone: args.Phone},
		Type:      args.Type,
		Sentiment: args.Sentiment,
	}
	if existing, ok := ca.customers[args.ID]; ok {
		rec.Deactivated = existing.Deactivated
	}

	if err := ca.authServer.UpsertUser(ctx, rec.ID, rolesFor(rec)); err != nil {
		return nil, CustomerSummary{}, fmt.Errorf("add_or_update_customer: %w", err)
	}
	ca.customers[rec.ID] = rec
	return nil, summarize(rec), nil
}

type MarkCustomerDeactivatedArgs struct {
	ID          string `json:"id"`
	Deactivated bool   `json:"deactivated"`
}

func (ca *customerAdmin) markCustomerDeactivated(ctx context.Context, req *mcp.CallToolRequest, args MarkCustomerDeactivatedArgs) (*mcp.CallToolResult, CustomerSummary, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec, ok := ca.customers[args.ID]
	if !ok {
		return nil, CustomerSummary{}, fmt.Errorf("mark_customer_deactivated: unknown customer %q", args.ID)
	}
	rec.Deactivated = args.Deactivated

	if err := ca.authServer.UpsertUser(ctx, rec.ID, rolesFor(rec)); err != nil {
		return nil, CustomerSummary{}, fmt.Errorf("mark_customer_deactivated: %w", err)
	}
	ca.customers[rec.ID] = rec
	return nil, summarize(rec), nil
}

type ModifyCustomerSentimentArgs struct {
	ID        string `json:"id"`
	Sentiment string `json:"sentiment"`
}

// modifyCustomerSentiment is local-only — sentiment isn't an auth server
// role, so no admin API call is made.
func (ca *customerAdmin) modifyCustomerSentiment(ctx context.Context, req *mcp.CallToolRequest, args ModifyCustomerSentimentArgs) (*mcp.CallToolResult, CustomerSummary, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec, ok := ca.customers[args.ID]
	if !ok {
		return nil, CustomerSummary{}, fmt.Errorf("modify_customer_sentiment: unknown customer %q", args.ID)
	}
	rec.Sentiment = args.Sentiment
	ca.customers[rec.ID] = rec
	return nil, summarize(rec), nil
}

type ModifyCustomerTypeArgs struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// modifyCustomerType is the relay target for garden-business-ops'
// update_customer_type.
func (ca *customerAdmin) modifyCustomerType(ctx context.Context, req *mcp.CallToolRequest, args ModifyCustomerTypeArgs) (*mcp.CallToolResult, CustomerSummary, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec, ok := ca.customers[args.ID]
	if !ok {
		return nil, CustomerSummary{}, fmt.Errorf("modify_customer_type: unknown customer %q", args.ID)
	}
	rec.Type = args.Type

	if err := ca.authServer.UpsertUser(ctx, rec.ID, rolesFor(rec)); err != nil {
		return nil, CustomerSummary{}, fmt.Errorf("modify_customer_type: %w", err)
	}
	ca.customers[rec.ID] = rec
	return nil, summarize(rec), nil
}

type GetCustomersArgs struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty" jsonschema:"Case-insensitive substring match."`
	Type        string `json:"type,omitempty"`
	Sentiment   string `json:"sentiment,omitempty"`
	Deactivated *bool  `json:"deactivated,omitempty" jsonschema:"Omit for no filter on deactivated status."`
}

// GetCustomersResult wraps get_customers' list in a named field rather
// than returning a bare array: some MCP clients (e.g. Claude Desktop)
// require every tool's inferred outputSchema to be a JSON object at the
// top level, and a bare []CustomerSummary Out infers as an array instead.
type GetCustomersResult struct {
	Customers []CustomerSummary `json:"customers"`
}

// getCustomers ANDs together whichever filters are non-empty; no filters
// at all returns every customer. The name filter still matches against the
// stored contact details even though they're never returned here - see
// CustomerSummary's own doc comment.
func (ca *customerAdmin) getCustomers(ctx context.Context, req *mcp.CallToolRequest, args GetCustomersArgs) (*mcp.CallToolResult, GetCustomersResult, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	nameQuery := strings.ToLower(args.Name)
	out := make([]CustomerSummary, 0, len(ca.customers))
	for _, rec := range ca.customers {
		if args.ID != "" && rec.ID != args.ID {
			continue
		}
		if nameQuery != "" && !strings.Contains(strings.ToLower(rec.Details.Name), nameQuery) {
			continue
		}
		if args.Type != "" && rec.Type != args.Type {
			continue
		}
		if args.Sentiment != "" && rec.Sentiment != args.Sentiment {
			continue
		}
		if args.Deactivated != nil && rec.Deactivated != *args.Deactivated {
			continue
		}
		out = append(out, summarize(rec))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetCustomersResult{Customers: out}, nil
}

type GetCustomerTypeArgs struct {
	ID string `json:"id"`
}

type GetCustomerTypeResult struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// getCustomerType is a narrow, read-only lookup - deliberately returning
// only {id, type} rather than the full CustomerRecord (name/address/
// phone/sentiment) get_customers exposes, so it's safe to grant broadly
// (see its own access-rule grant to role "customer" in
// client_access_rules.json) without leaking other customers' contact
// details. garden-online-store's purchase_products relays to this (see
// its own fetchCustomerType) to determine deal eligibility at checkout
// time - see entities.Deal's DiscountPercentages, keyed by customer type.
func (ca *customerAdmin) getCustomerType(ctx context.Context, req *mcp.CallToolRequest, args GetCustomerTypeArgs) (*mcp.CallToolResult, GetCustomerTypeResult, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec, ok := ca.customers[args.ID]
	if !ok {
		return nil, GetCustomerTypeResult{}, fmt.Errorf("get_customer_type: unknown customer %q", args.ID)
	}
	return nil, GetCustomerTypeResult{ID: rec.ID, Type: rec.Type}, nil
}

// CustomerStatus is one customer's {id, type, sentiment} - never name/
// address/phone - shared by every entry get_customers_status returns.
type CustomerStatus struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Sentiment string `json:"sentiment"`
}

// GetCustomersStatusResult wraps get_customers_status' list in a named
// field rather than returning a bare array - same top-level-object
// requirement as GetCustomersResult/GetStaffersResult above.
type GetCustomersStatusResult struct {
	Customers []CustomerStatus `json:"customers"`
}

// getCustomersStatus is a narrow, read-only bulk lookup - every customer's
// {id, type, sentiment}, never name/address/phone - same safety rationale
// as getCustomerType above, just covering every customer at once instead
// of a single id, for a caller (e.g. the analyst role) that needs an
// overview without the full get_customers listing's contact-detail-shaped
// surface (even though get_customers itself no longer returns contact
// details either, this stays deliberately narrower - just the two status
// fields, nothing else). No arguments; always returns every customer.
func (ca *customerAdmin) getCustomersStatus(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetCustomersStatusResult, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	out := make([]CustomerStatus, 0, len(ca.customers))
	for _, rec := range ca.customers {
		out = append(out, CustomerStatus{ID: rec.ID, Type: rec.Type, Sentiment: rec.Sentiment})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetCustomersStatusResult{Customers: out}, nil
}

type GetCustomerDetailsArgs struct {
	ID string `json:"id"`
}

type GetCustomerDetailsResult struct {
	ID      string                  `json:"id"`
	Details entities.ContactDetails `json:"details"`
}

// getCustomerDetails is the one place a customer's name/address/phone can
// be read back - every other tool in this file returns a CustomerSummary
// instead (see its own doc comment). Gated to exactly the roles that can
// already call add_or_update_customer - the only other tool that handles a
// customer's contact details - via client_access_rules.json's
// business-manager/customer-management grants, both a garden-customer-admin.*
// wildcard that already covers this tool with no separate access-rule
// change needed; admin's own "*" grant covers it the same way.
func (ca *customerAdmin) getCustomerDetails(ctx context.Context, req *mcp.CallToolRequest, args GetCustomerDetailsArgs) (*mcp.CallToolResult, GetCustomerDetailsResult, error) {
	ca.mu.Lock()
	defer ca.mu.Unlock()

	rec, ok := ca.customers[args.ID]
	if !ok {
		return nil, GetCustomerDetailsResult{}, fmt.Errorf("get_customer_details: unknown customer %q", args.ID)
	}
	return nil, GetCustomerDetailsResult{ID: rec.ID, Details: rec.Details}, nil
}

func createServer(ca *customerAdmin) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garden-customer-admin", Version: "1.0.0"}, nil)
	prompts := promptstore.New(server)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_customers",
		Description: "Bulk-load the customer list (online_customers.json's shape), syncing each customer's auth server roles.",
	}, ca.loadCustomers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_or_update_customer",
		Description: "Add or update (upsert by id) one customer, syncing their auth server roles.",
	}, ca.addOrUpdateCustomer)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mark_customer_deactivated",
		Description: "Set a customer's deactivated flag, adding/removing the \"deactivated\" role in the auth server without disturbing their other roles.",
	}, ca.markCustomerDeactivated)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "modify_customer_sentiment",
		Description: "Update a customer's sentiment (local only - sentiment isn't an auth server role).",
	}, ca.modifyCustomerSentiment)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "modify_customer_type",
		Description: "Update a customer's type, syncing the corresponding role change in the auth server.",
	}, ca.modifyCustomerType)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customers",
		Description: "List customers, optionally filtered by id/name/type/sentiment/deactivated (all filters given are ANDed together; no filters returns everyone).",
	}, ca.getCustomers)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customer_type",
		Description: "Look up one customer's type (one-time-customer/returning-customer/vip) by id - a narrow read, unlike get_customers, safe to grant to the customer role itself (used by garden-online-store.purchase_products to determine deal eligibility at checkout).",
	}, ca.getCustomerType)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customers_status",
		Description: "List every customer's type and sentiment - a narrow, contact-detail-free read like get_customer_type, just covering all customers at once. No arguments.",
	}, ca.getCustomersStatus)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customer_details",
		Description: "Look up one customer's contact details (name/address/phone) by id - the only tool in this server that returns them; every other tool here returns a CustomerSummary (id/type/sentiment/deactivated) instead. Intended to be granted only to the roles that can already call add_or_update_customer.",
	}, ca.getCustomerDetails)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_prompts",
		Description: "Bulk-load this server's MCP prompts (prompts.json's shape, filtered to this server's entries). Replaces the current set - a prompt loaded before but not named this time is removed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args LoadPromptsArgs) (*mcp.CallToolResult, LoadResult, error) {
		return nil, LoadResult{Loaded: prompts.Load(args.Prompts)}, nil
	})

	return server
}

func main() {
	var port, authServerURL, authServerUsername, authServerPassword, logLevel string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&authServerURL, "auth-server-url", "http://host.docker.internal:8102",
		"Base URL of the auth server's admin API. Defaults to demo-authorization-server (it has no k8s manifest and runs host-side, hence host.docker.internal) - overridden at deploy time via scripts/deploy.sh's --auth-server-url flag.")
	flag.StringVar(&authServerUsername, "auth-server-username", "admin", "Basic Auth username for the auth server's admin API")
	flag.StringVar(&authServerPassword, "auth-server-password", "admin", "Basic Auth password for the auth server's admin API")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this server has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	// Never log authServerPassword.
	logging.Infof("startup configuration: authServerUrl=%s authServerUsername=%s", authServerURL, authServerUsername)

	authServer := authserver.New(authServerURL, authServerUsername, authServerPassword)
	ca := newCustomerAdmin(authServer)
	server := createServer(ca)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting garden-customer-admin on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
