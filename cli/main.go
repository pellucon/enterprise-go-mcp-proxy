// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// cli is a manual CLI for exercising a running go-proxy instance end-to-end:
// it logs in via OAuth (see oauthclient/) against an authorization
// server — by default demo-authorization-server — and issues MCP calls
// through the proxy with the resulting access token, the same way
// mcp_client_test.go's test-only helpers do, but as a standalone binary you
// can point at any endpoint (local, port-forwarded, or a NodePort).
//
// It authenticates once, then either runs one command given on argv and
// exits (scriptable mode) or, when no command is given, drops into an
// interactive REPL reading one command per line until exit/quit/EOF — the
// same one-login/many-commands shape as go-gardening-demo/cli, though this
// tool's own command set stays flat (list-tools, call-tool, ...), not
// reorganized into that CLI's <section> <tool> hierarchy.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

const topLevelUsage = `cli: manually exercise MCP servers through a running go-proxy.

Usage:
  cli [flags] [<command> [-flag value...]]

If a command is given, it runs once and the CLI exits (scriptable mode).
Otherwise the CLI drops into an interactive loop: type one command per line, "exit"/"quit" to leave.

Commands:
  list-tools                      List tools visible to the given role
  list-prompts                    List prompts visible to the given role
  list-resources                  List resources visible to the given role
  call-tool                       Call a tool
  get-prompt                      Fetch a prompt
  read-resource                   Read a resource
  export-role-permissions         Export the gateway's role rules, with their version, as JSON (admin only)
  update-role-permissions         Replace the gateway's role rules (admin only)
  export-orchestration            Export the orchestration config, with its version, as JSON (admin only)
  update-orchestration            Replace the orchestration config (admin only)
  export-discovery                Export the discovery config, with its version, as JSON (admin only)
  update-discovery                Replace the discovery config (admin only)
  export-event-dictionary         Export the event dictionary, with its version, as JSON (admin only)
  update-event-dictionary         Replace the event dictionary (admin only)
  export-declarative-guardrails   Export the declarative-guardrails config, with its version, as JSON (admin only)
  update-declarative-guardrails   Replace the declarative-guardrails config (admin only)
  export-event-aggregation        Export event-aggregator's ingestion filter and tiered-storage
                                   thresholds, with its version, as JSON (admin only)
  update-event-aggregation        Replace event-aggregator's ingestion filter and tiered-storage
                                   thresholds (admin only)
  export-rate-limiting            Export the rate limiting config, with its version, as JSON (admin only)
  update-rate-limiting            Replace the rate limiting config (admin only)
  export-log-level                Export the runtime log level, with its version, as JSON (admin only)
  update-log-level                Replace the runtime log level (admin only)
  export-full-config              Export every section as one proxy-server.cfg-shaped document, with no
                                   version numbers - deploying with it reproduces the same configuration
                                   (admin only)
  find-relevant-tools             Ask the configured LLM to filter your visible tools down to a task
  list-events                     List events from event-aggregator, paging via cursor/nextCursor
                                   automatically until every match has been printed
  load-events                     Load a local JSON file (-file, required) of events into event-aggregator's
                                   buffer, replacing its contents

Flags (given once, before any command — they apply for the whole run, one-shot or REPL):
  -endpoint string      Proxy MCP endpoint URL (default "http://localhost/mcp" — the stable
                         k8s deployment URL; ":80" is the default HTTP port, so it's omitted:
                         http://localhost/mcp and http://localhost:80/mcp are identical)
  -auth-server string   Base URL of the OAuth authorization server to log in against (default
                         "http://localhost:8102" — demo-authorization-server's default local
                         port; see scripts/deploy.sh's --start-demo-auth)
  -auth-issuer string   Issuer the authorization server's metadata must name, when it differs
                         from -auth-server - a server reached at another address than its issuer
                         (default: -auth-server)
  -client-id string     Pre-registered OAuth client_id to use instead of dynamic client
                         registration. Needed for providers that don't support DCR (most
                         real-world providers); normally left empty against
                         demo-authorization-server, which always supports it
  -redirect-port int    Fixed local port to bind for the OAuth redirect_uri (0 = pick an
                         ephemeral port). Only meaningful together with -client-id, when that
                         client was pre-registered against one specific, fixed redirect_uri in a
                         real provider's console
  -audience string      OAuth "audience" parameter to request. Required by Auth0 (and similar
                         providers) to receive a signed JWT access token instead of an opaque
                         one scoped to the provider's own userinfo endpoint; unused against
                         demo-authorization-server, which ignores it
  -scope string         Space-separated OAuth "scope" parameter to request (e.g.
                         "openid mcp.access"). Required by Okta custom authorization servers (and
                         similar providers that reject a scope-less authorization request); empty
                         (default) sends no scope at all, which is all demo-authorization-server
                         needs
  -username string      Username for a direct credential login, skipping the browser entirely
                         (must be paired with -password). Only supported by
                         demo-authorization-server — real OAuth providers require the browser
                         redirect flow, which is what happens by default when -username/-password
                         are omitted
  -password string      Password for a direct credential login (see -username)

If you omit -username/-password, cli prints an authorization URL and waits for you to open it in
a browser and log in there; it does not launch a browser itself.

Once connected, type "help" at the "> " prompt to print this same text again, or "exit"/"quit"
to leave.

Examples:
  # Against the k8s deployment (default endpoint, no -endpoint needed) - prints a login URL
  # and waits for you to open it in a browser, then drops into the interactive loop
  cli

  # Log in without a browser, by POSTing credentials directly (demo-authorization-server only),
  # then drop into the interactive loop
  cli -username admin -password admin
    > list-tools
    > call-tool -name event-aggregator.list_events -args '{"limit":10}'
    > load-events -file go-gardening-demo/initial-data/sample-events.json
    > help
    > exit

  # Against a locally-running standalone go-proxy instead (./go-proxy, :8081)
  cli -endpoint http://localhost:8081/mcp -username admin -password admin

  # Scriptable one-shot mode: flags come before the command, since parsing stops at the first
  # non-flag argument - the command name
  cli -username admin -password admin list-tools
  cli -username admin -password admin call-tool -name event-aggregator.list_events -args '{"limit":10}'

  # Exercise access control as a specific role: the user must actually exist in
  # demo-authorization-server first (via its admin API), then log in as them - the "role" and
  # "sub" claims now come entirely from who logs in, not from a flag
  echo '[{"username":"alice","roles":["finance"]}]' > alice.json
  demo-authorization-server/client add-users -file alice.json
  cli -username alice -password alice list-tools

  # ...or with multiple roles at once
  echo '[{"username":"alice","roles":["finance","manager"]}]' > alice.json
  demo-authorization-server/client add-users -file alice.json
  cli -username alice -password alice list-tools

  # Export the current role rules (as a {"version":N,"config":{...}} envelope),
  # edit config in place, then push the whole file back - a version mismatch
  # against the config map's actual current version (e.g. another admin's
  # update landed first) is rejected, so re-export and retry
  cli -username admin -password admin export-role-permissions > rules.json
  cli -username admin -password admin update-role-permissions -file rules.json

  # Same shape for the other 7 configsync-managed sections
  cli -username admin -password admin export-orchestration > orchestration.json
  cli -username admin -password admin update-orchestration -file orchestration.json
  cli -username admin -password admin export-discovery > discovery.json
  cli -username admin -password admin update-discovery -file discovery.json
  cli -username admin -password admin export-event-dictionary > event-dictionary.json
  cli -username admin -password admin update-event-dictionary -file event-dictionary.json
  cli -username admin -password admin export-declarative-guardrails > declarative-guardrails.json
  cli -username admin -password admin update-declarative-guardrails -file declarative-guardrails.json
  cli -username admin -password admin export-event-aggregation > event-aggregation.json
  cli -username admin -password admin update-event-aggregation -file event-aggregation.json
  cli -username admin -password admin export-rate-limiting > rate-limiting.json
  cli -username admin -password admin update-rate-limiting -file rate-limiting.json
  cli -username admin -password admin export-log-level > log-level.json
  cli -username admin -password admin update-log-level -file log-level.json

  # Export the gateway's entire current configuration as a ready-to-deploy
  # proxy-server.cfg (no version numbers - unlike the per-section exports
  # above, this isn't meant to be fed back into an update-* command). Do
  # this after live updates and redeploy it, so the cfg matches the live
  # config: go-proxy refuses to start with a cfg that was changed so that
  # it differs from the live config
  cli -username admin -password admin export-full-config > proxy-server.cfg

  # Ask the configured LLM which of your visible tools are relevant to a task
  cli -username admin -password admin find-relevant-tools -task "book a flight and find a hotel"

  # List every triggered tool - reserved for the proxy's own orchestration engine and hidden
  # from an ordinary list-tools, so this is the only way to see one's name at all. Default is
  # just names; -args '{"verbose":true}' returns the full Tool object for each
  cli -username admin -password admin call-tool -name proxy_utility.list_triggered_tools
  cli -username admin -password admin call-tool -name proxy_utility.list_triggered_tools -args '{"verbose":true}'

  # Load a local events dataset (e.g. go-gardening-demo/initial-data/sample-events.json,
  # generated by go-gardening-demo/gendata - normally loaded automatically by
  # go-gardening-demo/cli's own "init"/"events load_events", not this command),
  # then filter it with every list_events filter field at once (all fields are
  # AND'd; roles matches if the event has at least one of the given roles;
  # tool/reason accept "*" wildcards) via a single call-tool invocation (one
  # page only - see list-events below for automatic pagination)
  cli -username admin -password admin load-events -file go-gardening-demo/initial-data/sample-events.json
  cli -username admin -password admin call-tool -name event-aggregator.list_events -args '{"type":"alert","user":"alice","roles":["admin","business-manager"],"tool":"garden-online-store.*","reason":"*blocked*","toolCallId":"3f29...","limit":20}'

  # Same filters as list_events itself, but as flags, and paged automatically:
  # list-events keeps following nextCursor - across both event-aggregator's
  # in-memory buffer and its on-disk log - until every match has been
  # printed, rather than returning just one page like call-tool above.
  # -page-size sets events-per-underlying-call (default 50), not a cap on
  # the total printed.
  cli -username admin -password admin list-events
  cli -username admin -password admin list-events -type alert -roles admin,business-manager -page-size 100
  cli -username admin -password admin list-events -since 2026-01-01T00:00:00Z -tool "garden-online-store.*"

  # Against a real OAuth provider instead of demo-authorization-server: supply a pre-registered
  # client_id and, if that provider requires one fixed redirect_uri port, -redirect-port to match
  cli -auth-server https://accounts.example.com -client-id abc123 -redirect-port 8765

  # Against a free Auth0 tenant specifically: Auth0 requires -audience (its API Identifier) to
  # issue a signed JWT access token instead of an opaque one - proxy-server.cfg's own auth.jwksUrl/
  # issuer/audience/roleClaim must be pointed at the same tenant/API/custom-claim (see
  # CLAUDE.md's "Auth and access control" section)
  cli -auth-server https://YOUR_TENANT.us.auth0.com -client-id abc123 -redirect-port 8765 \
    -audience https://go-proxy-api

  # Against an enterprise deployment behind nginx with Okta (see docs/enterprise-deployment/enterprise-deployment.md):
  # Okta has no anonymous DCR, so -client-id is required, and its custom authorization server
  # rejects a request without -scope
  cli -endpoint https://mcp.acme.com/mcp -auth-server https://acme.okta.com/oauth2/default \
    -client-id 0oa1b2c3d4 -redirect-port 8765 -scope "openid mcp.access"
`

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help") {
		fmt.Print(topLevelUsage)
		return
	}

	fs := flag.NewFlagSet("cli", flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, topLevelUsage) }
	cf := registerCommonFlags(fs)
	fs.Parse(os.Args[1:]) // stops at the first non-flag argument (the command name, if any)
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
