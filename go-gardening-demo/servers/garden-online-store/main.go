// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// garden-online-store is a go-gardening-demo MCP backend server: it holds
// both the store's retail order records and its product supply/restock
// data (formerly garden-inventory, merged into this server since
// purchase_products was already this server's only caller of it) in
// memory, and derives a live per-product stock count from the latter.
// Stock is mutated as supply orders are fulfilled (+) and as purchases are
// made via purchase_products (-) — see the repo's CLAUDE.md and
// go-gardening-demo/README.md for the wider go-gardening-demo feature this
// is part of.
//
// purchase_products used to relay back through the proxy to a separate
// garden-inventory server for its price lookup and stock deduction (see
// the removed sample-mcp-servers/misc-demo-server's get_my_account_balance
// for the relay pattern this used to follow). Now that both are the same
// server, those two steps are plain in-process calls (pricesByProduct/
// deductStock) — no relay needed for either. purchase_products does still
// relay for one thing: determining the caller's deal-eligible discount at
// checkout time (garden-business-ops.get_active_deals +
// garden-customer-admin.get_customer_type — see purchaseProducts' own doc
// comment), the same identity-assertion/callback-URL mechanism
// get_catalogue's fetchActiveDeals already uses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/promptstore"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const dateLayout = "2006-01-02"

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// onlineStore is the server's single mutex-guarded in-memory state: retail
// orders plus the product supply/restock data formerly owned by
// garden-inventory.
type onlineStore struct {
	mu            sync.Mutex
	orders        map[string]entities.OnlineCustomerOrder
	productSupply map[string]entities.ProductSupply // by ProductSupply.ID
	supplyOrders  map[string]entities.SupplyOrder   // by SupplyOrder.ID
	stock         map[string]int                    // productId -> units available
	catalog       map[string]string                 // productId -> product name
}

func newOnlineStore() *onlineStore {
	return &onlineStore{
		orders:        make(map[string]entities.OnlineCustomerOrder),
		productSupply: make(map[string]entities.ProductSupply),
		supplyOrders:  make(map[string]entities.SupplyOrder),
		stock:         make(map[string]int),
		catalog:       make(map[string]string),
	}
}

// nextOrderID finds the lowest-numbered unused "ord-NNNN" id (matching
// the seed data's own 4-digit format, e.g. "ord-0001".."ord-0500"). Only
// ever called from within an already-locked method, matching this
// codebase's established convention for this kind of helper (see
// garden-business-ops' nextDealID/garden-customer-service's nextCallID).
func (store *onlineStore) nextOrderID() string {
	for n := 1; ; n++ {
		id := fmt.Sprintf("ord-%04d", n)
		if _, exists := store.orders[id]; !exists {
			return id
		}
	}
}

// pricesByProduct returns each known product's unit price
// (costToUserPerRetailUnit), sourced from whichever product_supply.json
// row is encountered first for it — every supplier row for the same
// product carries the same price (see go-gardening-demo/README.md), so
// this is safe despite Go's randomized map iteration order. Only ever
// called from within an already-locked method; shared by getInventory and
// purchaseProducts.
func (store *onlineStore) pricesByProduct() map[string]float64 {
	prices := make(map[string]float64, len(store.productSupply))
	for _, ps := range store.productSupply {
		if _, ok := prices[ps.ProductID]; !ok {
			prices[ps.ProductID] = ps.CostToUserPerRetailUnit
		}
	}
	return prices
}

// deductStock lowers a product's stock by unitsSold. It performs no bounds
// check itself and can still be driven negative if called directly (see
// TestOnlineStoreInventoryFulfillmentAndDeduction) — purchaseProducts is
// what enforces sufficient stock, as a separate validation pass before any
// deduction happens, so this low-level helper stays a plain subtract. Only
// ever called from within an
// already-locked method; purchaseProducts' own use is its only caller.
func (store *onlineStore) deductStock(productID string, unitsSold int) {
	store.stock[productID] -= unitsSold
}

type LoadResult struct {
	Loaded int `json:"loaded"`
}

type LoadCustomerOrdersArgs struct {
	Orders []entities.OnlineCustomerOrder `json:"orders" jsonschema:"The full online_customer_orders.json list to load."`
}

// LoadPromptsArgs is load_prompts' own bulk-load shape - see
// go-gardening-demo/promptstore's own doc comment for why this
// server's whole prompt set lives in data (prompts.json) rather than
// hand-written Go prompt functions.
type LoadPromptsArgs struct {
	Prompts []promptstore.Prompt `json:"prompts" jsonschema:"This server's entries from prompts.json (server field stripped - already implied by which server's load_prompts you're calling)."`
}

// loadCustomerOrders is local-only — this server's own copy of order
// history. Does not retroactively touch the stock baseline (see
// load_historical_purchases for that, loaded independently by cli init
// from the same file). Named load_customer_orders, not load_orders, to
// stay unambiguous against this server's own supply-order tools
// (load_supply_orders/add_supply_order/get_supply_orders) - see
// get_customer_orders below for the same reasoning on the read side.
func (store *onlineStore) loadCustomerOrders(ctx context.Context, req *mcp.CallToolRequest, args LoadCustomerOrdersArgs) (*mcp.CallToolResult, LoadResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, o := range args.Orders {
		store.orders[o.ID] = o
	}
	return nil, LoadResult{Loaded: len(args.Orders)}, nil
}

type PurchaseItem struct {
	ProductID string `json:"productId"`
	Units     int    `json:"units"`
}

