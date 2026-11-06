// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dealCallRequest builds a CallAuthorizationInput.CallRequest for
// add_or_update_deal the same shape a real MCP wire round-trip produces
// (see garden-online-store's purchaseCallRequest for the same convention).
func dealCallRequest(t *testing.T, args AddOrUpdateDealArgs) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Name      string              `json:"name"`
		Arguments AddOrUpdateDealArgs `json:"arguments"`
	}{
		Name:      "garden-business-ops.add_or_update_deal",
		Arguments: args,
	})
	if err != nil {
		t.Fatalf("failed to build add_or_update_deal CallRequest: %v", err)
	}
	return raw
}

// strPtr is a small helper for entities.Deal's *string StartDate/EndDate
// fields (nil means unbounded - see its own doc comment) in test literals.
func strPtr(s string) *string { return &s }

func TestBusinessOpsLoadDealsAndTypes(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()

	_, res, err := bo.loadDeals(ctx, nil, LoadDealsArgs{Deals: []entities.Deal{
		{ID: "deal-01", Name: "Fall Cleanup Clearance", StartDate: strPtr("2025-10-01"), EndDate: strPtr("2025-10-20"), DiscountPercentages: map[string]float64{"one-time-customer": 20, "returning-customer": 25, "vip": 30}},
	}})
	if err != nil || res.Loaded != 1 {
		t.Fatalf("loadDeals: res=%+v err=%v", res, err)
	}

	_, res, err = bo.loadCustomerTypes(ctx, nil, LoadCustomerTypesArgs{CustomerTypes: []entities.CustomerType{
		{ID: "vip", Name: "VIP"},
		{ID: "one-time-customer", Name: "One-Time Customer"},
	}})
	if err != nil || res.Loaded != 2 {
		t.Fatalf("loadCustomerTypes: res=%+v err=%v", res, err)
	}
	if len(bo.customerTypes) != 2 {
		t.Errorf("expected 2 customer types stored, got %d", len(bo.customerTypes))
	}
}

func TestBusinessOpsAddOrUpdateDeal(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.deals["deal-01"] = entities.Deal{ID: "deal-01", Name: "Existing"}

	// Auto-generated id skips the already-loaded deal-01.
	_, deal, err := bo.addOrUpdateDeal(ctx, nil, AddOrUpdateDealArgs{
		Name: "Spring Planting Sale", StartDate: "2026-03-01", EndDate: "2026-03-21",
		IDs: []string{"ps-038"}, DiscountPercentages: map[string]float64{"one-time-customer": 20},
	})
	if err != nil {
		t.Fatalf("addOrUpdateDeal: %v", err)
	}
	if deal.ID != "deal-02" {
		t.Errorf("expected auto-generated id deal-02, got %q", deal.ID)
	}
	if deal.StartDate == nil || *deal.StartDate != "2026-03-01" {
		t.Errorf("expected StartDate 2026-03-01, got %v", deal.StartDate)
	}

	// Explicit id upserts in place.
	_, updated, err := bo.addOrUpdateDeal(ctx, nil, AddOrUpdateDealArgs{ID: "deal-01", Name: "Renamed", DiscountPercentages: map[string]float64{"vip": 30}})
	if err != nil {
		t.Fatalf("addOrUpdateDeal (explicit id): %v", err)
	}
	if updated.ID != "deal-01" || updated.Name != "Renamed" {
		t.Errorf("expected deal-01 upserted with new name, got %+v", updated)
	}
	if len(bo.deals) != 2 {
		t.Errorf("expected 2 deals total (deal-01 upserted, deal-02 added), got %d", len(bo.deals))
	}

	// Omitted startDate/endDate means unbounded (an always-in-effect deal).
	_, alwaysOn, err := bo.addOrUpdateDeal(ctx, nil, AddOrUpdateDealArgs{
		ID: "deal-vip", Name: "VIP Loyalty Discount", IDs: []string{"ps-001"},
		DiscountPercentages: map[string]float64{"vip": 5},
	})
	if err != nil {
		t.Fatalf("addOrUpdateDeal (always-on): %v", err)
	}
	if alwaysOn.StartDate != nil || alwaysOn.EndDate != nil {
		t.Errorf("expected nil StartDate/EndDate for an omitted-dates deal, got %+v", alwaysOn)
	}
}

