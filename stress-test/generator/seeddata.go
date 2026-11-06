// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SeedData holds every id pool the generator draws from, all read directly
// from go-gardening-demo's own committed seed/config files rather than
// hardcoded — so a change to the seed data (more products, a different
// customer count, ...) is picked up automatically the next time the
// generator is re-run, instead of silently drifting out of sync with a
// stale hardcoded range.
type SeedData struct {
	ProductIDs         []string // distinct productId values from product_supply.json
	ProductSupplyIDs   []string // "id" field of product_supply.json rows (ps-NNN)
	CustomerIDs        []string // "id" field of online_customers.json rows
	CustomerTypes      []string // "id" field of customer_types.json rows
	CustomerSentiments []string // "id" field of customer_sentiments.json rows
	StaffRoleIDs       []string // "id" field of staff_roles.json rows
	StafferIDs         []string // "id" field of staffers.json rows
	DealIDs            []string // "id" field of deals.json rows

	// PendingOrderIDs is every online_customer_orders.json row whose
	// fulfillmentActualDate is null — the only valid targets for
	// purchase_fulfilled, since a newly-created order's id is assigned at
	// runtime (lowest unused ord-NNNN, racy across concurrent clients) and
	// can never be known at generation time.
	PendingOrderIDs []string

	// UnresolvedServiceCallIDs is every customer_service_calls.json row
	// whose resolutionDate is null — the only valid targets for
	// resolve_customer_service_call. Expected to be a very small pool
	// (single digits) that exhausts almost immediately once the stress
	// test starts calling resolve_customer_service_call — see
	// stress-test/README.md for why that's an anticipated outcome, not a
	// generation bug.
	UnresolvedServiceCallIDs []string

	// DeclarationNames is every declarativeGuardrails.declarations[].name
	// in go-gardening-demo/gardening-demo.cfg — the real, current set of
	// declaration names evaluate_declarations calls may reference.
	DeclarationNames []string
}

// dataDir is go-gardening-demo/initial-data, resolved relative to repo
// root — the generator, like every other tool in this repo, is meant to be
// run with the repo root as the working directory (e.g. `go run
// ./stress-test/generator` from the repo root).
const dataDir = "go-gardening-demo/initial-data"
const gardeningCfgPath = "go-gardening-demo/gardening-demo.cfg"

// loadSeedData reads every file SeedData needs and extracts the id pools
// above. Fails closed (returns an error) on any read/parse problem rather
// than falling back to an empty pool, since an empty pool would silently
// produce call files missing whole categories of valid arguments.
func loadSeedData() (*SeedData, error) {
	sd := &SeedData{}

	productIDSet := map[string]bool{}
	var productSupplyRows []map[string]any
	if err := readJSONArray(filepath.Join(dataDir, "product_supply.json"), &productSupplyRows); err != nil {
		return nil, err
	}
	for _, row := range productSupplyRows {
		id, _ := row["id"].(string)
		productID, _ := row["productId"].(string)
		if id != "" {
			sd.ProductSupplyIDs = append(sd.ProductSupplyIDs, id)
		}
		if productID != "" && !productIDSet[productID] {
			productIDSet[productID] = true
			sd.ProductIDs = append(sd.ProductIDs, productID)
		}
	}

	var customerRows []map[string]any
	if err := readJSONArray(filepath.Join(dataDir, "online_customers.json"), &customerRows); err != nil {
		return nil, err
	}
	for _, row := range customerRows {
		if id, _ := row["id"].(string); id != "" {
			sd.CustomerIDs = append(sd.CustomerIDs, id)
		}
	}

	if err := readIDField(filepath.Join(dataDir, "customer_types.json"), &sd.CustomerTypes); err != nil {
		return nil, err
	}
	if err := readIDField(filepath.Join(dataDir, "customer_sentiments.json"), &sd.CustomerSentiments); err != nil {
		return nil, err
	}
	if err := readIDField(filepath.Join(dataDir, "staff_roles.json"), &sd.StaffRoleIDs); err != nil {
		return nil, err
	}
	if err := readIDField(filepath.Join(dataDir, "staffers.json"), &sd.StafferIDs); err != nil {
		return nil, err
	}
	if err := readIDField(filepath.Join(dataDir, "deals.json"), &sd.DealIDs); err != nil {
		return nil, err
	}

	var orderRows []map[string]any
	if err := readJSONArray(filepath.Join(dataDir, "online_customer_orders.json"), &orderRows); err != nil {
		return nil, err
	}
	for _, row := range orderRows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		if row["fulfillmentActualDate"] == nil {
			sd.PendingOrderIDs = append(sd.PendingOrderIDs, id)
		}
	}
	if len(sd.PendingOrderIDs) == 0 {
		return nil, fmt.Errorf("no pending (fulfillmentActualDate:null) orders found in %s — purchase_fulfilled would have no valid targets", "online_customer_orders.json")
	}

	var callRows []map[string]any
	if err := readJSONArray(filepath.Join(dataDir, "customer_service_calls.json"), &callRows); err != nil {
		return nil, err
	}
	for _, row := range callRows {
		id, _ := row["id"].(string)
		if id == "" {
			continue
		}
		if row["resolutionDate"] == nil {
			sd.UnresolvedServiceCallIDs = append(sd.UnresolvedServiceCallIDs, id)
		}
	}
	if len(sd.UnresolvedServiceCallIDs) == 0 {
		return nil, fmt.Errorf("no unresolved (resolutionDate:null) service calls found in %s — resolve_customer_service_call would have no valid targets", "customer_service_calls.json")
	}

	declarations, err := readDeclarationNames(gardeningCfgPath)
	if err != nil {
		return nil, err
	}
	sd.DeclarationNames = declarations

	return sd, nil
}

// readJSONArray unmarshals a JSON array file into dst (a pointer to a
// []map[string]any), used when the caller needs more than just each row's
// "id" field.
func readJSONArray(path string, dst *[]map[string]any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	return nil
}

// readIDField unmarshals a JSON array file and extracts just each row's
// "id" field, for the several seed files that are only ever used as a flat
// id list.
func readIDField(path string, dst *[]string) error {
	var rows []map[string]any
	if err := readJSONArray(path, &rows); err != nil {
		return err
	}
	for _, row := range rows {
		if id, _ := row["id"].(string); id != "" {
			*dst = append(*dst, id)
		}
	}
	if len(*dst) == 0 {
		return fmt.Errorf("no ids found in %s", path)
	}
	return nil
}

// readDeclarationNames extracts declarativeGuardrails.declarations[].name
// from the real gardening-demo.cfg, so evaluate_declarations calls always
// reference declarations that actually exist in the deployed config.
func readDeclarationNames(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg struct {
		DeclarativeGuardrails struct {
			Declarations []struct {
				Name string `json:"name"`
			} `json:"declarations"`
		} `json:"declarativeGuardrails"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	var names []string
	for _, d := range cfg.DeclarativeGuardrails.Declarations {
		if d.Name != "" {
			names = append(names, d.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no declarativeGuardrails.declarations found in %s", path)
	}
	return names, nil
}
