// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// declarativeGuardrailEvaluateArgs/declarativeGuardrailEvaluateOutput mirror
// declarative-guardrail/main.go's own EvaluateDeclarationsArgs/Output field
// for field — that server is package main, so those types can't be
// imported directly (the same constraint every other in-process backend
// stand-in in this repo works around). Results reuses guardrailcfg.
// ActionResult directly, since that struct lives in the importable
// guardrailcfg subpackage, not package main — admin_tools.go already
// imports guardrailcfg the same way.
type declarativeGuardrailEvaluateArgs struct {
	Roles            []string `json:"roles"`
	DeclarationNames []string `json:"declarationNames"`
}
type declarativeGuardrailEvaluateOutput struct {
	// A pointer, so an answer without "results" is told apart from an
	// empty list (see evaluateDeclarations).
	Results *[]guardrailcfg.ActionResult `json:"results"`
}

// triggeredDeclaration is a declaration whose actions apply to the call:
// its constraint was violated, or it couldn't be checked at all
// (uncheckedReason set). Every guardrail check fails closed per
// declaration: a check that can't produce a verdict - a validator error, an
// unreachable validator, an invalid constraint, data too large to check -
// counts as triggered, and the declaration's own actions (block/alert/log)
// decide what happens, exactly as for a real violation.
type triggeredDeclaration struct {
	guardrailcfg.ActionResult
	uncheckedReason string
}

func violatedDeclaration(ar guardrailcfg.ActionResult) triggeredDeclaration {
	return triggeredDeclaration{ActionResult: ar}
}

// uncheckedDeclaration logs why ar couldn't be checked and returns it as
// triggered, so its own actions apply.
func uncheckedDeclaration(step string, ar guardrailcfg.ActionResult, format string, args ...any) triggeredDeclaration {
	reason := fmt.Sprintf(format, args...)
	logging.Errorf("%s: declaration %q could not be checked, failing closed: %s", step, ar.DeclarationName, reason)
	return triggeredDeclaration{ActionResult: ar, uncheckedReason: reason}
}

// toolErrorText is the text of an error tool result's content, for an
// error message (formatting the content slice itself with %v prints
// pointers).
func toolErrorText(content []mcp.Content) string {
	var parts []string
	for _, c := range content {
		if text, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		} else {
			parts = append(parts, fmt.Sprintf("<%T>", c))
		}
	}
	return strings.Join(parts, " ")
}

// defaultDeclarativeGuardrailServer is the discovered server name
// constraints_validation calls evaluate_declarations on when a step's
// Parameters doesn't override it via guardrailServer.
const defaultDeclarativeGuardrailServer = "declarative-guardrail"

// constraintsValidationParameters is request_constraints_validation's/
// result_constraints_validation's shared OrchestrationStep.Parameters shape.
// DeclarationNames is required — an empty/absent Parameters (or an explicit
// empty list) means this step has nothing to check, so it always
// authorizes, mirroring authorization-mcp-server's existing
// no-op-on-absent convention for Parameters. GuardrailServer is optional
// (defaults to defaultDeclarativeGuardrailServer) — an operator who renames
// or duplicates the declarative-guardrail backend can still target it
// without a code change, the same config-driven flexibility
// OrchestrationStep.Server/Tool already give every other step.
type constraintsValidationParameters struct {
	DeclarationNames []string `json:"declarationNames"`
	GuardrailServer  string   `json:"guardrailServer,omitempty"`
}

func decodeConstraintsValidationParameters(parameters string) (constraintsValidationParameters, error) {
	p := constraintsValidationParameters{GuardrailServer: defaultDeclarativeGuardrailServer}
	if parameters == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(parameters), &p); err != nil {
		return p, fmt.Errorf("failed to decode parameters %q: %w", parameters, err)
	}
	if p.GuardrailServer == "" {
		p.GuardrailServer = defaultDeclarativeGuardrailServer
	}
	return p, nil
}

