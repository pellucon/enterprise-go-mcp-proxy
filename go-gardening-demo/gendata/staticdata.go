// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"

	"github.com/pellucon/enterprise-go-mcp-proxy/go-gardening-demo/entities"
)

// products is the fixed catalog of 30 canonical gardening-equipment
// products. Index order matters: productSuppliers below assigns the first
// 10 three suppliers each and the remaining 20 a single supplier each, so
// that 10*3 + 20*1 = 50 ProductSupply rows total, over 30 unique products
// (matches the spec's "50 products in ProductSupply (30 unique products)
// ... some are supplied by up to 3 suppliers").
var products = []entities.Product{
	{ID: "prod-01", Name: "Steel Garden Spade"},
	{ID: "prod-02", Name: "Ergonomic Hand Trowel"},
	{ID: "prod-03", Name: "Bypass Hand Pruners"},
	{ID: "prod-04", Name: "Anvil Loppers"},
	{ID: "prod-05", Name: "Leather Garden Gloves"},
	{ID: "prod-06", Name: "Adjustable Garden Rake"},
	{ID: "prod-07", Name: "Bow Rake"},
	{ID: "prod-08", Name: "50ft Expandable Garden Hose"},
	{ID: "prod-09", Name: "Heavy Duty Hose Reel Cart"},
	{ID: "prod-10", Name: "Brass 8-Pattern Hose Nozzle"},
	{ID: "prod-11", Name: "Galvanized Steel Watering Can (2 Gal)"},
	{ID: "prod-12", Name: "Plastic Watering Can (1 Gal)"},
	{ID: "prod-13", Name: "Steel Wheelbarrow (6 cu ft)"},
	{ID: "prod-14", Name: "Poly Wheelbarrow (4 cu ft)"},
	{ID: "prod-15", Name: "Folding Hand Saw"},
	{ID: "prod-16", Name: "Hedge Shears"},
	{ID: "prod-17", Name: "Garden Kneeler and Seat"},
	{ID: "prod-18", Name: "Bamboo Plant Stakes (Pack of 25)"},
	{ID: "prod-19", Name: "Tomato Cages (Pack of 6)"},
	{ID: "prod-20", Name: "Organic All-Purpose Fertilizer (10 lb)"},
	{ID: "prod-21", Name: "Slow-Release Granular Fertilizer (20 lb)"},
	{ID: "prod-22", Name: "Heirloom Tomato Seeds Variety Pack"},
	{ID: "prod-23", Name: "Wildflower Seed Mix"},
	{ID: "prod-24", Name: "Raised Garden Bed Kit (4x4)"},
	{ID: "prod-25", Name: "Compost Bin (80 Gal)"},
	{ID: "prod-26", Name: "Garden Cart with Removable Sides"},
	{ID: "prod-27", Name: "Soil pH Test Kit"},
	{ID: "prod-28", Name: "Sun Hat and Gardening Apron Set"},
	{ID: "prod-29", Name: "Long Handle Digging Fork"},
	{ID: "prod-30", Name: "Telescoping Tree Pruner"},
}

// productSupplyIDs returns the ProductSupply row ids buildProductSupply
// generates for the product at the given 0-based index into products
// (products[0] -> index 0, i.e. prod-01) - replicating its counter logic
// (the first 10 products get 3 rows each, the rest get 1) without needing
// an *rand.Rand, since id assignment itself doesn't depend on any of
// buildProductSupply's random draws, only on index order. Used by
// dealDefs below to express deal coverage as ProductSupply row ids
// (matching entities.Deal.IDs) while still reading as "which products"
// in the source.
func productSupplyIDs(productIndex int) []string {
	rowsBefore := 0
	for i := 0; i < productIndex; i++ {
		if i < 10 {
			rowsBefore += 3
		} else {
			rowsBefore++
		}
	}
	count := 1
	if productIndex < 10 {
		count = 3
	}
	ids := make([]string, count)
	for i := 0; i < count; i++ {
		ids[i] = fmt.Sprintf("ps-%03d", rowsBefore+i+1)
	}
	return ids
}

