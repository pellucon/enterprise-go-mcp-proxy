// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// startInProcessGuardrailSession builds a hermetic, in-memory (no real
// network socket, no HTTP) evaluate_declarations server from raw and
// returns a connected client session to it — a lighter-weight companion to
// TestPlatformServersOrchestration's end-to-end createTestDeclarativeGuardrailServer,
// used here to unit-test constraintsValidation directly against a
// hand-built declaration set.
func startInProcessGuardrailSession(t *testing.T, ctx context.Context, raw guardrailcfg.RawConfig) *mcp.ClientSession {
	t.Helper()
	rawJSON, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("failed to marshal test RawConfig: %v", err)
	}
	guardrails, err := guardrailcfg.ParseConfig(rawJSON)
	if err != nil {
		t.Fatalf("failed to parse test RawConfig: %v", err)
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

	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("failed to connect in-memory server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("failed to connect in-memory client: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestConstraintsValidation_PartialFailureTolerance proves the "try all of
// them even if one of them failed" requirement directly against
// constraintsValidation: a malformed constraint (unparseable JSON) can't be
// checked, so it counts as triggered (fail closed) - without aborting
// evaluation of the other declarations in the same call. Also covers: an
// empty constraint always triggers, a satisfied schema constraint doesn't,
// a violated one does, and "format" is enforced.
func TestConstraintsValidation_PartialFailureTolerance(t *testing.T) {
	declaration := func(name, constraint string) guardrailcfg.RawDeclaration {
		return guardrailcfg.RawDeclaration{Name: name, Constraint: constraint, RoleExpression: "_any_role", Actions: []string{"log"}}
	}
	raw := guardrailcfg.RawConfig{
		Actions: []string{"log"},
		Declarations: []guardrailcfg.RawDeclaration{
			declaration("empty-constraint", ""),
			declaration("malformed", "{not valid json"),
			declaration("satisfied", `{"type":"object","properties":{"x":{"type":"number","maximum":100}}}`),
			declaration("violated", `{"type":"object","properties":{"x":{"type":"number","maximum":10}}}`),
			declaration("format-violated", `{"type":"object","properties":{"email":{"type":"string","format":"email"}}}`),
		},
	}

	ctx := context.Background()
	sess := startInProcessGuardrailSession(t, ctx, raw)

	target := map[string]any{"x": 50, "email": "not-an-email"}
	declarationNames := []string{"empty-constraint", "malformed", "satisfied", "violated", "format-violated"}
	results, err := constraintsValidation(ctx, sess, target, []string{"user"}, declarationNames)
	if err != nil {
		t.Fatalf("constraintsValidation failed: %v", err)
	}

	got := map[string]string{}
	for _, r := range results {
		got[r.DeclarationName] = r.uncheckedReason
	}
	want := []string{"empty-constraint", "malformed", "violated", "format-violated"}
	if len(got) != len(want) {
		t.Fatalf("triggered declarations = %v, want %v", got, want)
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("declaration %q didn't trigger; triggered: %v", name, got)
		}
	}
	if !strings.Contains(got["malformed"], "invalid constraint") {
		t.Errorf("malformed constraint's reason = %q, want it to say it's an invalid constraint", got["malformed"])
	}
	for _, name := range []string{"empty-constraint", "violated", "format-violated"} {
		if got[name] != "" {
			t.Errorf("declaration %q was checked and violated, but has unchecked reason %q", name, got[name])
		}
	}
}

// startInProcessAIValidator serves an ai_result_validator stand-in whose
// verdict reply decides; calls counts its invocations.
func startInProcessAIValidator(t *testing.T, ctx context.Context, reply func() (*mcp.CallToolResult, error), calls *int) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "ai-powered-tools", Version: "1.0.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "ai_result_validator", InputSchema: &jsonschema.Schema{Type: "object"}},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			*calls++
			return reply()
		})
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("failed to connect in-memory AI validator: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("failed to connect to in-memory AI validator: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// TestAIConstraintsValidation_FailsClosed checks that every way the AI
// validator can fail to give a verdict makes the declaration triggered,
// with a "could not be checked" reason, and that a real verdict is used.
func TestAIConstraintsValidation_FailsClosed(t *testing.T) {
	ctx := context.Background()
	raw := guardrailcfg.RawConfig{
		Actions:      []string{"block"},
		Declarations: []guardrailcfg.RawDeclaration{{Name: "no-ssn", Constraint: "must not disclose an SSN", RoleExpression: "_any_role", Actions: []string{"block"}}},
	}
	guardrailSess := startInProcessGuardrailSession(t, ctx, raw)
	verdict := func(ok bool) func() (*mcp.CallToolResult, error) {
		return func() (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: map[string]any{"ok": ok}}, nil
		}
	}

	cases := []struct {
		name          string
		reply         func() (*mcp.CallToolResult, error)
		sessErr       error
		target        any
		maxInput      int
		wantTriggered bool
		wantUnchecked string // substring of the unchecked reason; "" means checked
		wantCalls     int
	}{
		{name: "ok verdict", reply: verdict(true), target: "fine", maxInput: 1024, wantTriggered: false, wantCalls: 1},
		{name: "violation verdict", reply: verdict(false), target: "123-45-6789", maxInput: 1024, wantTriggered: true, wantCalls: 1},
		{name: "validator error result", reply: func() (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "LLM returned 429"}}}, nil
		}, target: "x", maxInput: 1024, wantTriggered: true, wantUnchecked: "rejected the call", wantCalls: 1},
		{name: "unparseable output", reply: func() (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{StructuredContent: "not an object"}, nil
		}, target: "x", maxInput: 1024, wantTriggered: true, wantUnchecked: "decode", wantCalls: 1},
		{name: "validator unavailable", sessErr: errors.New("not discovered"), target: "x", maxInput: 1024, wantTriggered: true, wantUnchecked: "AI validator unavailable", wantCalls: 0},
		{name: "too large", reply: verdict(true), target: strings.Repeat("a", 2000), maxInput: 1024, wantTriggered: true, wantUnchecked: "too large to check", wantCalls: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			var aiSess *mcp.ClientSession
			if tc.sessErr == nil {
				aiSess = startInProcessAIValidator(t, ctx, tc.reply, &calls)
			}
			results, err := aiConstraintsValidation(ctx, guardrailSess, aiSess, tc.sessErr, tc.target, []string{"user"}, []string{"no-ssn"}, tc.maxInput)
			if err != nil {
				t.Fatalf("aiConstraintsValidation: %v", err)
			}
			if (len(results) == 1) != tc.wantTriggered {
				t.Fatalf("triggered = %v, want %v", results, tc.wantTriggered)
			}
			if tc.wantTriggered && !strings.Contains(results[0].uncheckedReason, tc.wantUnchecked) {
				t.Errorf("unchecked reason = %q, want it to contain %q", results[0].uncheckedReason, tc.wantUnchecked)
			}
			if tc.wantUnchecked == "" && tc.wantTriggered && results[0].uncheckedReason != "" {
				t.Errorf("a real violation has unchecked reason %q", results[0].uncheckedReason)
			}
			if calls != tc.wantCalls {
				t.Errorf("validator called %d times, want %d", calls, tc.wantCalls)
			}
		})
	}
}

