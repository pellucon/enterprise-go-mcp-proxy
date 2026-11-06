// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"time"
)

// IMPORTANT: no string value used anywhere in a Build function below may
// contain a space. cli/repl.go's REPL tokenizer only tracks its OWN
// literal quote-toggle state — it has no concept of "inside a JSON
// string" — and since a JSON object token always starts with '{' (not
// '"'), that toggle state never actually activates for the -args token at
// all (see lines.go's renderLine doc comment). That means ANY space
// anywhere inside the rendered -args JSON, including one buried inside a
// string value like "Stress Test Supply Row", gets treated as an ordinary
// token boundary and silently splits one -args value into several
// tokens — lines.go's self-check tokenizer catches this at generation
// time (it did, for exactly this reason, while writing this file) rather
// than failing much later inside a live stress-test run.

// weightedTool is one entry in the call-mix catalog: a namespaced tool
// name, its relative selection weight, and a builder that produces one
// concrete argument set per call using the file's own genCtx (random
// source + seed-data pools).
type weightedTool struct {
	Tool   string
	Weight float64
	Build  func(g *genCtx) map[string]any
}

// gardeningWeightFloor is the minimum number of lines/file any gardening
// tool gets, regardless of what its normalized weight would otherwise
// round down to — enforced by main.go's line-count allocation pass, so no
// included tool ever goes effectively untested across a run.
const gardeningWeightFloor = 15

// mark_supply_order_fulfilled deliberately has no entry in gardeningTools:
// its frequency is entirely driven by how many add_supply_order calls
// happen to be generated (main.go pairs each one with a later
// mark_supply_order_fulfilled referencing the same synthetic id), not by
// an independent weight — see stress-test/README.md.

// gardeningTools is the 25 independently-weighted gardening tools (26
// includable tools minus mark_supply_order_fulfilled, paired separately).
// Weights are relative, not percentages — main.go normalizes them by
// their own sum. Grouped by bucket purely for readability; the buckets
// themselves aren't a separate mechanism.
func gardeningTools() []weightedTool {
	return []weightedTool{
		// High-frequency reads (~35% combined)
		{"garden-online-store.get_inventory", 9, buildGetInventory},
		{"garden-online-store.get_catalogue", 8, buildGetCatalogue},
		{"garden-customer-admin.get_customers", 7, buildGetCustomers},
		{"garden-online-store.get_customer_orders", 6, buildNoArgs},
		{"garden-business-ops.get_active_deals", 5, buildNoArgs},

		// Core transactional writes (~25% combined)
		{"garden-online-store.purchase_products", 15, buildPurchaseProducts},
		{"garden-online-store.purchase_fulfilled", 4, buildPurchaseFulfilled},
		{"garden-customer-service.add_customer_service_call", 4, buildAddCustomerServiceCall},
		{"garden-customer-service.resolve_customer_service_call", 2, buildResolveCustomerServiceCall},

		// Secondary reads (~20% combined)
		{"garden-online-store.get_supply_orders", 3, buildNoArgs},
		{"garden-online-store.get_product_supply", 3, buildNoArgs},
		{"garden-customer-admin.get_customers_status", 2.5, buildNoArgs},
		{"garden-customer-admin.get_customer_type", 2.5, buildGetCustomerType},
		{"garden-customer-admin.get_customer_details", 2.5, buildGetCustomerDetails},
		{"garden-staff-admin.get_staffers", 2.5, buildGetStaffers},
		{"garden-business-ops.get_deals", 2.5, buildNoArgs},
		{"garden-customer-service.get_customer_service_calls", 2.5, buildGetCustomerServiceCalls},

		// Administrative upserts (~15% combined, deliberately spread thin)
		{"garden-online-store.add_product_supply", 1.5, buildAddProductSupply},
		{"garden-online-store.add_supply_order", 1.5, buildAddSupplyOrder},
		{"garden-customer-admin.add_or_update_customer", 1.5, buildAddOrUpdateCustomer},
		{"garden-customer-admin.mark_customer_deactivated", 1, buildMarkCustomerDeactivated},
		{"garden-customer-admin.modify_customer_sentiment", 1, buildModifyCustomerSentimentAdmin},
		{"garden-customer-admin.modify_customer_type", 1, buildModifyCustomerType},
		{"garden-business-ops.add_or_update_deal", 1.5, buildAddOrUpdateDeal},
		{"garden-business-ops.update_customer_type", 1, buildUpdateCustomerType},
		{"garden-staff-admin.add_or_update_staffer", 1.5, buildAddOrUpdateStaffer},
		{"garden-staff-admin.mark_staffer_deactivated", 1, buildMarkStafferDeactivated},
		{"garden-customer-service.modify_customer_sentiment", 1, buildModifyCustomerSentimentService},
	}
}

