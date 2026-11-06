// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func strPtr(s string) *string { return &s }

// purchaseCallRequest builds the json.RawMessage shape
// CallAuthorizationInput.CallRequest actually arrives as - the whole
// marshaled *mcp.CallToolParamsRaw ({"name":..., "arguments":{...}}, see
// go-proxy's own constraints_validation.go's callRequestToolName) - for a
// purchase_products call carrying items.
func purchaseCallRequest(t *testing.T, items []PurchaseItem) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Name      string               `json:"name"`
		Arguments PurchaseProductsArgs `json:"arguments"`
	}{
		Name:      "garden-online-store.purchase_products",
		Arguments: PurchaseProductsArgs{CustomerID: "cust-0001", Items: items},
	})
	if err != nil {
		t.Fatalf("failed to build purchase CallRequest: %v", err)
	}
	return raw
}

func TestOnlineStoreLoadCustomerOrders(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	_, res, err := store.loadCustomerOrders(ctx, nil, LoadCustomerOrdersArgs{Orders: []entities.OnlineCustomerOrder{
		{ID: "ord-0001", CustomerID: "cust-0001", ProductID: "prod-01", NumberOfRetailUnits: 2, TotalPrice: 19.98},
	}})
	if err != nil || res.Loaded != 1 {
		t.Fatalf("loadCustomerOrders: res=%+v err=%v", res, err)
	}
	if len(store.orders) != 1 {
		t.Errorf("expected 1 order stored, got %d", len(store.orders))
	}
}

func TestOnlineStoreNextOrderIDAvoidsCollisions(t *testing.T) {
	store := newOnlineStore()
	store.orders["ord-0001"] = entities.OnlineCustomerOrder{ID: "ord-0001"}
	store.orders["ord-0003"] = entities.OnlineCustomerOrder{ID: "ord-0003"}
	got := store.nextOrderID()
	if got != "ord-0002" {
		t.Errorf("expected ord-0002, got %q", got)
	}
}

func TestOnlineStorePurchaseFulfilled(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()
	store.orders["ord-0001"] = entities.OnlineCustomerOrder{ID: "ord-0001", CustomerID: "cust-0001"}

	_, order, err := store.purchaseFulfilled(ctx, nil, PurchaseFulfilledArgs{OrderID: "ord-0001", FulfillmentActualDate: "2026-02-01"})
	if err != nil {
		t.Fatalf("purchaseFulfilled: %v", err)
	}
	if order.FulfillmentActualDate == nil || *order.FulfillmentActualDate != "2026-02-01" {
		t.Errorf("expected fulfillmentActualDate 2026-02-01, got %v", order.FulfillmentActualDate)
	}

	if _, _, err := store.purchaseFulfilled(ctx, nil, PurchaseFulfilledArgs{OrderID: "ord-does-not-exist"}); err == nil {
		t.Error("expected purchaseFulfilled to fail for an unknown order id")
	}
}

