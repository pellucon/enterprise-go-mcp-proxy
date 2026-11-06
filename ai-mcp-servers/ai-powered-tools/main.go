// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// ai-powered-tools is a standalone MCP backend server exposing
// _filter_tool: given a task description and a candidate tool list, asks a
// configured LLM to filter the candidates down to the ones relevant to the
// task.
//
// This server has no privileged access to go-proxy's gateway internals —
// it's an ordinary discovered backend, forwarded to exactly like
// customer-service or company-service. Unlike this server's own predecessor
// design (which fetched its own candidate list by relaying a tools/list
// call back into the proxy, presenting the identity assertion it was
// forwarded), _filter_tool takes its candidates as a plain input parameter
// instead: it's meant to be called only by go-proxy's own native
// "proxy_utility.find_relevant_tools" tool (see main.go's registerUtilityTools in
// the go-proxy module), which already has in-process access to the
// caller's authorized tool set and calls _filter_tool directly, bypassing
// the ordinary access-rule/orchestration path entirely for that inner call
// (see CLAUDE.md's "Utility tools" and "AI-powered tool discovery"
// sections). _filter_tool is deliberately left unreachable by ordinary
// clients — no accessRules entry grants any non-admin role access to it —
// so this is the only path that ever reaches it in practice.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/mcptrace"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultPromptPrefix = "Filter the list of the following tools and return only tools relevant to the following task. Return the tools as a JSON list of tool definitions. The task is: "

// FilterToolArgs is _filter_tool's input: the task plus the candidate tool
// list to filter, supplied by the caller (go-proxy's own
// proxy_utility.find_relevant_tools) rather than fetched by this server
// itself. Candidates is []map[string]any, not []mcp.Tool: mcp.Tool's own
// InputSchema/OutputSchema fields are themselves declared `any` in the
// go-sdk, and jsonschema-go infers a bare `any` struct field as the JSON
// Schema boolean-shorthand `true` rather than an object - valid per the
// JSON Schema spec, but rejected by at least one real MCP client (Claude
// Code CLI's own stricter schema validator, which requires every schema
// position to be an object). That's a go-sdk type, not one of ours, so it
// can't be fixed by retyping a field - the fix here is to not let
// jsonschema-go recurse into mcp.Tool's own definition at all.
// toolsFromMaps/mapsToTools (below) round-trip through JSON to recover
// real *mcp.Tool values for actual use, exactly like the `any`-field
// trick used elsewhere in this repo (see AIResultValidatorArgs.CallResult
// below) - only the wire-facing Go type differs, never the JSON that
// actually flows.
type FilterToolArgs struct {
	Task       string           `json:"task" jsonschema:"A natural-language description of the task you're trying to accomplish. It is used to filter the candidate tools down to just the ones relevant to that task — be specific enough that unrelated tools can be told apart from relevant ones."`
	Candidates []map[string]any `json:"candidates" jsonschema:"The candidate tools to filter, already restricted to the tools the original caller is authorized to use. Each entry is a full tool definition (name, description, inputSchema, ...)."`
}

// FilterToolResult wraps _filter_tool's list in a named field rather than
// returning a bare array: some MCP clients (e.g. Claude Desktop) require
// every tool's inferred outputSchema to be a JSON object at the top
// level, and a bare slice Out infers as an array instead. Tools is
// []map[string]any rather than []mcp.Tool for the same mcp.Tool-nested-
// any reason documented on FilterToolArgs.Candidates above. The Content
// text this produces is what go-proxy's proxy_utility.find_relevant_tools
// (utility_tools.go) and the root cli's find-relevant-tools command both
// parse, so both were updated to expect {"tools": [...]} instead of a
// bare array - the actual per-tool JSON shape is unchanged either way,
// since toolsFromMaps/mapsFromTools round-trip real mcp.Tool values
// through these maps rather than constructing anything different.
type FilterToolResult struct {
	Tools []map[string]any `json:"tools"`
}

// toolsFromMaps recovers real *mcp.Tool values from their
// map[string]any wire representation (see FilterToolArgs.Candidates'
// own doc comment for why candidates arrive this way instead of typed
// directly as []mcp.Tool).
func toolsFromMaps(maps []map[string]any) ([]*mcp.Tool, error) {
	tools := make([]*mcp.Tool, len(maps))
	for i, m := range maps {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("candidate %d: %w", i, err)
		}
		var t mcp.Tool
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, fmt.Errorf("candidate %d: %w", i, err)
		}
		tools[i] = &t
	}
	return tools, nil
}

// mapsFromTools is toolsFromMaps' inverse, used to build FilterToolResult.
func mapsFromTools(tools []mcp.Tool) ([]map[string]any, error) {
	maps := make([]map[string]any, len(tools))
	for i, t := range tools {
		raw, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("tool %d (%s): %w", i, t.Name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("tool %d (%s): %w", i, t.Name, err)
		}
		maps[i] = m
	}
	return maps, nil
}