// proxyPlatformTools is the 6 non-gardening tools the spec calls for at
// ~5% of a file's total lines, split evenly.
func proxyPlatformTools() []weightedTool {
	return []weightedTool{
		{"proxy_utility.get_my_id_details", 1, buildNoArgs},
		{"proxy_utility.get_prompts", 1, buildGetPrompts},
		{"declarative-guardrail.evaluate_declarations", 1, buildEvaluateDeclarations},
		{"event-aggregator.get_event_stats", 1, buildNoArgs},
		{"event-aggregator.reset_events", 1, buildNoArgs},
		{"rate-limiter.get_stats", 1, buildRateLimiterStats},
	}
}

func buildNoArgs(g *genCtx) map[string]any { return map[string]any{} }

// isoDate formats t as the YYYY-MM-DD shape every gardening tool's
// optional date fields expect.
func isoDate(t time.Time) string { return t.Format("2006-01-02") }

// nearToday returns an ISO date within [-daysBack, +daysForward] of the
// generator's own run time — plausible-looking without needing to track
// per-call simulated time.
func nearToday(g *genCtx, daysBack, daysForward int) string {
	offset := g.randInt(-daysBack, daysForward+1)
	return isoDate(time.Now().AddDate(0, 0, offset))
}

// --- garden-online-store ---

func buildPurchaseProducts(g *genCtx) map[string]any {
	n := g.randInt(1, 4)
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{
			"productId": g.pick(g.seed.ProductIDs),
			"units":     g.randInt(1, 6),
		}
	}
	args := map[string]any{
		"customerId": g.pick(g.seed.CustomerIDs),
		"items":      items,
	}
	if g.randBool() {
		args["orderDate"] = nearToday(g, 2, 0)
	}
	return args
}

func buildPurchaseFulfilled(g *genCtx) map[string]any {
	args := map[string]any{"orderId": g.nextPendingOrderID()}
	if g.randBool() {
		args["fulfillmentActualDate"] = nearToday(g, 0, 1)
	}
	return args
}

func buildAddProductSupply(g *genCtx) map[string]any {
	return map[string]any{
		"id":                      g.nextSyntheticID("ps"),
		"productId":               g.pick(g.seed.ProductIDs),
		"name":                    "Stress-Test-Supply-Row",
		"supplierName":            "Stress-Test-Supplier",
		"unitsPerWholesaleUnit":   g.randInt(5, 25),
		"costPerWholesaleUnit":    g.randFloat(20, 150),
		"costToUserPerRetailUnit": g.randFloat(5, 60),
	}
}

// buildAddSupplyOrder is only used by main.go's pairing logic (not placed
// directly in gardeningTools' Build slot without pairing) — see
// generateAddSupplyOrderWithPairedFulfillment in lines.go.
func buildAddSupplyOrder(g *genCtx) map[string]any {
	return map[string]any{
		"id":                   g.nextSyntheticSupplyOrderID(),
		"productSupplyId":      g.pick(g.seed.ProductSupplyIDs),
		"numberOfOrderedUnits": g.randInt(10, 60),
	}
}

func buildGetInventory(g *genCtx) map[string]any {
	if g.randBool() {
		return map[string]any{}
	}
	return map[string]any{"productId": g.pick(g.seed.ProductIDs)}
}

func buildGetCatalogue(g *genCtx) map[string]any {
	switch g.randInt(0, 3) {
	case 0:
		return map[string]any{}
	case 1:
		return map[string]any{"productId": g.pick(g.seed.ProductIDs)}
	default:
		return map[string]any{"id": g.pick(g.seed.ProductSupplyIDs)}
	}
}

// --- garden-customer-admin ---

func buildAddOrUpdateCustomer(g *genCtx) map[string]any {
	id := g.pick(g.seed.CustomerIDs)
	if g.randBool() {
		id = g.nextSyntheticID("cust")
	}
	args := map[string]any{
		"id":   id,
		"name": "Stress-Test-Customer",
		"type": g.pick(g.seed.CustomerTypes),
	}
	if g.randBool() {
		args["sentiment"] = g.pick(g.seed.CustomerSentiments)
	}
	return args
}

