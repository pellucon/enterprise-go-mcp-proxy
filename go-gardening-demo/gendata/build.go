// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
)

// buildProductSupply builds the 50 ProductSupply rows over the 30
// products in the products slice: the first 10 (index < 10) each get 3
// distinct suppliers (rotated through the suppliers pool so they're never
// repeated for one product), the remaining 20 get 1 supplier each - see
// staticdata.go's products doc comment for why this yields exactly 50
// rows. CostToUserPerRetailUnit is computed once per product and reused
// for every row of that product, since the customer-facing price
// shouldn't vary by supplier - and specifically computed off whichever of
// that product's suppliers has the highest per-retail-unit wholesale cost
// (not just an arbitrary one), so the store is never selling below cost
// regardless of which supplier happens to be fulfilling a given order.
func buildProductSupply(rng *rand.Rand) []entities.ProductSupply {
	var rows []entities.ProductSupply
	counter := 0
	for i, p := range products {
		numSuppliers := 1
		if i < 10 {
			numSuppliers = 3
		}

		type supplyEconomics struct {
			supplier              string
			unitsPerWholesale     int
			wholesaleCost         float64
			costPerRetailUnitPaid float64
		}
		econ := make([]supplyEconomics, numSuppliers)
		worstCostPerRetailUnit := 0.0
		for s := 0; s < numSuppliers; s++ {
			unitsPerWholesale := rng.Intn(20) + 6           // 6..25
			wholesaleCost := round2(40 + rng.Float64()*280) // $40..$320
			costPerRetailUnitPaid := wholesaleCost / float64(unitsPerWholesale)
			econ[s] = supplyEconomics{
				supplier:              suppliers[(i+s)%len(suppliers)],
				unitsPerWholesale:     unitsPerWholesale,
				wholesaleCost:         wholesaleCost,
				costPerRetailUnitPaid: costPerRetailUnitPaid,
			}
			if costPerRetailUnitPaid > worstCostPerRetailUnit {
				worstCostPerRetailUnit = costPerRetailUnitPaid
			}
		}

		markup := 1.2 + rng.Float64()*0.3 // 1.2x..1.5x (20%..50% margin) over the worst-case supplier
		retailPrice := roundToNinetyNine(worstCostPerRetailUnit * markup)

		for _, e := range econ {
			counter++
			rows = append(rows, entities.ProductSupply{
				ID:                      fmt.Sprintf("ps-%03d", counter),
				ProductID:               p.ID,
				Name:                    p.Name,
				SupplierName:            e.supplier,
				UnitsPerWholesaleUnit:   e.unitsPerWholesale,
				CostPerWholesaleUnit:    e.wholesaleCost,
				CostToUserPerRetailUnit: retailPrice,
			})
		}
	}
	return rows
}

// buildSupplyOrders builds 100 restock orders against random ProductSupply
// rows, all fulfilled (see SupplyOrder's doc comment). Order dates are kept
// at least 25 days before referenceToday so a fulfillment target/actual
// date has room to land before "today" in the common case.
func buildSupplyOrders(rng *rand.Rand, supply []entities.ProductSupply, datasetStart, referenceToday time.Time) []entities.SupplyOrder {
	latestOrderDate := referenceToday.AddDate(0, 0, -25)

	orders := make([]entities.SupplyOrder, 100)
	for i := range orders {
		ps := supply[rng.Intn(len(supply))]
		orderDate := randDate(rng, datasetStart, latestOrderDate)
		targetDate := orderDate.AddDate(0, 0, 5+rng.Intn(17)) // +5..+21 days

		actualDate := targetDate.AddDate(0, 0, -3+rng.Intn(11)) // target -3..+7 days
		if actualDate.Before(orderDate) {
			actualDate = orderDate
		}
		if actualDate.After(referenceToday) {
			actualDate = referenceToday
		}

		units := rng.Intn(36) + 5 // 5..40 wholesale units
		actualDateStr := fmtDate(actualDate)
		orders[i] = entities.SupplyOrder{
			ID:                    fmt.Sprintf("so-%04d", i+1),
			ProductSupplyID:       ps.ID,
			OrderDate:             fmtDate(orderDate),
			FulfillmentTargetDate: fmtDate(targetDate),
			FulfillmentActualDate: &actualDateStr,
			NumberOfOrderedUnits:  units,
			TotalCost:             round2(float64(units) * ps.CostPerWholesaleUnit),
		}
	}
	return orders
}

