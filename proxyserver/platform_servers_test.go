// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	PLATFORM_TEST_PROXY_URL     = "http://localhost:8091/mcp"
	PLATFORM_TEST_PROXY_PORT    = ":8091"
	ORCHESTRATION_DEMO_PORT     = ":8092"
	PLATFORM_AUTHORIZATION_PORT = ":8093"
	PLATFORM_GUARDRAIL_PORT     = ":8094"
	// PLATFORM_AI_PORT is the in-process ai-powered-tools stand-in
	// (createTestAIPoweredToolsServer)'s port — :8116, since every port
	// through :8115 is already claimed across this repo's other test files.
	PLATFORM_AI_PORT = ":8116"

	// PLATFORM_RATE_LIMITER_PORT is the in-process rate-limiter stand-in
	// (createTestRateLimiterServer)'s port.
	PLATFORM_RATE_LIMITER_PORT = ":8117"

	// resultLengthLimit mirrors sample-mcp-servers/authorization-mcp-server's
	// result-authorization-length-check threshold, for the in-process stand-in.
	resultLengthLimit = 50
)

// badToolInvocationTracker records whether a given orchestration-demo tool
// handler was ever actually invoked, so a call-authorization-reject
// scenario can assert the real backend call never happened. Reused for
// both bad_tool and amount_tool (each gets its own instance).
type badToolInvocationTracker struct {
	mu      sync.Mutex
	invoked bool
}

func (b *badToolInvocationTracker) markInvoked() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.invoked = true
}

func (b *badToolInvocationTracker) wasInvoked() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.invoked
}

// AmountArgs/AmountResult are amount_tool's input/output — a numeric field
// on both the request arguments and the typed result's StructuredContent,
// used to exercise proxy_utility.request_constraints_validation/
// result_constraints_validation against orchestration_test.cfg's
// "amount-limit"/"result-amount-limit" declarative constraints.
type AmountArgs struct {
	Amount float64 `json:"amount"`
}
type AmountResult struct {
	Total float64 `json:"total"`
}

// BudgetArgs/BudgetResult are budget_tool's input/output, exercising
// proxy_utility.business_logic_constraints_validation (see
// validateBudgetToolInput below): unlike amount_tool (guarded by
// request_constraints_validation's own generic JSON-Schema check against
// the raw request), budget_tool is guarded by a business validation tool -
// validate_budget_tool, below - hosted on the SAME server as budget_tool
// itself, standing in for garden-online-store.validate_store_requests'
// real shape (hand-written Go logic specific to one target tool, not a
// generic schema check).
type BudgetArgs struct {
	Amount float64 `json:"amount"`
}
type BudgetResult struct {
	Total float64 `json:"total"`
}

// validateBudgetToolParameters is validate_budget_tool's own opaque
// Parameters shape - the declarative guardrail declaration "budget-limit"
// (orchestration_test.cfg) supplies {"maxAmount": 1000} via its Constraint,
// which proxy_utility.business_logic_constraints_validation passes through
// verbatim as CallAuthorizationInput.Parameters.
type validateBudgetToolParameters struct {
	MaxAmount float64 `json:"maxAmount"`
}

// DisclosureArgs is disclosure_tool's input, mirroring sample-mcp-servers/
// misc-demo-server's own DisclosureArgs (package main there, can't be
// imported) — used to exercise proxy_utility.result_ai_constraints_validation
// against orchestration_test.cfg's "result-no-ssn-disclosure" declaration.
type DisclosureArgs struct {
	Violate bool `json:"violate"`
}

