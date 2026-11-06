// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import "github.com/pellucon/enterprise-go-mcp-proxy/access_control"

// buildClientAccessRules is the staffer-roles spec's tools-mapping,
// inverted from its per-tool ROLES: lists into one rule per role (the only shape
// access_control.AccessibilityRule supports - Role is a single glob
// pattern, not a list, so "these 3 roles can call this tool" has to be
// expressed as 3 separate per-role rules each naming the tool, not one
// rule naming 3 roles).
//
// roleAdmin's own rule is included (full access, mirroring the shape of
// proxy-server.cfg's/gardening_demo_test.cfg's own pre-existing {"role":
// "admin", "allowed_tools": ["*"], ...} rule exactly) purely so this file
// is self-documenting on its own - a reader shouldn't have to know a
// separate, un-generated admin rule exists elsewhere to understand who
// can call what. It's redundant with that pre-existing rule in every
// committed *.cfg today, but cli/admin.go's mergeAccessRules replaces
// (never duplicates) any existing rule sharing a role with one of these,
// admin included, so applying this file repeatedly stays idempotent
// rather than accumulating duplicate admin rules over time.
//
// This whole function is purely static (no rng dependency), unlike the
// rest of gendata - the mapping itself has no randomized component.
func buildClientAccessRules() []access_control.AccessibilityRule {
	rule := func(role string, tools ...string) access_control.AccessibilityRule {
		return access_control.AccessibilityRule{
			Role:                     role,
			AllowedTools:             tools,
			AllowedPrompts:           []string{},
			AllowedResources:         []string{},
			AllowedResourceTemplates: []string{},
		}
	}

	return []access_control.AccessibilityRule{
		{
			Role:                     roleAdmin,
			AllowedTools:             []string{"*"},
			AllowedPrompts:           []string{"*"},
			AllowedResources:         []string{"*"},
			AllowedResourceTemplates: []string{"*"},
		},
		rule(roleBizManager,
			"garden-customer-admin.*",
			"garden-business-ops.*",
			"garden-online-store.load_customer_orders",
			"garden-online-store.purchase_fulfilled",
			"garden-online-store.load_product_supply",
			"garden-online-store.load_supply_orders",
			"garden-online-store.load_historical_purchases",
			"garden-online-store.add_product_supply",
			"garden-online-store.add_supply_order",
			"garden-online-store.mark_supply_order_fulfilled",
			"garden-online-store.get_inventory",
			"garden-online-store.get_catalogue",
		),
		rule(roleHRManager,
			"garden-staff-admin.*",
		),
		rule(roleCustSupportManager,
			"garden-customer-service.*",
		),
		rule(roleCustManagement,
			"garden-customer-admin.*",
		),
		rule(roleCustSupport,
			"garden-customer-service.add_customer_service_call",
			"garden-customer-service.resolve_customer_service_call",
		),
		rule(roleHRStaffer,
			"garden-staff-admin.load_staffers",
			"garden-staff-admin.add_or_update_staffer",
			"garden-staff-admin.get_staffers",
		),
		rule(roleInventoryClerk,
			"garden-online-store.load_product_supply",
			"garden-online-store.load_supply_orders",
			"garden-online-store.add_supply_order",
			"garden-online-store.mark_supply_order_fulfilled",
			"garden-online-store.get_inventory",
			"garden-online-store.get_catalogue",
			"garden-business-ops.get_deals",
			"garden-business-ops.get_active_deals",
			"garden-online-store.get_supply_orders",
			"garden-online-store.get_customer_orders",
			"garden-online-store.get_product_supply",
		),
		rule(roleBizStaff,
			"garden-business-ops.update_customer_type",
			"garden-online-store.load_customer_orders",
			"garden-online-store.purchase_fulfilled",
			"garden-online-store.load_product_supply",
			"garden-online-store.load_supply_orders",
			"garden-online-store.load_historical_purchases",
			"garden-online-store.add_product_supply",
			"garden-online-store.add_supply_order",
			"garden-online-store.mark_supply_order_fulfilled",
			"garden-online-store.get_inventory",
			"garden-online-store.get_catalogue",
			"garden-business-ops.get_deals",
			"garden-business-ops.get_active_deals",
		),
		// garden-customer-admin.get_customer_type is deliberately not
		// garden-customer-admin.* - a customer must not see other
		// customers' contact details via get_customers, only this one
		// narrow id->type lookup (see its own doc comment). purchase_products
		// relays to it to determine deal eligibility at checkout.
		rule("customer",
			"garden-online-store.purchase_products",
			"garden-online-store.get_catalogue",
			"garden-business-ops.get_active_deals",
			"garden-customer-admin.get_customer_type",
		),
		// Role-pattern rules below (Role itself is a glob, matched via
		// filepath.Match against the caller's actual role - see
		// access_control.matchAny) grant a tool to a whole role family at
		// once, rather than duplicating the grant into every individual
		// role's own rule above. "customer-*" (not "customer*") is
		// deliberate: filepath.Match's "*" matches zero or more
		// characters, so a bare "customer*" would also match the literal
		// "customer" role (the end retail consumer) - a customer must not
		// see other customers' service call records. The hyphen makes
		// this match only the customer-*-prefixed staff roles
		// (customer-support, customer-management, customer-support-manager).
		rule("customer-*",
			"garden-customer-service.get_customer_service_calls",
		),
		rule("business*",
			"garden-online-store.get_supply_orders",
			"garden-online-store.get_customer_orders",
			"garden-online-store.get_product_supply",
		),
		// roleAnalyst gets garden-business-ops.get_active_deals (not just
		// the explicitly-requested get_deals) because get_catalogue relays
		// back through the proxy to garden-business-ops.get_active_deals
		// as the calling analyst's own identity (see CLAUDE.md's
		// "Internal identity assertions" - a relayed call carries no more
		// authority than the original caller had) - without this grant,
		// analyst's own get_catalogue calls would fail at that relay step.
		rule(roleAnalyst,
			"event-aggregator.list_events",
			"garden-online-store.get_catalogue",
			"garden-business-ops.get_deals",
			"garden-business-ops.get_active_deals",
			"garden-customer-service.get_customer_service_calls",
			"garden-online-store.get_customer_orders",
		),
	}
}