// TestDispatchConstraintActions_UncheckedFollowsActions checks that a
// declaration that couldn't be checked is handled by its own actions -
// "block" blocks (and says it couldn't be checked), "alert"/"log" don't.
func TestDispatchConstraintActions_UncheckedFollowsActions(t *testing.T) {
	old := eventsClient
	eventsClient = &events.Client{} // no dictionary: alerts are attempted and logged as failed
	t.Cleanup(func() { eventsClient = old })

	unchecked := func(name string, actions ...string) triggeredDeclaration {
		return triggeredDeclaration{ActionResult: guardrailcfg.ActionResult{DeclarationName: name, Actions: actions}, uncheckedReason: "AI validator unavailable"}
	}
	if blocked, _ := dispatchConstraintActions("b", "a", "u", nil, "t", "id", []triggeredDeclaration{unchecked("alert-only", "alert"), unchecked("log-only", "log")}); blocked {
		t.Error("alert/log-only declarations that couldn't be checked blocked the call")
	}
	blocked, names := dispatchConstraintActions("b", "a", "u", nil, "t", "id", []triggeredDeclaration{unchecked("must-block", "block", "alert")})
	if !blocked || len(names) != 1 || names[0] != "must-block (could not be checked)" {
		t.Errorf("block declaration that couldn't be checked: blocked=%v names=%v; want blocked, named as not checked", blocked, names)
	}
}

