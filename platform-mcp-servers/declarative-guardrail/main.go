// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// declarative-guardrail is a sample MCP server exposing one ordinary
// (non-triggered) tool, evaluate_declarations: given a caller's roles and
// the names of configured "declarations" to check, it reports which of
// them are currently triggered and what actions each specifies (e.g.
// block, alert, log).
//
// This tool doesn't inspect any actual user input or MCP call result
// itself - the intended caller workflow is: take some user input (or
// another MCP call's result), call evaluate_declarations with the
// caller's roles and the declarations relevant to that input, and for
// each triggered ActionResult, validate the input/result against its
// Constraint - e.g. as a JSON Schema fragment via a schema-based
// validator (see go-proxy's constraints_validation), or as free-form text
// via an LLM prompt asking whether the content violates that
// restriction. An empty Constraint is a convention meaning skip that
// validation step - every input/result counts as violating it. Only once
// a violation is confirmed (or Constraint is empty) does the caller
// perform Actions. See guardrailcfg.ActionResult's doc comment.
//
// Declarations and the catalog of valid actions both live in one JSON
// config file, loaded and validated by guardrailcfg (extracted into its
// own importable package specifically so go-proxy's admin tools can
// reuse the same parsing/validation logic - see CLAUDE.md's
// "Unify configuration into a single config-map mechanism" notes). A bad
// config still fails this process fast via log.Fatal at startup, before
// it ever starts listening. After startup, -nats-url (if set) lets a
// go-proxy _admin.update_declarative_guardrails call reach this process
// live, via configsync.Watcher reloading -config on a NATS ping - no
// restart needed. See CLAUDE.md's "Triggered tools / orchestration"
// section for how this server fits alongside authorization-mcp-server
// (which unlike this one hosts triggered tools, hidden from ordinary
// clients).
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/mcptrace"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"

	// Embeds the IANA time zone database into the binary, so a
	// declaration's optional Timezone resolves via time.LoadLocation
	// regardless of whether the deployment environment has its own
	// tzdata installed - the shared Dockerfile's alpine:3.20 base image
	// does not include the tzdata package by default, which would
	// otherwise make any non-UTC timezone fail at config-load time only
	// in the container, not in local dev on a machine with system tzdata.
	_ "time/tzdata"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EvaluateDeclarationsArgs is evaluate_declarations' input.
type EvaluateDeclarationsArgs struct {
	Roles            []string `json:"roles" jsonschema:"the caller's roles"`
	DeclarationNames []string `json:"declarationNames" jsonschema:"names of the configured declarations to evaluate"`
	AsOf             string   `json:"asOf,omitempty" jsonschema:"optional RFC3339 timestamp to evaluate as of; defaults to the current time"`
}

// EvaluateDeclarationsOutput is evaluate_declarations' output.
type EvaluateDeclarationsOutput struct {
	Results []guardrailcfg.ActionResult `json:"results"`
}

// ConfigVersionOutput is get_config_version's output.
type ConfigVersionOutput struct {
	Version int64 `json:"version" jsonschema:"configsync version of the guardrails config this server is currently evaluating with (0 = not yet loaded from the override ConfigMap)"`
}

func createServer(guardrails *configsync.Watcher[guardrailcfg.Guardrails]) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "declarative-guardrail", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(mcptrace.Middleware())

	mcp.AddTool(server, &mcp.Tool{
		Name:        "evaluate_declarations",
		Description: "Evaluate named guardrail declarations against a caller's roles. For each triggered declaration, returns a restriction (constraint) to validate your actual input/result against - if it's violated, perform the specified action(s) (e.g. block, alert, log). An empty constraint means treat every input/result as violating it, no validation needed.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args EvaluateDeclarationsArgs) (*mcp.CallToolResult, EvaluateDeclarationsOutput, error) {
		now := time.Now().UTC()
		if args.AsOf != "" {
			parsed, err := time.Parse(time.RFC3339, args.AsOf)
			if err != nil {
				return nil, EvaluateDeclarationsOutput{}, fmt.Errorf("invalid asOf %q: %w", args.AsOf, err)
			}
			now = parsed
		}

		results, err := guardrailcfg.Evaluate(guardrails.Current(), args.Roles, args.DeclarationNames, now)
		if err != nil {
			return nil, EvaluateDeclarationsOutput{}, err
		}
		return nil, EvaluateDeclarationsOutput{Results: results}, nil
	})

	// go-proxy's _admin.update_declarative_guardrails polls this after
	// writing a new version, and only returns once this server reports it
	// (or a timeout passes) - so an orchestration update that follows it,
	// referencing a just-added declaration, never reaches this server
	// before the declaration does. Without it, the kubelet's
	// ConfigMap-to-volume sync lag (up to a couple of minutes) left
	// evaluate_declarations answering "unknown declaration", and every
	// call through that orchestration step failed closed meanwhile.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_config_version",
		Description: "Report the version of the guardrails configuration this server is currently using.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ConfigVersionOutput, error) {
		return nil, ConfigVersionOutput{Version: guardrails.Version()}, nil
	})

	return server
}

func main() {
	var port, configPath, natsURL, logLevelConfig string
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&configPath, "config", "./guardrails_test.json", `Path to the guardrails config file, as a {"version":N,"config":{"actions":[...],"declarations":[...]}} envelope (matching every configsync-managed section) — this is also what a go-proxy declarative-guardrails-override ConfigMap is mounted at in k8s`)
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL for live config reload via a go-proxy _admin.update_declarative_guardrails call (empty disables live reload: -config is read once at startup)")
	flag.StringVar(&logLevelConfig, "log-level-config", "./log_level_test.json", `Path to the log-level config file, as a {"version":N,"config":{"level":"error|info|debug"}} envelope — also what a go-proxy log-level-override ConfigMap is mounted at in k8s, live-reloaded via the same NATS connection used for guardrails config reload; a missing/not-yet-seeded file is not fatal, the process just keeps its default ("info") level`)
	flag.Parse()

	if _, err := os.Stat(configPath); err != nil {
		logging.Fatalf("Guardrails config file %q not found: %v", configPath, err)
	}

	nc, err := configsync.Connect(natsURL)
	if err != nil {
		logging.Fatalf("Failed to connect to NATS at %q: %v", natsURL, err)
	}
	// initial is nil, not a zero-value fallback: the os.Stat check above
	// already guarantees configPath exists, so NewWatcher always takes the
	// file-parse branch and initial is never actually used - this still
	// fails closed (log.Fatal) on a missing/malformed config.
	guardrails, err := configsync.NewWatcher("declarative-guardrails", configPath, nc, (*guardrailcfg.Guardrails)(nil), guardrailcfg.ParseConfig, nil)
	if err != nil {
		logging.Fatalf("Failed to load guardrails config %q: %v", configPath, err)
	}

	// A missing/not-yet-seeded log-level-override ConfigMap must not block
	// startup — unlike configPath above, no os.Stat pre-check: a non-nil
	// initial lets configsync.NewWatcher fall back to it when
	// logLevelConfig doesn't exist yet.
	if _, err := configsync.NewWatcher("log-level", logLevelConfig, nc, &logging.LevelConfig{Level: "info"}, logging.ParseConfig, logging.ApplyConfig); err != nil {
		logging.Fatalf("Failed to load log level config %q: %v", logLevelConfig, err)
	}

	server := createServer(guardrails)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting declarative-guardrail on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
