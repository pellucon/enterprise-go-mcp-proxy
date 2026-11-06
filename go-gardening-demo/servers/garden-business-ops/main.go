// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// garden-business-ops is a go-gardening-demo MCP backend server: it holds
// deals and the customer-type reference list in memory. Its
// update_customer_type tool relays back through the proxy to
// garden-customer-admin.modify_customer_type — the same backend-calls-
// back-into-the-proxy mechanism demonstrated by sample-mcp-servers/
// misc-demo-server's get_my_account_balance (see CLAUDE.md's
// "Internal identity assertions" section), reused unchanged here rather
// than reimplemented.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// dealDateLayout matches Deal.StartDate/EndDate's YYYY-MM-DD shape - see
// garden-online-store/main.go's own dateLayout for the same convention.
const dealDateLayout = "2006-01-02"

type businessOps struct {
	mu            sync.Mutex
	deals         map[string]entities.Deal
	customerTypes map[string]entities.CustomerType // local reference only
}

func newBusinessOps() *businessOps {
	return &businessOps{
		deals:         make(map[string]entities.Deal),
		customerTypes: make(map[string]entities.CustomerType),
	}
}

// nextDealID finds the lowest-numbered unused "deal-NN" id (matching the
// seed data's own 2-digit format, e.g. "deal-01".."deal-04").
func (bo *businessOps) nextDealID() string {
	for n := 1; ; n++ {
		id := fmt.Sprintf("deal-%02d", n)
		if _, exists := bo.deals[id]; !exists {
			return id
		}
	}
}

type LoadResult struct {
	Loaded int `json:"loaded"`
}

type LoadDealsArgs struct {
	Deals []entities.Deal `json:"deals" jsonschema:"The full deals.json list to load."`
}

func (bo *businessOps) loadDeals(ctx context.Context, req *mcp.CallToolRequest, args LoadDealsArgs) (*mcp.CallToolResult, LoadResult, error) {
	bo.mu.Lock()
	defer bo.mu.Unlock()
	for _, d := range args.Deals {
		bo.deals[d.ID] = d
	}
	return nil, LoadResult{Loaded: len(args.Deals)}, nil
}

type LoadCustomerTypesArgs struct {
	CustomerTypes []entities.CustomerType `json:"customerTypes" jsonschema:"The full customer_types.json list to load."`
}

// LoadPromptsArgs is load_prompts' own bulk-load shape - see
// go-gardening-demo/promptstore's own doc comment for why this
// server's whole prompt set lives in data (prompts.json) rather than
// hand-written Go prompt functions.
type LoadPromptsArgs struct {
	Prompts []promptstore.Prompt `json:"prompts" jsonschema:"This server's entries from prompts.json (server field stripped - already implied by which server's load_prompts you're calling)."`
}

// loadCustomerTypes is a local-only reference load.
func (bo *businessOps) loadCustomerTypes(ctx context.Context, req *mcp.CallToolRequest, args LoadCustomerTypesArgs) (*mcp.CallToolResult, LoadResult, error) {
	bo.mu.Lock()
	defer bo.mu.Unlock()
	for _, t := range args.CustomerTypes {
		bo.customerTypes[t.ID] = t
	}
	return nil, LoadResult{Loaded: len(args.CustomerTypes)}, nil
}

type AddOrUpdateDealArgs struct {
	ID        string   `json:"id,omitempty" jsonschema:"Upsert key; auto-generated (deal-NN) if omitted, which always creates a new deal."`
	Name      string   `json:"name"`
	StartDate string   `json:"startDate,omitempty" jsonschema:"YYYY-MM-DD; omit for no lower bound (combined with an omitted endDate, an always-in-effect deal)."`
	EndDate   string   `json:"endDate,omitempty" jsonschema:"YYYY-MM-DD; omit for no upper bound."`
	IDs       []string `json:"ids" jsonschema:"ProductSupply row ids this deal covers (e.g. ps-022), not productIds - a deal can single out one supplier's row."`
	// DiscountPercentages is keyed by customer type id (see entities.Deal's
	// own doc comment) - a type absent from the map isn't eligible for
	// this deal at all, e.g. {"vip": 5} for a VIP-only program. Note this
	// field can't be set via go-gardening-demo/cli's own per-flag
	// convention (see its tools.go's buildArguments) since it's a nested
	// map, not a scalar/list - use the repo's other CLI's generic
	// call-tool -args '{"discountPercentages":{"vip":5}}' instead, or
	// bulk-load via load_deals.
	DiscountPercentages map[string]float64 `json:"discountPercentages"`
}