// constraintsValidation calls declarative-guardrail's evaluate_declarations
// tool via sess with roles/declarationNames, then — for every returned
// ActionResult — decides whether it "triggered": an empty Constraint always
// counts as triggered (declarative-guardrail's own documented convention:
// every input/result violates an empty constraint, no validation needed);
// a non-empty Constraint is compiled as a JSON Schema fragment and target
// is validated against it, and a validation FAILURE means target violates
// it (triggered). A Constraint that fails to compile, or a validate call
// that errors for some other reason, can't be checked, so it's triggered
// too (fail closed) - and never aborts evaluation of the rest ("try all of
// them even if one of them failed"). A failure to call
// evaluate_declarations itself is a hard error: there's no ActionResult
// list - and so no per-declaration actions - to go on.
func constraintsValidation(ctx context.Context, sess *mcp.ClientSession, target any, roles []string, declarationNames []string) ([]triggeredDeclaration, error) {
	results, err := evaluateDeclarations(ctx, sess, roles, declarationNames)
	if err != nil {
		return nil, err
	}

	targetJSON, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal target for schema validation: %w", err)
	}
	// jsonschema.UnmarshalJSON (rather than plain encoding/json.Unmarshal)
	// decodes using json.Number, matching how the constraint schema itself
	// gets decoded below — required for correct numeric-constraint
	// semantics (e.g. a "maximum" check).
	targetInstance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(targetJSON)))
	if err != nil {
		return nil, fmt.Errorf("failed to decode target for schema validation: %w", err)
	}

	triggered := make([]triggeredDeclaration, 0, len(results))
	for _, ar := range results {
		if ar.Constraint == "" {
			triggered = append(triggered, violatedDeclaration(ar))
			continue
		}

		violated, err := violatesConstraint(ar.Constraint, targetInstance)
		if err != nil {
			triggered = append(triggered, uncheckedDeclaration("constraints_validation", ar, "invalid constraint: %v", err))
			continue
		}
		if violated {
			triggered = append(triggered, violatedDeclaration(ar))
		}
	}
	return triggered, nil
}

// evaluateDeclarations calls declarative-guardrail's evaluate_declarations
// tool via sess with roles/declarationNames, returning every matched
// ActionResult verbatim — shared by constraintsValidation (JSON-Schema-based
// triggering, above) and aiConstraintsValidation (LLM-based triggering,
// below), which differ only in how they decide whether a returned
// ActionResult "triggered".
func evaluateDeclarations(ctx context.Context, sess *mcp.ClientSession, roles []string, declarationNames []string) ([]guardrailcfg.ActionResult, error) {
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "evaluate_declarations",
		Arguments: declarativeGuardrailEvaluateArgs{Roles: roles, DeclarationNames: declarationNames},
	})
	if err != nil {
		return nil, fmt.Errorf("evaluate_declarations call failed: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("evaluate_declarations rejected the call: %s", toolErrorText(res.Content))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return nil, fmt.Errorf("failed to re-marshal evaluate_declarations' StructuredContent: %w", err)
	}
	var out declarativeGuardrailEvaluateOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("failed to decode evaluate_declarations output: %w", err)
	}
	// Fail closed on an answer that says nothing (no structured content, or
	// no "results"): read as an empty list, it would let every call
	// through as if no declaration triggered.
	if out.Results == nil {
		return nil, fmt.Errorf("evaluate_declarations returned no results")
	}
	return *out.Results, nil
}

// defaultAIValidatorServer is the discovered server name
// aiConstraintsValidation/resultAIConstraintsValidation call
// ai_result_validator on when a step's Parameters doesn't override it via
// aiValidatorServer.
const defaultAIValidatorServer = "ai-powered-tools"

// aiConstraintsValidationParameters is result_ai_constraints_validation's
// own OrchestrationStep.Parameters shape — kept separate from
// constraintsValidationParameters (rather than adding AIValidatorServer to
// that shared struct) so the two JSON-Schema-based tools don't silently
// accept a dead field they'd never use.
//
// MaxInputBytes caps the size of the request/result (as JSON) sent to the
// AI validator; anything larger is never sent and counts as "could not be
// checked" (fail closed). It bounds what an attacker can make the LLM
// process - padding a request past the provider's limit would otherwise
// make every check error - and the LLM cost one call can run up. Defaults
// to defaultMaxAIInputBytes.
type aiConstraintsValidationParameters struct {
	DeclarationNames  []string `json:"declarationNames"`
	GuardrailServer   string   `json:"guardrailServer,omitempty"`
	AIValidatorServer string   `json:"aiValidatorServer,omitempty"`
	MaxInputBytes     int      `json:"maxInputBytes,omitempty"`
}