func TestBusinessOpsUpdateCustomerTypeRejectsUnknownType(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.customerTypes["vip"] = entities.CustomerType{ID: "vip", Name: "VIP"}

	// No relay headers present either, but the unknown-type check must
	// fire first (a clearer error than "missing identity assertion").
	res, _, err := bo.updateCustomerType(ctx, nil, UpdateCustomerTypeArgs{CustomerID: "cust-0001", Type: "not-a-real-type"})
	if err != nil {
		t.Fatalf("updateCustomerType: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected an error result for an unknown customer type")
	}
}

// TestBusinessOpsGetDeals covers get_deals returning every loaded deal
// unfiltered - past, present, and future - unlike get_active_deals below,
// which filters to today.
func TestBusinessOpsGetDeals(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()

	bo.deals["deal-past"] = entities.Deal{
		ID: "deal-past", Name: "Past", IDs: []string{"ps-004"}, DiscountPercentages: map[string]float64{"one-time-customer": 25},
		StartDate: strPtr("2020-01-01"), EndDate: strPtr("2020-01-20"),
	}
	bo.deals["deal-future"] = entities.Deal{
		ID: "deal-future", Name: "Future", IDs: []string{"ps-007"}, DiscountPercentages: map[string]float64{"one-time-customer": 10},
		StartDate: strPtr("2099-01-01"), EndDate: strPtr("2099-01-20"),
	}

	_, out, err := bo.getDeals(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("getDeals: %v", err)
	}
	if len(out.Deals) != 2 {
		t.Fatalf("expected both deals returned unfiltered, got %d: %+v", len(out.Deals), out.Deals)
	}
	gotIDs := map[string]bool{}
	for _, d := range out.Deals {
		gotIDs[d.ID] = true
	}
	if !gotIDs["deal-past"] || !gotIDs["deal-future"] {
		t.Errorf("expected deal-past and deal-future both present, got %+v", out.Deals)
	}
}

func TestBusinessOpsGetActiveDeals(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()

	today := time.Now()
	fmtDate := func(d time.Time) string { return d.Format(dealDateLayout) }

	bo.deals["deal-active"] = entities.Deal{
		ID: "deal-active", Name: "Active", IDs: []string{"ps-001"}, DiscountPercentages: map[string]float64{"one-time-customer": 15},
		StartDate: strPtr(fmtDate(today.AddDate(0, 0, -1))), EndDate: strPtr(fmtDate(today.AddDate(0, 0, 1))),
	}
	bo.deals["deal-past"] = entities.Deal{
		ID: "deal-past", Name: "Past", IDs: []string{"ps-004"}, DiscountPercentages: map[string]float64{"one-time-customer": 25},
		StartDate: strPtr(fmtDate(today.AddDate(0, 0, -20))), EndDate: strPtr(fmtDate(today.AddDate(0, 0, -10))),
	}
	bo.deals["deal-future"] = entities.Deal{
		ID: "deal-future", Name: "Future", IDs: []string{"ps-007"}, DiscountPercentages: map[string]float64{"one-time-customer": 10},
		StartDate: strPtr(fmtDate(today.AddDate(0, 0, 10))), EndDate: strPtr(fmtDate(today.AddDate(0, 0, 20))),
	}
	bo.deals["deal-today-boundary"] = entities.Deal{
		ID: "deal-today-boundary", Name: "Starts and ends today", IDs: []string{"ps-010"}, DiscountPercentages: map[string]float64{"one-time-customer": 5},
		StartDate: strPtr(fmtDate(today)), EndDate: strPtr(fmtDate(today)),
	}
	bo.deals["deal-always-on"] = entities.Deal{
		ID: "deal-always-on", Name: "VIP Loyalty Discount", IDs: []string{"ps-001"},
		DiscountPercentages: map[string]float64{"vip": 5},
		// StartDate/EndDate deliberately left nil - always in effect.
	}

	_, out, err := bo.getActiveDeals(ctx, nil, struct{}{})
	if err != nil {
		t.Fatalf("getActiveDeals: %v", err)
	}
	if len(out.Deals) != 3 {
		t.Fatalf("expected 3 active deals (deal-active, deal-today-boundary, deal-always-on), got %d: %+v", len(out.Deals), out.Deals)
	}
	gotIDs := map[string]bool{}
	for _, d := range out.Deals {
		gotIDs[d.ID] = true
	}
	if !gotIDs["deal-active"] || !gotIDs["deal-today-boundary"] || !gotIDs["deal-always-on"] {
		t.Errorf("expected deal-active, deal-today-boundary, and deal-always-on to be active, got %+v", out.Deals)
	}
	if gotIDs["deal-past"] || gotIDs["deal-future"] {
		t.Errorf("expected deal-past and deal-future to be excluded, got %+v", out.Deals)
	}
}