// buildOnlineCustomers builds 300 customers: exactly 150 (50%) returning,
// 15 (5%) VIP, 135 (45%) one-time, and (an assumption not specified by the
// spec, see ../README.md) 135 (45%) neutral / 105 (35%) happy / 60 (20%)
// unhappy sentiment - both distributions are built as fixed-count slices
// and then shuffled, so the exact proportions hold regardless of rng
// draws.
func buildOnlineCustomers(rng *rand.Rand) []entities.OnlineCustomer {
	types := make([]string, 0, 300)
	for i := 0; i < 150; i++ {
		types = append(types, customerTypeReturning)
	}
	for i := 0; i < 15; i++ {
		types = append(types, customerTypeVIP)
	}
	for i := 0; i < 135; i++ {
		types = append(types, customerTypeOneTime)
	}
	rng.Shuffle(len(types), func(i, j int) { types[i], types[j] = types[j], types[i] })

	sentiments := make([]string, 0, 300)
	for i := 0; i < 135; i++ {
		sentiments = append(sentiments, sentimentNeutral)
	}
	for i := 0; i < 105; i++ {
		sentiments = append(sentiments, sentimentHappy)
	}
	for i := 0; i < 60; i++ {
		sentiments = append(sentiments, sentimentUnhappy)
	}
	rng.Shuffle(len(sentiments), func(i, j int) { sentiments[i], sentiments[j] = sentiments[j], sentiments[i] })

	customers := make([]entities.OnlineCustomer, 300)
	for i := range customers {
		name := firstNames[rng.Intn(len(firstNames))] + " " + lastNames[rng.Intn(len(lastNames))]
		address := fmt.Sprintf("%d %s, %s", rng.Intn(900)+10, streetNames[rng.Intn(len(streetNames))], cityStateZips[rng.Intn(len(cityStateZips))])
		phone := fmt.Sprintf("555-%03d-%04d", rng.Intn(900)+100, rng.Intn(9000)+1000)
		customers[i] = entities.OnlineCustomer{
			ID:        fmt.Sprintf("cust-%04d", i+1),
			Details:   entities.ContactDetails{Name: name, Address: address, Phone: phone},
			Type:      types[i],
			Sentiment: sentiments[i],
		}
	}
	return customers
}

func buildCustomerTypes() []entities.CustomerType { return customerTypes }

func buildCustomerSentiments() []entities.CustomerSentiment { return customerSentiments }

func buildStaffRoles() []entities.StaffRole { return staffRoles }

func buildStaffers() []entities.Staffer {
	out := make([]entities.Staffer, len(staffers))
	for i, s := range staffers {
		out[i] = entities.Staffer{
			ID:      s.ID,
			Details: entities.ContactDetails{Name: s.Name, Address: s.Address, Phone: s.Phone},
			Roles:   s.Roles,
		}
	}
	return out
}

func buildDeals() []entities.Deal {
	out := make([]entities.Deal, len(dealDefs))
	for i, d := range dealDefs {
		out[i] = entities.Deal{
			ID:                  d.ID,
			Name:                d.Name,
			StartDate:           optionalDate(d.Start),
			EndDate:             optionalDate(d.End),
			IDs:                 d.IDs,
			DiscountPercentages: d.DiscountPercentages,
		}
	}
	return out
}