// defaultMaxAIInputBytes is aiConstraintsValidationParameters.MaxInputBytes'
// default: well above an ordinary tool request or result, far below any
// LLM provider's request limit.
const defaultMaxAIInputBytes = 64 << 10

func decodeAIConstraintsValidationParameters(parameters string) (aiConstraintsValidationParameters, error) {
	p := aiConstraintsValidationParameters{
		GuardrailServer:   defaultDeclarativeGuardrailServer,
		AIValidatorServer: defaultAIValidatorServer,
		MaxInputBytes:     defaultMaxAIInputBytes,
	}
	if parameters == "" {
		return p, nil
	}
	if err := json.Unmarshal([]byte(parameters), &p); err != nil {
		return p, fmt.Errorf("failed to decode parameters %q: %w", parameters, err)
	}
	if p.GuardrailServer == "" {
		p.GuardrailServer = defaultDeclarativeGuardrailServer
	}
	if p.AIValidatorServer == "" {
		p.AIValidatorServer = defaultAIValidatorServer
	}
	if p.MaxInputBytes <= 0 {
		p.MaxInputBytes = defaultMaxAIInputBytes
	}
	return p, nil
}

// aiConstraintsValidation mirrors constraintsValidation, but decides
// whether a returned ActionResult "triggered" by asking a configured LLM
// (ai-powered-tools' ai_result_validator tool) whether target violates
// ActionResult.Constraint — here a natural-language description, not a
// JSON Schema fragment — rather than compiling Constraint as JSON Schema.
// An empty Constraint always triggers, the same convention
// constraintsValidation uses. Fails closed per declaration: if the AI
// validator is unavailable (aiSessErr), target is larger than
// maxInputBytes, or the validator errors (transport error, IsError, an LLM
// error or timeout, an unparseable answer), the declaration can't be
// checked and counts as triggered - its own actions decide what happens.
func aiConstraintsValidation(ctx context.Context, guardrailSess, aiSess *mcp.ClientSession, aiSessErr error, target any, roles []string, declarationNames []string, maxInputBytes int) ([]triggeredDeclaration, error) {
	const step = "ai_constraints_validation"
	results, err := evaluateDeclarations(ctx, guardrailSess, roles, declarationNames)
	if err != nil {
		return nil, err
	}
	targetJSON, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal target for AI validation: %w", err)
	}

	triggered := make([]triggeredDeclaration, 0, len(results))
	for _, ar := range results {
		if ar.Constraint == "" {
			triggered = append(triggered, violatedDeclaration(ar))
			continue
		}
		switch {
		case aiSessErr != nil:
			triggered = append(triggered, uncheckedDeclaration(step, ar, "AI validator unavailable: %v", aiSessErr))
			continue
		case len(targetJSON) > maxInputBytes:
			triggered = append(triggered, uncheckedDeclaration(step, ar, "too large to check (%d bytes; the AI validator is given at most %d)", len(targetJSON), maxInputBytes))
			continue
		}

		ok, err := aiValidateResult(ctx, aiSess, target, ar.Constraint)
		if err != nil {
			triggered = append(triggered, uncheckedDeclaration(step, ar, "%v", err))
			continue
		}
		if !ok {
			triggered = append(triggered, violatedDeclaration(ar))
		}
	}
	return triggered, nil
}

// aiValidateResult calls ai-powered-tools' ai_result_validator tool via
// sess, asking whether target violates constraint. Returns the tool's own
// verdict: true means target does NOT violate constraint ("ok").
func aiValidateResult(ctx context.Context, sess *mcp.ClientSession, target any, constraint string) (bool, error) {
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name:      "ai_result_validator",
		Arguments: map[string]any{"callResult": target, "constraint": constraint},
	})
	if err != nil {
		return false, fmt.Errorf("ai_result_validator call failed: %w", err)
	}
	if res.IsError {
		return false, fmt.Errorf("ai_result_validator rejected the call: %s", toolErrorText(res.Content))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return false, fmt.Errorf("failed to re-marshal ai_result_validator's StructuredContent: %w", err)
	}
	var out struct {
		Ok bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("failed to decode ai_result_validator output: %w", err)
	}
	return out.Ok, nil
}