// addOrUpdateDeal upserts by id, auto-generating one if omitted. Not in
// the original spec's tool list for Business Operations (only "Load deals
// from json list" was) — added because every other entity in this demo
// has a way to create new data, and there's no reason deals should be the
// one exception.
func (bo *businessOps) addOrUpdateDeal(ctx context.Context, req *mcp.CallToolRequest, args AddOrUpdateDealArgs) (*mcp.CallToolResult, entities.Deal, error) {
	bo.mu.Lock()
	defer bo.mu.Unlock()

	id := args.ID
	if id == "" {
		id = bo.nextDealID()
	}
	deal := entities.Deal{
		ID:                  id,
		Name:                args.Name,
		StartDate:           optionalDealDate(args.StartDate),
		EndDate:             optionalDealDate(args.EndDate),
		IDs:                 args.IDs,
		DiscountPercentages: args.DiscountPercentages,
	}
	bo.deals[deal.ID] = deal
	return nil, deal, nil
}

// optionalDealDate turns an empty startDate/endDate arg into entities.Deal's
// own nil-means-unbounded *string convention.
func optionalDealDate(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// vipCustomerTypeID is the one CustomerType id exempt from
// validateDealRequests' no-overlapping-active-deals rule below - matching
// gendata/staticdata.go's own "vip" seed id (garden-business-ops has no
// compile-time dependency on gendata, so this is its own copy of that
// literal rather than an imported constant).
const vipCustomerTypeID = "vip"

// dealCoversNonVIP reports whether deal grants a discount to any customer
// type other than vipCustomerTypeID - a deal whose DiscountPercentages
// names only "vip" (e.g. the always-in-effect VIP Loyalty Discount) can
// never conflict under validateDealRequests' rule.
func dealCoversNonVIP(deal entities.Deal) bool {
	for customerType := range deal.DiscountPercentages {
		if customerType != vipCustomerTypeID {
			return true
		}
	}
	return false
}

// dealsOverlap reports whether two deals' [startDate, endDate] windows
// intersect at all - nil is unbounded on that side (see entities.Deal's
// own doc comment), and plain YYYY-MM-DD string comparison is sufficient
// since that format sorts lexically the same as chronologically (same
// convention as getActiveDeals' own "today" check above).
func dealsOverlap(a, b entities.Deal) bool {
	if a.EndDate != nil && b.StartDate != nil && *a.EndDate < *b.StartDate {
		return false
	}
	if b.EndDate != nil && a.StartDate != nil && *b.EndDate < *a.StartDate {
		return false
	}
	return true
}

// sharesProductSupplyID reports whether a and b cover at least one common
// ProductSupply row id (Deal.IDs - see entities.Deal's own doc comment on
// why this is ProductSupply.ID, not Product.ID).
func sharesProductSupplyID(a, b entities.Deal) bool {
	ids := make(map[string]bool, len(a.IDs))
	for _, id := range a.IDs {
		ids[id] = true
	}
	for _, id := range b.IDs {
		if ids[id] {
			return true
		}
	}
	return false
}

// validateDealRequests is a business-logic-validation triggered tool (see
// CLAUDE.md's "Business logic validation" section) guarding
// add_or_update_deal: rejects a deal that would leave two deals covering
// the same ProductSupply row, with overlapping date windows, both offering
// a discount to some customer type other than VIP - letting that stand
// would make it ambiguous which deal's discount a non-VIP order should
// get. VIP-only overlaps are exempt (dealCoversNonVIP), so e.g. the
// always-in-effect VIP Loyalty Discount can coexist with any other deal on
// the same products. Registered as a triggered tool purely so go-proxy's
// orchestration engine can invoke it as a call-authorization step guarding
// garden-business-ops.add_or_update_deal; never reachable by an ordinary
// client.
func (bo *businessOps) validateDealRequests(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
	// in.CallRequest's real Go type depends on how this tool was invoked -
	// see garden-online-store's validateStoreRequests for why re-marshaling
	// handles both the native (json.RawMessage) and discovered/relayed
	// (map[string]any) cases uniformly.
	raw, err := json.Marshal(in.CallRequest)
	if err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_deal_requests: failed to re-marshal CallRequest: %w", err)
	}
	var call struct {
		Arguments AddOrUpdateDealArgs `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_deal_requests: failed to decode CallRequest: %w", err)
	}

	incoming := entities.Deal{
		ID:                  call.Arguments.ID,
		StartDate:           optionalDealDate(call.Arguments.StartDate),
		EndDate:             optionalDealDate(call.Arguments.EndDate),
		IDs:                 call.Arguments.IDs,
		DiscountPercentages: call.Arguments.DiscountPercentages,
	}
	if !dealCoversNonVIP(incoming) {
		return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	bo.mu.Lock()
	defer bo.mu.Unlock()
	for _, existing := range bo.deals {
		if existing.ID == incoming.ID {
			continue // updating this same deal - not a conflict with itself
		}
		if !dealCoversNonVIP(existing) {
			continue
		}
		if sharesProductSupplyID(incoming, existing) && dealsOverlap(incoming, existing) {
			return nil, triggered_tools_runtime.AuthorizationOutput{
				Authorized: false,
				Reason:     fmt.Sprintf("would overlap existing deal %q (%s) on a shared product for a non-VIP customer type", existing.ID, existing.Name),
			}, nil
		}
	}
	return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
}

// GetDealsResult is get_deals' output - deliberately wrapped in a named
// field (rather than a bare []entities.Deal) so decoding it back out of a
// relayed CallToolResult's StructuredContent has a fixed field name to
// target, the same convention GetActiveDealsResult below follows.
type GetDealsResult struct {
	Deals []entities.Deal `json:"deals"`
}

// getDeals lists every loaded deal, active or not - each carries its own
// startDate/endDate, so a caller that wants that distinction can compute
// it itself rather than relying on get_active_deals below. Read-only, no
// args.
func (bo *businessOps) getDeals(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetDealsResult, error) {
	bo.mu.Lock()
	defer bo.mu.Unlock()

	deals := make([]entities.Deal, 0, len(bo.deals))
	for _, d := range bo.deals {
		deals = append(deals, d)
	}
	return nil, GetDealsResult{Deals: deals}, nil
}

// GetActiveDealsResult is get_active_deals' output - see GetDealsResult's
// own doc comment for why this is a named-field wrapper.
type GetActiveDealsResult struct {
	Deals []entities.Deal `json:"deals"`
}

// getActiveDeals lists every deal whose [startDate, endDate] window covers
// today - plain YYYY-MM-DD string comparison is sufficient (and avoids a
// time.Parse per deal) since that format sorts lexically the same as
// chronologically. A nil StartDate/EndDate means no bound on that side
// (see entities.Deal's own doc comment) - e.g. an always-in-effect,
// VIP-only loyalty deal has both nil and so is always included here.
// Read-only, no args: garden-online-store.get_catalogue is the only
// caller today, relaying back through the proxy (see its own doc comment)
// rather than duplicating deal state locally.
func (bo *businessOps) getActiveDeals(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetActiveDealsResult, error) {
	bo.mu.Lock()
	defer bo.mu.Unlock()

	today := time.Now().Format(dealDateLayout)
	active := make([]entities.Deal, 0, len(bo.deals))
	for _, d := range bo.deals {
		if d.StartDate != nil && today < *d.StartDate {
			continue
		}
		if d.EndDate != nil && today > *d.EndDate {
			continue
		}
		active = append(active, d)
	}
	return nil, GetActiveDealsResult{Deals: active}, nil
}

type UpdateCustomerTypeArgs struct {
	CustomerID string `json:"customerId"`
	Type       string `json:"type"`
}

// updateCustomerType validates Type against the locally-loaded customer
// type reference list (a nicety - catches a typo'd type before relaying
// it as a demo-authorization-server role change), then relays to
// garden-customer-admin.modify_customer_type. No local deal/type state is
// mutated by this call.
func (bo *businessOps) updateCustomerType(ctx context.Context, req *mcp.CallToolRequest, args UpdateCustomerTypeArgs) (*mcp.CallToolResult, any, error) {
	bo.mu.Lock()
	_, knownType := bo.customerTypes[args.Type]
	bo.mu.Unlock()
	if !knownType {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("unknown customer type %q (call load_customer_types first, or check the id)", args.Type)}},
		}, nil, nil
	}

	incoming := relayheaders.Extract(req.GetExtra().Header)
	if incoming.Assertion == "" || incoming.CallbackURL == "" {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "missing identity assertion or callback URL — is internalAuth and selfUrl configured on the proxy?"}},
		}, nil, nil
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "garden-business-ops", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   incoming.CallbackURL,
		HTTPClient: incoming.Client(nil),
	}, nil)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to connect back to proxy at %s: %v", incoming.CallbackURL, err)}},
		}, nil, nil
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "garden-customer-admin.modify_customer_type",
		Arguments: map[string]any{"id": args.CustomerID, "type": args.Type},
	})
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("relayed call failed: %v", err)}},
		}, nil, nil
	}
	return &mcp.CallToolResult{IsError: result.IsError, Content: result.Content, StructuredContent: result.StructuredContent}, nil, nil
}

func createServer(bo *businessOps) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garden-business-ops", Version: "1.0.0"}, nil)
	prompts := promptstore.New(server)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_deals",
		Description: "Bulk-load the deals list (deals.json's shape).",
	}, bo.loadDeals)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_customer_types",
		Description: "Bulk-load the customer type reference list (customer_types.json's shape). Local reference only.",
	}, bo.loadCustomerTypes)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_or_update_deal",
		Description: "Add or update (upsert by id) one deal. Omit id to always create a new one.",
	}, bo.addOrUpdateDeal)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_deal_requests",
		Description: "Business logic validation for add_or_update_deal: reject a deal that would overlap another active deal on a shared product-supply row for a customer type other than VIP. Hidden from ordinary clients - invocable only by go-proxy's orchestration engine as a call-authorization step.",
		Meta:        mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true},
	}, bo.validateDealRequests)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "update_customer_type",
		Description: "Update a customer's type by relaying back through the proxy to garden-customer-admin.",
	}, bo.updateCustomerType)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_deals",
		Description: "List every loaded deal (active or not) with its startDate/endDate window, so the caller can determine which are currently active. Read-only; no arguments.",
	}, bo.getDeals)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_active_deals",
		Description: "List every deal currently active today (its startDate/endDate window covers today). Read-only; no arguments.",
	}, bo.getActiveDeals)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_prompts",
		Description: "Bulk-load this server's MCP prompts (prompts.json's shape, filtered to this server's entries). Replaces the current set - a prompt loaded before but not named this time is removed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args LoadPromptsArgs) (*mcp.CallToolResult, LoadResult, error) {
		return nil, LoadResult{Loaded: prompts.Load(args.Prompts)}, nil
	})

	return server
}

func main() {
	var port, logLevel string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this server has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	bo := newBusinessOps()
	server := createServer(bo)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting garden-business-ops on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