type PurchaseProductsArgs struct {
	CustomerID string         `json:"customerId"`
	OrderDate  string         `json:"orderDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
	Items      []PurchaseItem `json:"items"`
}

// purchaseProducts looks up each item's real catalog price (the caller
// never supplies one), applies whichever currently-active deal the
// purchasing customer is eligible for (see matchedDiscount), and deducts
// stock, in-process — pricesByProduct/deductStock, formerly two relayed
// calls to a separate garden-inventory server (see this file's own doc
// comment). One order is created per item. Out is declared any, not
// []entities.OnlineCustomerOrder: this handler manually constructs error
// *mcp.CallToolResults — a concrete slice/struct Out type still gets
// serialized and appended as a spurious extra "null" content block by
// mcp.AddTool even on the error path, since a nil slice isn't the same as
// a true absence of a value the way a nil `any` is.
//
// Deal eligibility requires knowing the purchasing customer's type,
// fetched via a relay to garden-customer-admin.get_customer_type (see
// fetchCustomerType) alongside the existing relay to
// garden-business-ops.get_active_deals (fetchActiveDeals) - both read-only
// backend calls, done before applyPurchase (which takes store.mu) is ever
// called, so a slow/stuck relay never blocks other tool calls needing the
// lock. Either relay failing fails the whole purchase (IsError) rather
// than silently falling back to full price or guessing - same "fail
// closed" choice get_catalogue's own fetchActiveDeals call already makes,
// now extended to an operation that actually charges money, not just
// displays a price. The actual pricing/stock-mutation logic (including
// its own atomic stock-sufficiency pass) lives in applyPurchase below.
func (store *onlineStore) purchaseProducts(ctx context.Context, req *mcp.CallToolRequest, args PurchaseProductsArgs) (*mcp.CallToolResult, any, error) {
	orderDate := args.OrderDate
	if orderDate == "" {
		orderDate = time.Now().Format(dateLayout)
	}
	parsedOrderDate, err := time.Parse(dateLayout, orderDate)
	if err != nil {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("invalid orderDate %q: %v", orderDate, err)}},
		}, nil, nil
	}
	targetDate := parsedOrderDate.AddDate(0, 0, 7).Format(dateLayout) // documented default: 7 days out

	activeDeals, errResult := fetchActiveDeals(ctx, req)
	if errResult != nil {
		return errResult, nil, nil
	}
	customerType, errResult := fetchCustomerType(ctx, req, args.CustomerID)
	if errResult != nil {
		return errResult, nil, nil
	}

	errResult, created := store.applyPurchase(args, orderDate, targetDate, activeDeals, customerType)
	if errResult != nil {
		return errResult, nil, nil
	}
	return nil, created, nil
}

// applyPurchase is purchaseProducts' pure pricing/stock-mutation core,
// split out from the relay-fetching handler above for the same reason
// getCatalogue/buildCatalogue already are (see buildCatalogue's own doc
// comment): directly unit-testable with a hand-built activeDeals/
// customerType, without needing a live proxy relay. Takes store.mu itself
// (caller must not already hold it).
//
// Stock sufficiency is checked in its own pass, before any item is
// deducted or any order created: item units for the same productId are
// summed first (a single call can list the same product more than once),
// then checked against current stock, so the whole call is rejected
// atomically rather than partially applied when one line item runs out.
func (store *onlineStore) applyPurchase(args PurchaseProductsArgs, orderDate, targetDate string, activeDeals []entities.Deal, customerType string) (*mcp.CallToolResult, []entities.OnlineCustomerOrder) {
	store.mu.Lock()
	defer store.mu.Unlock()

	prices := store.pricesByProduct()
	productSupplyProductID := store.productSupplyProductIDIndex()

	if ok, reason := purchaseFeasible(args.Items, prices, store.stock); !ok {
		return &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: reason}},
		}, nil
	}

	created := make([]entities.OnlineCustomerOrder, 0, len(args.Items))
	for _, item := range args.Items {
		basePrice := prices[item.ProductID]
		unitPrice, dealID := priceItem(basePrice, activeDeals, productSupplyProductID, item.ProductID, customerType)
		store.deductStock(item.ProductID, item.Units)

		order := entities.OnlineCustomerOrder{
			ID:                    store.nextOrderID(),
			CustomerID:            args.CustomerID,
			ProductID:             item.ProductID,
			OrderDate:             orderDate,
			FulfillmentTargetDate: targetDate,
			FulfillmentActualDate: nil,
			NumberOfRetailUnits:   item.Units,
			TotalPrice:            round2(unitPrice * float64(item.Units)),
			DealID:                dealID,
		}
		store.orders[order.ID] = order
		created = append(created, order)
	}

	return nil, created
}

// purchaseFeasible reports whether every item in items could actually be
// purchased right now - a known price and sufficient stock (item units for
// the same productId are summed first, since a single call can list the
// same product more than once) - without mutating anything. Shared,
// side-effect-free core of applyPurchase's own atomic pre-check, reused by
// computePurchaseTotal (validate_store_requests' dry-run) so the two can
// never disagree about which purchases would actually succeed. Caller must
// hold store.mu.
func purchaseFeasible(items []PurchaseItem, prices map[string]float64, stock map[string]int) (ok bool, reason string) {
	requestedUnits := make(map[string]int, len(items))
	for _, item := range items {
		if _, known := prices[item.ProductID]; !known {
			return false, fmt.Sprintf("could not determine a price for unknown product %q", item.ProductID)
		}
		requestedUnits[item.ProductID] += item.Units
	}
	for productID, units := range requestedUnits {
		if available := stock[productID]; units > available {
			return false, fmt.Sprintf("insufficient inventory for product %q: requested %d, only %d in stock", productID, units, available)
		}
	}
	return true, ""
}

// priceItem computes one item's per-unit price given the customer's type
// and currently-known active deals, applying the first eligible deal's
// discount (see matchedDiscount) - shared, side-effect-free pricing logic
// used by both applyPurchase (which actually charges it) and
// computePurchaseTotal (which only estimates it, for validate_store_requests'
// no-side-effects dry run), so the two can never drift apart.
func priceItem(basePrice float64, activeDeals []entities.Deal, productSupplyProductID map[string]string, productID, customerType string) (unitPrice float64, dealID *string) {
	unitPrice = basePrice
	if id, discount, ok := matchedDiscount(activeDeals, productSupplyProductID, productID, customerType); ok {
		unitPrice = round2(basePrice * (1 - discount/100))
		dealIDCopy := id
		dealID = &dealIDCopy
	}
	return unitPrice, dealID
}

// matchedDiscount returns the id and discount percentage of the first
// deal in activeDeals covering productID that customerType is eligible
// for (see entities.Deal.DiscountFor - a type absent from a deal's own
// DiscountPercentages simply isn't eligible for it) - "first configured
// deal wins", mirroring go-gardening-demo/gendata's own activeDeal helper
// (deliberately kept consistent with it, since that's what decided
// historical seed-data pricing). activeDeals is assumed already-filtered
// to currently-active ones - garden-business-ops.get_active_deals' own
// job, the same trust boundary buildCatalogue already relies on.
func matchedDiscount(activeDeals []entities.Deal, productSupplyProductID map[string]string, productID, customerType string) (dealID string, percentage float64, ok bool) {
	for _, d := range activeDeals {
		discount, eligible := d.DiscountFor(customerType)
		if !eligible {
			continue
		}
		for _, supplyID := range d.IDs {
			if productSupplyProductID[supplyID] == productID {
				return d.ID, discount, true
			}
		}
	}
	return "", 0, false
}

// productSupplyProductIDIndex maps each ProductSupply row id to its
// ProductID - the reverse of the per-row shape store.productSupply is
// already keyed by, needed to resolve a deal's row-id list (see
// entities.Deal's own doc comment) back to which product it covers.
// Caller must hold store.mu.
func (store *onlineStore) productSupplyProductIDIndex() map[string]string {
	m := make(map[string]string, len(store.productSupply))
	for id, ps := range store.productSupply {
		m[id] = ps.ProductID
	}
	return m
}

// fetchCustomerType relays garden-customer-admin.get_customer_type back
// through the proxy, presenting the same identity assertion/callback URL
// this call itself arrived with - same relay mechanism/reasoning as
// fetchActiveDeals below, just against a different backend. get_customer_type
// is a deliberately narrow lookup (id -> type only, not the full customer
// record) specifically so granting the "customer" role access to it (see
// go-gardening-demo's client_access_rules.json) doesn't expose other
// customers' contact details the way granting get_customers itself would.
func fetchCustomerType(ctx context.Context, req *mcp.CallToolRequest, customerID string) (string, *mcp.CallToolResult) {
	incoming := relayheaders.Extract(req.GetExtra().Header)
	if incoming.Assertion == "" || incoming.CallbackURL == "" {
		return "", &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "missing identity assertion or callback URL — is internalAuth and selfUrl configured on the proxy?"}},
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "garden-online-store", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   incoming.CallbackURL,
		HTTPClient: incoming.Client(nil),
	}, nil)
	if err != nil {
		return "", &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to connect back to proxy at %s: %v", incoming.CallbackURL, err)}},
		}
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "garden-customer-admin.get_customer_type",
		Arguments: map[string]any{"id": customerID},
	})
	if err != nil {
		return "", &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("relayed call to garden-customer-admin.get_customer_type failed: %v", err)}},
		}
	}
	if result.IsError {
		return "", &mcp.CallToolResult{IsError: true, Content: result.Content}
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return "", &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to re-marshal get_customer_type's StructuredContent: %v", err)}},
		}
	}
	var out struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to decode get_customer_type output: %v", err)}},
		}
	}
	return out.Type, nil
}

type PurchaseFulfilledArgs struct {
	OrderID               string `json:"orderId"`
	FulfillmentActualDate string `json:"fulfillmentActualDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
}