// violatesConstraint compiles constraint as a JSON Schema fragment and
// validates targetInstance against it. Returns true if the schema was
// violated (a *jsonschema.ValidationError) — false, nil if it was
// satisfied. A non-nil error means the constraint itself couldn't be
// compiled or evaluated at all (e.g. malformed JSON, an invalid schema) —
// the caller then treats the declaration as "could not be checked"
// (triggered), never as a pass.
func violatesConstraint(constraint string, targetInstance any) (bool, error) {
	schemaDoc, err := jsonschema.UnmarshalJSON(strings.NewReader(constraint))
	if err != nil {
		return false, fmt.Errorf("failed to parse constraint as JSON: %w", err)
	}

	// Scheme must be a valid net/url scheme (letters/digits/+/-/. only, no
	// underscore) - "constraints_validation://..." fails to parse for
	// exactly this reason.
	const resourceURL = "mem://constraints-validation/constraint.json"
	compiler := jsonschema.NewCompiler()
	// No external $refs: the default loader reads file:// URLs, which would
	// let a constraint (from an admin, or a compromised guardrail server)
	// make go-proxy read local files. A constraint is one self-contained
	// schema.
	compiler.UseLoader(jsonschema.SchemeURLLoader{})
	// Enforce "format" (email, uri, date-time, ...): without this the
	// library treats it as an annotation only, so a constraint relying on
	// it would never trigger.
	compiler.AssertFormat()
	if err := compiler.AddResource(resourceURL, schemaDoc); err != nil {
		return false, fmt.Errorf("failed to add constraint schema: %w", err)
	}
	schema, err := compiler.Compile(resourceURL)
	if err != nil {
		return false, fmt.Errorf("failed to compile constraint schema: %w", err)
	}

	err = schema.Validate(targetInstance)
	if err == nil {
		return false, nil
	}
	var valErr *jsonschema.ValidationError
	if errors.As(err, &valErr) {
		return true, nil
	}
	return false, fmt.Errorf("failed to validate target against constraint schema: %w", err)
}

// requestConstraintsValidation is the native call-authorization handler
// behind "proxy_utility.request_constraints_validation" — registered via
// triggeredRuntime.RegisterNativeCallAuthorization in doRunOn (main.go).
// target = in.CallRequest, the whole marshaled *mcp.CallToolParamsRaw
// (i.e. {"name": ..., "arguments": {...}}, not just the bare arguments) —
// a constraint meant to validate "the request parameters" must be written
// against that shape, e.g. nesting under "arguments".
func requestConstraintsValidation(ctx context.Context, in triggered_tools_runtime.CallAuthorizationInput) (triggered_tools_runtime.AuthorizationOutput, error) {
	params, err := decodeConstraintsValidationParameters(in.Parameters)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_constraints_validation: %w", err)
	}
	if len(params.DeclarationNames) == 0 {
		return triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	sess, err := triggeredRuntime.SessionFor(ctx, params.GuardrailServer)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_constraints_validation: %w", err)
	}

	triggered, err := constraintsValidation(ctx, sess, in.CallRequest, in.Roles, params.DeclarationNames)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_constraints_validation: %w", err)
	}

	tool, _ := callRequestToolName(in.CallRequest)
	blocked, blockedDeclarations := dispatchConstraintActions("tool-call-blocked", "tool-call-alert", in.User, in.Roles, tool, in.ToolCallID, triggered)
	out := triggered_tools_runtime.AuthorizationOutput{Authorized: !blocked}
	if blocked {
		out.Reason = fmt.Sprintf("blocked by constraint(s): %s", strings.Join(blockedDeclarations, ", "))
	}
	return out, nil
}