// TestBusinessOpsValidateDealRequestsRejectsNonVIPOverlap covers the core
// rule: a new deal covering a product-supply id already covered by another
// deal, with overlapping date windows, that both offer a discount to some
// customer type other than VIP, must be rejected.
func TestBusinessOpsValidateDealRequestsRejectsNonVIPOverlap(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.deals["deal-01"] = entities.Deal{
		ID: "deal-01", Name: "Fall Cleanup Clearance", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"one-time-customer": 20},
		StartDate:           strPtr("2026-03-01"), EndDate: strPtr("2026-03-21"),
	}

	callRequest := dealCallRequest(t, AddOrUpdateDealArgs{
		Name: "Spring Planting Sale", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"returning-customer": 15},
		StartDate:           "2026-03-15", EndDate: "2026-04-01",
	})
	res, out, err := bo.validateDealRequests(ctx, nil, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateDealRequests: unexpected Go error %v", err)
	}
	if res != nil {
		t.Fatalf("expected no CallToolResult (rejection carried via AuthorizationOutput), got %+v", res)
	}
	if out.Authorized {
		t.Fatalf("expected the overlapping non-VIP deal to be rejected, got Authorized=true")
	}
	if out.Reason == "" {
		t.Error("expected a non-empty Reason explaining the rejection")
	}
}

// TestBusinessOpsValidateDealRequestsAllowsVIPOverlap covers the exemption:
// a deal offering a discount only to VIP customers may freely overlap
// another deal on the same product-supply id (e.g. the always-in-effect
// VIP Loyalty Discount coexisting with any seasonal sale).
func TestBusinessOpsValidateDealRequestsAllowsVIPOverlap(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.deals["deal-01"] = entities.Deal{
		ID: "deal-01", Name: "Fall Cleanup Clearance", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"one-time-customer": 20},
		StartDate:           strPtr("2026-03-01"), EndDate: strPtr("2026-03-21"),
	}

	callRequest := dealCallRequest(t, AddOrUpdateDealArgs{
		ID: "deal-vip", Name: "VIP Loyalty Discount", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"vip": 5},
		// StartDate/EndDate omitted - always in effect, overlapping deal-01.
	})
	_, out, err := bo.validateDealRequests(ctx, nil, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateDealRequests: unexpected Go error %v", err)
	}
	if !out.Authorized {
		t.Errorf("expected a VIP-only deal to be exempt from the overlap rule, got Reason=%q", out.Reason)
	}
}

// TestBusinessOpsValidateDealRequestsAllowsDifferentProduct covers that the
// rule is scoped per product-supply id: two non-VIP deals with overlapping
// date windows but no shared id don't conflict.
func TestBusinessOpsValidateDealRequestsAllowsDifferentProduct(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.deals["deal-01"] = entities.Deal{
		ID: "deal-01", Name: "Fall Cleanup Clearance", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"one-time-customer": 20},
		StartDate:           strPtr("2026-03-01"), EndDate: strPtr("2026-03-21"),
	}

	callRequest := dealCallRequest(t, AddOrUpdateDealArgs{
		Name: "Unrelated Product Sale", IDs: []string{"ps-999"},
		DiscountPercentages: map[string]float64{"returning-customer": 15},
		StartDate:           "2026-03-15", EndDate: "2026-04-01",
	})
	_, out, err := bo.validateDealRequests(ctx, nil, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateDealRequests: unexpected Go error %v", err)
	}
	if !out.Authorized {
		t.Errorf("expected no conflict for a disjoint product-supply id, got Reason=%q", out.Reason)
	}
}