// TestOnlineStoreApplyPurchasePriceAndStock covers what used to require
// mocking a relay back through the proxy to a separate garden-inventory
// server: purchase_products' pricing/stock core (applyPurchase, split out
// from the relay-fetching purchaseProducts handler the same way
// buildCatalogue is split from getCatalogue - see applyPurchase's own doc
// comment) looks up price and deducts stock entirely in-process
// (pricesByProduct/deductStock), directly testable like any other
// handler, without needing a live proxy relay for
// get_active_deals/get_customer_type. No deals/customer type are given
// here (nil/"") - TestOnlineStoreApplyPurchaseAppliesDeal below covers
// discount application specifically.
func TestOnlineStoreApplyPurchasePriceAndStock(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	if _, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Spade", UnitsPerWholesaleUnit: 10, CostPerWholesaleUnit: 50, CostToUserPerRetailUnit: 9.99},
	}}); err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}
	store.stock["prod-01"] = 20

	res, created := store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-0001",
		OrderDate:  "2026-01-01",
		Items:      []PurchaseItem{{ProductID: "prod-01", Units: 3}},
	}, "2026-01-01", "2026-01-08", nil, "one-time-customer")
	if res != nil {
		t.Fatalf("applyPurchase: unexpected error result %+v", res)
	}
	if len(created) != 1 {
		t.Fatalf("expected 1 created order, got %#v", created)
	}
	if created[0].TotalPrice != 29.97 { // 3 * 9.99, no deal
		t.Errorf("expected totalPrice 29.97, got %v", created[0].TotalPrice)
	}
	if created[0].DealID != nil {
		t.Errorf("expected no deal applied, got %+v", created[0].DealID)
	}
	if store.stock["prod-01"] != 17 { // 20 - 3
		t.Errorf("expected stock 17 after purchase, got %d", store.stock["prod-01"])
	}

	res, _ = store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-0001",
		Items:      []PurchaseItem{{ProductID: "prod-unknown", Units: 1}},
	}, "2026-01-01", "2026-01-08", nil, "one-time-customer")
	if res == nil || !res.IsError {
		t.Errorf("expected an IsError result for an unknown product, got %+v", res)
	}

	// prod-01 has 17 units left (20 - 3 from the purchase above); asking
	// for more than that must be rejected atomically, with stock left
	// untouched.
	res, _ = store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-0001",
		OrderDate:  "2026-01-01",
		Items:      []PurchaseItem{{ProductID: "prod-01", Units: 18}},
	}, "2026-01-01", "2026-01-08", nil, "one-time-customer")
	if res == nil || !res.IsError {
		t.Errorf("expected an IsError result for insufficient inventory, got %+v", res)
	}
	if store.stock["prod-01"] != 17 {
		t.Errorf("expected stock to stay at 17 after a rejected purchase, got %d", store.stock["prod-01"])
	}
}

// TestOnlineStoreApplyPurchaseAppliesDeal covers matchedDiscount's own
// eligibility logic end-to-end through applyPurchase: a deal covering
// ps-001 (prod-01) with per-type discounts applies for an eligible
// customer type and is reflected in both TotalPrice and DealID, doesn't
// apply at all for an ineligible type (full price, no DealID - the same
// as if no deal existed), and doesn't apply to a product the deal's IDs
// don't cover.
func TestOnlineStoreApplyPurchaseAppliesDeal(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	if _, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Spade", CostToUserPerRetailUnit: 10},
		{ID: "ps-002", ProductID: "prod-02", Name: "Trowel", CostToUserPerRetailUnit: 20},
	}}); err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}
	store.stock["prod-01"] = 100
	store.stock["prod-02"] = 100

	deals := []entities.Deal{
		{ID: "deal-vip", Name: "VIP Loyalty Discount", IDs: []string{"ps-001"}, DiscountPercentages: map[string]float64{"vip": 10}},
	}

	// VIP customer buying the covered product: discount applied.
	res, created := store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-vip", OrderDate: "2026-01-01",
		Items: []PurchaseItem{{ProductID: "prod-01", Units: 2}},
	}, "2026-01-01", "2026-01-08", deals, "vip")
	if res != nil {
		t.Fatalf("applyPurchase (vip, covered product): unexpected error %+v", res)
	}
	if created[0].TotalPrice != 18 { // 2 * 10 * (1 - 0.10)
		t.Errorf("expected discounted totalPrice 18, got %v", created[0].TotalPrice)
	}
	if created[0].DealID == nil || *created[0].DealID != "deal-vip" {
		t.Errorf("expected DealID deal-vip, got %v", created[0].DealID)
	}

	// One-time customer buying the same product: not eligible, full price.
	res, created = store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-onetime", OrderDate: "2026-01-01",
		Items: []PurchaseItem{{ProductID: "prod-01", Units: 2}},
	}, "2026-01-01", "2026-01-08", deals, "one-time-customer")
	if res != nil {
		t.Fatalf("applyPurchase (one-time, covered product): unexpected error %+v", res)
	}
	if created[0].TotalPrice != 20 { // 2 * 10, no discount
		t.Errorf("expected full-price totalPrice 20, got %v", created[0].TotalPrice)
	}
	if created[0].DealID != nil {
		t.Errorf("expected no DealID for an ineligible type, got %v", created[0].DealID)
	}

	// VIP customer buying a product the deal doesn't cover: full price.
	res, created = store.applyPurchase(PurchaseProductsArgs{
		CustomerID: "cust-vip", OrderDate: "2026-01-01",
		Items: []PurchaseItem{{ProductID: "prod-02", Units: 1}},
	}, "2026-01-01", "2026-01-08", deals, "vip")
	if res != nil {
		t.Fatalf("applyPurchase (vip, uncovered product): unexpected error %+v", res)
	}
	if created[0].TotalPrice != 20 || created[0].DealID != nil {
		t.Errorf("expected full price and no deal for an uncovered product, got %+v", created[0])
	}
}