// requestAIConstraintsValidation is the native call-authorization handler
// behind "proxy_utility.request_ai_constraints_validation". Same contract
// and same evaluate_declarations-driven declaration lookup as
// requestConstraintsValidation, but decides whether each returned
// ActionResult "triggered" by asking a configured LLM (ai-powered-tools'
// ai_result_validator tool, via aiConstraintsValidation) rather than
// compiling Constraint as JSON Schema — see aiConstraintsValidation's doc
// comment. target = in.CallRequest, same shape/convention as
// requestConstraintsValidation (the whole marshaled *mcp.CallToolParamsRaw,
// not just the bare arguments) — a constraint meant to judge "the request"
// should be written with that shape in mind, e.g. "the request's
// issueText argument must ...".
func requestAIConstraintsValidation(ctx context.Context, in triggered_tools_runtime.CallAuthorizationInput) (triggered_tools_runtime.AuthorizationOutput, error) {
	params, err := decodeAIConstraintsValidationParameters(in.Parameters)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_ai_constraints_validation: %w", err)
	}
	if len(params.DeclarationNames) == 0 {
		return triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	guardrailSess, err := triggeredRuntime.SessionFor(ctx, params.GuardrailServer)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_ai_constraints_validation: %w", err)
	}
	// An unreachable AI validator isn't a step error: each declaration is
	// then "could not be checked", and its own actions apply.
	aiSess, aiSessErr := triggeredRuntime.SessionFor(ctx, params.AIValidatorServer)

	triggered, err := aiConstraintsValidation(ctx, guardrailSess, aiSess, aiSessErr, in.CallRequest, in.Roles, params.DeclarationNames, params.MaxInputBytes)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.request_ai_constraints_validation: %w", err)
	}

	tool, _ := callRequestToolName(in.CallRequest)
	blocked, blockedDeclarations := dispatchConstraintActions("tool-call-blocked", "tool-call-alert", in.User, in.Roles, tool, in.ToolCallID, triggered)
	out := triggered_tools_runtime.AuthorizationOutput{Authorized: !blocked}
	if blocked {
		out.Reason = fmt.Sprintf("blocked by constraint(s): %s", strings.Join(blockedDeclarations, ", "))
	}
	return out, nil
}

// resultConstraintsValidation is the native result-authorization handler
// behind "proxy_utility.result_constraints_validation". target =
// in.CallResult, the whole marshaled *mcp.CallToolResult (i.e.
// {"content": [...], "structuredContent": ..., "isError": ...}) — a
// constraint meant to validate "the result" must be written against that
// shape, e.g. nesting under "structuredContent". tool is always "" here,
// matching authorization-mcp-server's own convention that
// ResultAuthorizationInput carries no tool name to report.
func resultConstraintsValidation(ctx context.Context, in triggered_tools_runtime.ResultAuthorizationInput) (triggered_tools_runtime.AuthorizationOutput, error) {
	params, err := decodeConstraintsValidationParameters(in.Parameters)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_constraints_validation: %w", err)
	}
	if len(params.DeclarationNames) == 0 {
		return triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	sess, err := triggeredRuntime.SessionFor(ctx, params.GuardrailServer)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_constraints_validation: %w", err)
	}

	triggered, err := constraintsValidation(ctx, sess, in.CallResult, in.Roles, params.DeclarationNames)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_constraints_validation: %w", err)
	}

	blocked, blockedDeclarations := dispatchConstraintActions("result-blocked", "result-alert", in.User, in.Roles, "", in.ToolCallID, triggered)
	out := triggered_tools_runtime.AuthorizationOutput{Authorized: !blocked}
	if blocked {
		out.Reason = fmt.Sprintf("blocked by constraint(s): %s", strings.Join(blockedDeclarations, ", "))
	}
	return out, nil
}