// purchaseFulfilled does not touch stock — it was already deducted at
// purchase time (see purchaseProducts), not fulfillment time, an
// intentional asymmetry with mark_supply_order_fulfilled (which does add
// to stock at fulfillment time) mirroring the spec's own wording:
// purchases affect inventory when bought, supply orders affect it when
// fulfilled.
func (store *onlineStore) purchaseFulfilled(ctx context.Context, req *mcp.CallToolRequest, args PurchaseFulfilledArgs) (*mcp.CallToolResult, entities.OnlineCustomerOrder, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	order, ok := store.orders[args.OrderID]
	if !ok {
		return nil, entities.OnlineCustomerOrder{}, fmt.Errorf("purchase_fulfilled: unknown order %q", args.OrderID)
	}
	date := args.FulfillmentActualDate
	if date == "" {
		date = time.Now().Format(dateLayout)
	}
	order.FulfillmentActualDate = &date
	store.orders[order.ID] = order
	return nil, order, nil
}

type LoadProductSupplyArgs struct {
	ProductSupply []entities.ProductSupply `json:"productSupply" jsonschema:"The full product_supply.json list to load."`
}

// loadProductSupply populates the product catalog. Does not itself change
// stock levels — only load_supply_orders/mark_supply_order_fulfilled do.
func (store *onlineStore) loadProductSupply(ctx context.Context, req *mcp.CallToolRequest, args LoadProductSupplyArgs) (*mcp.CallToolResult, LoadResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, ps := range args.ProductSupply {
		store.productSupply[ps.ID] = ps
		store.catalog[ps.ProductID] = ps.Name
	}
	return nil, LoadResult{Loaded: len(args.ProductSupply)}, nil
}

type LoadSupplyOrdersArgs struct {
	SupplyOrders []entities.SupplyOrder `json:"supplyOrders" jsonschema:"The full supply_orders.json list to load. load_product_supply must have been called first, since resolving each order's wholesale-to-retail-unit conversion requires it."`
}