func TestOnlineStoreInventoryLoadAndBaseline(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	_, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Spade", UnitsPerWholesaleUnit: 10, CostPerWholesaleUnit: 50, CostToUserPerRetailUnit: 9.99},
	}})
	if err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}

	_, res, err := store.loadSupplyOrders(ctx, nil, LoadSupplyOrdersArgs{SupplyOrders: []entities.SupplyOrder{
		{ID: "so-001", ProductSupplyID: "ps-001", NumberOfOrderedUnits: 5, FulfillmentActualDate: strPtr("2026-01-01")},
		{ID: "so-002", ProductSupplyID: "ps-001", NumberOfOrderedUnits: 3, FulfillmentActualDate: nil}, // not yet fulfilled - no stock change
	}})
	if err != nil {
		t.Fatalf("loadSupplyOrders: %v", err)
	}
	if res.Loaded != 2 {
		t.Errorf("expected 2 loaded, got %d", res.Loaded)
	}
	// 5 wholesale units * 10 units/wholesale-unit = 50 retail units; the
	// unfulfilled order contributes nothing.
	if store.stock["prod-01"] != 50 {
		t.Fatalf("expected baseline stock 50, got %d", store.stock["prod-01"])
	}

	_, loadRes, err := store.loadHistoricalPurchases(ctx, nil, LoadHistoricalPurchasesArgs{Orders: []entities.OnlineCustomerOrder{
		{ID: "ord-001", ProductID: "prod-01", NumberOfRetailUnits: 4, FulfillmentActualDate: strPtr("2026-01-05")},
		{ID: "ord-002", ProductID: "prod-01", NumberOfRetailUnits: 100, FulfillmentActualDate: nil}, // not yet fulfilled - ignored
	}})
	if err != nil {
		t.Fatalf("loadHistoricalPurchases: %v", err)
	}
	if loadRes.Loaded != 1 {
		t.Errorf("expected 1 historical purchase netted, got %d", loadRes.Loaded)
	}
	if store.stock["prod-01"] != 46 {
		t.Fatalf("expected stock 46 after netting historical purchases, got %d", store.stock["prod-01"])
	}

	// load_supply_orders must reject a batch referencing an unknown
	// productSupplyId, atomically (no partial apply).
	_, _, err = store.loadSupplyOrders(ctx, nil, LoadSupplyOrdersArgs{SupplyOrders: []entities.SupplyOrder{
		{ID: "so-999", ProductSupplyID: "ps-does-not-exist", NumberOfOrderedUnits: 1, FulfillmentActualDate: strPtr("2026-01-01")},
	}})
	if err == nil {
		t.Error("expected loadSupplyOrders to reject an unknown productSupplyId")
	}
}