// resultAIConstraintsValidation is the native result-authorization handler
// behind "proxy_utility.result_ai_constraints_validation". Same contract
// and same evaluate_declarations-driven declaration lookup as
// resultConstraintsValidation, but decides whether each returned
// ActionResult "triggered" by asking a configured LLM (ai-powered-tools'
// ai_result_validator tool, via aiConstraintsValidation) rather than
// compiling Constraint as JSON Schema — see aiConstraintsValidation's doc
// comment. target = in.CallResult, same shape/convention as
// resultConstraintsValidation.
func resultAIConstraintsValidation(ctx context.Context, in triggered_tools_runtime.ResultAuthorizationInput) (triggered_tools_runtime.AuthorizationOutput, error) {
	params, err := decodeAIConstraintsValidationParameters(in.Parameters)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_ai_constraints_validation: %w", err)
	}
	if len(params.DeclarationNames) == 0 {
		return triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}

	guardrailSess, err := triggeredRuntime.SessionFor(ctx, params.GuardrailServer)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_ai_constraints_validation: %w", err)
	}
	// An unreachable AI validator isn't a step error: each declaration is
	// then "could not be checked", and its own actions apply.
	aiSess, aiSessErr := triggeredRuntime.SessionFor(ctx, params.AIValidatorServer)

	triggered, err := aiConstraintsValidation(ctx, guardrailSess, aiSess, aiSessErr, in.CallResult, in.Roles, params.DeclarationNames, params.MaxInputBytes)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.result_ai_constraints_validation: %w", err)
	}

	blocked, blockedDeclarations := dispatchConstraintActions("result-blocked", "result-alert", in.User, in.Roles, "", in.ToolCallID, triggered)
	out := triggered_tools_runtime.AuthorizationOutput{Authorized: !blocked}
	if blocked {
		out.Reason = fmt.Sprintf("blocked by constraint(s): %s", strings.Join(blockedDeclarations, ", "))
	}
	return out, nil
}

// businessValidationConstraint is proxy_utility.business_constraints_
// validation's own interpretation of ActionResult.Constraint - unlike
// request_constraints_validation (a JSON-Schema fragment) or
// result_ai_constraints_validation (free-form text for an LLM), here
// Constraint is itself a small JSON object naming which business
// validation tool - hand-written Go code specific to the target tool's own
// semantics, hosted on the SAME server as the call being guarded (see
// CallAuthorizationInput.TargetServer) - to delegate the actual decision
// to, plus an opaque parameters object meaningful only to that tool (e.g.
// {"maxTotalCost": 1000} for garden-online-store.validate_store_requests).
type businessValidationConstraint struct {
	ValidationTool string          `json:"validationTool"`
	Parameters     json.RawMessage `json:"parameters,omitempty"`
}

// decodeBusinessValidationConstraint parses constraint as a
// businessValidationConstraint - ok is false for an empty, malformed, or
// incomplete (missing validationTool) constraint, which the caller treats
// as "could not be checked" (fail closed), the same as constraintsValidation
// does for a JSON-Schema fragment that fails to compile.
func decodeBusinessValidationConstraint(constraint string) (businessValidationConstraint, bool) {
	var bvc businessValidationConstraint
	if constraint == "" {
		return bvc, false
	}
	if err := json.Unmarshal([]byte(constraint), &bvc); err != nil || bvc.ValidationTool == "" {
		return bvc, false
	}
	return bvc, true
}

// callBusinessValidationTool invokes one business validation tool (sess, on
// in.TargetServer) using the same CallAuthorizationInput contract any other
// call-authorization step uses - a business validation tool is an ordinary
// triggered tool (see CLAUDE.md's "Triggered tools / orchestration")
// that happens to also be reachable this way, not a separate ad hoc shape.
// Only Parameters differs from in: the declaration's own opaque parameters
// object (re-stringified), so the business validation tool decodes it
// exactly as it would if it had been wired directly as a callAuthorization
// step's own Parameters - "although the tool can be triggered directly
// using the orchestration system, it's not going to be the typical way of
// using it".
func callBusinessValidationTool(ctx context.Context, sess *mcp.ClientSession, bvc businessValidationConstraint, in triggered_tools_runtime.CallAuthorizationInput) (bool, error) {
	stepInput := triggered_tools_runtime.CallAuthorizationInput{
		User:         in.User,
		Roles:        in.Roles,
		TargetServer: in.TargetServer,
		TargetTool:   in.TargetTool,
		CallRequest:  in.CallRequest,
		ToolCallID:   in.ToolCallID,
		Parameters:   string(bvc.Parameters),
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: bvc.ValidationTool, Arguments: stepInput})
	if err != nil {
		return false, fmt.Errorf("%s call failed: %w", bvc.ValidationTool, err)
	}
	if res.IsError {
		return false, fmt.Errorf("%s rejected the call: %v", bvc.ValidationTool, res.Content)
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return false, fmt.Errorf("failed to re-marshal %s's StructuredContent: %w", bvc.ValidationTool, err)
	}
	var out triggered_tools_runtime.AuthorizationOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("failed to decode %s output: %w", bvc.ValidationTool, err)
	}
	return out.Authorized, nil
}