// concatIDs flattens multiple productSupplyIDs results into one []string,
// for a deal that covers more than one product.
func concatIDs(idLists ...[]string) []string {
	var out []string
	for _, ids := range idLists {
		out = append(out, ids...)
	}
	return out
}

// suppliers is the pool of wholesale suppliers ProductSupply rows draw
// from.
var suppliers = []string{
	"GreenThumb Wholesale",
	"EarthWorks Supply Co.",
	"Bloom & Branch Distributors",
	"Harvest Trail Supply",
	"TerraCraft Wholesale",
	"Meadowline Distributors",
}

// customerTypeIDs / customerTypes back CustomerType lookup entries and
// OnlineCustomer.Type values. Order matters for buildOnlineCustomers'
// proportional assignment.
const (
	customerTypeOneTime   = "one-time-customer"
	customerTypeReturning = "returning-customer"
	customerTypeVIP       = "vip"
)

var customerTypes = []entities.CustomerType{
	{ID: customerTypeOneTime, Name: "One-Time Customer"},
	{ID: customerTypeReturning, Name: "Returning Customer"},
	{ID: customerTypeVIP, Name: "VIP"},
}

const (
	sentimentNeutral = "neutral"
	sentimentHappy   = "happy"
	sentimentUnhappy = "unhappy"
)

var customerSentiments = []entities.CustomerSentiment{
	{ID: sentimentNeutral, Name: "Neutral"},
	{ID: sentimentHappy, Name: "Happy"},
	{ID: sentimentUnhappy, Name: "Unhappy"},
}

// Staff role ids. roleDeactivated is a later addition (not in the original
// spec) marking a staffer as no longer active, held alongside a staffer's
// existing functional role(s) rather than replacing them - see staffers
// below. This whole set (roleAdmin excepted, see below) replaced an earlier
// role catalog (customer-service-representative/-manager, it-operations,
// business-admin, business-analyst) entirely, at the user's request, driven
// by the staffer-roles spec - a real per-tool
// role-based access model (see gendata/accessrules.go's
// buildClientAccessRules, which is the actual source of truth for who can
// call what; staffRoles here is just a descriptive reference list, never
// validated against).
//
// roleAdmin is included in staffRoles this time - a reversal of an earlier
// design decision that deliberately excluded it as "not a store-specific
// role": the staffer-roles spec's own headcount table lists "admin" on equal
// footing with every other role (1 staffer, same as the others), so it's
// listed here too now. It's still also go-proxy's own system-recognized
// role (proxy-server.cfg's clientAccessRules grants role "admin"
// unrestricted access, and demo-authorization-server's built-in "admin"
// user already holds it) - a staffer holding it gets real go-proxy admin
// access once garden-staff-admin syncs their roles to
// demo-authorization-server.
const (
	roleAdmin              = "admin"
	roleBizManager         = "business-manager"
	roleCustSupportManager = "customer-support-manager"
	roleCustManagement     = "customer-management"
	roleCustSupport        = "customer-support"
	roleHRManager          = "human_resource_manager"
	roleHRStaffer          = "human_resource_staffer"
	roleInventoryClerk     = "inventory-clerk"
	roleBizStaff           = "business-staff"
	roleAnalyst            = "analyst"
	roleDeactivated        = "deactivated"
)

var staffRoles = []entities.StaffRole{
	{ID: roleAdmin, Name: "Admin"},
	{ID: roleBizManager, Name: "Business Manager"},
	{ID: roleCustSupportManager, Name: "Customer Support Manager"},
	{ID: roleCustManagement, Name: "Customer Management"},
	{ID: roleCustSupport, Name: "Customer Support"},
	{ID: roleHRManager, Name: "Human Resource Manager"},
	{ID: roleHRStaffer, Name: "Human Resource Staffer"},
	{ID: roleInventoryClerk, Name: "Inventory Clerk"},
	{ID: roleBizStaff, Name: "Business Staff"},
	{ID: roleAnalyst, Name: "Analyst"},
	{ID: roleDeactivated, Name: "Deactivated"},
}