// loadSupplyOrders requires every referenced ProductSupplyID to already be
// known (validated up front, all-or-nothing — matches the bulk-load
// convention used across every gardening-demo server) since it needs each
// order's UnitsPerWholesaleUnit to convert wholesale units into the retail
// units stock is tracked in. Each already-fulfilled entry adds to the
// stock baseline.
func (store *onlineStore) loadSupplyOrders(ctx context.Context, req *mcp.CallToolRequest, args LoadSupplyOrdersArgs) (*mcp.CallToolResult, LoadResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	for _, so := range args.SupplyOrders {
		if _, ok := store.productSupply[so.ProductSupplyID]; !ok {
			return nil, LoadResult{}, fmt.Errorf("load_supply_orders: unknown productSupplyId %q (call load_product_supply first)", so.ProductSupplyID)
		}
	}

	for _, so := range args.SupplyOrders {
		store.supplyOrders[so.ID] = so
		if so.FulfillmentActualDate != nil {
			ps := store.productSupply[so.ProductSupplyID]
			store.stock[ps.ProductID] += so.NumberOfOrderedUnits * ps.UnitsPerWholesaleUnit
		}
	}
	return nil, LoadResult{Loaded: len(args.SupplyOrders)}, nil
}

type LoadHistoricalPurchasesArgs struct {
	Orders []entities.OnlineCustomerOrder `json:"orders" jsonschema:"The full online_customer_orders.json list. Only already-fulfilled entries are netted out of the stock baseline, so get_inventory reflects what's actually in stock right now (net of everything ever sold), not just everything ever supplied. Does not itself repopulate this server's own order records - that's load_customer_orders' job, on the same file."`
}

func (store *onlineStore) loadHistoricalPurchases(ctx context.Context, req *mcp.CallToolRequest, args LoadHistoricalPurchasesArgs) (*mcp.CallToolResult, LoadResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	deducted := 0
	for _, o := range args.Orders {
		if o.FulfillmentActualDate == nil {
			continue
		}
		store.stock[o.ProductID] -= o.NumberOfRetailUnits
		deducted++
	}
	return nil, LoadResult{Loaded: deducted}, nil
}

// computePurchaseTotal is applyPurchase's pure pricing core, minus the
// mutation - it never calls deductStock or writes store.orders, and takes
// activeDeals/customerType as already-fetched inputs, the same shape
// applyPurchase itself takes them in. Returns 0 if the purchase wouldn't
// actually succeed (unknown product, insufficient stock - see
// purchaseFeasible), per validate_store_requests' own contract: a failing
// purchase can't overspend, so it should never be blocked on cost grounds.
func (store *onlineStore) computePurchaseTotal(items []PurchaseItem, activeDeals []entities.Deal, customerType string) float64 {
	store.mu.Lock()
	defer store.mu.Unlock()

	prices := store.pricesByProduct()
	if ok, _ := purchaseFeasible(items, prices, store.stock); !ok {
		return 0
	}

	productSupplyProductID := store.productSupplyProductIDIndex()

	var total float64
	for _, item := range items {
		unitPrice, _ := priceItem(prices[item.ProductID], activeDeals, productSupplyProductID, item.ProductID, customerType)
		total += unitPrice * float64(item.Units)
	}
	return round2(total)
}

// ValidateStoreRequestsParameters is validate_store_requests' own
// OrchestrationStep.Parameters shape - the orchestration-config-declared
// maximum total purchase cost. MaxTotalCost <= 0 (including an absent/
// empty Parameters string, which decodes to the zero value) means no
// limit is configured, so the call is always authorized - the same
// "nothing configured means nothing to check" convention
// constraintsValidationParameters.DeclarationNames uses in go-proxy's own
// constraints_validation.go.
type ValidateStoreRequestsParameters struct {
	MaxTotalCost float64 `json:"maxTotalCost"`
}