func TestOnlineStoreInventoryFulfillmentAndDeduction(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	if _, _, err := store.addProductSupply(ctx, nil, AddProductSupplyArgs{
		ID: "ps-100", ProductID: "prod-99", Name: "Trowel", SupplierName: "Acme",
		UnitsPerWholesaleUnit: 12, CostPerWholesaleUnit: 60, CostToUserPerRetailUnit: 7.99,
	}); err != nil {
		t.Fatalf("addProductSupply: %v", err)
	}

	_, so, err := store.addSupplyOrder(ctx, nil, AddSupplyOrderArgs{
		ID: "so-100", ProductSupplyID: "ps-100", OrderDate: "2026-01-01", NumberOfOrderedUnits: 2,
	})
	if err != nil {
		t.Fatalf("addSupplyOrder: %v", err)
	}
	if so.FulfillmentActualDate != nil {
		t.Error("expected a freshly added supply order to be unfulfilled")
	}
	if store.stock["prod-99"] != 0 {
		t.Errorf("expected no stock change before fulfillment, got %d", store.stock["prod-99"])
	}

	if _, _, err := store.markSupplyOrderFulfilled(ctx, nil, MarkSupplyOrderFulfilledArgs{ID: "so-100", FulfillmentActualDate: "2026-01-05"}); err != nil {
		t.Fatalf("markSupplyOrderFulfilled: %v", err)
	}
	if store.stock["prod-99"] != 24 { // 2 wholesale units * 12 units/wholesale-unit
		t.Fatalf("expected stock 24 after fulfillment, got %d", store.stock["prod-99"])
	}
	if _, _, err := store.markSupplyOrderFulfilled(ctx, nil, MarkSupplyOrderFulfilledArgs{ID: "so-100"}); err == nil {
		t.Error("expected marking an already-fulfilled order fulfilled again to fail")
	}

	// deductStock (purchaseProducts' own internal deduction step, exercised
	// end-to-end in TestOnlineStorePurchaseProductsPriceAndStock) must also
	// allow going negative when called directly.
	store.mu.Lock()
	store.deductStock("prod-99", 30)
	store.mu.Unlock()
	if store.stock["prod-99"] != -6 { // 24-30=-6
		t.Errorf("expected deductStock to allow going negative, got %d", store.stock["prod-99"])
	}

	_, invRes, err := store.getInventory(ctx, nil, GetInventoryArgs{ProductID: "prod-99"})
	items := invRes.Items
	if err != nil {
		t.Fatalf("getInventory: %v", err)
	}
	if len(items) != 1 || items[0].UnitsAvailable != -6 || items[0].UnitPrice != 7.99 {
		t.Fatalf("unexpected getInventory result: %+v", items)
	}

	if _, _, err := store.getInventory(ctx, nil, GetInventoryArgs{ProductID: "prod-unknown"}); err == nil {
		t.Error("expected getInventory to fail for an unknown productId")
	}

	_, allRes, err := store.getInventory(ctx, nil, GetInventoryArgs{})
	all := allRes.Items
	if err != nil {
		t.Fatalf("getInventory (all): %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("expected 1 catalog entry, got %d", len(all))
	}
}

// TestOnlineStoreBuildCatalogue covers get_catalogue's filtering/
// deal-annotation logic directly, given an already-fetched active-deals
// list — see buildCatalogue's own doc comment for why the relay call
// (fetchActiveDeals) is kept out of this function so it stays unit
// testable without a live proxy relay. buildCatalogue trusts that
// whatever it's given is already active (garden-business-ops.get_active_
// deals is the one that decides that), so the hand-built deal below
// carries no dates at all. The deal names only ps-001, not ps-002 (both
// prod-01), specifically to cover entities.Deal.IDs' per-supplier-row
// granularity: ps-002 must not inherit ps-001's deal just for sharing a
// productId.
func TestOnlineStoreBuildCatalogue(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	if _, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Steel Garden Spade", SupplierName: "GreenThumb Wholesale", CostToUserPerRetailUnit: 23.99},
		{ID: "ps-002", ProductID: "prod-01", Name: "Steel Garden Spade", SupplierName: "EarthWorks Supply Co.", CostToUserPerRetailUnit: 23.99},
		{ID: "ps-003", ProductID: "prod-02", Name: "Cotton Garden Gloves", SupplierName: "GreenThumb Wholesale", CostToUserPerRetailUnit: 9.99},
	}}); err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}
	store.stock["prod-01"] = 5
	store.stock["prod-02"] = 0

	deals := []entities.Deal{
		{ID: "deal-01", Name: "Spade Sale", IDs: []string{"ps-001"}, DiscountPercentages: map[string]float64{"one-time-customer": 15, "returning-customer": 20, "vip": 25}},
	}

	// No filter: all 3 supplier rows, sorted by id.
	items := store.buildCatalogue(GetCatalogueArgs{}, deals)
	if len(items) != 3 {
		t.Fatalf("expected 3 catalogue rows, got %d: %+v", len(items), items)
	}
	if items[0].ID != "ps-001" || items[1].ID != "ps-002" || items[2].ID != "ps-003" {
		t.Errorf("expected rows sorted by id, got %+v", items)
	}

	// ps-001 has an active deal and stock.
	if !items[0].HasDeal || items[0].DealPercentages["one-time-customer"] != 15 {
		t.Errorf("expected ps-001 to carry the active deal, got %+v", items[0])
	}
	if items[0].UnitsAvailable != 5 {
		t.Errorf("expected ps-001 unitsAvailable 5, got %v", items[0].UnitsAvailable)
	}
	if items[0].CostToUserPerRetailUnit != 23.99 {
		t.Errorf("expected ps-001 costToUserPerRetailUnit 23.99, got %v", items[0].CostToUserPerRetailUnit)
	}

	// ps-002 is the same product (prod-01) but not named by the deal, so
	// it must not carry it.
	if items[1].HasDeal {
		t.Errorf("expected ps-002 to carry no deal (deal only names ps-001), got %+v", items[1])
	}

	// prod-02 has no deal and 0 stock -> "Not Available".
	if items[2].HasDeal {
		t.Errorf("expected ps-003 to carry no deal, got %+v", items[2])
	}
	if items[2].UnitsAvailable != "Not Available" {
		t.Errorf("expected ps-003 unitsAvailable \"Not Available\", got %v", items[2].UnitsAvailable)
	}

	// productId exact-match filter.
	items = store.buildCatalogue(GetCatalogueArgs{ProductID: "prod-02"}, deals)
	if len(items) != 1 || items[0].ID != "ps-003" {
		t.Fatalf("expected productId filter to match only ps-003, got %+v", items)
	}

	// id exact-match filter.
	items = store.buildCatalogue(GetCatalogueArgs{ID: "ps-002"}, deals)
	if len(items) != 1 || items[0].ID != "ps-002" {
		t.Fatalf("expected id filter to match only ps-002, got %+v", items)
	}

	// name substring filter, case-insensitive, no wildcards.
	items = store.buildCatalogue(GetCatalogueArgs{Name: "garden gl"}, deals)
	if len(items) != 1 || items[0].ID != "ps-003" {
		t.Fatalf("expected name substring filter to match only ps-003, got %+v", items)
	}

	// supplierName substring filter, ANDed with productId.
	items = store.buildCatalogue(GetCatalogueArgs{ProductID: "prod-01", SupplierName: "earthworks"}, deals)
	if len(items) != 1 || items[0].ID != "ps-002" {
		t.Fatalf("expected supplierName+productId filter to match only ps-002, got %+v", items)
	}

	// No match.
	items = store.buildCatalogue(GetCatalogueArgs{Name: "does-not-exist"}, deals)
	if len(items) != 0 {
		t.Fatalf("expected no matches, got %+v", items)
	}
}