// AIResultValidatorArgs is ai_result_validator's input: the tool call
// result to judge, plus a natural-language constraint describing what it
// must not do — supplied by the caller (go-proxy's own
// proxy_utility.result_ai_constraints_validation). CallResult is
// map[string]any, not json.RawMessage: json.RawMessage infers as a
// JSON-schema array (see the same reasoning documented on
// triggered_tools_runtime.ResultAuthorizationInput.CallResult in
// triggered-tools-runtime/contract.go) and would reject a real
// result object. A bare `any` would infer schema-unconstrained too, but
// as the literal JSON Schema boolean-shorthand `true` rather than an
// object - rejected by at least one real MCP client (Claude Code CLI).
// map[string]any infers as {"type":"object","additionalProperties":true}
// instead: still fully unconstrained, but an actual object schema - a
// safe restriction here since CallResult is always the whole marshaled
// *mcp.CallToolResult (see constraints_validation.go's own doc comment
// on this same convention), which is always a JSON object per the MCP
// spec.
type AIResultValidatorArgs struct {
	CallResult map[string]any `json:"callResult" jsonschema:"the tool call result to validate"`
	Constraint string         `json:"constraint" jsonschema:"natural-language description of what the result must not do; the tool checks whether the result violates this"`
}

// AIResultValidatorOutput is ai_result_validator's output: Ok is true when
// the result does NOT violate the constraint.
type AIResultValidatorOutput struct {
	Ok bool `json:"ok" jsonschema:"true if the result does not violate the constraint, false if it does"`
}

func createServer(runtime *Runtime) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "ai-powered-tools", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(mcptrace.Middleware())

	mcp.AddTool(server, &mcp.Tool{
		Name:        "_filter_tool",
		Description: "Given a task and a candidate tool list, return only the candidates relevant to that task. Not meant to be called directly by ordinary clients — see go-proxy's proxy_utility.find_relevant_tools.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args FilterToolArgs) (*mcp.CallToolResult, FilterToolResult, error) {
		candidates, err := toolsFromMaps(args.Candidates)
		if err != nil {
			return nil, FilterToolResult{}, fmt.Errorf("_filter_tool: invalid candidates: %w", err)
		}

		tools, err := runtime.FindRelevantTools(ctx, args.Task, candidates)
		if err != nil {
			return nil, FilterToolResult{}, fmt.Errorf("_filter_tool failed: %w", err)
		}
		resultMaps, err := mapsFromTools(tools)
		if err != nil {
			return nil, FilterToolResult{}, fmt.Errorf("_filter_tool: failed to encode result: %w", err)
		}
		return nil, FilterToolResult{Tools: resultMaps}, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "ai_result_validator",
		Description: "Given a tool call result and a natural-language constraint describing what it must not do, ask the configured LLM whether the result violates it. Not meant to be called directly by ordinary clients — see go-proxy's proxy_utility.result_ai_constraints_validation.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args AIResultValidatorArgs) (*mcp.CallToolResult, AIResultValidatorOutput, error) {
		ok, err := runtime.ValidateResult(ctx, args.CallResult, args.Constraint)
		if err != nil {
			return nil, AIResultValidatorOutput{}, fmt.Errorf("ai_result_validator failed: %w", err)
		}
		return nil, AIResultValidatorOutput{Ok: ok}, nil
	})

	return server
}

// readOptionalSecretFile reads path, if given, applying the convention this
// server uses for every secret file its k8s manifest may point at an
// optional Secret mount for: a missing file means "unconfigured" (returns
// "", logging a warning naming flagName), while any other read error or an
// empty-after-trimming file is a real misconfiguration and fails fast.
func readOptionalSecretFile(flagName, path string) string {
	if path == "" {
		return ""
	}
	fileBytes, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logging.Errorf("warning: -%s %s does not exist — _filter_tool is unconfigured, calls will fail cleanly at call time", flagName, path)
		return ""
	case err != nil:
		logging.Fatalf("failed to read -%s %s: %v", flagName, path, err)
	}
	value := strings.TrimSpace(string(fileBytes))
	if value == "" {
		logging.Fatalf("-%s %s is empty", flagName, path)
	}
	return value
}

// defaultLLMBaseURL is the one place the default LLM endpoint is defined:
// Google Gemini's OpenAI-compatible API. Neither k8s/deployment.yaml nor
// scripts/deploy.sh repeats it - deploy.sh's --ai-base-url only ever sets
// llmBaseURLEnv, and a plain kubectl apply of the manifest sets nothing,
// so both fall through to this.
const defaultLLMBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

// llmBaseURLEnv overrides the default endpoint without changing args - the
// standard k8s way (kubectl set env, an overlay's env entry), which is how
// scripts/deploy.sh's --ai-base-url applies it.
const llmBaseURLEnv = "LLM_BASE_URL"