func buildMarkCustomerDeactivated(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.CustomerIDs), "deactivated": g.randBool()}
}

func buildModifyCustomerSentimentAdmin(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.CustomerIDs), "sentiment": g.pick(g.seed.CustomerSentiments)}
}

func buildModifyCustomerType(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.CustomerIDs), "type": g.pick(g.seed.CustomerTypes)}
}

func buildGetCustomers(g *genCtx) map[string]any {
	switch g.randInt(0, 3) {
	case 0:
		return map[string]any{}
	case 1:
		return map[string]any{"type": g.pick(g.seed.CustomerTypes)}
	default:
		return map[string]any{"sentiment": g.pick(g.seed.CustomerSentiments)}
	}
}

func buildGetCustomerType(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.CustomerIDs)}
}

func buildGetCustomerDetails(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.CustomerIDs)}
}

// --- garden-business-ops ---

func buildAddOrUpdateDeal(g *genCtx) map[string]any {
	discounts := map[string]any{}
	for _, ct := range g.seed.CustomerTypes {
		discounts[ct] = float64(g.randInt(5, 30))
	}
	args := map[string]any{
		"name":                "Stress-Test-Deal",
		"ids":                 g.pickN(g.seed.ProductSupplyIDs, g.randInt(2, 6)),
		"discountPercentages": discounts,
	}
	if g.randBool() {
		args["id"] = g.pick(g.seed.DealIDs)
	}
	return args
}

func buildUpdateCustomerType(g *genCtx) map[string]any {
	return map[string]any{"customerId": g.pick(g.seed.CustomerIDs), "type": g.pick(g.seed.CustomerTypes)}
}

// --- garden-staff-admin ---

func buildAddOrUpdateStaffer(g *genCtx) map[string]any {
	id := g.pick(g.seed.StafferIDs)
	if g.randBool() {
		id = g.nextSyntheticID("staff")
	}
	return map[string]any{
		"id":    id,
		"name":  "Stress-Test-Staffer",
		"roles": g.pickN(g.seed.StaffRoleIDs, g.randInt(1, 3)),
	}
}

func buildMarkStafferDeactivated(g *genCtx) map[string]any {
	return map[string]any{"id": g.pick(g.seed.StafferIDs), "deactivated": g.randBool()}
}

func buildGetStaffers(g *genCtx) map[string]any {
	if g.randBool() {
		return map[string]any{}
	}
	return map[string]any{"role": g.pick(g.seed.StaffRoleIDs)}
}

// --- garden-customer-service ---

func buildAddCustomerServiceCall(g *genCtx) map[string]any {
	args := map[string]any{
		"customerId":         g.pick(g.seed.CustomerIDs),
		"handledByStafferId": g.pick(g.seed.StafferIDs),
		"issueText":          "Stress-test-generated-service-call.",
	}
	if g.randBool() {
		args["escalatedToManager"] = false
	}
	return args
}

func buildResolveCustomerServiceCall(g *genCtx) map[string]any {
	args := map[string]any{"id": g.nextUnresolvedCallID()}
	if g.randBool() {
		args["resolutionDate"] = nearToday(g, 0, 1)
	}
	return args
}

func buildModifyCustomerSentimentService(g *genCtx) map[string]any {
	return map[string]any{"customerId": g.pick(g.seed.CustomerIDs), "sentiment": g.pick(g.seed.CustomerSentiments)}
}

func buildGetCustomerServiceCalls(g *genCtx) map[string]any {
	if g.randBool() {
		return map[string]any{}
	}
	return map[string]any{"handledByStafferId": g.pick(g.seed.StafferIDs)}
}

// --- proxy / platform tools ---

func buildGetPrompts(g *genCtx) map[string]any {
	if g.randBool() {
		return map[string]any{}
	}
	return map[string]any{"namePattern": "garden-*"}
}

func buildEvaluateDeclarations(g *genCtx) map[string]any {
	return map[string]any{
		"roles":            []string{"admin"},
		"declarationNames": g.pickN(g.seed.DeclarationNames, g.randInt(1, 4)),
	}
}

func buildRateLimiterStats(g *genCtx) map[string]any {
	if g.randBool() {
		return map[string]any{}
	}
	return map[string]any{"minutes": g.randInt(1, 61)}
}