// serveMCP serves server over Streamable HTTP for the test's lifetime and
// returns its URL.
func serveMCP(t *testing.T, server *mcp.Server) string {
	t.Helper()
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestBusinessLogicConstraintsValidation_FailsClosed checks the business
// validation path's per-declaration fail-closed cases: a malformed
// constraint, and a target server that isn't reachable, both make a
// "block" declaration block the call with a "could not be checked" reason.
func TestBusinessLogicConstraintsValidation_FailsClosed(t *testing.T) {
	oldRuntime, oldEvents := triggeredRuntime, eventsClient
	t.Cleanup(func() { triggeredRuntime, eventsClient = oldRuntime, oldEvents })
	eventsClient = &events.Client{}

	raw := guardrailcfg.RawConfig{
		Actions: []string{"block"},
		Declarations: []guardrailcfg.RawDeclaration{
			{Name: "malformed", Constraint: "{not json", RoleExpression: "_any_role", Actions: []string{"block"}},
			{Name: "well-formed", Constraint: `{"validationTool":"validate_it"}`, RoleExpression: "_any_role", Actions: []string{"block"}},
		},
	}
	rawJSON, _ := json.Marshal(raw)
	guardrails, err := guardrailcfg.ParseConfig(rawJSON)
	if err != nil {
		t.Fatal(err)
	}
	guardrail := mcp.NewServer(&mcp.Implementation{Name: "declarative-guardrail", Version: "1.0.0"}, nil)
	mcp.AddTool(guardrail, &mcp.Tool{Name: "evaluate_declarations"},
		func(ctx context.Context, req *mcp.CallToolRequest, args guardrailEvaluateArgs) (*mcp.CallToolResult, guardrailEvaluateOutput, error) {
			results, err := guardrailcfg.Evaluate(guardrails, args.Roles, args.DeclarationNames, time.Now().UTC())
			return nil, guardrailEvaluateOutput{Results: results}, err
		})

	triggeredRuntime = triggered_tools_runtime.NewRuntime(nil)
	triggeredRuntime.OnAdded(discover_mcp_servers.ServerMap{
		"declarative-guardrail": {ServerName: "declarative-guardrail", ServerUrl: serveMCP(t, guardrail)},
	})

	call := func(declaration string) triggered_tools_runtime.AuthorizationOutput {
		t.Helper()
		out, err := businessLogicConstraintsValidation(context.Background(), triggered_tools_runtime.CallAuthorizationInput{
			User: "alice", Roles: []string{"user"}, TargetServer: "store", TargetTool: "buy",
			CallRequest: map[string]any{"name": "store.buy"},
			Parameters:  `{"declarationNames":["` + declaration + `"]}`,
		})
		if err != nil {
			t.Fatalf("%s: step failed instead of deciding per declaration: %v", declaration, err)
		}
		return out
	}
	// "store" isn't discovered, so the target server is unreachable.
	for _, declaration := range []string{"malformed", "well-formed"} {
		out := call(declaration)
		if out.Authorized || !strings.Contains(out.Reason, "could not be checked") {
			t.Errorf("%s: got %+v; want blocked, as could not be checked", declaration, out)
		}
	}
}

func TestToolErrorTextShowsText(t *testing.T) {
	got := toolErrorText([]mcp.Content{&mcp.TextContent{Text: "LLM returned 401"}, &mcp.TextContent{Text: "bad key"}})
	if got != "LLM returned 401 bad key" {
		t.Errorf("toolErrorText = %q", got)
	}
}

// TestConstraintsValidation_NoResultsFailsClosed checks that an
// evaluate_declarations answer without "results" is an error, not an empty
// list: read as one, it would let every call through as if no declaration
// triggered.
func TestConstraintsValidation_NoResultsFailsClosed(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "declarative-guardrail", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "evaluate_declarations"},
		func(context.Context, *mcp.CallToolRequest, guardrailEvaluateArgs) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil // answers {} - no "results"
		})
	t1, t2 := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, t1, nil); err != nil {
		t.Fatal(err)
	}
	sess, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil).Connect(ctx, t2, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	if _, err := constraintsValidation(ctx, sess, map[string]any{"x": 1}, []string{"user"}, []string{"any"}); err == nil {
		t.Fatal("an answer without results was accepted")
	}
}

// TestViolatesConstraintLoadsNoExternalRefs checks that a constraint can't
// make go-proxy read a local file through $ref.
func TestViolatesConstraintLoadsNoExternalRefs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(path, []byte(`{"type":"string"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := violatesConstraint(`{"$ref":"file://`+path+`"}`, 5); err == nil {
		t.Fatal("a constraint referring to a local file was compiled")
	}
}