// TestOnlineStoreGetCatalogueFailsClosedWithoutRelayHeaders covers
// get_catalogue's own fail-closed behavior when called with no identity
// assertion/callback URL (e.g. internalAuth/selfUrl not configured on the
// proxy) — same convention garden-business-ops.updateCustomerType already
// follows for its own relay call.
func TestOnlineStoreGetCatalogueFailsClosedWithoutRelayHeaders(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	res, _, err := store.getCatalogue(ctx, &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}, GetCatalogueArgs{})
	if err != nil {
		t.Fatalf("getCatalogue: unexpected Go error %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an IsError result when no relay headers are present, got %+v", res)
	}
}

func TestOnlineStoreGetSupplyOrdersOrdersProductSupply(t *testing.T) {
	ctx := context.Background()
	store := newOnlineStore()

	if _, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Steel Garden Spade", SupplierName: "GreenThumb Wholesale", CostToUserPerRetailUnit: 23.99},
	}}); err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}
	if _, _, err := store.loadSupplyOrders(ctx, nil, LoadSupplyOrdersArgs{SupplyOrders: []entities.SupplyOrder{
		{ID: "so-001", ProductSupplyID: "ps-001", NumberOfOrderedUnits: 5, FulfillmentActualDate: strPtr("2026-01-01")},
	}}); err != nil {
		t.Fatalf("loadSupplyOrders: %v", err)
	}
	if _, _, err := store.loadCustomerOrders(ctx, nil, LoadCustomerOrdersArgs{Orders: []entities.OnlineCustomerOrder{
		{ID: "ord-0001", CustomerID: "cust-0001", ProductID: "prod-01", NumberOfRetailUnits: 2, TotalPrice: 19.98},
	}}); err != nil {
		t.Fatalf("loadCustomerOrders: %v", err)
	}

	_, supplyOrdersRes, err := store.getSupplyOrders(ctx, nil, struct{}{})
	supplyOrders := supplyOrdersRes.SupplyOrders
	if err != nil {
		t.Fatalf("getSupplyOrders: %v", err)
	}
	if len(supplyOrders) != 1 || supplyOrders[0].ID != "so-001" {
		t.Fatalf("expected 1 supply order so-001, got %+v", supplyOrders)
	}

	_, ordersRes, err := store.getCustomerOrders(ctx, nil, struct{}{})
	orders := ordersRes.Orders
	if err != nil {
		t.Fatalf("getCustomerOrders: %v", err)
	}
	if len(orders) != 1 || orders[0].ID != "ord-0001" {
		t.Fatalf("expected 1 order ord-0001, got %+v", orders)
	}

	_, productSupplyRes, err := store.getProductSupply(ctx, nil, struct{}{})
	productSupply := productSupplyRes.ProductSupply
	if err != nil {
		t.Fatalf("getProductSupply: %v", err)
	}
	if len(productSupply) != 1 || productSupply[0].ID != "ps-001" {
		t.Fatalf("expected 1 product-supply row ps-001, got %+v", productSupply)
	}
}