// resolveLLMBaseURL picks the endpoint: an explicit -llm-base-url flag
// wins, then the LLM_BASE_URL environment variable, then the built-in
// default.
func resolveLLMBaseURL(flagValue, envValue string) string {
	if flagValue != "" {
		return flagValue
	}
	if envValue != "" {
		return envValue
	}
	return defaultLLMBaseURL
}

func main() {
	var (
		port                  string
		llmBaseURL            string
		model                 string
		apiKeyPath            string
		requestTimeoutSeconds int
		promptPrefix          string
		useHeadlessClaude     bool
		claudeBridgeURL       string
		claudeBridgeTokenPath string
		natsURL               string
		logLevelConfig        string
	)
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&llmBaseURL, "llm-base-url", "", "Base URL of the OpenAI-compatible chat completions endpoint. Empty means the "+llmBaseURLEnv+" environment variable if set, else "+defaultLLMBaseURL+" (Google Gemini)")
	flag.StringVar(&model, "model", "gemini-2.5-flash", "Model name to request")
	flag.StringVar(&apiKeyPath, "api-key-path", "", "Path to a file containing the LLM API key. Empty, or pointing at a file that doesn't exist, means _filter_tool is unconfigured: the tool stays registered but every call fails cleanly at call time")
	flag.IntVar(&requestTimeoutSeconds, "request-timeout-seconds", 60, "Timeout, in seconds, for a single LLM request")
	flag.StringVar(&promptPrefix, "prompt-prefix", defaultPromptPrefix, "Text prepended to the task description in the prompt sent to the LLM")
	flag.BoolVar(&useHeadlessClaude, "use-headless-claude", false, "Development-time only: get completions via a local claude-bridge instance (a developer's own authenticated Claude Code session) instead of a real LLM API key. Never for a real deployment — see CLAUDE.md's \"AI-powered tool discovery\" section")
	flag.StringVar(&claudeBridgeURL, "claude-bridge-url", "http://host.docker.internal:8099", "Base URL of the local claude-bridge instance. Only consulted when -use-headless-claude is set")
	flag.StringVar(&claudeBridgeTokenPath, "claude-bridge-token-path", "", "Path to a file containing the claude-bridge shared-secret token. Only consulted when -use-headless-claude is set; empty, or pointing at a file that doesn't exist, means _filter_tool is unconfigured, same convention as -api-key-path")
	flag.StringVar(&natsURL, "nats-url", "", "NATS server URL for live log-level reload via a go-proxy _admin.update_log_level call (empty disables live reload)")
	flag.StringVar(&logLevelConfig, "log-level-config", "./log_level_test.json", `Path to the log-level config file, as a {"version":N,"config":{"level":"error|info|debug"}} envelope — also what a go-proxy log-level-override ConfigMap is mounted at in k8s; a missing/not-yet-seeded file is not fatal, the process just keeps its default ("info") level`)
	flag.Parse()
	llmBaseURL = resolveLLMBaseURL(llmBaseURL, os.Getenv(llmBaseURLEnv))

	nc, err := configsync.Connect(natsURL)
	if err != nil {
		logging.Fatalf("Failed to connect to NATS at %q: %v", natsURL, err)
	}
	if _, err := configsync.NewWatcher("log-level", logLevelConfig, nc, &logging.LevelConfig{Level: "info"}, logging.ParseConfig, logging.ApplyConfig); err != nil {
		logging.Fatalf("Failed to load log level config %q: %v", logLevelConfig, err)
	}

	// k8s/deployment.yaml always passes -api-key-path/-claude-bridge-token-path
	// pointing at their respective (optional) Secret mounts, whether or not
	// those Secrets were actually created (see scripts/deploy.sh's
	// deploy_ai_tools) — an optional Secret that doesn't exist mounts as an
	// empty directory, so "file not found" means "unconfigured", not
	// "misconfigured", and must not crash the process either way.
	var runtime *Runtime
	if useHeadlessClaude {
		if apiKeyPath != "" {
			logging.Infof("note: -use-headless-claude is set — -api-key-path %s is ignored", apiKeyPath)
		}
		token := readOptionalSecretFile("claude-bridge-token-path", claudeBridgeTokenPath)
		runtime = NewHeadlessBridgeRuntime(claudeBridgeURL, token, time.Duration(requestTimeoutSeconds)*time.Second, promptPrefix)
		logging.Infof("startup configuration: useHeadlessClaude=true claudeBridgeUrl=%s filterToolConfigured=%v", claudeBridgeURL, token != "")
	} else {
		apiKey := readOptionalSecretFile("api-key-path", apiKeyPath)
		runtime = NewRuntime(llmBaseURL, model, apiKey, time.Duration(requestTimeoutSeconds)*time.Second, promptPrefix)
		logging.Infof("startup configuration: llmBaseUrl=%s model=%s requestTimeoutSeconds=%d filterToolConfigured=%v", llmBaseURL, model, requestTimeoutSeconds, apiKey != "")
	}

	server := createServer(runtime)
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	logging.Debugf("Starting ai-powered-tools on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