// businessLogicConstraintsValidation is the native call-authorization handler
// behind "proxy_utility.business_logic_constraints_validation" - registered via
// triggeredRuntime.RegisterNativeCallAuthorization in doRunOn (main.go).
// Same declarationNames/evaluate_declarations lookup as
// requestConstraintsValidation, but instead of validating in.CallRequest
// against a JSON-Schema constraint itself, each activated ActionResult
// names a business validation tool to delegate the actual decision to
// (decodeBusinessValidationConstraint) - hand-written Go code, specific to
// the target tool's own semantics, that can only run on the target tool's
// own server (in.TargetServer), since that's the only place with access to
// whatever private state the check needs (e.g. garden-online-store's own
// stock/pricing for validate_store_requests). This is the "typical way" of
// using a business validation tool, as opposed to wiring it directly as a
// callAuthorization step.
//
// Fails closed per declaration: a Constraint that doesn't decode, a target
// server that can't be reached, or a business validation tool call that
// fails (network error, tool error, bad output) means the declaration
// couldn't be checked, so it counts as triggered and its own actions
// apply.
func businessLogicConstraintsValidation(ctx context.Context, in triggered_tools_runtime.CallAuthorizationInput) (triggered_tools_runtime.AuthorizationOutput, error) {
	params, err := decodeConstraintsValidationParameters(in.Parameters)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.business_logic_constraints_validation: %w", err)
	}
	if len(params.DeclarationNames) == 0 {
		return triggered_tools_runtime.AuthorizationOutput{Authorized: true}, nil
	}
	if in.TargetServer == "" {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.business_logic_constraints_validation: CallAuthorizationInput carries no TargetServer to dispatch a business validation tool to")
	}

	guardrailSess, err := triggeredRuntime.SessionFor(ctx, params.GuardrailServer)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.business_logic_constraints_validation: %w", err)
	}
	results, err := evaluateDeclarations(ctx, guardrailSess, in.Roles, params.DeclarationNames)
	if err != nil {
		return triggered_tools_runtime.AuthorizationOutput{}, fmt.Errorf("proxy_utility.business_logic_constraints_validation: %w", err)
	}

	targetSess, targetSessErr := triggeredRuntime.SessionFor(ctx, in.TargetServer)

	const step = "proxy_utility.business_logic_constraints_validation"
	triggered := make([]triggeredDeclaration, 0, len(results))
	for _, ar := range results {
		bvc, ok := decodeBusinessValidationConstraint(ar.Constraint)
		if !ok {
			triggered = append(triggered, uncheckedDeclaration(step, ar, "invalid constraint (expected {validationTool, parameters}): %q", ar.Constraint))
			continue
		}
		if targetSessErr != nil {
			triggered = append(triggered, uncheckedDeclaration(step, ar, "server %q unavailable: %v", in.TargetServer, targetSessErr))
			continue
		}

		authorized, err := callBusinessValidationTool(ctx, targetSess, bvc, in)
		if err != nil {
			triggered = append(triggered, uncheckedDeclaration(step, ar, "business validation tool %q on server %q failed: %v", bvc.ValidationTool, in.TargetServer, err))
			continue
		}
		if !authorized {
			triggered = append(triggered, violatedDeclaration(ar))
		}
	}

	tool, _ := callRequestToolName(in.CallRequest)
	blocked, blockedDeclarations := dispatchConstraintActions("tool-call-blocked", "tool-call-alert", in.User, in.Roles, tool, in.ToolCallID, triggered)
	out := triggered_tools_runtime.AuthorizationOutput{Authorized: !blocked}
	if blocked {
		out.Reason = fmt.Sprintf("blocked by constraint(s): %s", strings.Join(blockedDeclarations, ", "))
	}
	return out, nil
}

