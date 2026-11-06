// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
)

// eventBusinessHourStart/End bound the random time-of-day synthesized for
// each event: online_customer_orders.json/supply_orders.json only carry
// dates (YYYY-MM-DD), but a real NATS event stream needs distinct,
// plausible timestamps - business hours read more like a real store than
// a uniformly random hour would.
const (
	eventBusinessHourStart = 8
	eventBusinessHourEnd   = 20
)

// randEventTime returns a random business-hours timestamp on date.
func randEventTime(rng *rand.Rand, date time.Time) time.Time {
	hour := eventBusinessHourStart + rng.Intn(eventBusinessHourEnd-eventBusinessHourStart)
	minute := rng.Intn(60)
	second := rng.Intn(60)
	return time.Date(date.Year(), date.Month(), date.Day(), hour, minute, second, 0, time.UTC)
}

// randToolCallID synthesizes a UUID-v4-shaped, rng-seeded call correlation
// id - a stand-in for the real uuid.NewString() every actual tool call
// gets (see CLAUDE.md's "Tool-call correlation ID" section).
// *rand.Rand implements io.Reader, so this stays deterministic under the
// same seed like every other gendata output.
func randToolCallID(rng *rand.Rand) string {
	b := make([]byte, 16)
	rng.Read(b) //nolint:errcheck // *rand.Rand.Read never errors
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func mustParseDate(s string) time.Time {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(fmt.Sprintf("gendata: invalid date %q in seed data: %v", s, err))
	}
	return t
}

// rolesByStafferID/staffRolesByID look up a staffer's own role list by id
// - used so a reverse-engineered call's "roles" metadata matches the
// real staffer who plausibly made it, not a fabricated set.
func staffRolesByID(staffers []entities.Staffer, id string) []string {
	for _, s := range staffers {
		if s.ID == id {
			return s.Roles
		}
	}
	return nil
}

// reconstructedCall is one reverse-engineered real tool call this whole
// dataset is built from: the "backbone" tool-request-result/call-succeeded
// event every reconstructedCall always produces, plus the optional
// tool-request/tool-call-alert/result-alert events sprinkled around it
// (see buildSampleEvents).
type reconstructedCall struct {
	toolCallID string
	tool       string
	user       string
	roles      []string
	request    map[string]any
	result     any
	resultTime time.Time
	// highValue/valueNote mark a call worth an occasional tool-call-alert/
	// result-alert sprinkle - derived from the call's own data (order/
	// supply-order total), not purely random, so the "smart" framing
	// extends to which calls get flagged, not just which calls exist.
	highValue bool
	valueNote string
}

