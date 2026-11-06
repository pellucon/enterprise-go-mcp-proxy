// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// splitNamespacedToolName splits a namespaced tool name (<serverName>.<name>,
// see dynamic_proxy.Gateway's own namespacing) into its two halves, for
// CallAuthorizationInput.TargetServer/TargetTool - a local equivalent of
// dynamic_proxy's own removeFirstComponent, which is unexported and so
// can't be called from here even though this package already imports
// dynamic_proxy for ReservedServerName/UtilityServerName.
func splitNamespacedToolName(name string) (server, tool string) {
	server, tool, found := strings.Cut(name, ".")
	if !found {
		return name, ""
	}
	return server, tool
}

// auditTimeout bounds how long a detached audit-tool invocation is allowed
// to run after the triggering request has already returned to its caller,
// so a wedged platform server can't leak goroutines forever.
const auditTimeout = 10 * time.Second

// CallerIdentity is the caller-name/roles pair extracted from the incoming
// JWT, passed through to every triggered-tool invocation Guard makes.
type CallerIdentity struct {
	User  string
	Roles []string
}

// Guard runs orchName's phase-based guardrail flow around one matched tool
// call: all call-authorization steps (short-circuit/fail-closed on the
// first rejection or error) -> the "tool-request" publish switch (see
// fireRequestAudit), fired the instant that decision is known, whether
// rejected or not, and always before doCall -> doCall, invoked at most once
// and only if every call-authorization step passed -> all
// result-authorization steps (short-circuit/fail-closed the same way) ->
// the "tool-request-result" publish switch (see fireAudit), fired via one
// detached goroutine, unconditionally, exactly once, regardless of which
// phase (if any) rejected the call. The two publish switches are
// independent (ToolRequestAuditSentinel/ToolCallAuditSentinel) — either,
// both, or neither may be configured; a misconfigured-orchestration
// failure (ensureValidated) fires only the "tool-request-result" switch,
// since call-authorization never actually ran to produce a decision.
//
// A rejection at any phase surfaces to the caller as a Go error (mirroring
// createMiddleware's existing isToolRoleAuthorized deny convention), not a
// "clean" CallToolResult{IsError:true}.
func (r *Runtime) Guard(
	ctx context.Context,
	orchName string,
	caller CallerIdentity,
	toolCallID string,
	reqParams *mcp.CallToolParamsRaw,
	doCall func(ctx context.Context) (*mcp.CallToolResult, error),
) (*mcp.CallToolResult, error) {
	var orch Orchestration
	found := false
	if cfg := r.Snapshot(); cfg != nil {
		orch, found = cfg.Orchestrations[orchName]
	}

	reqJSON, err := json.Marshal(reqParams)
	if err != nil {
		return nil, fmt.Errorf("orchestration %q: failed to marshal call request for guardrail evaluation: %w", orchName, err)
	}

	// The tool is mapped to orchName but it doesn't exist (a mapping to a
	// missing orchestration, or a config update removing it after
	// MatchOrchestration): fail closed rather than run the call with no
	// steps at all.
	if !found {
		reason := fmt.Sprintf("orchestration %q not found, failing closed", orchName)
		r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, nil, ToolRequestResultCallFailed, reason)
		return nil, fmt.Errorf("%s", reason)
	}

	if err := r.ensureValidated(ctx, orchName, orch); err != nil {
		reason := fmt.Sprintf("orchestration %q is misconfigured, failing closed: %v", orchName, err)
		r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, nil, ToolRequestResultCallFailed, reason)
		return nil, fmt.Errorf("%s", reason)
	}

	targetServer, targetTool := splitNamespacedToolName(reqParams.Name)
	for _, step := range orch.CallAuthorization {
		out, err := r.invokeCallAuthorization(ctx, step, caller, toolCallID, reqJSON, targetServer, targetTool)
		if err != nil {
			logging.Errorf("[triggered-tools-runtime] call-authorization step (server=%q tool=%q) failed, failing closed: %v", step.Server, step.Tool, err)
			out = AuthorizationOutput{Authorized: false}
		}
		if !out.Authorized {
			reason := fmt.Sprintf("tool call rejected by call-authorization step (server=%q tool=%q) in orchestration %q", step.Server, step.Tool, orchName)
			if out.Reason != "" {
				reason = fmt.Sprintf("%s: %s", reason, out.Reason)
			}
			r.fireRequestAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, ToolRequestBlocked, reason)
			r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, nil, ToolRequestResultRequestBlocked, reason)
			return nil, fmt.Errorf("%s", reason)
		}
	}

	// Every call-authorization step passed (or there were none): the
	// request is known to be allowed to proceed. Fire "tool-request" now,
	// before attempting the real call - this is deliberately not folded
	// into the "tool-request-result" audit below, which only fires once
	// the whole flow (including doCall and result-authorization) finishes.
	r.fireRequestAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, ToolRequestOK, "")

	result, err := doCall(ctx)
	if err != nil {
		// A real backend failure, not a guardrail decision — audit still
		// fires (no result to report), and the original error propagates
		// unchanged.
		r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, nil, ToolRequestResultCallFailed, err.Error())
		return nil, err
	}

	// An "input required" result (protocol 2026-07-28+) isn't the tool's
	// answer: it asks the client for user input, and the client then calls
	// the tool again with the answers. It still goes through the result
	// checks: it reaches the client like any result, and both its questions
	// and any content alongside them come from the backend, so it can carry
	// exactly what a result check is there to stop. Only the result audit
	// waits - the follow-up call is a full call of its own, checked and
	// audited normally. Passed back unchanged when the checks allow it, so
	// elicitation still works.
	needsInput := result != nil && result.NeedsInput()

	resultJSON, err := json.Marshal(result)
	if err != nil {
		reason := fmt.Sprintf("orchestration %q: failed to marshal call result for guardrail evaluation: %v", orchName, err)
		r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, nil, ToolRequestResultCallFailed, reason)
		return nil, fmt.Errorf("%s", reason)
	}

	for _, step := range orch.ResultAuthorization {
		out, err := r.invokeResultAuthorization(ctx, step, caller, toolCallID, resultJSON)
		if err != nil {
			logging.Errorf("[triggered-tools-runtime] result-authorization step (server=%q tool=%q) failed, failing closed: %v", step.Server, step.Tool, err)
			out = AuthorizationOutput{Authorized: false}
		}
		if !out.Authorized {
			reason := fmt.Sprintf("tool call result rejected by result-authorization step (server=%q tool=%q) in orchestration %q", step.Server, step.Tool, orchName)
			if out.Reason != "" {
				reason = fmt.Sprintf("%s: %s", reason, out.Reason)
			}
			r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, resultJSON, ToolRequestResultRequestBlocked, reason)
			return nil, fmt.Errorf("%s", reason)
		}
	}

	if needsInput {
		logging.Debugf("[triggered-tools-runtime] %s returned an input-required result; result checks passed, the audit waits for the follow-up call", reqParams.Name)
		return result, nil
	}
	r.fireAudit(ctx, orch, caller, toolCallID, reqParams.Name, reqJSON, resultJSON, ToolRequestResultCallSucceeded, "")
	return result, nil
}