// validateStoreRequests is a business-logic-validation triggered tool (see
// CLAUDE.md's "Triggered tools / orchestration" for the general
// mechanism): unlike go-proxy's own generic, tool-agnostic
// request_constraints_validation/result_ai_constraints_validation, this is
// hand-written Go code specific to purchase_products' own semantics,
// re-running its real pricing logic (fetchActiveDeals/fetchCustomerType/
// priceItem/purchaseFeasible - the exact same helpers purchaseProducts/
// applyPurchase themselves use) to compute what a purchase would cost,
// with no side effects - no stock deduction, no order creation. Registered
// as a triggered tool (see createServer's Meta annotation) purely so
// go-proxy's orchestration engine can invoke it as a call-authorization
// step guarding garden-online-store.purchase_products; never reachable by
// an ordinary client.
//
// A discovered triggered tool now carries the same relay headers (identity
// assertion/callback URL) an ordinary backend call already does - see
// triggered-tools-runtime/runtime.go's platformHTTPClient and
// CLAUDE.md's "Internal identity assertions" section - so this
// calls fetchActiveDeals/fetchCustomerType directly, unchanged, exactly as
// purchaseProducts itself does, rather than deriving an approximation from
// in.Roles or a local reference copy of deal data.
func (store *onlineStore) validateStoreRequests(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
	var params ValidateStoreRequestsParameters
	if in.Parameters != "" {
		if err := json.Unmarshal([]byte(in.Parameters), &params); err != nil {
			return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_store_requests: invalid Parameters: %w", err)
		}
	}

	// in.CallRequest's real Go type depends on how this tool was invoked:
	// json.RawMessage when called in-process (a native step, or this
	// file's own unit tests calling validateStoreRequests directly), but
	// map[string]any once it's gone over a real MCP wire round-trip as a
	// discovered tool's typed argument (encoding/json decodes a JSON
	// object into an untyped `any` field as a map, not raw bytes) - which
	// is how this tool is actually invoked in production, whether directly
	// as an orchestration step or via proxy_utility.business_logic_
	// constraints_validation's own relayed call. re-marshaling handles both
	// uniformly rather than assuming one specific concrete type.
	raw, err := json.Marshal(in.CallRequest)
	if err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_store_requests: failed to re-marshal CallRequest: %w", err)
	}
	var call struct {
		Arguments PurchaseProductsArgs `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &call); err != nil {
		return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_store_requests: failed to decode CallRequest: %w", err)
	}

	activeDeals, errResult := fetchActiveDeals(ctx, req)
	if errResult != nil {
		return errResult, triggered_tools_runtime.AuthorizationOutput{}, nil
	}
	customerType, errResult := fetchCustomerType(ctx, req, call.Arguments.CustomerID)
	if errResult != nil {
		return errResult, triggered_tools_runtime.AuthorizationOutput{}, nil
	}

	total := store.computePurchaseTotal(call.Arguments.Items, activeDeals, customerType)

	if params.MaxTotalCost > 0 && total > params.MaxTotalCost {
		return nil, triggered_tools_runtime.AuthorizationOutput{
			Authorized: false,
			Reason:     fmt.Sprintf("purchase total %.2f exceeds the configured maximum of %.2f", total, params.MaxTotalCost),
		}, nil
	}
	return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
}

type AddProductSupplyArgs struct {
	ID                      string  `json:"id" jsonschema:"Unique id for this supplier row, e.g. ps-051."`
	ProductID               string  `json:"productId" jsonschema:"Canonical product id, e.g. prod-01."`
	Name                    string  `json:"name" jsonschema:"Product name."`
	SupplierName            string  `json:"supplierName"`
	UnitsPerWholesaleUnit   int     `json:"unitsPerWholesaleUnit"`
	CostPerWholesaleUnit    float64 `json:"costPerWholesaleUnit"`
	CostToUserPerRetailUnit float64 `json:"costToUserPerRetailUnit"`
}

// addProductSupply upserts by id — an existing id's row is replaced
// entirely, matching Customer/Staff Admin's "add or update" convention.
// Does not itself change stock.
func (store *onlineStore) addProductSupply(ctx context.Context, req *mcp.CallToolRequest, args AddProductSupplyArgs) (*mcp.CallToolResult, entities.ProductSupply, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	ps := entities.ProductSupply{
		ID:                      args.ID,
		ProductID:               args.ProductID,
		Name:                    args.Name,
		SupplierName:            args.SupplierName,
		UnitsPerWholesaleUnit:   args.UnitsPerWholesaleUnit,
		CostPerWholesaleUnit:    args.CostPerWholesaleUnit,
		CostToUserPerRetailUnit: args.CostToUserPerRetailUnit,
	}
	store.productSupply[ps.ID] = ps
	store.catalog[ps.ProductID] = ps.Name
	return nil, ps, nil
}

type AddSupplyOrderArgs struct {
	ID                    string `json:"id"`
	ProductSupplyID       string `json:"productSupplyId"`
	OrderDate             string `json:"orderDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
	FulfillmentTargetDate string `json:"fulfillmentTargetDate,omitempty" jsonschema:"Defaults to 14 days after orderDate if omitted."`
	NumberOfOrderedUnits  int    `json:"numberOfOrderedUnits"`
}

// addSupplyOrder upserts by id, created unfulfilled — no stock change
// until mark_supply_order_fulfilled is called on it.
func (store *onlineStore) addSupplyOrder(ctx context.Context, req *mcp.CallToolRequest, args AddSupplyOrderArgs) (*mcp.CallToolResult, entities.SupplyOrder, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	ps, ok := store.productSupply[args.ProductSupplyID]
	if !ok {
		return nil, entities.SupplyOrder{}, fmt.Errorf("add_supply_order: unknown productSupplyId %q", args.ProductSupplyID)
	}

	orderDate := args.OrderDate
	if orderDate == "" {
		orderDate = time.Now().Format(dateLayout)
	}
	parsedOrderDate, err := time.Parse(dateLayout, orderDate)
	if err != nil {
		return nil, entities.SupplyOrder{}, fmt.Errorf("add_supply_order: invalid orderDate %q: %w", orderDate, err)
	}
	targetDate := args.FulfillmentTargetDate
	if targetDate == "" {
		targetDate = parsedOrderDate.AddDate(0, 0, 14).Format(dateLayout)
	}

	so := entities.SupplyOrder{
		ID:                    args.ID,
		ProductSupplyID:       args.ProductSupplyID,
		OrderDate:             orderDate,
		FulfillmentTargetDate: targetDate,
		FulfillmentActualDate: nil,
		NumberOfOrderedUnits:  args.NumberOfOrderedUnits,
		TotalCost:             round2(float64(args.NumberOfOrderedUnits) * ps.CostPerWholesaleUnit),
	}
	store.supplyOrders[so.ID] = so
	return nil, so, nil
}

type MarkSupplyOrderFulfilledArgs struct {
	ID                    string `json:"id"`
	FulfillmentActualDate string `json:"fulfillmentActualDate,omitempty" jsonschema:"Defaults to today (YYYY-MM-DD) if omitted."`
}