// dispatchConstraintActions applies alert/log/block semantics for every
// triggered ActionResult constraintsValidation returned: "block" publishes
// an alert event with blockedSubtype, contributes to the returned bool, AND
// adds the declaration's name to the returned list; "alert" publishes an
// alert event with alertSubtype (independently of "block" — a single
// triggered ActionResult naming both actions publishes two separate
// events); "log" just logs via logging.Debugf; an action name this function
// doesn't recognize is logged and ignored — the catalog of valid actions is
// declarative-guardrail's own config-time-validated closed set
// (guardrailcfg.RawConfig.Actions), this function only defines behavior for
// the three the spec names. The returned declaration-name list is what
// requestConstraintsValidation/resultConstraintsValidation report back via
// AuthorizationOutput.Reason, so a caller's error message names exactly
// which constraint(s) caused the block.
func dispatchConstraintActions(blockedSubtype, alertSubtype, user string, roles []string, tool, toolCallID string, results []triggeredDeclaration) (blocked bool, blockedDeclarations []string) {
	for _, ar := range results {
		reason := fmt.Sprintf("declaration %q triggered", ar.DeclarationName)
		switch {
		case ar.uncheckedReason != "":
			reason = fmt.Sprintf("declaration %q could not be checked: %s", ar.DeclarationName, ar.uncheckedReason)
		case ar.Constraint != "":
			reason = fmt.Sprintf("declaration %q triggered: target violates constraint %s", ar.DeclarationName, ar.Constraint)
		}
		for _, action := range ar.Actions {
			switch action {
			case "block":
				blocked = true
				name := ar.DeclarationName
				if ar.uncheckedReason != "" {
					name += " (could not be checked)"
				}
				blockedDeclarations = append(blockedDeclarations, name)
				publishConstraintAlert(blockedSubtype, user, roles, tool, reason, toolCallID)
			case "alert":
				publishConstraintAlert(alertSubtype, user, roles, tool, reason, toolCallID)
			case "log":
				logging.Debugf("proxy_utility.constraints_validation: declaration %q: %s", ar.DeclarationName, reason)
			default:
				logging.Errorf("proxy_utility.constraints_validation: declaration %q: unrecognized action %q, ignoring", ar.DeclarationName, action)
			}
		}
	}
	return blocked, blockedDeclarations
}

// publishConstraintAlert mirrors authorization-mcp-server's publishAlert
// exactly (same "alert" event type, same 5 required metadata keys), reusing
// go-proxy's own package-level eventsClient (main.go) rather than a second
// connection. Fire-and-forget: a publish error is logged, never changes the
// authorization decision.
func publishConstraintAlert(subtype, user string, roles []string, tool, reason, toolCallID string) {
	err := eventsClient.Publish(&events.Event{
		Type:    "alert",
		Subtype: subtype,
		Time:    time.Now().UTC(),
		Title:   "Declarative constraint triggered",
		Text:    reason,
		Metadata: map[string]any{
			"user":       user,
			"roles":      roles,
			"tool":       tool,
			"reason":     reason,
			"toolCallId": toolCallID,
		},
	})
	if err != nil {
		logging.Errorf("proxy_utility.constraints_validation: failed to publish alert event: %v", err)
	}
}

// callRequestToolName extracts the "name" field from a call request value.
// Duplicated from authorization-mcp-server's own callRequestName (package
// main there, can't be imported) — but decodes from json.RawMessage rather
// than type-asserting a map[string]any, since Runtime.invokeCallAuthorization
// always marshals the real *mcp.CallToolParamsRaw before invoking a native
// step (see orchestrate.go), so in.CallRequest here is always raw JSON
// bytes wrapped in an any, never an already-decoded map.
func callRequestToolName(callRequest any) (string, bool) {
	raw, ok := callRequest.(json.RawMessage)
	if !ok {
		return "", false
	}
	var m struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", false
	}
	return m.Name, m.Name != ""
}