// buildSampleEvents reverse-engineers the plausible sequence of MCP tool
// calls that would have produced orders/supplyOrders - one
// tool-request-result/call-succeeded event per real create/fulfill call
// (garden-online-store.purchase_products/purchase_fulfilled/
// add_supply_order/mark_supply_order_fulfilled) - then sprinkles in:
//   - a tool-request/request-ok event for a random subset of those real
//     calls, sharing the same toolCallId and timestamped a few seconds
//     before its own tool-request-result, exactly like a real orchestrated
//     call-authorization phase would produce;
//   - a tool-call-alert and/or result-alert event for a random subset of
//     the highest-value real calls (large order/supply totals), as if a
//     declarative guardrail flagged them for review without blocking them;
//   - a small, fixed set of made-up tool-call-blocked/result-blocked
//     alerts for attempts that never actually happened (so never produced
//     a data record) - tool-call-blocked for scenarios a real
//     call-authorization check would reject before the call ever ran
//     (wrong role, unknown product), result-blocked for ones only a
//     result-authorization check would catch after the call ran
//     (insufficient inventory, a double-fulfillment attempt) - see
//     CLAUDE.md's "Triggered tools / orchestration" section for
//     the real two-phase model this mirrors.
//
// Every event's timestamp comes from the underlying data's own dates (or,
// for the made-up blocked ones, a uniformly random date across the same
// window) plus a synthesized business-hours time-of-day; sorting the
// final result by time is what actually "sprinkles" every extra event
// among the real ones, rather than clustering them separately at the end.
func buildSampleEvents(rng *rand.Rand, orders []entities.OnlineCustomerOrder, supplyOrders []entities.SupplyOrder, customers []entities.OnlineCustomer, staffers []entities.Staffer, datasetStart, referenceToday time.Time) []events.Event {
	customerByID := make(map[string]entities.OnlineCustomer, len(customers))
	for _, c := range customers {
		customerByID[c.ID] = c
	}

	var fulfillmentStaffers []string // business-manager/business-staff - purchase_fulfilled
	var supplyStaffers []string      // inventory-clerk/business-staff - add_supply_order/mark_supply_order_fulfilled
	for _, s := range staffers {
		if hasRole(s.Roles, roleBizManager) || hasRole(s.Roles, roleBizStaff) {
			fulfillmentStaffers = append(fulfillmentStaffers, s.ID)
		}
		if hasRole(s.Roles, roleInventoryClerk) || hasRole(s.Roles, roleBizStaff) {
			supplyStaffers = append(supplyStaffers, s.ID)
		}
	}

	var calls []reconstructedCall

	// --- Customer orders: purchase_products (+ purchase_fulfilled) ---
	const highValueOrderThreshold = 500.0
	for _, o := range orders {
		c := customerByID[o.CustomerID]
		roles := []string{"customer", c.Type}
		orderDate := mustParseDate(o.OrderDate)

		calls = append(calls, reconstructedCall{
			toolCallID: randToolCallID(rng),
			tool:       "garden-online-store.purchase_products",
			user:       o.CustomerID,
			roles:      roles,
			request: map[string]any{
				"customerId": o.CustomerID,
				"orderDate":  o.OrderDate,
				"items": []map[string]any{
					{"productId": o.ProductID, "units": o.NumberOfRetailUnits},
				},
			},
			result:     []entities.OnlineCustomerOrder{o},
			resultTime: randEventTime(rng, orderDate),
			highValue:  o.TotalPrice >= highValueOrderThreshold,
			valueNote:  fmt.Sprintf("order %s total $%.2f exceeds the $%.0f review threshold", o.ID, o.TotalPrice, highValueOrderThreshold),
		})

		if o.FulfillmentActualDate != nil {
			fulfilledDate := mustParseDate(*o.FulfillmentActualDate)
			fulfilledBy := fulfillmentStaffers[rng.Intn(len(fulfillmentStaffers))]
			calls = append(calls, reconstructedCall{
				toolCallID: randToolCallID(rng),
				tool:       "garden-online-store.purchase_fulfilled",
				user:       fulfilledBy,
				roles:      staffRolesByID(staffers, fulfilledBy),
				request: map[string]any{
					"orderId":               o.ID,
					"fulfillmentActualDate": *o.FulfillmentActualDate,
				},
				result:     o,
				resultTime: randEventTime(rng, fulfilledDate),
			})
		}
	}

	// --- Supply orders: add_supply_order + mark_supply_order_fulfilled ---
	// (every seed supply order is fulfilled - see entities.SupplyOrder's
	// own doc comment - so unlike customer orders this pair is
	// unconditional.)
	const highValueSupplyThreshold = 5000.0
	for _, so := range supplyOrders {
		orderDate := mustParseDate(so.OrderDate)
		fulfilledDate := mustParseDate(*so.FulfillmentActualDate)
		orderedBy := supplyStaffers[rng.Intn(len(supplyStaffers))]
		fulfilledBy := supplyStaffers[rng.Intn(len(supplyStaffers))]

		unfulfilled := so
		unfulfilled.FulfillmentActualDate = nil

		calls = append(calls, reconstructedCall{
			toolCallID: randToolCallID(rng),
			tool:       "garden-online-store.add_supply_order",
			user:       orderedBy,
			roles:      staffRolesByID(staffers, orderedBy),
			request: map[string]any{
				"id":                    so.ID,
				"productSupplyId":       so.ProductSupplyID,
				"orderDate":             so.OrderDate,
				"fulfillmentTargetDate": so.FulfillmentTargetDate,
				"numberOfOrderedUnits":  so.NumberOfOrderedUnits,
			},
			result:     unfulfilled,
			resultTime: randEventTime(rng, orderDate),
			highValue:  so.TotalCost >= highValueSupplyThreshold,
			valueNote:  fmt.Sprintf("supply order %s total $%.2f exceeds the $%.0f review threshold", so.ID, so.TotalCost, highValueSupplyThreshold),
		})

		calls = append(calls, reconstructedCall{
			toolCallID: randToolCallID(rng),
			tool:       "garden-online-store.mark_supply_order_fulfilled",
			user:       fulfilledBy,
			roles:      staffRolesByID(staffers, fulfilledBy),
			request: map[string]any{
				"id":                    so.ID,
				"fulfillmentActualDate": *so.FulfillmentActualDate,
			},
			result:     so,
			resultTime: randEventTime(rng, fulfilledDate),
		})
	}

	var out []events.Event
	for _, c := range calls {
		out = append(out, events.Event{
			Type:    "tool-request-result",
			Subtype: "call-succeeded",
			Time:    c.resultTime,
			Title:   fmt.Sprintf("%s succeeded", c.tool),
			Text:    fmt.Sprintf("%s called by %s", c.tool, c.user),
			Metadata: map[string]any{
				"user":       c.user,
				"roles":      c.roles,
				"tool":       c.tool,
				"toolCallId": c.toolCallID,
				"request":    c.request,
				"result":     c.result,
			},
		})

		// ~30% of real calls also get a matching tool-request/request-ok,
		// timestamped a few seconds before their own result.
		if rng.Intn(10) < 3 {
			reqTime := c.resultTime.Add(-time.Duration(1+rng.Intn(30)) * time.Second)
			out = append(out, events.Event{
				Type:    "tool-request",
				Subtype: "request-ok",
				Time:    reqTime,
				Title:   fmt.Sprintf("%s authorized", c.tool),
				Text:    fmt.Sprintf("%s requested by %s", c.tool, c.user),
				Metadata: map[string]any{
					"user":       c.user,
					"roles":      c.roles,
					"tool":       c.tool,
					"toolCallId": c.toolCallID,
					"request":    c.request,
				},
			})
		}

		// Roughly half of unusually large calls get flagged for review -
		// the call/result still succeeded, this is a non-blocking alert.
		if c.highValue && rng.Intn(2) == 0 {
			alertTime := c.resultTime.Add(time.Duration(1+rng.Intn(10)) * time.Second)
			out = append(out, events.Event{
				Type:    "alert",
				Subtype: "tool-call-alert",
				Time:    alertTime,
				Title:   "High-value call flagged for review",
				Text:    c.valueNote,
				Metadata: map[string]any{
					"user":       c.user,
					"roles":      c.roles,
					"tool":       c.tool,
					"toolCallId": c.toolCallID,
					"reason":     c.valueNote,
				},
			})
			if rng.Intn(2) == 0 {
				out = append(out, events.Event{
					Type:    "alert",
					Subtype: "result-alert",
					Time:    alertTime.Add(time.Second),
					Title:   "High-value result flagged for review",
					Text:    c.valueNote,
					Metadata: map[string]any{
						"user":       c.user,
						"roles":      c.roles,
						"tool":       c.tool,
						"toolCallId": c.toolCallID,
						"reason":     c.valueNote,
					},
				})
			}
		}
	}

	out = append(out, madeUpBlockedAlerts(rng, orders, supplyOrders, customers, staffers, datasetStart, referenceToday)...)

	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// madeUpBlockedAlerts synthesizes a small, fixed set of alert events for
// attempts that never actually happened - so, unlike every event built in
// buildSampleEvents above, these have no corresponding order/supply-order
// record to derive from. Each still reuses real customer/staffer/product/
// supply-order ids for flavor; only the outcome (and the fact the attempt
// happened at all) is fabricated.
//
// Each scenario also produces the matching tool-request/tool-request-result
// event a real orchestrated call would have alongside its alert - not
// sprinkled/optional the way buildSampleEvents' real-call tool-request
// events are, since a blocked attempt's verdict is exactly what those two
// event types exist to record:
//   - tool-call-blocked scenarios mirror a call-authorization-style
//     rejection, decided before the real call would ever run (wrong role,
//     unknown product) - paired with a tool-request/request-blocked event
//     sharing the same toolCallId, timestamped the instant before (the
//     verdict is known first; the alert is published as a consequence of
//     it).
//   - result-blocked scenarios mirror a result-authorization-style
//     rejection (the call would have run, but its outcome violates a
//     business rule) - paired with a tool-request-result/result-blocked
//     event, same toolCallId, timestamped the instant before.
//
// See CLAUDE.md's "Triggered tools / orchestration" and
// "Tool-call correlation ID" sections for the real two-phase model and
// toolCallId-sharing convention this reflects. Timestamped uniformly at
// random across the whole dataset window, independent of any real event's
// own date.
func madeUpBlockedAlerts(rng *rand.Rand, orders []entities.OnlineCustomerOrder, supplyOrders []entities.SupplyOrder, customers []entities.OnlineCustomer, staffers []entities.Staffer, datasetStart, referenceToday time.Time) []events.Event {
	randCustomer := func() entities.OnlineCustomer { return customers[rng.Intn(len(customers))] }
	randStafferWithRole := func(roleID string) entities.Staffer {
		var pool []entities.Staffer
		for _, s := range staffers {
			if hasRole(s.Roles, roleID) {
				pool = append(pool, s)
			}
		}
		return pool[rng.Intn(len(pool))]
	}
	randTime := func() time.Time { return randEventTime(rng, randDate(rng, datasetStart, referenceToday)) }

	type blockedScenario struct {
		subtype string // "tool-call-blocked" or "result-blocked"
		tool    string
		user    string
		roles   []string
		request map[string]any
		reason  string
	}

	unauthorizedCustomer := randCustomer()
	crossCustomerViewer := randCustomer()
	otherCustomer := randCustomer()
	hrRestrictedStaffer := randStafferWithRole(roleInventoryClerk)
	insufficientInventoryCustomer := randCustomer()
	alreadyFulfilled := supplyOrders[rng.Intn(len(supplyOrders))]
	doubleFulfillBy := randStafferWithRole(roleInventoryClerk)
	unknownProductCustomer := randCustomer()

	scenarios := []blockedScenario{
		{
			subtype: "tool-call-blocked",
			tool:    "garden-online-store.add_supply_order",
			user:    unauthorizedCustomer.ID,
			roles:   []string{"customer", unauthorizedCustomer.Type},
			request: map[string]any{"productSupplyId": "ps-001", "numberOfOrderedUnits": 20},
			reason:  fmt.Sprintf("role %q is not authorized to call garden-online-store.add_supply_order", "customer"),
		},
		{
			subtype: "tool-call-blocked",
			tool:    "garden-customer-service.get_customer_service_calls",
			user:    crossCustomerViewer.ID,
			roles:   []string{"customer", crossCustomerViewer.Type},
			request: map[string]any{"customerId": otherCustomer.ID},
			reason:  fmt.Sprintf("customer %q may only view their own service call history, not customer %q's", crossCustomerViewer.ID, otherCustomer.ID),
		},
		{
			subtype: "tool-call-blocked",
			tool:    "garden-staff-admin.add_or_update_staffer",
			user:    hrRestrictedStaffer.ID,
			roles:   hrRestrictedStaffer.Roles,
			request: map[string]any{"name": "New Hire", "roles": []string{roleHRStaffer}},
			reason:  fmt.Sprintf("role %q is not authorized to call garden-staff-admin.add_or_update_staffer", roleInventoryClerk),
		},
		{
			subtype: "result-blocked",
			tool:    "garden-online-store.purchase_products",
			user:    insufficientInventoryCustomer.ID,
			roles:   []string{"customer", insufficientInventoryCustomer.Type},
			request: map[string]any{"customerId": insufficientInventoryCustomer.ID, "items": []map[string]any{{"productId": "prod-24", "units": 12}}},
			reason:  "insufficient inventory for product \"prod-24\": requested 12, only 3 in stock",
		},
		{
			subtype: "result-blocked",
			tool:    "garden-online-store.mark_supply_order_fulfilled",
			user:    doubleFulfillBy.ID,
			roles:   doubleFulfillBy.Roles,
			request: map[string]any{"id": alreadyFulfilled.ID, "fulfillmentActualDate": *alreadyFulfilled.FulfillmentActualDate},
			reason:  fmt.Sprintf("mark_supply_order_fulfilled: supply order %q is already fulfilled", alreadyFulfilled.ID),
		},
		{
			subtype: "result-blocked",
			tool:    "garden-online-store.purchase_products",
			user:    unknownProductCustomer.ID,
			roles:   []string{"customer", unknownProductCustomer.Type},
			request: map[string]any{"customerId": unknownProductCustomer.ID, "items": []map[string]any{{"productId": "prod-99", "units": 1}}},
			reason:  "could not determine a price for unknown product \"prod-99\"",
		},
	}

	out := make([]events.Event, 0, len(scenarios)*2)
	for _, s := range scenarios {
		toolCallID := randToolCallID(rng)
		verdictTime := randTime()
		alertTime := verdictTime.Add(time.Second)

		switch s.subtype {
		case "tool-call-blocked":
			out = append(out, events.Event{
				Type:    "tool-request",
				Subtype: "request-blocked",
				Time:    verdictTime,
				Title:   fmt.Sprintf("%s blocked", s.tool),
				Text:    s.reason,
				Metadata: map[string]any{
					"user":       s.user,
					"roles":      s.roles,
					"tool":       s.tool,
					"toolCallId": toolCallID,
					"request":    s.request,
					"reason":     s.reason,
				},
			})
		case "result-blocked":
			out = append(out, events.Event{
				Type:    "tool-request-result",
				Subtype: "result-blocked",
				Time:    verdictTime,
				Title:   fmt.Sprintf("%s result blocked", s.tool),
				Text:    s.reason,
				Metadata: map[string]any{
					"user":       s.user,
					"roles":      s.roles,
					"tool":       s.tool,
					"toolCallId": toolCallID,
					"request":    s.request,
					"reason":     s.reason,
				},
			})
		}

		out = append(out, events.Event{
			Type:    "alert",
			Subtype: s.subtype,
			Time:    alertTime,
			Title:   fmt.Sprintf("Blocked: %s", s.tool),
			Text:    s.reason,
			Metadata: map[string]any{
				"user":       s.user,
				"roles":      s.roles,
				"tool":       s.tool,
				"toolCallId": toolCallID,
				"reason":     s.reason,
			},
		})
	}
	return out
}
