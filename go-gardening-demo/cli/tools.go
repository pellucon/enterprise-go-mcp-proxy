// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// toolSpec names one MCP tool reachable through a CLI section. LoadField,
// when non-empty, marks this as a bulk "load_*" tool: its only accepted
// flag is -file <path>, and the file's parsed JSON array is wrapped in
// {LoadField: [...]} as the call's argument, mirroring
// demo-authorization-server/client's own `add-users -file` (read a local
// file, send its content) — see go-gardening-demo/README.md's design
// decisions.
type toolSpec struct {
	Server    string
	Tool      string
	LoadField string
}

// sections maps each of the spec's own section names to its tools. Bare
// tool names here (the map keys) are what a caller types; toolSpec.Server
// supplies the "garden-*." namespace prefix actually sent over the wire.
var sections = map[string]map[string]toolSpec{
	"customer-admin": {
		"load_customers":            {Server: "garden-customer-admin", Tool: "load_customers", LoadField: "customers"},
		"add_or_update_customer":    {Server: "garden-customer-admin", Tool: "add_or_update_customer"},
		"mark_customer_deactivated": {Server: "garden-customer-admin", Tool: "mark_customer_deactivated"},
		"modify_customer_sentiment": {Server: "garden-customer-admin", Tool: "modify_customer_sentiment"},
		"modify_customer_type":      {Server: "garden-customer-admin", Tool: "modify_customer_type"},
		"get_customers":             {Server: "garden-customer-admin", Tool: "get_customers"},
		"get_customer_type":         {Server: "garden-customer-admin", Tool: "get_customer_type"},
	},
	"staff-admin": {
		"load_staff_roles":         {Server: "garden-staff-admin", Tool: "load_staff_roles", LoadField: "staffRoles"},
		"load_staffers":            {Server: "garden-staff-admin", Tool: "load_staffers", LoadField: "staffers"},
		"add_or_update_staffer":    {Server: "garden-staff-admin", Tool: "add_or_update_staffer"},
		"mark_staffer_deactivated": {Server: "garden-staff-admin", Tool: "mark_staffer_deactivated"},
		"get_staffers":             {Server: "garden-staff-admin", Tool: "get_staffers"},
	},
	"customer-service": {
		"load_customer_sentiments":      {Server: "garden-customer-service", Tool: "load_customer_sentiments", LoadField: "sentiments"},
		"load_customer_service_calls":   {Server: "garden-customer-service", Tool: "load_customer_service_calls", LoadField: "calls"},
		"add_customer_service_call":     {Server: "garden-customer-service", Tool: "add_customer_service_call"},
		"resolve_customer_service_call": {Server: "garden-customer-service", Tool: "resolve_customer_service_call"},
		"modify_customer_sentiment":     {Server: "garden-customer-service", Tool: "modify_customer_sentiment"},
		"get_customer_service_calls":    {Server: "garden-customer-service", Tool: "get_customer_service_calls"},
	},
	"business-ops": {
		"load_deals":           {Server: "garden-business-ops", Tool: "load_deals", LoadField: "deals"},
		"load_customer_types":  {Server: "garden-business-ops", Tool: "load_customer_types", LoadField: "customerTypes"},
		"add_or_update_deal":   {Server: "garden-business-ops", Tool: "add_or_update_deal"},
		"update_customer_type": {Server: "garden-business-ops", Tool: "update_customer_type"},
		"get_deals":            {Server: "garden-business-ops", Tool: "get_deals"},
		"get_active_deals":     {Server: "garden-business-ops", Tool: "get_active_deals"},
	},
	// Includes the tools formerly hosted by their own garden-inventory
	// server, merged into garden-online-store (its only caller) once
	// purchase_products' relay to it became an in-process call — see
	// garden-online-store/main.go's own doc comment.
	"online-store": {
		"load_customer_orders":        {Server: "garden-online-store", Tool: "load_customer_orders", LoadField: "orders"},
		"purchase_products":           {Server: "garden-online-store", Tool: "purchase_products"},
		"purchase_fulfilled":          {Server: "garden-online-store", Tool: "purchase_fulfilled"},
		"load_product_supply":         {Server: "garden-online-store", Tool: "load_product_supply", LoadField: "productSupply"},
		"load_supply_orders":          {Server: "garden-online-store", Tool: "load_supply_orders", LoadField: "supplyOrders"},
		"load_historical_purchases":   {Server: "garden-online-store", Tool: "load_historical_purchases", LoadField: "orders"},
		"add_product_supply":          {Server: "garden-online-store", Tool: "add_product_supply"},
		"add_supply_order":            {Server: "garden-online-store", Tool: "add_supply_order"},
		"mark_supply_order_fulfilled": {Server: "garden-online-store", Tool: "mark_supply_order_fulfilled"},
		"get_inventory":               {Server: "garden-online-store", Tool: "get_inventory"},
		"get_catalogue":               {Server: "garden-online-store", Tool: "get_catalogue"},
		"get_supply_orders":           {Server: "garden-online-store", Tool: "get_supply_orders"},
		"get_customer_orders":         {Server: "garden-online-store", Tool: "get_customer_orders"},
		"get_product_supply":          {Server: "garden-online-store", Tool: "get_product_supply"},
	},
}