// newValidateStoreRequestsFixture builds a store with one product
// (prod-01, $10/unit, 100 in stock) shared by every
// TestOnlineStoreComputePurchaseTotal*/TestOnlineStoreValidateStoreRequests*
// test below.
func newValidateStoreRequestsFixture(t *testing.T) *onlineStore {
	t.Helper()
	ctx := context.Background()
	store := newOnlineStore()
	if _, _, err := store.loadProductSupply(ctx, nil, LoadProductSupplyArgs{ProductSupply: []entities.ProductSupply{
		{ID: "ps-001", ProductID: "prod-01", Name: "Spade", CostToUserPerRetailUnit: 10},
	}}); err != nil {
		t.Fatalf("loadProductSupply: %v", err)
	}
	store.stock["prod-01"] = 100
	return store
}

// TestOnlineStoreComputePurchaseTotalBasic covers computePurchaseTotal's
// plain (no-deal) pricing, called directly with hand-built activeDeals/
// customerType - the same "test the pure logic directly, without needing a
// live proxy relay" convention applyPurchase's own tests already follow
// (see TestOnlineStoreApplyPurchasePriceAndStock's doc comment).
func TestOnlineStoreComputePurchaseTotalBasic(t *testing.T) {
	store := newValidateStoreRequestsFixture(t)

	total := store.computePurchaseTotal([]PurchaseItem{{ProductID: "prod-01", Units: 5}}, nil, "one-time-customer")
	if total != 50 {
		t.Errorf("expected total 50 (5*10), got %v", total)
	}
}

// TestOnlineStoreComputePurchaseTotalFailingPurchaseIsZero covers the
// user's own explicit spec: a purchase that would fail for a reason
// unrelated to price (here, insufficient inventory, or an unknown product)
// must price at 0, since purchaseFeasible - the same check applyPurchase
// itself uses - decides feasibility first.
func TestOnlineStoreComputePurchaseTotalFailingPurchaseIsZero(t *testing.T) {
	store := newValidateStoreRequestsFixture(t)

	total := store.computePurchaseTotal([]PurchaseItem{{ProductID: "prod-01", Units: 1000}}, nil, "one-time-customer") // more than the 100 in stock
	if total != 0 {
		t.Errorf("expected an infeasible (insufficient-inventory) purchase to price at 0, got %v", total)
	}

	total = store.computePurchaseTotal([]PurchaseItem{{ProductID: "prod-unknown", Units: 1}}, nil, "one-time-customer")
	if total != 0 {
		t.Errorf("expected a purchase of an unknown product to price at 0, got %v", total)
	}
}

