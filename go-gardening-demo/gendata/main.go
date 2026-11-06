// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Command gendata generates the go-gardening-demo's seed data: one JSON
// file per entity, written to go-gardening-demo/initial-data/ (or the
// directory given as the first argument). Deterministic (fixed rng seed),
// so re-running it reproduces byte-identical output - see
// ../README.md for the full entity/assumption writeup.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

// datasetStart/referenceToday bound every generated date: a roughly
// 12-month window ending shortly before this repo's current date, wide
// enough to fit all 4 seasonal deals (staticdata.go's dealDefs) and leave
// room for a "not yet fulfilled" tail of recent orders.
var (
	datasetStart   = time.Date(2025, 8, 15, 0, 0, 0, 0, time.UTC)
	referenceToday = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
)

const seed = 42

func main() {
	outDir := "initial-data"
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatalf("failed to create output directory %q: %v", outDir, err)
	}

	rng := rand.New(rand.NewSource(seed))

	productSupply := buildProductSupply(rng)
	supplyOrders := buildSupplyOrders(rng, productSupply, datasetStart, referenceToday)
	customerTypesOut := buildCustomerTypes()
	customerSentimentsOut := buildCustomerSentiments()
	onlineCustomers := buildOnlineCustomers(rng)
	staffRolesOut := buildStaffRoles()
	staffersOut := buildStaffers()
	deals := buildDeals()
	onlineCustomerOrders := buildOnlineCustomerOrders(rng, onlineCustomers, productSupply, supplyOrders, deals, datasetStart, referenceToday)
	customerServiceCalls := buildCustomerServiceCalls(rng, onlineCustomerOrders, staffersOut, datasetStart, referenceToday)
	clientAccessRules := buildClientAccessRules()
	sampleEvents := buildSampleEvents(rng, onlineCustomerOrders, supplyOrders, onlineCustomers, staffersOut, datasetStart, referenceToday)

	writeJSON(outDir, "product_supply.json", productSupply)
	writeJSON(outDir, "supply_orders.json", supplyOrders)
	writeJSON(outDir, "customer_types.json", customerTypesOut)
	writeJSON(outDir, "customer_sentiments.json", customerSentimentsOut)
	writeJSON(outDir, "online_customers.json", onlineCustomers)
	writeJSON(outDir, "online_customer_orders.json", onlineCustomerOrders)
	writeJSON(outDir, "staff_roles.json", staffRolesOut)
	writeJSON(outDir, "staffers.json", staffersOut)
	writeJSON(outDir, "customer_service_calls.json", customerServiceCalls)
	writeJSON(outDir, "deals.json", deals)
	writeJSON(outDir, "client_access_rules.json", clientAccessRules)
	writeJSON(outDir, "sample-events.json", sampleEvents)

	fmt.Println("Wrote go-gardening-demo initial data to", outDir)
}

func writeJSON(dir, name string, v any) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		log.Fatalf("failed to marshal %s: %v", name, err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		log.Fatalf("failed to write %s: %v", path, err)
	}
}
