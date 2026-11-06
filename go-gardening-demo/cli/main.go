// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// cli is the manual-operation CLI for go-gardening-demo's 5 MCP servers,
// per the gardening-demo spec: authenticates and connects once (same
// endpoint/auth flags and defaults as the repo's other CLI, the root cli/
// tool — a distinct binary at a different import path, cli/ vs
// go-gardening-demo/cli, which later adopted this package's own
// REPL/history/dispatch shape for its own command set), then either runs
// one command given on argv and exits (scriptable mode — useful for `cli
// init` and shell scripts) or, per the spec's own "called once and then
// run in a loop to allow multiple call[s]", drops into an interactive
// REPL reading one command per line until EOF/exit/quit.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

const topLevelUsage = `go-gardening-demo cli: manually exercise the gardening-demo MCP servers through a running go-proxy.

Usage:
  cli [flags] [<section> <tool> [-flag value...]]

If a <section> <tool> command is given, it runs once and the CLI exits (scriptable mode).
Otherwise the CLI drops into an interactive loop: type one command per line, "exit"/"quit" to leave.

Sections and their tools:
  customer-admin    load_customers add_or_update_customer mark_customer_deactivated
                     modify_customer_sentiment modify_customer_type get_customers
                     get_customer_type
  staff-admin       load_staff_roles load_staffers add_or_update_staffer
                     mark_staffer_deactivated get_staffers
  customer-service  load_customer_sentiments load_customer_service_calls
                     add_customer_service_call resolve_customer_service_call
                     modify_customer_sentiment get_customer_service_calls
  business-ops      load_deals load_customer_types add_or_update_deal update_customer_type
                     get_deals get_active_deals
  online-store      load_customer_orders purchase_products purchase_fulfilled
                     load_product_supply load_supply_orders load_historical_purchases
                     add_product_supply add_supply_order mark_supply_order_fulfilled
                     get_inventory get_catalogue get_supply_orders get_customer_orders
                     get_product_supply
  admin             list_application_tools list_platform_tools update_access_rules
                     update_declarative_guardrails update_orchestration load_prompts

Every load_* tool takes -file <path> (a local JSON array file) instead of per-field flags.
purchase_products takes repeatable -item productId:units flags instead of a single value.
Every other tool takes one flag per field, e.g. -id -name -type -deactivated.

list_application_tools/list_platform_tools take one optional flag, -verbose: by default each
prints just tool names, one per line; -verbose additionally prints each tool's Description/
InputSchema/OutputSchema. list_application_tools prints every visible tool belonging to one
of the garden-* servers above; list_platform_tools prints every other visible tool (_admin.*,
proxy_utility.*, platform-mcp-servers' own tools, etc.), including triggered tools normally
hidden from tools/list — those are suffixed " *triggered".

update_access_rules takes -file <path> (a JSON array of access-rule objects, e.g.
go-gardening-demo/initial-data/client_access_rules.json) and merges it into the gateway's
live access rules via _admin.export_role_permissions/update_role_permissions — any existing
rule whose allowed_tools are all garden-*-namespaced is replaced; everything else (the admin
role's own rule, proxy_utility.call_tool, etc.) is left untouched. cli init runs this
automatically as its last step, using that same file.

load_prompts takes -file <path> (a JSON array of {server, name, description, text} entries,
e.g. go-gardening-demo/initial-data/prompts.json) - unlike every other admin tool, it doesn't
go through a proxy _admin.* tool at all: prompts are backend-owned data, not a proxy config
section, so this groups the file's entries by their "server" field and calls that server's own
<server>.load_prompts tool directly, once per server named in the file. Each call fully
replaces that server's prompt set (a prompt loaded before but no longer listed is removed).

Every admin tool requires a role authorized to see/call what it reports or forwards to (the
"admin" role in every committed *.cfg).

Flags (same as the root cli/ tool):
  -endpoint string      Proxy MCP endpoint URL (default "http://localhost/mcp")
  -auth-server string   Base URL of the OAuth authorization server (default "http://localhost:8102")
  -auth-issuer string   Issuer the auth server's metadata must name, when it differs from -auth-server
                         (default: -auth-server)
  -client-id string     Pre-registered OAuth client_id, skipping dynamic client registration
  -redirect-port int    Fixed local port for the OAuth redirect_uri (0 = ephemeral)
  -username string      Direct credential login username, skipping the browser (demo-authorization-server only)
  -password string      Direct credential login password (see -username)

Examples:
  cli -username admin -password admin init
  cli -username admin -password admin online-store get_inventory
  cli -username admin -password admin customer-admin add_or_update_customer -id cust-0301 -name "Alice Smith" -type vip
  cli -username admin -password admin online-store purchase_products -customerId cust-0301 -item prod-01:2
  cli -username admin -password admin admin list_platform_tools
  cli -username admin -password admin admin list_application_tools
  cli -username admin -password admin admin list_application_tools -verbose
  cli -username admin -password admin admin update_access_rules -file go-gardening-demo/initial-data/client_access_rules.json
  cli -username admin -password admin admin load_prompts -file go-gardening-demo/initial-data/prompts.json

  # sample-events.json (see "init" above, which loads it automatically) can
  # also be (re)loaded on its own via the repo's other CLI, cli/,
  # which already has its own load-events command for event-aggregator:
  #   go run ./cli load-events -file go-gardening-demo/initial-data/sample-events.json

  cli -username admin -password admin
    > staff-admin get_staffers -role customer-support
    > help
    > exit
`

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		fmt.Print(topLevelUsage)
		return
	}

	fs := flag.NewFlagSet("cli", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, topLevelUsage) }
	cf := registerCommonFlags(fs)
	fs.Parse(os.Args[1:]) // stops at the first non-flag argument (the section name, if any)
	rest := fs.Args()

	ctx := context.Background()
	session, err := cf.connect(ctx)
	if err != nil {
		fatal(err)
	}
	defer session.Close()

	if len(rest) > 0 {
		if err := dispatch(ctx, session, rest); err != nil {
			fatal(err)
		}
		return
	}

	runREPL(ctx, session)
}