// TestOnlineStoreComputePurchaseTotalDealAware cross-checks
// computePurchaseTotal's figure against what applyPurchase would actually
// charge for the identical input (activeDeals/customerType), proving the
// two share real pricing logic (priceItem) rather than merely similar
// logic.
func TestOnlineStoreComputePurchaseTotalDealAware(t *testing.T) {
	store := newValidateStoreRequestsFixture(t)
	items := []PurchaseItem{{ProductID: "prod-01", Units: 4}}
	deals := []entities.Deal{
		{ID: "deal-vip", Name: "VIP Loyalty Discount", IDs: []string{"ps-001"}, DiscountPercentages: map[string]float64{"vip": 10}},
	}

	res, created := store.applyPurchase(PurchaseProductsArgs{CustomerID: "cust-vip", OrderDate: "2026-01-01", Items: items},
		"2026-01-01", "2026-01-08", deals, "vip")
	if res != nil {
		t.Fatalf("applyPurchase: unexpected error result %+v", res)
	}
	wantTotal := created[0].TotalPrice // 4 * 10 * 0.9 = 36

	gotTotal := store.computePurchaseTotal(items, deals, "vip")
	if gotTotal != wantTotal {
		t.Errorf("expected computePurchaseTotal to match applyPurchase's real (deal-discounted) total %.2f, got %.2f", wantTotal, gotTotal)
	}
}

// TestOnlineStoreValidateStoreRequestsFailsClosedWithoutRelayHeaders covers
// validate_store_requests' own fail-closed behavior when called with no
// identity assertion/callback URL present - the same convention
// getCatalogue's own fetchActiveDeals call already follows (see
// TestOnlineStoreGetCatalogueFailsClosedWithoutRelayHeaders) - now that a
// discovered triggered tool carries the same relay capability an ordinary
// backend call does (see runtime.go's platformHTTPClient). Also doubles as
// the regression test for a real bug this tool once had: in.CallRequest
// arrives as map[string]any (not json.RawMessage) once it's gone over a
// real MCP wire round-trip, and a naive type assertion used to fail closed
// on every real call - reaching the relay-missing error here (rather than
// a CallRequest-decode error) proves the decode step itself still succeeds
// for a map-shaped CallRequest.
func TestOnlineStoreValidateStoreRequestsFailsClosedWithoutRelayHeaders(t *testing.T) {
	ctx := context.Background()
	store := newValidateStoreRequestsFixture(t)

	rawJSON := purchaseCallRequest(t, []PurchaseItem{{ProductID: "prod-01", Units: 5}})
	var callRequest any // decodes as map[string]any, matching a real wire round-trip
	if err := json.Unmarshal(rawJSON, &callRequest); err != nil {
		t.Fatalf("failed to decode CallRequest into map[string]any: %v", err)
	}
	if _, ok := callRequest.(map[string]any); !ok {
		t.Fatalf("test setup bug: expected callRequest to decode as map[string]any, got %T", callRequest)
	}

	res, _, err := store.validateStoreRequests(ctx, &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}, triggered_tools_runtime.CallAuthorizationInput{
		Roles:       []string{"customer", "one-time-customer"},
		CallRequest: callRequest,
		Parameters:  `{"maxTotalCost": 100}`,
	})
	if err != nil {
		t.Fatalf("validateStoreRequests: unexpected Go error %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an IsError result when no relay headers are present, got %+v", res)
	}
}

// TestOnlineStoreValidateStoreRequestsNoSideEffects proves the "no side
// effects" requirement directly: calling validateStoreRequests must never
// deduct stock or create an order, unlike a real purchase_products call -
// true unconditionally (it never calls deductStock or writes store.orders
// at all), so this doesn't need a working relay to demonstrate.
func TestOnlineStoreValidateStoreRequestsNoSideEffects(t *testing.T) {
	ctx := context.Background()
	store := newValidateStoreRequestsFixture(t)
	stockBefore := store.stock["prod-01"]
	ordersBefore := len(store.orders)

	if _, _, err := store.validateStoreRequests(ctx, &mcp.CallToolRequest{Extra: &mcp.RequestExtra{}}, triggered_tools_runtime.CallAuthorizationInput{
		Roles:       []string{"customer", "one-time-customer"},
		CallRequest: purchaseCallRequest(t, []PurchaseItem{{ProductID: "prod-01", Units: 5}}),
		Parameters:  `{"maxTotalCost": 1}`,
	}); err != nil {
		t.Fatalf("validateStoreRequests: %v", err)
	}

	if store.stock["prod-01"] != stockBefore {
		t.Errorf("expected stock unchanged, got %d (was %d)", store.stock["prod-01"], stockBefore)
	}
	if len(store.orders) != ordersBefore {
		t.Errorf("expected no orders created, got %d (was %d)", len(store.orders), ordersBefore)
	}
}

