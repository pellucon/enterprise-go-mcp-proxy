// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// initStep is one file-to-tool load, in the order runInit executes them.
// The only hard ordering constraint anywhere in this list is internal to
// the "online-store" section's inventory-related tools: load_product_supply
// must run before load_supply_orders (which resolves each order's
// UnitsPerWholesaleUnit from it) and before load_historical_purchases
// (which needs the product catalog it populates) — everything else is
// independent of load order, since no server loads another server's
// lookup table.
var initSteps = []struct {
	Section string
	Tool    string
	File    string
}{
	{"online-store", "load_product_supply", "product_supply.json"},
	{"online-store", "load_supply_orders", "supply_orders.json"},
	{"staff-admin", "load_staff_roles", "staff_roles.json"},
	{"staff-admin", "load_staffers", "staffers.json"},
	{"customer-admin", "load_customers", "online_customers.json"},
	{"customer-service", "load_customer_sentiments", "customer_sentiments.json"},
	{"customer-service", "load_customer_service_calls", "customer_service_calls.json"},
	{"business-ops", "load_deals", "deals.json"},
	{"business-ops", "load_customer_types", "customer_types.json"},
	{"online-store", "load_customer_orders", "online_customer_orders.json"},
	// Netting historical purchases out of the stock baseline (see
	// go-gardening-demo/README.md) - a separate load_* call from
	// load_customer_orders above, both on garden-online-store now (not a
	// relay between them; each independently nets/populates its own state
	// from the same file).
	{"online-store", "load_historical_purchases", "online_customer_orders.json"},
}

// runInit implements `cli init`: reads each go-gardening-demo/initial-data
// JSON file locally and calls the corresponding load_* tool with its
// parsed content, in initSteps' order; then, as later steps - policy and
// events after data - loads sample-events.json directly into
// event-aggregator.load_events (see runLoadSampleEvents below),
// client_access_rules.json/declarative_guardrails.json/orchestration.json
// via their own admin update_* tools (see cli/admin.go), and finally
// prompts.json via admin load_prompts (see runLoadPrompts). None of these
// later steps is folded into initSteps itself: initSteps assumes one plain
// section/tool call per step, but event-aggregator isn't one of
// go-gardening-demo/servers/* at all (it's the shared platform event
// store - see CLAUDE.md's "NATS event system" - reachable
// through this CLI at all only for this one seeding step), the admin
// update_* tools are each a bespoke export-merge-update flow, and
// load_prompts fans out to however many distinct servers prompts.json
// names, not exactly one - the same reason init/admin are already
// special-cased in dispatch() rather than going through sections. Stops
// at the first failure.
func runInit(ctx context.Context, session *mcp.ClientSession, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "go-gardening-demo/initial-data", "Directory containing the seed JSON files (product_supply.json, staffers.json, ...)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	for _, step := range initSteps {
		path := filepath.Join(*dataDir, step.File)
		fmt.Printf("=== %s %s -file %s ===\n", step.Section, step.Tool, path)
		if err := runCommand(ctx, session, step.Section, step.Tool, []string{"-file", path}); err != nil {
			return fmt.Errorf("init: %s %s failed: %w", step.Section, step.Tool, err)
		}
	}

	eventsPath := filepath.Join(*dataDir, "sample-events.json")
	fmt.Printf("=== event-aggregator.load_events -file %s ===\n", eventsPath)
	if err := runLoadSampleEvents(ctx, session, eventsPath); err != nil {
		return fmt.Errorf("init: event-aggregator.load_events failed: %w", err)
	}

	accessRulesPath := filepath.Join(*dataDir, "client_access_rules.json")
	fmt.Printf("=== admin update_access_rules -file %s ===\n", accessRulesPath)
	if err := runUpdateAccessRules(ctx, session, accessRulesPath); err != nil {
		return fmt.Errorf("init: admin update_access_rules failed: %w", err)
	}

	// Business logic validation (CLAUDE.md's "Triggered tools /
	// orchestration" and its declarative-guardrail-driven dispatch via
	// proxy_utility.business_logic_constraints_validation): the declaration
	// naming garden-online-store.validate_store_requests, then the
	// orchestration/toolMapping wiring purchase_products through it.
	// Declarative guardrails first, since the orchestration step
	// references its declaration by name - an orchestration update that
	// landed first would reference a not-yet-existing declaration only
	// briefly (Guard's own lazy validation happens per call, not at
	// config-write time), but there's no reason to risk the ordering.
	declarativeGuardrailsPath := filepath.Join(*dataDir, "declarative_guardrails.json")
	fmt.Printf("=== admin update_declarative_guardrails -file %s ===\n", declarativeGuardrailsPath)
	if err := runUpdateDeclarativeGuardrails(ctx, session, declarativeGuardrailsPath); err != nil {
		return fmt.Errorf("init: admin update_declarative_guardrails failed: %w", err)
	}

	orchestrationPath := filepath.Join(*dataDir, "orchestration.json")
	fmt.Printf("=== admin update_orchestration -file %s ===\n", orchestrationPath)
	if err := runUpdateOrchestration(ctx, session, orchestrationPath); err != nil {
		return fmt.Errorf("init: admin update_orchestration failed: %w", err)
	}

	promptsPath := filepath.Join(*dataDir, "prompts.json")
	fmt.Printf("=== admin load_prompts -file %s ===\n", promptsPath)
	if err := runLoadPrompts(ctx, session, promptsPath); err != nil {
		return fmt.Errorf("init: admin load_prompts failed: %w", err)
	}

	fmt.Println("init complete.")
	return nil
}

// runLoadSampleEvents reads path (a JSON array of events, e.g.
// go-gardening-demo/initial-data/sample-events.json - reverse-engineered
// from the seed data by go-gardening-demo/gendata's buildSampleEvents,
// see CLAUDE.md's "NATS event system") and calls
// event-aggregator.load_events directly with {"events": [...]} - the same
// {LoadField: [...]} wrapping every other load_* tool call gets via
// buildArguments/runCommand, just built by hand here since event-aggregator
// has no entry in the sections table above (it isn't a go-gardening-demo/
// servers/* server at all).
func runLoadSampleEvents(ctx context.Context, session *mcp.ClientSession, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	var parsedEvents []any
	if err := json.Unmarshal(raw, &parsedEvents); err != nil {
		return fmt.Errorf("failed to parse %s as a JSON array: %w", path, err)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "event-aggregator.load_events",
		Arguments: map[string]any{"events": parsedEvents},
	})
	if err != nil {
		return fmt.Errorf("event-aggregator.load_events call failed: %w", err)
	}
	printContent(res.Content)
	if res.IsError {
		return fmt.Errorf("event-aggregator.load_events returned an error (see output above)")
	}
	return nil
}