// staffers holds the staffer-roles spec's original 14-employee headcount table
// exactly: 1 admin, 1 human_resource_manager, 1 business-manager, 1
// business-manager+customer-support-manager, 2
// customer-management+customer-support, 2 human_resource_staffer, 2
// customer-support, 1 inventory-clerk+business-staff, 3 business-staff (9
// distinct roles each covered by at least one staffer), plus staff-15 -
// added later, outside that spec, holding the newer roleAnalyst - for 15
// total and 10 distinct roles. The spec's "cutomer-support"/
// "customer_support" (two spellings for what's clearly the same role) is
// canonicalized to roleCustSupport - confirmed with the user directly.
// The first 8 reuse the previous roster's names (now holding different
// roles); staff-09..15 are newly invented, same style (fictional US
// address, 555-201-10NN phone).
//
// No staffer currently holds roleDeactivated - see its own doc comment
// above.
var staffers = []struct {
	ID      string
	Name    string
	Address string
	Phone   string
	Roles   []string
}{
	{ID: "staff-01", Name: "Morgan Ellery", Address: "12 Birch Rd, Columbus, OH 43215", Phone: "555-201-1001", Roles: []string{roleAdmin}},
	{ID: "staff-02", Name: "Priya Anand", Address: "44 Cedar Ln, Denver, CO 80203", Phone: "555-201-1002", Roles: []string{roleHRManager}},
	{ID: "staff-03", Name: "Jordan Lee", Address: "8 Maple St, Austin, TX 73301", Phone: "555-201-1003", Roles: []string{roleBizManager}},
	{ID: "staff-04", Name: "Sam Whitfield", Address: "271 Oak Ave, Portland, OR 97201", Phone: "555-201-1004", Roles: []string{roleBizManager, roleCustSupportManager}},
	{ID: "staff-05", Name: "Taylor Brooks", Address: "19 Willow Dr, Raleigh, NC 27601", Phone: "555-201-1005", Roles: []string{roleCustManagement, roleCustSupport}},
	{ID: "staff-06", Name: "Devon Ortiz", Address: "530 Pine Way, Madison, WI 53703", Phone: "555-201-1006", Roles: []string{roleCustManagement, roleCustSupport}},
	{ID: "staff-07", Name: "Casey Nguyen", Address: "77 Aspen Blvd, Boise, ID 83702", Phone: "555-201-1007", Roles: []string{roleHRStaffer}},
	{ID: "staff-08", Name: "Riley Foster", Address: "6 Magnolia Ter, Nashville, TN 37201", Phone: "555-201-1008", Roles: []string{roleHRStaffer}},
	{ID: "staff-09", Name: "Elena Marsh", Address: "142 Cypress Ct, Tampa, FL 33602", Phone: "555-201-1009", Roles: []string{roleCustSupport}},
	{ID: "staff-10", Name: "Marcus Webb", Address: "58 Redwood Ave, Sacramento, CA 95814", Phone: "555-201-1010", Roles: []string{roleCustSupport}},
	{ID: "staff-11", Name: "Nina Patel", Address: "9 Larkspur Ln, Tucson, AZ 85701", Phone: "555-201-1011", Roles: []string{roleInventoryClerk, roleBizStaff}},
	{ID: "staff-12", Name: "Owen Kaczmarek", Address: "233 Hawthorne St, Louisville, KY 40202", Phone: "555-201-1012", Roles: []string{roleBizStaff}},
	{ID: "staff-13", Name: "Bianca Solis", Address: "77 Driftwood Dr, Albuquerque, NM 87102", Phone: "555-201-1013", Roles: []string{roleBizStaff}},
	{ID: "staff-14", Name: "Trevor Lindqvist", Address: "14 Meadowbrook Rd, Omaha, NE 68102", Phone: "555-201-1014", Roles: []string{roleBizStaff}},
	{ID: "staff-15", Name: "Harper Douglas", Address: "21 Fairview Ct, Spokane, WA 99201", Phone: "555-201-1015", Roles: []string{roleAnalyst}},
}