// newFakeGardeningRelayProxy is an in-process stand-in for go-proxy itself,
// exposing the two namespaced tools validate_store_requests relays to
// (fetchActiveDeals/fetchCustomerType) - garden-business-ops.get_active_
// deals and garden-customer-admin.get_customer_type - so
// TestOnlineStoreValidateStoreRequestsRelaySuccess can exercise a real
// relay round trip end to end, not just the fail-closed path. customerType
// is returned for every customerId (this fake has no real customer
// records - one fixed type is enough to prove the wiring, not garden-
// customer-admin's own lookup logic, which has its own tests).
func newFakeGardeningRelayProxy(t *testing.T, deals []entities.Deal, customerType string) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fake-gardening-proxy", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "garden-business-ops.get_active_deals"},
		func(ctx context.Context, req *mcp.CallToolRequest, args struct{}) (*mcp.CallToolResult, struct {
			Deals []entities.Deal `json:"deals"`
		}, error) {
			return nil, struct {
				Deals []entities.Deal `json:"deals"`
			}{Deals: deals}, nil
		})
	mcp.AddTool(server, &mcp.Tool{Name: "garden-customer-admin.get_customer_type"},
		func(ctx context.Context, req *mcp.CallToolRequest, args struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}, error) {
			return nil, struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			}{ID: args.ID, Type: customerType}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	return httpServer
}

// requestWithRelayHeaders builds a *mcp.CallToolRequest carrying the given
// callback URL as inbound relay headers, matching what a real triggered-
// tool invocation now presents (see runtime.go's platformHTTPClient) - the
// assertion value itself is never verified by this test's fake proxy, so
// any non-empty placeholder is fine.
func requestWithRelayHeaders(callbackURL string) *mcp.CallToolRequest {
	h := http.Header{}
	relayheaders.Set{Assertion: "Bearer test-assertion", CallbackURL: callbackURL}.Apply(h)
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
}

// TestOnlineStoreValidateStoreRequestsRelaySuccess is the full end-to-end
// proof that validate_store_requests, as a discovered triggered tool, can
// now relay back through the proxy exactly as purchaseProducts itself
// does - fetching real deal-eligible pricing via fetchActiveDeals/
// fetchCustomerType rather than approximating from local state - and that
// its maxTotalCost decision is correct against the resulting deal-aware
// total.
func TestOnlineStoreValidateStoreRequestsRelaySuccess(t *testing.T) {
	ctx := context.Background()
	store := newValidateStoreRequestsFixture(t)

	deals := []entities.Deal{
		{ID: "deal-vip", Name: "VIP Loyalty Discount", IDs: []string{"ps-001"}, DiscountPercentages: map[string]float64{"vip": 10}},
	}
	proxy := newFakeGardeningRelayProxy(t, deals, "vip")
	items := []PurchaseItem{{ProductID: "prod-01", Units: 4}} // 4 * 10 * 0.9 = 36, deal-discounted

	_, out, err := store.validateStoreRequests(ctx, requestWithRelayHeaders(proxy.URL+"/mcp"), triggered_tools_runtime.CallAuthorizationInput{
		Roles:       []string{"customer", "vip"},
		CallRequest: purchaseCallRequest(t, items),
		Parameters:  `{"maxTotalCost": 1000}`, // permissive - the real (discounted) total, 36, is well under it
	})
	if err != nil {
		t.Fatalf("validateStoreRequests: %v", err)
	}
	if !out.Authorized {
		t.Fatalf("expected authorization with a permissive limit, got %+v", out)
	}

	// Lowering the limit to just under the real deal-discounted total (36)
	// must now reject - proving the relayed deal/customer-type data
	// actually drove the computed total, not full (undiscounted) price.
	_, out, err = store.validateStoreRequests(ctx, requestWithRelayHeaders(proxy.URL+"/mcp"), triggered_tools_runtime.CallAuthorizationInput{
		Roles:       []string{"customer", "vip"},
		CallRequest: purchaseCallRequest(t, items),
		Parameters:  `{"maxTotalCost": 35}`,
	})
	if err != nil {
		t.Fatalf("validateStoreRequests: %v", err)
	}
	if out.Authorized {
		t.Errorf("expected a limit just under the real (deal-discounted) total to reject, got %+v", out)
	}
}