func (store *onlineStore) markSupplyOrderFulfilled(ctx context.Context, req *mcp.CallToolRequest, args MarkSupplyOrderFulfilledArgs) (*mcp.CallToolResult, entities.SupplyOrder, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	so, ok := store.supplyOrders[args.ID]
	if !ok {
		return nil, entities.SupplyOrder{}, fmt.Errorf("mark_supply_order_fulfilled: unknown supply order %q", args.ID)
	}
	if so.FulfillmentActualDate != nil {
		return nil, entities.SupplyOrder{}, fmt.Errorf("mark_supply_order_fulfilled: supply order %q is already fulfilled", args.ID)
	}
	ps, ok := store.productSupply[so.ProductSupplyID]
	if !ok {
		return nil, entities.SupplyOrder{}, fmt.Errorf("mark_supply_order_fulfilled: supply order %q references unknown productSupplyId %q", args.ID, so.ProductSupplyID)
	}

	actualDate := args.FulfillmentActualDate
	if actualDate == "" {
		actualDate = time.Now().Format(dateLayout)
	}
	so.FulfillmentActualDate = &actualDate
	so.TotalCost = round2(float64(so.NumberOfOrderedUnits) * ps.CostPerWholesaleUnit)
	store.supplyOrders[so.ID] = so
	store.stock[ps.ProductID] += so.NumberOfOrderedUnits * ps.UnitsPerWholesaleUnit

	return nil, so, nil
}

type GetInventoryArgs struct {
	ProductID string `json:"productId,omitempty" jsonschema:"Optional - filter to one product. Omit to list every known product."`
}

// InventoryItem is get_inventory's per-product output. UnitPrice
// (costToUserPerRetailUnit) is included specifically so this tool doubles
// as purchase_products' own price lookup — no separate price-lookup tool
// needed.
type InventoryItem struct {
	ProductID      string  `json:"productId"`
	Name           string  `json:"name"`
	UnitsAvailable int     `json:"unitsAvailable"`
	UnitPrice      float64 `json:"unitPrice"`
}

// GetInventoryResult wraps get_inventory's list in a named field rather
// than returning a bare array: some MCP clients (e.g. Claude Desktop)
// require every tool's inferred outputSchema to be a JSON object at the
// top level, and a bare []InventoryItem Out infers as an array instead.
type GetInventoryResult struct {
	Items []InventoryItem `json:"items"`
}