// createOrchestrationDemoServer builds the in-process ordinary backend
// guarded by orchestration_test.cfg's orchestration:
//   - say_hello: clean path, exercises full success.
//   - bad_tool: name contains "bad" — rejected by the orchestration's FIRST
//     callAuthorization step.
//   - guarded_tool: clean name — passes the first callAuthorization step,
//     but is rejected by the SECOND (call-authorization-role-check) when
//     called by role "guest", demonstrating that a phase's steps are all
//     consulted, not just the first.
//   - sneaky_tool: clean name, a "bad"-containing result — rejected by the
//     orchestration's FIRST resultAuthorization step.
//   - long_result_tool: clean name, a long-but-clean result — passes the
//     first resultAuthorization step but is rejected by the SECOND
//     (result-authorization-length-check).
//   - amount_tool: guarded by the "declarative-constraint-guardrails"
//     orchestration (proxy_utility.request_constraints_validation/
//     result_constraints_validation) instead of authorization-mcp-server —
//     an amount over 1000 violates "amount-limit" on the request phase; a
//     total over 1000 would violate "result-amount-limit" on the result
//     phase (amount_tool always echoes amount back as total, so the same
//     value drives both checks).
//   - disclosure_tool: guarded by the "ai-declarative-constraint-guardrails"
//     orchestration (proxy_utility.result_ai_constraints_validation) —
//     violate=true returns SSN-disclosing text, judged by the in-process
//     ai_result_validator stand-in (createTestAIPoweredToolsServer) against
//     the "result-no-ssn-disclosure" declaration.
//   - budget_tool: guarded by the "business-constraint-guardrails"
//     orchestration (proxy_utility.business_logic_constraints_validation), which
//     delegates to validate_budget_tool - a business validation tool
//     hosted on this SAME server, mirroring
//     garden-online-store.validate_store_requests' real shape - rather
//     than checking anything itself.
//   - validate_budget_tool: the business validation tool above - a
//     triggered tool (hidden from tools/list, invocable only by
//     business_logic_constraints_validation), rejecting when
//     CallRequest.arguments.amount exceeds its own Parameters'
//     ({"maxAmount": N}) limit.
func createOrchestrationDemoServer(badToolTracker, amountToolTracker, budgetToolTracker *badToolInvocationTracker) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "orchestration-demo", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "budget_tool", Description: "A tool guarded by business logic validation (validate_budget_tool) on its amount argument"},
		func(ctx context.Context, req *mcp.CallToolRequest, args BudgetArgs) (*mcp.CallToolResult, BudgetResult, error) {
			budgetToolTracker.markInvoked()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "processed"}}}, BudgetResult{Total: args.Amount}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "validate_budget_tool", Meta: mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			var params validateBudgetToolParameters
			if in.Parameters != "" {
				if err := json.Unmarshal([]byte(in.Parameters), &params); err != nil {
					return nil, triggered_tools_runtime.AuthorizationOutput{}, err
				}
			}
			// in.CallRequest is map[string]any here, not json.RawMessage:
			// this tool is invoked over a real MCP wire round-trip (by
			// business_logic_constraints_validation, itself relaying it to this
			// discovered server), and encoding/json decodes a JSON object
			// into an untyped `any` field as a map. Re-marshaling handles
			// this (and a native-style json.RawMessage) uniformly.
			raw, err := json.Marshal(in.CallRequest)
			if err != nil {
				return nil, triggered_tools_runtime.AuthorizationOutput{}, err
			}
			var call struct {
				Arguments BudgetArgs `json:"arguments"`
			}
			if err := json.Unmarshal(raw, &call); err != nil {
				return nil, triggered_tools_runtime.AuthorizationOutput{}, err
			}
			if params.MaxAmount > 0 && call.Arguments.Amount > params.MaxAmount {
				return nil, triggered_tools_runtime.AuthorizationOutput{
					Authorized: false,
					Reason:     fmt.Sprintf("amount %.2f exceeds validate_budget_tool's own limit of %.2f (target %s.%s)", call.Arguments.Amount, params.MaxAmount, in.TargetServer, in.TargetTool),
				}, nil
			}

			// Prove a discovered triggered tool can now relay back through
			// the real proxy exactly as an ordinary backend call already
			// can (see runtime.go's platformHTTPClient and
			// CLAUDE.md's "Internal identity assertions" section) - not
			// just that headers reach it (see contract_test.go/
			// runtime_test.go's own unit coverage of that), but that a
			// full relayed call through the real, running proxy actually
			// succeeds end to end. A relay failure here is deliberately
			// reported as a rejection, not silently ignored, so Scenario
			// 12 below (amount=500, expect success) only passes if this
			// relay genuinely worked.
			incoming := relayheaders.Extract(req.GetExtra().Header)
			if incoming.Assertion == "" || incoming.CallbackURL == "" {
				return nil, triggered_tools_runtime.AuthorizationOutput{
					Authorized: false,
					Reason:     "validate_budget_tool: missing identity assertion or callback URL - is internalAuth and selfUrl configured on the proxy?",
				}, nil
			}
			relayClient := mcp.NewClient(&mcp.Implementation{Name: "orchestration-demo", Version: "1.0.0"}, nil)
			relaySession, err := relayClient.Connect(ctx, &mcp.StreamableClientTransport{
				Endpoint:   incoming.CallbackURL,
				HTTPClient: incoming.Client(nil),
			}, nil)
			if err != nil {
				return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_budget_tool: failed to connect back to proxy at %s: %w", incoming.CallbackURL, err)
			}
			defer relaySession.Close()
			relayResult, err := relaySession.CallTool(ctx, &mcp.CallToolParams{
				Name:      "orchestration-demo.say_hello",
				Arguments: map[string]any{"language": "relay-check", "count": 1},
			})
			if err != nil {
				return nil, triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("validate_budget_tool: relayed say_hello call failed: %w", err)
			}
			if relayResult.IsError {
				return nil, triggered_tools_runtime.AuthorizationOutput{
					Authorized: false,
					Reason:     fmt.Sprintf("validate_budget_tool: relayed say_hello call rejected: %v", relayResult.Content),
				}, nil
			}

			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "amount_tool", Description: "A tool guarded by declarative constraint validation on its amount argument/total result"},
		func(ctx context.Context, req *mcp.CallToolRequest, args AmountArgs) (*mcp.CallToolResult, AmountResult, error) {
			amountToolTracker.markInvoked()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "processed"}}}, AmountResult{Total: args.Amount}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "disclosure_tool", Description: "A tool guarded by LLM-judged declarative constraint validation on its result"},
		func(ctx context.Context, req *mcp.CallToolRequest, args DisclosureArgs) (*mcp.CallToolResult, any, error) {
			text := "Order confirmed. Total: $42.10, ships in 3 days."
			if args.Violate {
				text = "Customer SSN on file: 123-45-6789. Order confirmed. Total: $42.10."
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "say_hello", Description: "Say hello"},
		func(ctx context.Context, req *mcp.CallToolRequest, args SayHello) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "Hello there mate " + args.Language}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "bad_tool", Description: "A tool whose name is rejected by call-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			badToolTracker.markInvoked()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "should never get here"}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "guarded_tool", Description: "A tool rejected by call-authorization-role-check for role \"guest\""},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "should never get here for guest"}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "sneaky_tool", Description: "A tool whose result is rejected by result-authorization"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "this result contains bad content"}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "long_result_tool", Description: "A tool whose result is rejected by result-authorization-length-check"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "This is a deliberately long, otherwise clean result used only to exceed the length limit."}}}, nil, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "rate_limited_tool", Description: "A trivial tool used only to exercise go-proxy's rate-limiting gate, never called by any other scenario"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})

	return server
}