// discountEscalation is how much more of a discount returning-customer and
// vip customers get over one-time-customer on the same deal, expressed as
// additional percentage points on top of the base (one-time-customer) rate
// - not specified per-deal below, so every seasonal deal in dealDefs uses
// the same schedule via escalateDiscount. The always-on VIP-only deal
// doesn't go through this at all - it names only "vip" in its own
// DiscountPercentages map, deliberately excluding the other two types.
const (
	discountEscalationReturning = 5.0  // returning-customer = one-time-customer + 5
	discountEscalationVIP       = 10.0 // vip = one-time-customer + 10 (5 more than returning)
)

// escalateDiscount builds the {one-time-customer, returning-customer, vip}
// DiscountPercentages map for a deal from a single base rate (the
// discount a one-time customer gets), applying discountEscalationReturning/
// VIP on top - see their own doc comment for the schedule.
func escalateDiscount(base float64) map[string]float64 {
	return map[string]float64{
		customerTypeOneTime:   base,
		customerTypeReturning: base + discountEscalationReturning,
		customerTypeVIP:       base + discountEscalationVIP,
	}
}

// allProductSupplyIDs returns every ProductSupply row id across the whole
// catalog (all 30 products) - used by the always-on VIP deal below, which
// (unlike the 4 seasonal deals) applies store-wide rather than to a
// thematic subset.
func allProductSupplyIDs() []string {
	var ids []string
	for i := range products {
		ids = append(ids, productSupplyIDs(i)...)
	}
	return ids
}

// dealDefs is the fixed set of 5 deals: 4 seasonal ones (one per season),
// each scoped to a thematically-relevant slice of the catalog and dated
// inside the generator's [datasetStart, referenceToday] window (see
// main.go) so real orders can actually land inside them, plus one
// always-in-effect, VIP-only loyalty discount covering the whole catalog
// (Start/End both "" - see the loop below that turns that into a nil
// *string, meaning "no bound on this side").
//
// IDs names ProductSupply row ids (e.g. "ps-022"), not productIds - see
// entities.Deal's own doc comment. Each seasonal deal below covers every
// supplier row of its listed products (concatIDs of each product's own
// productSupplyIDs, addressed by 0-based index - product prod-03 is index
// 2, etc.), so a deal still reads as "these products are on sale" the
// same way it did when it named productIds directly - nothing stops a
// future deal from naming only a subset of one product's rows instead.
var dealDefs = []struct {
	ID                  string
	Name                string
	Start               string // "" means no lower bound (always active on this side)
	End                 string // "" means no upper bound
	IDs                 []string
	DiscountPercentages map[string]float64
}{
	{
		// prod-03, prod-04, prod-06, prod-07, prod-25, prod-26.
		ID: "deal-01", Name: "Fall Cleanup Clearance",
		Start: "2025-10-01", End: "2025-10-20",
		IDs: concatIDs(
			productSupplyIDs(2), productSupplyIDs(3), productSupplyIDs(5),
			productSupplyIDs(6), productSupplyIDs(24), productSupplyIDs(25),
		),
		DiscountPercentages: escalateDiscount(20),
	},
	{
		// prod-05, prod-15, prod-17, prod-24, prod-28.
		ID: "deal-02", Name: "Winter Prep Bundle",
		Start: "2025-12-01", End: "2025-12-31",
		IDs: concatIDs(
			productSupplyIDs(4), productSupplyIDs(14), productSupplyIDs(16),
			productSupplyIDs(23), productSupplyIDs(27),
		),
		DiscountPercentages: escalateDiscount(15),
	},
	{
		// prod-18, prod-19, prod-20, prod-21, prod-22, prod-23, prod-27.
		ID: "deal-03", Name: "Spring Planting Sale",
		Start: "2026-03-01", End: "2026-03-21",
		IDs: concatIDs(
			productSupplyIDs(17), productSupplyIDs(18), productSupplyIDs(19),
			productSupplyIDs(20), productSupplyIDs(21), productSupplyIDs(22),
			productSupplyIDs(26),
		),
		DiscountPercentages: escalateDiscount(20),
	},
	{
		// prod-08, prod-09, prod-10, prod-11, prod-12, prod-13, prod-14.
		ID: "deal-04", Name: "Summer Watering Savings",
		Start: "2026-06-01", End: "2026-09-30",
		IDs: concatIDs(
			productSupplyIDs(7), productSupplyIDs(8), productSupplyIDs(9),
			productSupplyIDs(10), productSupplyIDs(11), productSupplyIDs(12),
			productSupplyIDs(13),
		),
		DiscountPercentages: escalateDiscount(25),
	},
	{
		// Always in effect (no Start/End), whole catalog, vip only - a
		// small standing loyalty perk rather than a seasonal clearance,
		// so it's kept deliberately modest (5%) and doesn't go through
		// escalateDiscount at all: one-time/returning customers aren't
		// eligible for it, full stop, not "eligible at 0%".
		ID: "deal-05", Name: "VIP Loyalty Discount",
		Start: "", End: "",
		IDs:                 allProductSupplyIDs(),
		DiscountPercentages: map[string]float64{customerTypeVIP: 5},
	},
}