func (store *onlineStore) getInventory(ctx context.Context, req *mcp.CallToolRequest, args GetInventoryArgs) (*mcp.CallToolResult, GetInventoryResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	prices := store.pricesByProduct()

	if args.ProductID != "" {
		name, known := store.catalog[args.ProductID]
		if !known {
			return nil, GetInventoryResult{}, fmt.Errorf("get_inventory: unknown productId %q", args.ProductID)
		}
		return nil, GetInventoryResult{Items: []InventoryItem{{
			ProductID:      args.ProductID,
			Name:           name,
			UnitsAvailable: store.stock[args.ProductID],
			UnitPrice:      prices[args.ProductID],
		}}}, nil
	}

	items := make([]InventoryItem, 0, len(store.catalog))
	for pid, name := range store.catalog {
		items = append(items, InventoryItem{
			ProductID:      pid,
			Name:           name,
			UnitsAvailable: store.stock[pid],
			UnitPrice:      prices[pid],
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ProductID < items[j].ProductID })
	return nil, GetInventoryResult{Items: items}, nil
}

type GetCatalogueArgs struct {
	ProductID    string `json:"productId,omitempty" jsonschema:"Optional - exact match on productId."`
	ID           string `json:"id,omitempty" jsonschema:"Optional - exact match on the product-supply row's own id (e.g. ps-001)."`
	Name         string `json:"name,omitempty" jsonschema:"Optional - case-insensitive substring match on the product name. No wildcards."`
	SupplierName string `json:"supplierName,omitempty" jsonschema:"Optional - case-insensitive substring match on supplierName. No wildcards."`
}

// CatalogueItem is get_catalogue's per-product-supply-row output.
// UnitsAvailable is declared any, not int: it's the live stock count, or
// the literal string "Not Available" once that count reaches zero (or
// below - see purchaseProducts' own atomic stock check, which should keep
// this from happening via the normal purchase path, but deductStock
// itself still allows negative when called directly - see its own doc
// comment). An any field marshals through as whichever concrete type is
// assigned and infers as an unconstrained (rather than wrongly-typed)
// output schema - the same trick CallRequest/CallResult and others use
// elsewhere in this repo (see CLAUDE.md's own note on this).
type CatalogueItem struct {
	ID        string `json:"id"`
	ProductID string `json:"productId"`
	Name      string `json:"name"`
	HasDeal   bool   `json:"hasDeal"`
	// DealPercentages is keyed by customer type id (see entities.Deal's
	// own doc comment) - a type absent from the map isn't eligible for
	// this row's deal at all, e.g. a VIP-only loyalty deal shows only a
	// "vip" entry. get_catalogue takes no customer identity, so this
	// shows every type's own rate rather than picking one.
	DealPercentages map[string]float64 `json:"dealPercentages"`
	UnitsAvailable  any                `json:"unitsAvailable"`
	// CostToUserPerRetailUnit is this row's own entities.ProductSupply
	// field, copied through verbatim - the catalog (pre-deal) retail price,
	// not what a given customer would actually pay after HasDeal/
	// DealPercentages are applied (that's priceItem's job at purchase time).
	CostToUserPerRetailUnit float64 `json:"costToUserPerRetailUnit"`
}

// fetchActiveDeals relays garden-business-ops.get_active_deals back
// through the proxy, presenting the same identity assertion/callback URL
// this call itself arrived with - garden-online-store has no other way to
// reach garden-business-ops, since only the proxy is network-reachable
// from a backend (see CLAUDE.md's "Internal identity assertions"
// trust boundary). Mirrors garden-business-ops.updateCustomerType's own
// relay call exactly, just in the other direction between these two
// servers. Returns only deals already-active today - garden-business-ops
// decides that, not this caller (see garden-business-ops.getActiveDeals).
func fetchActiveDeals(ctx context.Context, req *mcp.CallToolRequest) ([]entities.Deal, *mcp.CallToolResult) {
	incoming := relayheaders.Extract(req.GetExtra().Header)
	if incoming.Assertion == "" || incoming.CallbackURL == "" {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: "missing identity assertion or callback URL — is internalAuth and selfUrl configured on the proxy?"}},
		}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "garden-online-store", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   incoming.CallbackURL,
		HTTPClient: incoming.Client(nil),
	}, nil)
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to connect back to proxy at %s: %v", incoming.CallbackURL, err)}},
		}
	}
	defer session.Close()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "garden-business-ops.get_active_deals"})
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("relayed call to garden-business-ops.get_active_deals failed: %v", err)}},
		}
	}
	if result.IsError {
		return nil, &mcp.CallToolResult{IsError: true, Content: result.Content}
	}

	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to re-marshal get_active_deals' StructuredContent: %v", err)}},
		}
	}
	var out struct {
		Deals []entities.Deal `json:"deals"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &mcp.CallToolResult{
			IsError: true,
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("failed to decode get_active_deals output: %v", err)}},
		}
	}
	return out.Deals, nil
}

// getCatalogue lists product-supply rows (one per supplier per product -
// unlike getInventory, which is deduped to one entry per product), each
// annotated with whether its product is covered by a currently active
// deal and how many retail units remain. productId/id are exact-match
// filters; name/supplierName are case-insensitive substring filters (no
// glob/wildcard support, unlike access_control.GlobMatch elsewhere in this
// repo) - every given filter is ANDed together, the same convention
// event-aggregator's list_events already uses. The relay call
// (fetchActiveDeals) and the filtering/annotation logic (buildCatalogue)
// are kept separate specifically so the latter is directly unit-testable
// with a hand-built deals list, without needing a live proxy relay.
// GetCatalogueResult wraps get_catalogue's list in a named field rather
// than returning a bare array: some MCP clients (e.g. Claude Desktop)
// require every tool's inferred outputSchema to be a JSON object at the
// top level, and a bare []CatalogueItem Out infers as an array instead.
type GetCatalogueResult struct {
	Items []CatalogueItem `json:"items"`
}

func (store *onlineStore) getCatalogue(ctx context.Context, req *mcp.CallToolRequest, args GetCatalogueArgs) (*mcp.CallToolResult, GetCatalogueResult, error) {
	activeDeals, errResult := fetchActiveDeals(ctx, req)
	if errResult != nil {
		return errResult, GetCatalogueResult{}, nil
	}
	return nil, GetCatalogueResult{Items: store.buildCatalogue(args, activeDeals)}, nil
}

// buildCatalogue applies get_catalogue's filters/deal-annotation over the
// current product supply and stock, given an already-fetched active-deals
// list (garden-business-ops.get_active_deals has already filtered to
// today - this function trusts that and doesn't re-check dates). A deal's
// IDs name specific ProductSupply row ids (e.g. "ps-022"), not productIds
// - see entities.Deal's own doc comment - so matching is keyed by each
// row's own ID, not its ProductID: two supplier rows for the same product
// can show different hasDeal/dealPercentage if a deal names only one of
// them. See getCatalogue's own doc comment for why the relay call is kept
// separate from this function.
func (store *onlineStore) buildCatalogue(args GetCatalogueArgs, activeDeals []entities.Deal) []CatalogueItem {
	dealPercentagesBySupplyID := make(map[string]map[string]float64, len(activeDeals))
	for _, deal := range activeDeals {
		for _, id := range deal.IDs {
			if _, exists := dealPercentagesBySupplyID[id]; !exists {
				dealPercentagesBySupplyID[id] = deal.DiscountPercentages
			}
		}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	nameQuery := strings.ToLower(args.Name)
	supplierQuery := strings.ToLower(args.SupplierName)

	items := make([]CatalogueItem, 0, len(store.productSupply))
	for _, ps := range store.productSupply {
		if args.ProductID != "" && ps.ProductID != args.ProductID {
			continue
		}
		if args.ID != "" && ps.ID != args.ID {
			continue
		}
		if nameQuery != "" && !strings.Contains(strings.ToLower(ps.Name), nameQuery) {
			continue
		}
		if supplierQuery != "" && !strings.Contains(strings.ToLower(ps.SupplierName), supplierQuery) {
			continue
		}

		var unitsAvailable any = store.stock[ps.ProductID]
		if store.stock[ps.ProductID] <= 0 {
			unitsAvailable = "Not Available"
		}
		dealPercentages, hasDeal := dealPercentagesBySupplyID[ps.ID]

		items = append(items, CatalogueItem{
			ID:                      ps.ID,
			ProductID:               ps.ProductID,
			Name:                    ps.Name,
			HasDeal:                 hasDeal,
			DealPercentages:         dealPercentages,
			UnitsAvailable:          unitsAvailable,
			CostToUserPerRetailUnit: ps.CostToUserPerRetailUnit,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

// GetSupplyOrdersResult, GetCustomerOrdersResult, GetProductSupplyResult
// each wrap their tool's list in a named field rather than returning a
// bare array: some MCP clients (e.g. Claude Desktop) require every tool's
// inferred outputSchema to be a JSON object at the top level, and a bare
// slice Out infers as an array instead.
type GetSupplyOrdersResult struct {
	SupplyOrders []entities.SupplyOrder `json:"supplyOrders"`
}

type GetCustomerOrdersResult struct {
	Orders []entities.OnlineCustomerOrder `json:"orders"`
}

type GetProductSupplyResult struct {
	ProductSupply []entities.ProductSupply `json:"productSupply"`
}

// getSupplyOrders lists every loaded supply order, unfiltered.
func (store *onlineStore) getSupplyOrders(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetSupplyOrdersResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	out := make([]entities.SupplyOrder, 0, len(store.supplyOrders))
	for _, so := range store.supplyOrders {
		out = append(out, so)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetSupplyOrdersResult{SupplyOrders: out}, nil
}

// getCustomerOrders lists every online customer order, unfiltered - retail
// orders, not supply orders (see getSupplyOrders for those). Named
// get_customer_orders, not get_orders, to stay unambiguous against
// get_supply_orders.
func (store *onlineStore) getCustomerOrders(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetCustomerOrdersResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	out := make([]entities.OnlineCustomerOrder, 0, len(store.orders))
	for _, o := range store.orders {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetCustomerOrdersResult{Orders: out}, nil
}

// getProductSupply lists every loaded product-supply row, unfiltered - the
// raw per-supplier catalog/cost data, unlike get_inventory (deduped,
// stock-focused) or get_catalogue (deal-annotated).
func (store *onlineStore) getProductSupply(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, GetProductSupplyResult, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	out := make([]entities.ProductSupply, 0, len(store.productSupply))
	for _, ps := range store.productSupply {
		out = append(out, ps)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return nil, GetProductSupplyResult{ProductSupply: out}, nil
}

// getCatalogueOutputSchema builds get_catalogue's output schema by
// inferring the normal structure from GetCatalogueResult, then patching
// CatalogueItem.UnitsAvailable's own sub-schema by hand: it's a genuine
// scalar union (a live stock count, or the literal string "Not
// Available" - see CatalogueItem's own doc comment), which no single Go
// type can express, so the field is declared `any` and jsonschema-go
// infers that as the bare JSON Schema boolean-shorthand `true` - valid
// per the JSON Schema spec, but rejected by at least one real MCP client
// (Claude Code CLI's own stricter schema validator, which requires every
// schema position to be an object). A hand-built "oneOf" is a real
// object-shaped schema expressing exactly the same "int or string"
// constraint the field actually has - unlike the map[string]any fix used
// for other `any` fields elsewhere in this repo, which isn't an option
// here since the field's real values are scalars, never objects.
// mcp.AddTool skips its own automatic inference entirely whenever
// Tool.OutputSchema is already non-nil (see the go-sdk's setSchema), so
// providing this has no other effect on the rest of the inferred shape.
func getCatalogueOutputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[GetCatalogueResult](nil)
	if err != nil {
		panic(fmt.Sprintf("get_catalogue: failed to infer base output schema: %v", err))
	}
	schema.Properties["items"].Items.Properties["unitsAvailable"] = &jsonschema.Schema{
		OneOf: []*jsonschema.Schema{
			{Type: "integer"},
			{Type: "string"},
		},
	}
	return schema
}

func createServer(store *onlineStore) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "garden-online-store", Version: "1.0.0"}, nil)
	prompts := promptstore.New(server)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_customer_orders",
		Description: "Bulk-load customer (retail) order history (online_customer_orders.json's shape).",
	}, store.loadCustomerOrders)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "purchase_products",
		Description: "Record a customer purchasing one or more products. Price is looked up from the catalog (never supplied by the caller); stock is deducted too.",
	}, store.purchaseProducts)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "purchase_fulfilled",
		Description: "Mark a purchase fulfilled.",
	}, store.purchaseFulfilled)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_product_supply",
		Description: "Bulk-load the product supply catalog (product_supply.json's shape). Populates the product catalog; does not change stock levels.",
	}, store.loadProductSupply)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_supply_orders",
		Description: "Bulk-load supply orders (supply_orders.json's shape). load_product_supply must be called first. Each already-fulfilled entry adds to the stock baseline.",
	}, store.loadSupplyOrders)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_historical_purchases",
		Description: "Bulk-load historical online customer orders (online_customer_orders.json's shape) purely to net already-fulfilled purchases out of the stock baseline.",
	}, store.loadHistoricalPurchases)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_store_requests",
		Description: "Business logic validation for purchase_products: dry-run its real pricing logic (no side effects) and reject if the total would exceed the configured maxTotalCost. Hidden from ordinary clients - invocable only by go-proxy's orchestration engine as a call-authorization step.",
		Meta:        mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true},
	}, store.validateStoreRequests)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_product_supply",
		Description: "Add or update (upsert by id) one product supply row.",
	}, store.addProductSupply)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_supply_order",
		Description: "Add or update (upsert by id) one supply order. Created unfulfilled; does not change stock until mark_supply_order_fulfilled is called.",
	}, store.addSupplyOrder)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "mark_supply_order_fulfilled",
		Description: "Mark a supply order fulfilled, adding its units to stock.",
	}, store.markSupplyOrderFulfilled)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_inventory",
		Description: "List current stock levels and catalog price, optionally filtered to one product.",
	}, store.getInventory)

	mcp.AddTool(server, &mcp.Tool{
		Name:         "get_catalogue",
		Description:  "List product-supply rows (one per supplier per product), optionally filtered by exact productId/id or a case-insensitive substring of name/supplierName (no wildcards). Each result reports whether its product currently has an active deal (and its discount percentage) and how many retail units are left (\"Not Available\" once none remain).",
		OutputSchema: getCatalogueOutputSchema(),
	}, store.getCatalogue)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_supply_orders",
		Description: "List every loaded supply order (restock orders against a product-supply row). No arguments.",
	}, store.getSupplyOrders)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_customer_orders",
		Description: "List every online customer (retail) order - not supply orders, see get_supply_orders for those. No arguments.",
	}, store.getCustomerOrders)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_product_supply",
		Description: "List every loaded product-supply row: the raw per-supplier catalog/cost data (wholesale cost, units per wholesale unit, retail price). No arguments.",
	}, store.getProductSupply)

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

	store := newOnlineStore()
	server := createServer(store)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting garden-online-store on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