// createTestAuthorizationServer is an in-process stand-in for
// sample-mcp-servers/authorization-mcp-server, duplicated here since a
// package-main test file can't import another package-main directory.
// Two independent tools per phase, matching the real sample server, to
// exercise an orchestration whose callAuthorization/resultAuthorization
// lists each hold more than one step. Every tool carries the
// triggered-tool Meta annotation, matching the real sample server, so this
// test also exercises the tool-level hiding mechanism (see scenario 0 in
// TestPlatformServersOrchestration below) rather than relying on any
// server-level classification.
func createTestAuthorizationServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "authorization-mcp-server", Version: "1.0.0"}, nil)
	platformMeta := mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true}

	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization", Meta: platformMeta},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			name, _ := extractCallRequestName(in.CallRequest)
			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: !strings.Contains(name, "bad")}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "call-authorization-role-check", Meta: platformMeta},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.CallAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: !slices.Contains(in.Roles, "guest")}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "result-authorization", Meta: platformMeta},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.ResultAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: !resultTextContainsBad(in.CallResult)}, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "result-authorization-length-check", Meta: platformMeta},
		func(ctx context.Context, req *mcp.CallToolRequest, in triggered_tools_runtime.ResultAuthorizationInput) (*mcp.CallToolResult, triggered_tools_runtime.AuthorizationOutput, error) {
			return nil, triggered_tools_runtime.AuthorizationOutput{Authorized: !resultTextExceeds(in.CallResult, resultLengthLimit)}, nil
		})

	return server
}

// guardrailEvaluateArgs/guardrailEvaluateOutput mirror
// declarative-guardrail/main.go's own EvaluateDeclarationsArgs/Output field
// for field (package main there, can't be imported).
type guardrailEvaluateArgs struct {
	Roles            []string `json:"roles"`
	DeclarationNames []string `json:"declarationNames"`
}
type guardrailEvaluateOutput struct {
	Results []guardrailcfg.ActionResult `json:"results"`
}