func (r *Runtime) invokeCallAuthorization(ctx context.Context, step OrchestrationStep, caller CallerIdentity, toolCallID string, reqJSON json.RawMessage, targetServer, targetTool string) (AuthorizationOutput, error) {
	in := CallAuthorizationInput{
		User: caller.User, Roles: caller.Roles, CallRequest: reqJSON, ToolCallID: toolCallID, Parameters: step.Parameters,
		TargetServer: targetServer, TargetTool: targetTool,
	}

	if step.Server == dynamic_proxy.UtilityServerName {
		fn, ok := r.nativeCallAuthorization(step.Tool)
		if !ok {
			return AuthorizationOutput{}, fmt.Errorf("no native call-authorization tool %q is registered under server %q", step.Tool, step.Server)
		}
		return fn(ctx, in)
	}

	sess, err := r.session(ctx, step.Server)
	if err != nil {
		return AuthorizationOutput{}, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: step.Tool, Arguments: in})
	if err != nil {
		return AuthorizationOutput{}, err
	}
	return decodeAuthorizationOutput(res)
}

func (r *Runtime) invokeResultAuthorization(ctx context.Context, step OrchestrationStep, caller CallerIdentity, toolCallID string, resultJSON json.RawMessage) (AuthorizationOutput, error) {
	in := ResultAuthorizationInput{User: caller.User, Roles: caller.Roles, CallResult: resultJSON, ToolCallID: toolCallID, Parameters: step.Parameters}

	if step.Server == dynamic_proxy.UtilityServerName {
		fn, ok := r.nativeResultAuthorization(step.Tool)
		if !ok {
			return AuthorizationOutput{}, fmt.Errorf("no native result-authorization tool %q is registered under server %q", step.Tool, step.Server)
		}
		return fn(ctx, in)
	}

	sess, err := r.session(ctx, step.Server)
	if err != nil {
		return AuthorizationOutput{}, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: step.Tool, Arguments: in})
	if err != nil {
		return AuthorizationOutput{}, err
	}
	return decodeAuthorizationOutput(res)
}