// inferValue turns one flag's raw string value into a JSON-friendly Go
// value: a comma-containing value becomes []string (e.g. -roles
// "it-operations,business-analyst" or -ids "ps-001,ps-002"),
// otherwise the first of int64/float64/bool/string that raw actually
// parses as. This is what lets one generic flag parser serve every tool's
// differently-shaped arguments without a hand-written struct per tool —
// ids like "cust-0001"/"ps-001" always fail the numeric parses (they're
// letter-prefixed) and fall through to plain strings, as intended.
func inferValue(raw string) any {
	if strings.Contains(raw, ",") {
		parts := strings.Split(raw, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		return parts
	}
	if i, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil {
		return f
	}
	if b, err := strconv.ParseBool(raw); err == nil {
		return b
	}
	return raw
}

// buildArguments turns a command's trailing "-flag value" tokens into the
// MCP call's Arguments map. Two special cases beyond the generic
// -flag-per-field/inferValue path:
//   - spec.LoadField != "": the only accepted flag is -file <path>; its
//     content becomes {LoadField: <parsed array>}.
//   - purchase_products: repeatable -item productId:units flags build the
//     items array garden-online-store's PurchaseProductsArgs expects —
//     the one tool in this whole CLI whose input isn't flat key/value
//     pairs, since it's a list of {productId, units} objects.
func buildArguments(spec toolSpec, tokens []string) (map[string]any, error) {
	if spec.LoadField != "" {
		var file string
		for i := 0; i < len(tokens); i++ {
			if tokens[i] == "-file" {
				if i+1 >= len(tokens) {
					return nil, fmt.Errorf("-file needs a value")
				}
				file = tokens[i+1]
				i++
			}
		}
		if file == "" {
			return nil, fmt.Errorf("%s requires -file <path>", spec.Tool)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("failed to read %s: %w", file, err)
		}
		var items []any
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("failed to parse %s as a JSON array: %w", file, err)
		}
		return map[string]any{spec.LoadField: items}, nil
	}

	args := map[string]any{}
	var items []map[string]any
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !strings.HasPrefix(tok, "-") {
			return nil, fmt.Errorf("unexpected argument %q (flags must start with -)", tok)
		}
		name := strings.TrimPrefix(tok, "-")
		if i+1 >= len(tokens) {
			return nil, fmt.Errorf("flag -%s needs a value", name)
		}
		value := tokens[i+1]
		i++

		if name == "item" && spec.Tool == "purchase_products" {
			parts := strings.SplitN(value, ":", 2)
			if len(parts) != 2 {
				return nil, fmt.Errorf("-item must be productId:units, got %q", value)
			}
			units, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("-item units must be an integer: %w", err)
			}
			items = append(items, map[string]any{"productId": parts[0], "units": units})
			continue
		}

		args[name] = inferValue(value)
	}
	if items != nil {
		args["items"] = items
	}
	return args, nil
}

// runCommand resolves section/tool, builds arguments from the remaining
// tokens, calls the namespaced tool, and prints the result the same way
// for every command (one-shot or REPL alike).
func runCommand(ctx context.Context, session *mcp.ClientSession, section, tool string, tokens []string) error {
	secTools, ok := sections[section]
	if !ok {
		names := make([]string, 0, len(sections))
		for s := range sections {
			names = append(names, s)
		}
		return fmt.Errorf("unknown section %q (valid: %s)", section, strings.Join(names, ", "))
	}
	spec, ok := secTools[tool]
	if !ok {
		names := make([]string, 0, len(secTools))
		for t := range secTools {
			names = append(names, t)
		}
		return fmt.Errorf("unknown tool %q for section %q (valid: %s)", tool, section, strings.Join(names, ", "))
	}

	args, err := buildArguments(spec, tokens)
	if err != nil {
		return err
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      spec.Server + "." + spec.Tool,
		Arguments: args,
	})
	if err != nil {
		return err
	}
	printContent(res.Content)
	printStructuredContent(res.StructuredContent)
	if res.IsError {
		return fmt.Errorf("%s.%s returned an error (see output above)", spec.Server, spec.Tool)
	}
	return nil
}