// createTestDeclarativeGuardrailServer is an in-process stand-in for
// declarative-guardrail's evaluate_declarations tool. Unlike
// createTestAuthorizationServer, no logic is duplicated here — guardrailcfg
// is an importable package (not package main), so this reuses the real
// guardrailcfg.Evaluate/ParseConfig directly, against the same
// "declarativeGuardrails" section read straight out of
// orchestration_test.cfg (the single canonical example — see
// writeOrchestrationTestConfigWithNatsURL's own doc comment for the same
// rationale).
func createTestDeclarativeGuardrailServer(t *testing.T) *mcp.Server {
	t.Helper()
	data, err := os.ReadFile("testdata/orchestration_test.cfg")
	if err != nil {
		t.Fatalf("failed to read orchestration_test.cfg: %v", err)
	}
	var cfg struct {
		DeclarativeGuardrails json.RawMessage `json:"declarativeGuardrails"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to parse orchestration_test.cfg: %v", err)
	}
	guardrails, err := guardrailcfg.ParseConfig(cfg.DeclarativeGuardrails)
	if err != nil {
		t.Fatalf("failed to parse declarativeGuardrails section: %v", err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "declarative-guardrail", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "evaluate_declarations"},
		func(ctx context.Context, req *mcp.CallToolRequest, args guardrailEvaluateArgs) (*mcp.CallToolResult, guardrailEvaluateOutput, error) {
			results, err := guardrailcfg.Evaluate(guardrails, args.Roles, args.DeclarationNames, time.Now().UTC())
			if err != nil {
				return nil, guardrailEvaluateOutput{}, err
			}
			return nil, guardrailEvaluateOutput{Results: results}, nil
		})
	return server
}

// aiResultValidatorArgs/aiResultValidatorOutput mirror ai-mcp-servers/
// ai-powered-tools/main.go's own AIResultValidatorArgs/AIResultValidatorOutput
// field for field (package main there, can't be imported).
type aiResultValidatorArgs struct {
	CallResult any    `json:"callResult"`
	Constraint string `json:"constraint"`
}
type aiResultValidatorOutput struct {
	Ok bool `json:"ok"`
}

// createTestAIPoweredToolsServer is an in-process stand-in for
// ai-mcp-servers/ai-powered-tools' ai_result_validator tool. Unlike the
// real tool, this never calls an actual LLM: a real LLM call in
// go test ./... would be flaky, slow, and cost money on every run, so this
// judges the constraint with plain deterministic Go logic instead — the
// only thing this test needs to prove is that
// proxy_utility.result_ai_constraints_validation correctly wires a result
// through to whatever ai_result_validator decides, not that a real LLM
// judges English text correctly (that's exercised manually, see
// CLAUDE.md's "Declarative constraint validation" section).
func createTestAIPoweredToolsServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "ai-powered-tools", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "ai_result_validator"},
		func(ctx context.Context, req *mcp.CallToolRequest, args aiResultValidatorArgs) (*mcp.CallToolResult, aiResultValidatorOutput, error) {
			violates := anyResultText(args.CallResult, func(text string) bool { return strings.Contains(text, "SSN") })
			return nil, aiResultValidatorOutput{Ok: !violates}, nil
		})
	return server
}

func intPtr(v int) *int { return &v }

// testRoleStats/testStatsOutput mirror rate-limiter's own get_stats output
// shape closely enough for this test's assertions (total count) — a
// trimmed-down stand-in, not a byte-for-byte copy, since this test doesn't
// need to re-verify the full aggregation logic (that's covered directly by
// platform-mcp-servers/rate-limiter's own main_test.go).
type testRoleStats struct {
	Count int `json:"count"`
}
type testStatsOutput struct {
	Total testRoleStats `json:"total"`
}

// checkPermissionTestArgs/testDecision mirror rate-limiter's own
// CheckPermissionArgs/ratelimit.Decision wire shape (duplicated rather than
// imported: that server is its own package main, same "package main can't
// import another package main" constraint as every other stand-in here).
type checkPermissionTestArgs struct {
	Tool           string   `json:"tool"`
	Roles          []string `json:"roles"`
	TimeoutSeconds *int     `json:"timeoutSeconds,omitempty"`
}

type testDecision struct {
	Allowed bool `json:"allowed"`
}

// rateLimiterCallCounter counts how many times check_permission was actually
// invoked, per tool — so a scenario can assert an *unconfigured* tool never
// generated a single request to this stand-in at all.
type rateLimiterCallCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *rateLimiterCallCounter) increment(tool string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts == nil {
		c.counts = map[string]int{}
	}
	c.counts[tool]++
}

func (c *rateLimiterCallCounter) count(tool string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[tool]
}

// createTestRateLimiterServer is an in-process stand-in for
// platform-mcp-servers/rate-limiter, built on the real, importable
// ratelimit.Limiter engine (not reimplemented) so the test exercises the
// genuine algorithm, not a fake. Only check_permission's polling logic
// itself is duplicated here (a much shorter poll interval than production's
// 100ms, so timeout-based scenarios stay fast).
func createTestRateLimiterServer(t *testing.T, cfg *ratelimit.Config, calls *rateLimiterCallCounter) *mcp.Server {
	// A frozen clock, not the real one: two real HTTP round trips a few
	// milliseconds apart are enough real elapsed time for a low
	// globalPerMinute bucket to decay by a hair, which can flip a strict "<"
	// threshold comparison and admit a call that should have been denied.
	// Freezing time removes that timing sensitivity entirely, the same way
	// the ratelimit package's own unit tests do.
	frozenNow := time.Now()
	limiter, err := ratelimit.NewLimiter(cfg,
		ratelimit.WithClock(func() time.Time { return frozenNow }),
		ratelimit.WithPollInterval(20*time.Millisecond),
	)
	if err != nil {
		t.Fatalf("failed to build test rate limiter: %v", err)
	}
	var blockedMu sync.Mutex
	blockedCount := 0

	server := mcp.NewServer(&mcp.Implementation{Name: "rate-limiter", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name: "check_permission",
		Meta: mcp.Meta{triggered_tools_runtime.TriggeredToolMetaKey: true},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args checkPermissionTestArgs) (*mcp.CallToolResult, testDecision, error) {
		calls.increment(args.Tool)
		waitCtx := ctx
		if args.TimeoutSeconds != nil {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, time.Duration(*args.TimeoutSeconds)*time.Second)
			defer cancel()
		}
		d := limiter.Wait(waitCtx, args.Tool, args.Roles)
		if !d.Allowed {
			blockedMu.Lock()
			blockedCount++
			blockedMu.Unlock()
		}
		return nil, testDecision{Allowed: d.Allowed}, nil
	})
	mcp.AddTool(server, &mcp.Tool{Name: "get_stats", Description: "Report blocked-call counts (test stand-in)"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, testStatsOutput, error) {
			blockedMu.Lock()
			defer blockedMu.Unlock()
			return nil, testStatsOutput{Total: testRoleStats{Count: blockedCount}}, nil
		})
	return server
}

func extractCallRequestName(callRequest any) (string, bool) {
	m, ok := callRequest.(map[string]any)
	if !ok {
		return "", false
	}
	name, ok := m["name"].(string)
	return name, ok
}

func resultTextContainsBad(callResult any) bool {
	return anyResultText(callResult, func(text string) bool { return strings.Contains(text, "bad") })
}

func resultTextExceeds(callResult any, limit int) bool {
	return anyResultText(callResult, func(text string) bool { return len(text) > limit })
}

// anyResultText reports whether pred is true for any text content in a
// marshaled *mcp.CallToolResult (what Guard sends as CallResult).
func anyResultText(callResult any, pred func(text string) bool) bool {
	m, ok := callResult.(map[string]any)
	if !ok {
		return false
	}
	content, ok := m["content"].([]any)
	if !ok {
		return false
	}
	for _, c := range content {
		item, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := item["text"].(string); ok && pred(text) {
			return true
		}
	}
	return false
}

// assertToolRequestResultEvent polls buffer (via the shared
// waitForEventCount helper in event_system_test.go, same package) for the
// n-th published "tool-request-result" event and checks its Subtype and
// whether its "result" metadata key is present — the replacement for the
// old two-audit-tool recorder now that go-proxy itself publishes this
// event directly (see triggered_tools_runtime.ToolCallAuditSentinel)
// instead of invoking any tool. Also checks a non-empty "toolCallId" is
// present (see CLAUDE.md's "Tool-call correlation ID" section) and returns
// it, so callers can assert distinct calls get distinct IDs.
func assertToolRequestResultEvent(t *testing.T, buffer *testEventBuffer, n int, wantSubtype string, wantResult bool) string {
	t.Helper()
	entries := waitForEventCount(t, buffer, "tool-request-result", n)
	entry := entries[n-1]
	if entry.Subtype != wantSubtype {
		t.Errorf("tool-request-result entry #%d: expected Subtype=%q, got %+v", n, wantSubtype, entry)
	}
	if (entry.Metadata["result"] != nil) != wantResult {
		t.Errorf("tool-request-result entry #%d: expected result present=%v, got %+v", n, wantResult, entry)
	}
	toolCallID, _ := entry.Metadata["toolCallId"].(string)
	if toolCallID == "" {
		t.Errorf("tool-request-result entry #%d: expected a non-empty toolCallId, got %+v", n, entry)
	}
	return toolCallID
}

// connectAsRoles is connectAsRole (mcp_client_test.go) extended to mint a
// token asserting multiple roles at once (a JSON array "role" claim), for
// TestPlatformServersOrchestration's Scenario 3b below. Left as a
// standalone helper rather than modifying the shared
// GenerateTestTokenForRole/connectAsRole (which only ever mint a single
// role), to avoid any risk to the many existing tests that already depend
// on them - same rationale as internal_auth_test.go's
// mintExternalTokenForSubRole.
func connectAsRoles(ctx context.Context, url string, roles []string) (*mcp.ClientSession, error) {
	claims := jwt.MapClaims{
		"sub":  "udi_r",
		"role": roles,
		"exp":  time.Now().Add(time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(myRsaPrivateKey)
	if err != nil {
		return nil, err
	}
	testToken = tokenString

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: newTestTokenClient(),
	}, nil)
}

// writeOrchestrationTestConfigWithNatsURL loads the checked-in,
// human-readable orchestration_test.cfg, injects natsUrl (only known at
// test-run time, once the embedded NATS server has picked a random port),
// and writes the result to a t.TempDir() copy — keeping
// orchestration_test.cfg itself the single canonical example rather than
// duplicating its content as an inline Go string.
func writeOrchestrationTestConfigWithNatsURL(t *testing.T, natsURL string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/orchestration_test.cfg")
	if err != nil {
		t.Fatalf("failed to read orchestration_test.cfg: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("failed to parse orchestration_test.cfg: %v", err)
	}
	raw["natsUrl"] = natsURL
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("failed to re-marshal orchestration_test.cfg with natsUrl: %v", err)
	}
	path := t.TempDir() + "/orchestration-test-with-nats.cfg"
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	return path
}

// TestPlatformServersOrchestration follows integration_test.go's
// TestProxyClient pattern exactly (in-process backend servers on fixed
// ports, doRun-equivalent against a static-discovery test config) rather
// than deploying to a real k8s cluster — runs its own proxy instance via
// doRunOn on a distinct port/config so it coexists with TestProxyClient's
// permanently-bound :8081/:8082/:8083 in the same test binary.
func TestPlatformServersOrchestration(t *testing.T) {
	badToolTracker := &badToolInvocationTracker{}
	amountToolTracker := &badToolInvocationTracker{}
	budgetToolTracker := &badToolInvocationTracker{}
	demoServer := createOrchestrationDemoServer(badToolTracker, amountToolTracker, budgetToolTracker)
	authServer := createTestAuthorizationServer()
	guardrailServer := createTestDeclarativeGuardrailServer(t)
	aiServer := createTestAIPoweredToolsServer()
	rateLimiterCalls := &rateLimiterCallCounter{}
	// Must match orchestration_test.cfg's own "rateLimiting" section (the
	// copy go-proxy reads to decide whether to ask at all); a real
	// deployment sources both from the same config, kept in sync here by
	// hand since this stand-in never reads a file.
	rateLimiterServer := createTestRateLimiterServer(t, &ratelimit.Config{
		Tools: map[string]ratelimit.ToolConfig{
			"orchestration-demo.rate_limited_tool":    {GlobalPerMinute: intPtr(1)},
			"orchestration-demo.validate_budget_tool": {GlobalPerMinute: intPtr(1)},
		},
	}, rateLimiterCalls)

	// Embedded NATS + a subscriber into a buffer, standing in for a real
	// subscriber's (e.g. event-aggregator's) subscribe-and-log role — this
	// test only needs to observe what go-proxy itself publishes, not run a
	// real subscriber process. startTestNATSServer/testEventBuffer/waitForEventCount are
	// shared with event_system_test.go: both are package main test files
	// in the same directory/package, so no cross-package-main duplication
	// is needed here (unlike the in-process backend server stand-ins
	// above, which really do live in a different compiled binary).
	natsURL := startTestNATSServer(t)
	subscriberClient, err := events.NewClient(natsURL)
	if err != nil {
		t.Fatalf("failed to build subscriber events client: %v", err)
	}
	defer subscriberClient.Close()
	// The same dictionary go-proxy publishes with (orchestration_test.cfg's
	// eventDictionary), as event-aggregator uses in a real deployment:
	// Subscribe drops anything it doesn't declare.
	orchestrationCfgRaw, err := os.ReadFile("testdata/orchestration_test.cfg")
	if err != nil {
		t.Fatalf("failed to read orchestration_test.cfg: %v", err)
	}
	var orchestrationCfg struct {
		EventDictionary json.RawMessage `json:"eventDictionary"`
	}
	if err := json.Unmarshal(orchestrationCfgRaw, &orchestrationCfg); err != nil {
		t.Fatalf("failed to parse orchestration_test.cfg: %v", err)
	}
	dict, err := events.ParseDictionary(orchestrationCfg.EventDictionary)
	if err != nil {
		t.Fatalf("failed to parse test dictionary: %v", err)
	}
	subscriberClient.SetDictionary(dict)
	buffer := &testEventBuffer{}
	if _, err := subscriberClient.Subscribe("events.tool-request-result.>", buffer.add); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	// Also captures the alert events proxy_utility.request_constraints_validation/
	// result_constraints_validation publish (see Scenario 7 below) — a
	// separate subject from tool-request-result, so a distinct subscription
	// is needed (NATS wildcard matching is token-based, not a string
	// prefix — see CLAUDE.md's "NATS event system" section).
	if _, err := subscriberClient.Subscribe("events.alert.>", buffer.add); err != nil {
		t.Fatalf("failed to subscribe: %v", err)
	}
	if err := subscriberClient.Flush(); err != nil {
		t.Fatalf("failed to flush subscriber: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestBackend(t, ctx, cancel, ORCHESTRATION_DEMO_PORT, demoServer)
	startTestBackend(t, ctx, cancel, PLATFORM_AUTHORIZATION_PORT, authServer)
	startTestBackend(t, ctx, cancel, PLATFORM_GUARDRAIL_PORT, guardrailServer)
	startTestBackend(t, ctx, cancel, PLATFORM_AI_PORT, aiServer)
	startTestBackend(t, ctx, cancel, PLATFORM_RATE_LIMITER_PORT, rateLimiterServer)
	cfgPath := writeOrchestrationTestConfigWithNatsURL(t, natsURL)
	startTestProxy(t, ctx, cancel, cfgPath, PLATFORM_TEST_PROXY_PORT)

	// Scenario 0: the four authorization-mcp-server triggered tools must
	// never appear in tools/list, regardless of role — they're hidden
	// purely by their own _meta annotation now, not by which server they
	// happen to live on.
	adminSession, err := connectAsRole(context.Background(), PLATFORM_TEST_PROXY_URL, "admin")
	if err != nil {
		t.Fatalf("failed to connect as admin: %v", err)
	}
	defer adminSession.Close()
	toolsResult, err := adminSession.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("failed to list tools: %v", err)
	}
	hiddenToolNames := map[string]bool{
		"authorization-mcp-server.call-authorization":                true,
		"authorization-mcp-server.call-authorization-role-check":     true,
		"authorization-mcp-server.result-authorization":              true,
		"authorization-mcp-server.result-authorization-length-check": true,
	}
	for _, tool := range toolsResult.Tools {
		if hiddenToolNames[tool.Name] {
			t.Errorf("expected triggered tool %q to be hidden from tools/list, but it was returned", tool.Name)
		}
	}

	// Scenario 1: full success — every callAuthorization/resultAuthorization
	// step passes, the real result is returned, and go-proxy publishes one
	// tool-request-result/call-succeeded event with the actual result
	// included.
	sayHelloParams := mcp.CallToolParams{
		Name:      "orchestration-demo.say_hello",
		Arguments: map[string]any{"language": "english", "count": 1},
	}
	result, err := callTool(PLATFORM_TEST_PROXY_URL, sayHelloParams)
	if err != nil {
		t.Fatalf("expected say_hello to succeed, got error: %v", err)
	}
	if result != "Hello there mate english" {
		t.Errorf("unexpected say_hello result: %q", result)
	}
	sayHelloToolCallID := assertToolRequestResultEvent(t, buffer, 1, triggered_tools_runtime.ToolRequestResultCallSucceeded, true)

	// Scenario 2: rejected by the FIRST callAuthorization step (name-based)
	// — the client gets an error, the real backend handler is never
	// invoked, and go-proxy still publishes a result-blocked event with
	// no result.
	badToolParams := mcp.CallToolParams{Name: "orchestration-demo.bad_tool", Arguments: map[string]any{}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, badToolParams); err == nil {
		t.Error("expected bad_tool call to be rejected by call-authorization")
	}
	if badToolTracker.wasInvoked() {
		t.Error("expected bad_tool's handler to never be invoked (call-authorization should reject before the real call)")
	}
	badToolCallID := assertToolRequestResultEvent(t, buffer, 2, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)
	if badToolCallID == sayHelloToolCallID {
		t.Error("expected say_hello and bad_tool calls to get distinct toolCallIds")
	}

	// Scenario 3: passes the FIRST callAuthorization step but rejected by
	// the SECOND (call-authorization-role-check, role "guest") — proves
	// every step in a phase is actually consulted, not just the first.
	guestSession, err := connectAsRole(context.Background(), PLATFORM_TEST_PROXY_URL, "guest")
	if err != nil {
		t.Fatalf("failed to connect as guest: %v", err)
	}
	defer guestSession.Close()
	if _, err := guestSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "orchestration-demo.guarded_tool", Arguments: map[string]any{}}); err == nil {
		t.Error("expected guarded_tool call as role \"guest\" to be rejected by call-authorization-role-check")
	}
	assertToolRequestResultEvent(t, buffer, 3, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)

	// Scenario 3b: same as Scenario 3, but the caller holds "guest" AND
	// another role at once. Regression test for a real bug: the internal
	// identity assertion/CallerIdentity used to collapse multiple roles
	// into one comma-joined string (e.g. "guest,manager"), which
	// call-authorization-role-check's exact-match "!= guest" check would
	// never match, silently letting a guest-plus-other-role caller through.
	// Roles are now carried as a real []string end-to-end, so this must
	// still be rejected.
	multiRoleSession, err := connectAsRoles(context.Background(), PLATFORM_TEST_PROXY_URL, []string{"guest", "manager"})
	if err != nil {
		t.Fatalf("failed to connect as [guest, manager]: %v", err)
	}
	defer multiRoleSession.Close()
	if _, err := multiRoleSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "orchestration-demo.guarded_tool", Arguments: map[string]any{}}); err == nil {
		t.Error("expected guarded_tool call as roles [guest, manager] to still be rejected by call-authorization-role-check")
	}
	assertToolRequestResultEvent(t, buffer, 4, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)

	// Scenario 5: rejected by the FIRST resultAuthorization step (keyword
	// "bad") — the real backend call DOES happen, but the client still
	// gets an error, and go-proxy publishes a result-blocked event
	// without the rejected result: a result a guardrail kept from the
	// client mustn't reach the audit trail instead.
	sneakyParams := mcp.CallToolParams{Name: "orchestration-demo.sneaky_tool", Arguments: map[string]any{}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, sneakyParams); err == nil {
		t.Error("expected sneaky_tool call to be rejected by result-authorization")
	}
	assertToolRequestResultEvent(t, buffer, 5, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)

	// Scenario 6: passes the FIRST resultAuthorization step (no "bad"
	// keyword) but rejected by the SECOND
	// (result-authorization-length-check) — proves every step in a phase
	// is consulted here too, symmetric with scenario 3.
	longResultParams := mcp.CallToolParams{Name: "orchestration-demo.long_result_tool", Arguments: map[string]any{}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, longResultParams); err == nil {
		t.Error("expected long_result_tool call to be rejected by result-authorization-length-check")
	}
	assertToolRequestResultEvent(t, buffer, 6, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)

	// Scenario 7: orchestration-demo.amount_tool, guarded by the separate
	// "declarative-constraint-guardrails" orchestration (proxy_utility.
	// request_constraints_validation, not authorization-mcp-server). An
	// amount of 5000 violates the "amount-limit" declaration (maximum
	// 1000), whose actions are ["block", "alert"] — the call is rejected
	// before amount_tool's own handler runs, and BOTH a tool-call-blocked
	// and a tool-call-alert alert event are published (independently, one
	// per triggered action).
	tooLargeParams := mcp.CallToolParams{Name: "orchestration-demo.amount_tool", Arguments: map[string]any{"amount": 5000}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, tooLargeParams); err == nil {
		t.Error("expected amount_tool call with amount=5000 to be rejected by request_constraints_validation")
	}
	if amountToolTracker.wasInvoked() {
		t.Error("expected amount_tool's handler to never be invoked (call-authorization should reject before the real call)")
	}
	assertToolRequestResultEvent(t, buffer, 7, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)
	alertEvents := waitForEventCount(t, buffer, "alert", 2)
	var sawBlocked, sawAlert bool
	for _, e := range alertEvents {
		switch e.Subtype {
		case "tool-call-blocked":
			sawBlocked = true
		case "tool-call-alert":
			sawAlert = true
		}
	}
	if !sawBlocked {
		t.Errorf("expected a tool-call-blocked alert event, got: %+v", alertEvents)
	}
	if !sawAlert {
		t.Errorf("expected a tool-call-alert alert event, got: %+v", alertEvents)
	}

	// Scenario 8: amount_tool with amount=500 satisfies both the request-
	// and result-phase declarative constraints (maximum 1000 each) — the
	// real backend call happens (amount_tool echoes amount back as total)
	// and the result is returned normally.
	okAmountParams := mcp.CallToolParams{Name: "orchestration-demo.amount_tool", Arguments: map[string]any{"amount": 500}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, okAmountParams); err != nil {
		t.Fatalf("expected amount_tool call with amount=500 to succeed, got: %v", err)
	}
	if !amountToolTracker.wasInvoked() {
		t.Error("expected amount_tool's handler to be invoked for amount=500")
	}
	assertToolRequestResultEvent(t, buffer, 8, triggered_tools_runtime.ToolRequestResultCallSucceeded, true)

	// Scenario 9: orchestration-demo.disclosure_tool with violate=true,
	// guarded by the "ai-declarative-constraint-guardrails" orchestration
	// (proxy_utility.result_ai_constraints_validation). This is a
	// result-phase check, so the real backend call DOES happen (unlike
	// Scenario 7's request-phase rejection) — the in-process
	// ai_result_validator stand-in judges the SSN-disclosing text as
	// violating "result-no-ssn-disclosure" (whose actions are
	// ["block", "alert"]), so the call is rejected and BOTH a
	// result-blocked and a result-alert alert event are published.
	violatingParams := mcp.CallToolParams{Name: "orchestration-demo.disclosure_tool", Arguments: map[string]any{"violate": true}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, violatingParams); err == nil {
		t.Error("expected disclosure_tool call with violate=true to be rejected by result_ai_constraints_validation")
	}
	// No result on the audit event: the SSN the guardrail blocked must
	// not reach the audit trail.
	assertToolRequestResultEvent(t, buffer, 9, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)
	aiAlertEvents := waitForEventCount(t, buffer, "alert", 4)
	var sawResultBlocked, sawResultAlert bool
	for _, e := range aiAlertEvents {
		switch e.Subtype {
		case "result-blocked":
			sawResultBlocked = true
		case "result-alert":
			sawResultAlert = true
		}
	}
	if !sawResultBlocked {
		t.Errorf("expected a result-blocked alert event, got: %+v", aiAlertEvents)
	}
	if !sawResultAlert {
		t.Errorf("expected a result-alert alert event, got: %+v", aiAlertEvents)
	}

	// Scenario 10: disclosure_tool with violate=false — the AI validator
	// stand-in judges the clean text as not violating the constraint, so
	// the call succeeds normally.
	cleanParams := mcp.CallToolParams{Name: "orchestration-demo.disclosure_tool", Arguments: map[string]any{"violate": false}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, cleanParams); err != nil {
		t.Fatalf("expected disclosure_tool call with violate=false to succeed, got: %v", err)
	}
	assertToolRequestResultEvent(t, buffer, 10, triggered_tools_runtime.ToolRequestResultCallSucceeded, true)

	// Scenario 11: orchestration-demo.budget_tool, guarded by the
	// "business-constraint-guardrails" orchestration (proxy_utility.
	// business_logic_constraints_validation, not authorization-mcp-server or a
	// generic JSON-Schema check). An amount of 5000 violates the
	// "budget-limit" declaration's {"maxAmount": 1000} - decided entirely
	// by validate_budget_tool, the business validation tool hosted on
	// orchestration-demo itself, which business_logic_constraints_validation
	// dials via CallAuthorizationInput.TargetServer (populated by Guard
	// from the namespaced tool name being guarded) - proving the
	// TargetServer/TargetTool extension actually works end-to-end, not
	// just at the type level.
	tooLargeBudgetParams := mcp.CallToolParams{Name: "orchestration-demo.budget_tool", Arguments: map[string]any{"amount": 5000}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, tooLargeBudgetParams); err == nil {
		t.Error("expected budget_tool call with amount=5000 to be rejected by business_logic_constraints_validation")
	}
	if budgetToolTracker.wasInvoked() {
		t.Error("expected budget_tool's handler to never be invoked (call-authorization should reject before the real call)")
	}
	assertToolRequestResultEvent(t, buffer, 11, triggered_tools_runtime.ToolRequestResultRequestBlocked, false)

	// Scenario 12: budget_tool with amount=500 satisfies "budget-limit" -
	// validate_budget_tool authorizes it, the real backend call happens,
	// and the result is returned normally.
	okBudgetParams := mcp.CallToolParams{Name: "orchestration-demo.budget_tool", Arguments: map[string]any{"amount": 500}}
	if _, err := callTool(PLATFORM_TEST_PROXY_URL, okBudgetParams); err != nil {
		t.Fatalf("expected budget_tool call with amount=500 to succeed, got: %v", err)
	}
	if !budgetToolTracker.wasInvoked() {
		t.Error("expected budget_tool's handler to be invoked for amount=500")
	}
	assertToolRequestResultEvent(t, buffer, 12, triggered_tools_runtime.ToolRequestResultCallSucceeded, true)

	// Scenario 13: rate limiting. orchestration-demo.rate_limited_tool is
	// configured for globalPerMinute=1 with a 1-second timeoutSeconds and
	// has never been called by any earlier scenario, so its bucket starts
	// completely fresh here. The first call is admitted (calm phase); the
	// second is denied only after roughly the configured 1s wait (the
	// stand-in's check_permission polls every 20ms until its own inbound
	// ctx — bounded by go-proxy's timeoutSeconds — is cancelled).
	rateLimitedParams := mcp.CallToolParams{Name: "orchestration-demo.rate_limited_tool", Arguments: map[string]any{}}
	if _, err := adminSession.CallTool(context.Background(), &rateLimitedParams); err != nil {
		t.Fatalf("expected the first rate_limited_tool call to be admitted, got: %v", err)
	}
	start := time.Now()
	_, err = adminSession.CallTool(context.Background(), &rateLimitedParams)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("expected the second rate_limited_tool call to be rate limited")
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("expected the rate-limited call to wait roughly the configured 1s timeout before failing, only took %v", elapsed)
	}

	// Scenario 14: a tool NOT present in rateLimiting.tools must never
	// generate a single check_permission call, regardless of volume.
	guardedParams := mcp.CallToolParams{Name: "orchestration-demo.guarded_tool", Arguments: map[string]any{}}
	for i := 0; i < 5; i++ {
		if _, err := adminSession.CallTool(context.Background(), &guardedParams); err != nil {
			t.Fatalf("call %d: expected an unconfigured tool to be unaffected by rate limiting, got: %v", i, err)
		}
	}
	if n := rateLimiterCalls.count("orchestration-demo.guarded_tool"); n != 0 {
		t.Errorf("expected zero check_permission calls for an unconfigured tool, got %d", n)
	}

	// Scenario 15: get_stats reports Scenario 13's blocked call, admin-only.
	// Runs before Scenario 16 deliberately: Scenario 16 calls a completely
	// unregistered tool name, which the transport treats as fatal to the
	// whole client session (an HTTP-level "Bad Request", not an ordinary
	// JSON-RPC error) — reusing adminSession afterward would fail for an
	// unrelated, confusing reason, so every remaining adminSession use
	// happens first.
	//
	// A brief pause first: Scenario 13's rate-limited call returned to the
	// client the instant go-proxy's own outbound context deadline expired,
	// which races the stand-in's own goroutine actually executing its
	// blockedCount++ a moment later — without this, get_stats could be
	// queried before that increment has happened yet.
	time.Sleep(100 * time.Millisecond)
	statsResult, err := adminSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "rate-limiter.get_stats", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("expected admin to call rate-limiter.get_stats successfully, got: %v", err)
	}
	statsRaw, err := json.Marshal(statsResult.StructuredContent)
	if err != nil {
		t.Fatalf("failed to marshal get_stats output: %v", err)
	}
	var stats struct {
		Total struct {
			Count int `json:"count"`
		} `json:"total"`
	}
	if err := json.Unmarshal(statsRaw, &stats); err != nil {
		t.Fatalf("failed to decode get_stats output: %v", err)
	}
	if stats.Total.Count < 1 {
		t.Errorf("expected get_stats to report at least 1 blocked call, got %+v", stats)
	}

	// Reconnect fresh as guest rather than reusing the earlier guestSession:
	// this test harness's HTTP client reads a single shared, package-level
	// testToken at request time (see newTestTokenClient), set as a
	// side effect of connectAsRole/callTool — not bound per-session — so
	// after all the adminSession calls above, testToken is "admin" again
	// regardless of which *mcp.ClientSession object issues the call.
	// connectAsRole's own side effect of resetting testToken is exactly
	// what makes this reconnect meaningful here.
	freshGuestSession, err := connectAsRole(context.Background(), PLATFORM_TEST_PROXY_URL, "guest")
	if err != nil {
		t.Fatalf("failed to reconnect as guest: %v", err)
	}
	defer freshGuestSession.Close()
	if _, err := freshGuestSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "rate-limiter.get_stats", Arguments: map[string]any{}}); err == nil {
		t.Error("expected a non-admin role to be denied access to rate-limiter.get_stats")
	}

	// Scenario 16 (last, deliberately): orchestration-demo.validate_budget_tool
	// is a triggered tool (hidden, never registered on the gateway's own MCP
	// server) that ALSO — mistakenly — has a rateLimiting entry. Calling it
	// directly still fails (tool not found: it was never registered at all,
	// unrelated to rate limiting), but the important assertion is that
	// checkRateLimit's triggered-tool skip engaged: zero check_permission
	// calls, proving it was ignored with a warning rather than actually
	// rate-limited. Uses freshGuestSession (guest can reach
	// "orchestration-demo.*" by name pattern) rather than adminSession,
	// since testToken is "guest" again after the reconnect above — the
	// specific role doesn't matter for this assertion (the tool isn't
	// registered at all, regardless of caller).
	if _, err := freshGuestSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "orchestration-demo.validate_budget_tool", Arguments: map[string]any{}}); err == nil {
		t.Error("expected a direct call to a triggered tool to fail (it's never registered on the gateway)")
	}
	if n := rateLimiterCalls.count("orchestration-demo.validate_budget_tool"); n != 0 {
		t.Errorf("expected zero check_permission calls for a triggered tool (should be skipped, not rate-limited), got %d", n)
	}
}