// issueTemplates seed CustomerServiceCall.IssueText. Templates containing
// "%s" are filled in with the name of a product the calling customer
// actually ordered (see buildCustomerServiceCalls); the rest are
// product-agnostic.
var issueTemplates = []string{
	"Received a damaged %s and would like a replacement.",
	"The %s arrived with missing parts.",
	"Asking about the delivery status of my %s order.",
	"Wrong item shipped - ordered a %s but received something else.",
	"Requesting a refund for a %s that doesn't match the listing.",
	"The %s stopped working within a week of use.",
	"Question about how to assemble the %s.",
	"Billing was charged twice for the same order.",
	"Requesting to change the shipping address for a pending order.",
	"General question about upcoming seasonal deals.",
	"Complimenting the fast shipping on a recent order.",
	"Asking whether a discount code can still be applied after checkout.",
	"Package marked delivered but never arrived.",
	"Requesting a price-match against a Deal that started after the order was placed.",
	"Asking about the warranty coverage on gardening tools.",
}

// firstNames / lastNames / cityStateZips / streetNames back
// buildOnlineCustomers' deterministic (seeded) name/address generation.
var firstNames = []string{
	"James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda",
	"David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica",
	"Thomas", "Sarah", "Charles", "Karen", "Christopher", "Nancy", "Daniel", "Lisa",
	"Matthew", "Betty", "Anthony", "Margaret", "Mark", "Sandra", "Ava", "Noah",
	"Mia", "Liam", "Emma", "Olivia", "Ethan", "Sophia", "Lucas", "Isabella",
}

var lastNames = []string{
	"Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
	"Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson", "Thomas",
	"Taylor", "Moore", "Jackson", "Martin", "Lee", "Perez", "Thompson", "White",
	"Harris", "Sanchez", "Clark", "Ramirez", "Lewis", "Robinson",
}

var cityStateZips = []string{
	"Austin, TX 73301", "Denver, CO 80203", "Portland, OR 97201", "Raleigh, NC 27601",
	"Madison, WI 53703", "Boise, ID 83702", "Columbus, OH 43215", "Nashville, TN 37201",
	"Sacramento, CA 95814", "Providence, RI 02903", "Spokane, WA 99201", "Tucson, AZ 85701",
}

var streetNames = []string{
	"Maple St", "Oak Ave", "Cedar Ln", "Birch Rd", "Elm Ct", "Pine Way",
	"Willow Dr", "Aspen Blvd", "Magnolia Ter", "Sycamore Pl",
}