// optionalDate turns dealDefs' ""-means-unbounded string convention into
// entities.Deal's own nil-means-unbounded *string convention.
func optionalDate(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// retailPriceByProduct maps a product id to its (supplier-independent)
// retail price - see buildProductSupply's doc comment on why every row for
// a given product shares one price.
func retailPriceByProduct(supply []entities.ProductSupply) map[string]float64 {
	m := make(map[string]float64, len(products))
	for _, ps := range supply {
		if _, ok := m[ps.ProductID]; !ok {
			m[ps.ProductID] = ps.CostToUserPerRetailUnit
		}
	}
	return m
}

// supplyProductIDIndex maps a ProductSupply row id (e.g. "ps-022") to its
// ProductID - used by activeDeal to resolve a deal's row-id list back to
// the products it covers.
func supplyProductIDIndex(supply []entities.ProductSupply) map[string]string {
	m := make(map[string]string, len(supply))
	for _, ps := range supply {
		m[ps.ID] = ps.ProductID
	}
	return m
}

// supplyCapacityByProduct returns each product's total baseline retail-
// unit capacity from supplyOrders - garden-online-store's own
// load_supply_orders stock-accumulation formula (NumberOfOrderedUnits *
// UnitsPerWholesaleUnit), mirrored here at generation time so
// buildOnlineCustomerOrders can keep total demand for a product from
// exceeding what buildSupplyOrders actually supplied it. Every generated
// supply order is already fulfilled (see SupplyOrder's own doc comment),
// so none needs filtering out here.
func supplyCapacityByProduct(supplyOrders []entities.SupplyOrder, supply []entities.ProductSupply) map[string]int {
	unitsPerWholesale := make(map[string]int, len(supply))
	productIDBySupplyID := make(map[string]string, len(supply))
	for _, ps := range supply {
		unitsPerWholesale[ps.ID] = ps.UnitsPerWholesaleUnit
		productIDBySupplyID[ps.ID] = ps.ProductID
	}
	capacity := make(map[string]int, len(products))
	for _, so := range supplyOrders {
		capacity[productIDBySupplyID[so.ProductSupplyID]] += so.NumberOfOrderedUnits * unitsPerWholesale[so.ProductSupplyID]
	}
	return capacity
}

// weightedCustomerPool biases order-placement toward VIP (3x) and
// returning (2x) customers over one-time (1x) customers - a reasonable,
// if unspecified, assumption: repeat/VIP customers ordering more often
// than one-time visitors is what "returning"/"VIP" would actually mean in
// practice.
func weightedCustomerPool(customers []entities.OnlineCustomer) []int {
	var pool []int
	for i, c := range customers {
		weight := 1
		switch c.Type {
		case customerTypeVIP:
			weight = 3
		case customerTypeReturning:
			weight = 2
		}
		for w := 0; w < weight; w++ {
			pool = append(pool, i)
		}
	}
	return pool
}

// activeDeal returns the first deal covering productID, eligible for
// customerType (see entities.Deal.DiscountFor - a deal like the always-on
// VIP loyalty program simply doesn't match a non-vip customer at all),
// whose date range contains orderDate (a nil StartDate/EndDate means no
// bound on that side - see entities.Deal's own doc comment). Deal windows
// never overlap for the 4 seasonal deals (see staticdata.go's dealDefs),
// so among those "first match" is also "only match"; the always-on VIP
// deal can still coexist with a seasonal one covering the same row, in
// which case whichever is listed first in dealDefs wins - deliberately
// "first configured deal wins", not "best discount wins", matching how a
// real single-deal-per-row storefront rule would work. A deal's IDs name
// ProductSupply row ids (see entities.Deal's own doc comment), but an
// order only ever carries a bare productID (see go-gardening-demo/
// README.md's "a customer orders a product, not a specific supplier's
// batch") - so matching goes through productSupplyProductID to resolve
// each of a deal's row ids back to the product it belongs to.
func activeDeal(deals []entities.Deal, productSupplyProductID map[string]string, productID, customerType string, orderDate time.Time) (id string, discountPercentage float64, ok bool) {
	for _, d := range deals {
		if d.StartDate != nil {
			start, _ := time.Parse(dateLayout, *d.StartDate)
			if orderDate.Before(start) {
				continue
			}
		}
		if d.EndDate != nil {
			end, _ := time.Parse(dateLayout, *d.EndDate)
			if orderDate.After(end) {
				continue
			}
		}
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

// buildOnlineCustomerOrders builds 500 retail orders. The 50 (10%) most
// recently placed are left without a FulfillmentActualDate ("not yet
// fulfilled" - matches the spec exactly, unlike SupplyOrder which the spec
// never says has any unfulfilled ones). Orders are drafted first, sorted
// by date descending to pick that most-recent-50% tail, then re-sorted
// ascending and re-numbered so ids read chronologically.
func buildOnlineCustomerOrders(rng *rand.Rand, customers []entities.OnlineCustomer, supply []entities.ProductSupply, supplyOrders []entities.SupplyOrder, deals []entities.Deal, datasetStart, referenceToday time.Time) []entities.OnlineCustomerOrder {
	prices := retailPriceByProduct(supply)
	productSupplyProductID := supplyProductIDIndex(supply)
	customerPool := weightedCustomerPool(customers)

	type draft struct {
		orderDate    time.Time
		customerID   string
		customerType string
		productID    string
		units        int
	}

	// capacity/affordableIDs keep total demand drafted for a product from
	// exceeding what buildSupplyOrders actually supplied it - otherwise
	// garden-online-store's load_historical_purchases (which nets every
	// already-fulfilled order here out of the stock baseline it builds
	// from supply_orders.json) could drive a product's baseline negative
	// before any real purchase_products call ever happens, e.g. one of
	// the 20 single-supplier products (see productSupplyIDs' own doc
	// comment) drawing more of the uniformly-random 500 orders than its
	// comparatively small supply-order share can cover. Deliberately
	// conservative: every draft counts against capacity, including the
	// ~10% that end up unfulfilled (and so never actually net) - which
	// drafts land in that unfulfilled tail depends on order date, decided
	// only after this loop (see the sort.Slice below), so there's no way
	// to know in advance which ones can be exempted; reserving a little
	// extra headroom for those is harmless.
	capacity := supplyCapacityByProduct(supplyOrders, supply)
	affordableIDs := make([]string, 0, len(products))
	for _, p := range products {
		if capacity[p.ID] > 0 {
			affordableIDs = append(affordableIDs, p.ID)
		}
	}

	drafts := make([]draft, 500)
	for i := range drafts {
		c := customers[customerPool[rng.Intn(len(customerPool))]]

		if len(affordableIDs) == 0 {
			// Total supply-order volume (100 orders * 5..40 units *
			// 6..25 units/wholesale) is roughly an order of magnitude
			// larger than total possible demand here (500 orders * up to
			// 5 units), so exhausting every product at once isn't
			// expected to happen in practice - fail loudly rather than
			// silently drawing from an empty pool if it ever does.
			panic("gendata: exhausted supply capacity for every product while drafting online customer orders")
		}
		pickIdx := rng.Intn(len(affordableIDs))
		pid := affordableIDs[pickIdx]

		units := rng.Intn(5) + 1 // 1..5, same range as before
		if units > capacity[pid] {
			units = capacity[pid]
		}
		capacity[pid] -= units
		if capacity[pid] <= 0 {
			affordableIDs[pickIdx] = affordableIDs[len(affordableIDs)-1]
			affordableIDs = affordableIDs[:len(affordableIDs)-1]
		}

		drafts[i] = draft{
			orderDate:    randDate(rng, datasetStart, referenceToday),
			customerID:   c.ID,
			customerType: c.Type,
			productID:    pid,
			units:        units,
		}
	}
	sort.Slice(drafts, func(i, j int) bool { return drafts[i].orderDate.After(drafts[j].orderDate) })

	const unfulfilledCount = 50 // 10% of 500
	// maxDealOrders caps how many orders may be priced under a Deal at no
	// more than 25% of all orders - enforced explicitly here (rather than
	// left as an incidental property of how much the random order dates
	// happen to overlap the 4 deal windows) so the cap holds regardless of
	// rng draws or future changes to deal windows/product coverage. An
	// order whose date/product would otherwise qualify, once the cap is
	// reached, is simply priced at full price with no DealID - same as a
	// customer who didn't apply the deal.
	maxDealOrders := len(drafts) / 4 // 25% of 500 = 125
	dealOrdersApplied := 0

	orders := make([]entities.OnlineCustomerOrder, len(drafts))
	for i, d := range drafts {
		basePrice := prices[d.productID]
		subtotal := round2(basePrice * float64(d.units))
		total := subtotal
		var dealID *string
		if id, discount, ok := activeDeal(deals, productSupplyProductID, d.productID, d.customerType, d.orderDate); ok && dealOrdersApplied < maxDealOrders {
			total = round2(subtotal * (1 - discount/100))
			dealIDCopy := id
			dealID = &dealIDCopy
			dealOrdersApplied++
		}

		targetDate := d.orderDate.AddDate(0, 0, 3+rng.Intn(8)) // +3..+10 days

		var actualDate *string
		if i < unfulfilledCount {
			// drafts is sorted most-recent-first (see the sort.Slice call
			// above), so the first unfulfilledCount entries are the
			// unfulfilledCount most recently placed orders: leave
			// FulfillmentActualDate nil.
		} else {
			actual := targetDate.AddDate(0, 0, -2+rng.Intn(8)) // target -2..+5 days
			if actual.Before(d.orderDate) {
				actual = d.orderDate
			}
			if actual.After(referenceToday) {
				actual = referenceToday
			}
			s := fmtDate(actual)
			actualDate = &s
		}

		orders[i] = entities.OnlineCustomerOrder{
			CustomerID:            d.customerID,
			ProductID:             d.productID,
			OrderDate:             fmtDate(d.orderDate),
			FulfillmentTargetDate: fmtDate(targetDate),
			FulfillmentActualDate: actualDate,
			NumberOfRetailUnits:   d.units,
			TotalPrice:            total,
			DealID:                dealID,
		}
	}

	sort.Slice(orders, func(i, j int) bool { return orders[i].OrderDate < orders[j].OrderDate })
	for i := range orders {
		orders[i].ID = fmt.Sprintf("ord-%04d", i+1)
	}
	return orders
}

// hasRole reports whether roles contains roleID.
func hasRole(roles []string, roleID string) bool {
	for _, r := range roles {
		if r == roleID {
			return true
		}
	}
	return false
}

// buildCustomerServiceCalls builds 30 support calls, each tied to a real
// online customer who has actually placed at least one order (so the call
// is traceable, not floating - see CustomerServiceCall's doc comment) and,
// when its issue template references a product, the name of a product
// that customer actually ordered.
func buildCustomerServiceCalls(rng *rand.Rand, orders []entities.OnlineCustomerOrder, staffersOut []entities.Staffer, datasetStart, referenceToday time.Time) []entities.CustomerServiceCall {
	productNameByID := make(map[string]string, len(products))
	for _, p := range products {
		productNameByID[p.ID] = p.Name
	}

	customerOrderedProduct := make(map[string]string) // customerID -> a product name they ordered
	var orderingCustomerIDs []string
	for _, o := range orders {
		if _, seen := customerOrderedProduct[o.CustomerID]; !seen {
			orderingCustomerIDs = append(orderingCustomerIDs, o.CustomerID)
		}
		customerOrderedProduct[o.CustomerID] = productNameByID[o.ProductID]
	}

	var supportIDs []string
	managerID := ""
	for _, s := range staffersOut {
		if hasRole(s.Roles, roleDeactivated) {
			// A deactivated staffer keeps their role history (see
			// staticdata.go's staffers) but is no longer eligible to be
			// assigned a new call.
			continue
		}
		for _, r := range s.Roles {
			if r == roleCustSupport {
				supportIDs = append(supportIDs, s.ID)
			}
			if r == roleCustSupportManager {
				managerID = s.ID
			}
		}
	}

	calls := make([]entities.CustomerServiceCall, 30)
	for i := range calls {
		customerID := orderingCustomerIDs[rng.Intn(len(orderingCustomerIDs))]
		productName := customerOrderedProduct[customerID]
		template := issueTemplates[rng.Intn(len(issueTemplates))]
		issueText := template
		if strings.Contains(template, "%s") {
			issueText = fmt.Sprintf(template, productName)
		}

		callDate := randDate(rng, datasetStart, referenceToday)
		handledBy := supportIDs[rng.Intn(len(supportIDs))]

		var resolutionDate *string
		if i%10 != 0 { // ~90% (27/30) resolved - a handful of open tickets is realistic
			resolved := callDate.AddDate(0, 0, rng.Intn(6)) // same day..+5 days
			if resolved.After(referenceToday) {
				resolved = referenceToday
			}
			s := fmtDate(resolved)
			resolutionDate = &s
		}

		escalated := rng.Intn(5) == 0 // ~20% (6/30)
		var escalatedTo *string
		if escalated {
			id := managerID
			escalatedTo = &id
		}

		calls[i] = entities.CustomerServiceCall{
			CustomerID:           customerID,
			HandledByStafferID:   handledBy,
			CallDate:             fmtDate(callDate),
			IssueText:            issueText,
			ResolutionDate:       resolutionDate,
			EscalatedToManager:   escalated,
			EscalatedToStafferID: escalatedTo,
		}
	}

	sort.Slice(calls, func(i, j int) bool { return calls[i].CallDate < calls[j].CallDate })
	for i := range calls {
		calls[i].ID = fmt.Sprintf("csc-%02d", i+1)
	}
	return calls
}