// TestBusinessOpsValidateDealRequestsAllowsUpdatingSameDeal covers that
// re-submitting an update to an already-stored deal (same id) is never
// treated as conflicting with itself.
func TestBusinessOpsValidateDealRequestsAllowsUpdatingSameDeal(t *testing.T) {
	ctx := context.Background()
	bo := newBusinessOps()
	bo.deals["deal-01"] = entities.Deal{
		ID: "deal-01", Name: "Fall Cleanup Clearance", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"one-time-customer": 20},
		StartDate:           strPtr("2026-03-01"), EndDate: strPtr("2026-03-21"),
	}

	callRequest := dealCallRequest(t, AddOrUpdateDealArgs{
		ID: "deal-01", Name: "Fall Cleanup Clearance (renamed)", IDs: []string{"ps-038"},
		DiscountPercentages: map[string]float64{"one-time-customer": 25},
		StartDate:           "2026-03-01", EndDate: "2026-03-21",
	})
	_, out, err := bo.validateDealRequests(ctx, nil, triggered_tools_runtime.CallAuthorizationInput{CallRequest: callRequest})
	if err != nil {
		t.Fatalf("validateDealRequests: unexpected Go error %v", err)
	}
	if !out.Authorized {
		t.Errorf("expected updating deal-01 in place to not conflict with itself, got Reason=%q", out.Reason)
	}
}

// connectInProcess wires createServer's real *mcp.Server up to a real
// *mcp.ClientSession over an in-memory transport (mcp.NewInMemoryTransports) -
// the same pattern platform-mcp-servers/event-aggregator's own tests use -
// so load_prompts can be exercised through the actual MCP tools/prompts
// protocol rather than by calling a handler function directly (it's an
// anonymous closure in createServer, not a named method).
func connectInProcess(t *testing.T, bo *businessOps) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := createServer(bo)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("failed to connect in-memory server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("failed to connect in-memory client: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestLoadPromptsRegistersAndServesPrompts covers load_prompts end to end
// through the real MCP protocol: calling it registers prompts that then
// show up in prompts/list and answer prompts/get correctly - the data-driven
// replacement for what used to be hand-written Go prompt functions (see
// go-gardening-demo/promptstore).
func TestLoadPromptsRegistersAndServesPrompts(t *testing.T) {
	ctx := context.Background()
	sess := connectInProcess(t, newBusinessOps())

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "load_prompts",
		Arguments: map[string]any{
			"prompts": []map[string]string{
				{"name": "vip_spend_comparison", "description": "Do VIP customers spend on average more than other customers?", "text": "Do VIP customers spend on average more than other customers?"},
			},
		},
	})
	if err != nil {
		t.Fatalf("load_prompts call failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("load_prompts returned an error: %v", res.Content)
	}

	list, err := sess.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		t.Fatalf("prompts/list: %v", err)
	}
	if len(list.Prompts) != 1 || list.Prompts[0].Name != "vip_spend_comparison" {
		t.Fatalf("expected exactly [vip_spend_comparison], got %+v", list.Prompts)
	}

	got, err := sess.GetPrompt(ctx, &mcp.GetPromptParams{Name: "vip_spend_comparison"})
	if err != nil {
		t.Fatalf("prompts/get: %v", err)
	}
	if len(got.Messages) != 1 {
		t.Fatalf("expected exactly 1 message, got %+v", got.Messages)
	}
	text, ok := got.Messages[0].Content.(*mcp.TextContent)
	if !ok || text.Text != "Do VIP customers spend on average more than other customers?" {
		t.Errorf("expected the loaded prompt's text back verbatim, got %+v", got.Messages[0].Content)
	}
}
