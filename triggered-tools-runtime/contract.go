// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

// The two fixed, invoked triggered-tool contracts (call-authorization and
// result-authorization — the third phase, tool-call-audit, is a
// publish-only switch with no invoked tool and no contract type, see
// event_publisher.go and ToolCallAuditSentinel in config.go). These types
// are imported directly by the sample triggered-tool server
// (platform-mcp-servers/authorization-mcp-server) so its mcp.AddTool
// registrations share the exact same input/output shape that validate.go
// checks discovered tools against — signature drift between a platform
// tool and the runtime becomes a compile error for this sample, and a
// validation error (rather than a silent mismatch) for any other
// implementation.
//
// CallRequest/CallResult are declared as `any`, not json.RawMessage:
// jsonschema-go infers json.RawMessage (a []byte) as a JSON-schema "array"
// (of numbers), which then rejects any real request/result JSON object at
// validation time. `any` infers to an unconstrained ("accept any JSON
// value") schema, matching the spec's "the json request"/"the json
// result" intent literally. Runtime code (orchestrate.go) may still assign
// a json.RawMessage value into these fields — it marshals to the same
// wire bytes either way, since json.RawMessage implements json.Marshaler.

// CallAuthorizationInput is the input to a call-authorization tool: decide
// whether user/roles may make the call described by CallRequest. Roles is
// every role the caller holds (a JWT "role" claim may itself be a single
// string or an array - see access_control.RolesFromClaims), never
// collapsed into one delimited string: a tool doing an exact-match role
// check (e.g. rejecting "guest") must check membership in Roles, not
// equality against some joined representation, or a caller holding
// "guest" plus another role would silently evade the check.
type CallAuthorizationInput struct {
	User        string   `json:"user"`
	Roles       []string `json:"roles"`
	CallRequest any      `json:"callRequest"`
	// TargetServer/TargetTool are the (namespaced-name-split) server and
	// tool the real call is headed to - i.e. reqParams.Name's own two
	// halves (<serverName>.<name>, the same namespacing
	// dynamic_proxy.Gateway applies to every discovered tool), split by
	// orchestrate.go's own splitNamespacedToolName (dynamic_proxy's
	// equivalent helper is unexported). Populated once per Guard call and
	// passed unchanged to every call-authorization step for that call -
	// added specifically so a step like
	// proxy_utility.business_logic_constraints_validation, which must itself dial
	// the target tool's own server to invoke a business validation tool
	// hosted there, doesn't need to re-parse CallRequest's raw JSON "name"
	// field to learn it.
	TargetServer string `json:"targetServer,omitempty"`
	TargetTool   string `json:"targetTool,omitempty"`
	// ToolCallID is the correlation UUID assigned to the client tool call
	// this authorization step is guarding — see CLAUDE.md's "Tool-call
	// correlation ID" section. Passed as an explicit field, not solely via
	// the relayheaders.ToolCallIDHeader now also attached to every
	// triggered-tool invocation (see runtime.go's platformHTTPClient) —
	// this predates that header reaching triggered tools at all, and stays
	// as the authoritative source (a native step, e.g., is invoked as a
	// direct Go call with no header round-trip to read one from).
	ToolCallID string `json:"toolCallId"`
	// Parameters is this step's optional execution parameters, taken
	// verbatim from OrchestrationStep.Parameters (see config.go) — already
	// validated as a JSON object at orchestration-parse time, but which
	// fields it holds is otherwise entirely up to whoever authored the
	// orchestration config; this tool decodes whichever fields it expects,
	// or ignores it. Empty when the orchestration step didn't configure
	// any.
	Parameters string `json:"parameters,omitempty"`
}

// ResultAuthorizationInput is the input to a result-authorization tool:
// decide whether user/roles may receive CallResult. See
// CallAuthorizationInput's Roles doc for why this is a slice, not a
// delimited string.
type ResultAuthorizationInput struct {
	User       string   `json:"user"`
	Roles      []string `json:"roles"`
	CallResult any      `json:"callResult"`
	// ToolCallID mirrors CallAuthorizationInput.ToolCallID.
	ToolCallID string `json:"toolCallId"`
	// Parameters mirrors CallAuthorizationInput.Parameters.
	Parameters string `json:"parameters,omitempty"`
}

// AuthorizationOutput is the shared output shape for both
// call-authorization and result-authorization tools.
type AuthorizationOutput struct {
	Authorized bool `json:"authorized"`
	// Reason is an optional, human-readable explanation for a rejection
	// (e.g. which declaration(s)/rule(s) caused it) — Guard (orchestrate.go)
	// appends it to the error it returns to the caller when Authorized is
	// false and Reason is non-empty. A tool that doesn't set it (e.g.
	// authorization-mcp-server's tools today) leaves the caller with
	// Guard's existing generic "rejected by call-authorization step
	// (server=... tool=...)" message, unchanged from before this field
	// existed.
	Reason string `json:"reason,omitempty"`
}

// InputSchemaFor returns the real JSON Schema for stepType's input contract
// (CallAuthorizationInput/ResultAuthorizationInput above) — exported so a
// caller outside this package can build a genuine InputSchema for a native
// triggered tool (native.go), which has no discovered *mcp.Tool of its own
// to source one from. Reuses the exact same jsonschema.For[T] inference
// validate.go's own expectedPropertyNames relies on for signature
// validation, so the two can never drift apart.
func InputSchemaFor(stepType string) (*jsonschema.Schema, error) {
	switch stepType {
	case StepTypeCallAuthorization:
		return jsonschema.For[CallAuthorizationInput](nil)
	case StepTypeResultAuthorization:
		return jsonschema.For[ResultAuthorizationInput](nil)
	default:
		return nil, fmt.Errorf("unknown step type %q", stepType)
	}
}

// AuthorizationOutputSchema returns the real JSON Schema for
// AuthorizationOutput above — the shared output shape both
// call-authorization and result-authorization tools return, native or not.
func AuthorizationOutputSchema() (*jsonschema.Schema, error) {
	return jsonschema.For[AuthorizationOutput](nil)
}