// decodeAuthorizationOutput decodes an authorization tool's result into its
// full AuthorizationOutput (Authorized and, if the tool set one, Reason).
// StructuredContent is guaranteed populated because mcp.AddTool's typed
// handler auto-fills it for a non-any Out type (see the sample
// authorization-mcp-server) — no text-content fallback parsing, keeping the
// fail-closed philosophy simple.
func decodeAuthorizationOutput(res *mcp.CallToolResult) (AuthorizationOutput, error) {
	if res.IsError {
		return AuthorizationOutput{}, fmt.Errorf("authorization tool call returned an error result")
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return AuthorizationOutput{}, fmt.Errorf("failed to re-marshal authorization tool's StructuredContent: %w", err)
	}
	var out AuthorizationOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return AuthorizationOutput{}, fmt.Errorf("failed to decode AuthorizationOutput from authorization tool's StructuredContent: %w", err)
	}
	return out, nil
}

// fireAudit detaches from ctx (so the goroutine survives the triggering
// HTTP handler returning) and publishes a tool-request-result event via the
// injected EventPublisher, exactly once, regardless of outcome — mirroring
// the always-fires-exactly-once guarantee the old tool-call-audit phase
// gave. A total no-op unless orch.ToolCallAudit contains
// ToolCallAuditSentinel specifically (it may also independently contain
// ToolRequestAuditSentinel — see fireRequestAudit — which has no bearing
// here); if the sentinel is present but no publisher was ever set, this
// logs a warning and drops the event rather than erroring — the same
// nil-safe-optional-capability convention as auth/internalAuth/orchestration
// elsewhere in this codebase.
func (r *Runtime) fireAudit(parentCtx context.Context, orch Orchestration, caller CallerIdentity, toolCallID, tool string, reqJSON, resultJSON json.RawMessage, subtype, reason string) {
	if !slices.Contains(orch.ToolCallAudit, ToolCallAuditSentinel) {
		return
	}

	r.mu.RLock()
	publish := r.eventPublisher
	r.mu.RUnlock()
	if publish == nil {
		logging.Errorf("[triggered-tools-runtime] toolCallAudit is configured but no EventPublisher was ever set, dropping tool-request-result event (tool=%q subtype=%q)", tool, subtype)
		return
	}

	detachedCtx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), auditTimeout)
	e := ToolRequestResultEvent{
		Subtype:     subtype,
		Caller:      caller.User,
		Roles:       caller.Roles,
		Tool:        tool,
		CallRequest: reqJSON,
		Reason:      reason,
		ToolCallID:  toolCallID,
	}
	// Leave CallResult as an untyped nil (rather than a typed-nil
	// json.RawMessage) when there is no result to report, so main.go's
	// publisher closure can tell "no result" apart from "empty result".
	if len(resultJSON) > 0 {
		e.CallResult = resultJSON
	}
	go func() {
		defer cancel()
		if err := publish(detachedCtx, e); err != nil {
			logging.Errorf("[triggered-tools-runtime] failed to publish tool-request-result event (tool=%q subtype=%q): %v", tool, subtype, err)
		}
	}()
}

// fireRequestAudit is fireAudit's sibling for the "tool-request" event: it
// fires from Guard's call-authorization loop, before doCall is ever
// attempted, so there is no result to carry (see ToolRequestEvent). Gated
// on ToolRequestAuditSentinel specifically — independent of
// ToolCallAuditSentinel/fireAudit, which may or may not also be configured
// on the same orchestration. Same detached-goroutine and
// nil-publisher-logs-and-drops conventions as fireAudit.
func (r *Runtime) fireRequestAudit(parentCtx context.Context, orch Orchestration, caller CallerIdentity, toolCallID, tool string, reqJSON json.RawMessage, subtype, reason string) {
	if !slices.Contains(orch.ToolCallAudit, ToolRequestAuditSentinel) {
		return
	}

	r.mu.RLock()
	publish := r.requestEventPublisher
	r.mu.RUnlock()
	if publish == nil {
		logging.Errorf("[triggered-tools-runtime] toolCallAudit includes %q but no RequestEventPublisher was ever set, dropping tool-request event (tool=%q subtype=%q)", ToolRequestAuditSentinel, tool, subtype)
		return
	}

	detachedCtx, cancel := context.WithTimeout(context.WithoutCancel(parentCtx), auditTimeout)
	e := ToolRequestEvent{
		Subtype:     subtype,
		Caller:      caller.User,
		Roles:       caller.Roles,
		Tool:        tool,
		CallRequest: reqJSON,
		Reason:      reason,
		ToolCallID:  toolCallID,
	}
	go func() {
		defer cancel()
		if err := publish(detachedCtx, e); err != nil {
			logging.Errorf("[triggered-tools-runtime] failed to publish tool-request event (tool=%q subtype=%q): %v", tool, subtype, err)
		}
	}()
}
