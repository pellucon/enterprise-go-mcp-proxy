// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"encoding/json"
	"fmt"
)

// The two fixed triggered-tool contracts a call/result-authorization step
// can invoke — used internally (error messages, validate.go's
// expected-schema lookup) now that phase membership is expressed
// structurally (see Orchestration below) rather than via a per-step "type"
// field.
const (
	StepTypeCallAuthorization   = "call-authorization"
	StepTypeResultAuthorization = "result-authorization"
)

// ToolCallAuditSentinel is a ToolCallAudit entry that switches on
// publishing a "tool-request-result" event once Guard's whole flow for a
// call finishes — whichever phase (if any) rejected it, a real backend
// failure, or a full success. There is no triggered tool to invoke for
// this phase, just a publish-or-not switch (see fireAudit in
// orchestrate.go). See ToolRequestAuditSentinel for its sibling, fired
// earlier and independently.
const ToolCallAuditSentinel = "publish-tool-request-result"

// ToolRequestAuditSentinel is a ToolCallAudit entry that switches on
// publishing a "tool-request" event immediately after call-authorization
// decides whether the call may proceed — rejected or not — but before the
// real backend call is attempted (see fireRequestAudit in orchestrate.go).
// Unlike ToolCallAuditSentinel's event, this one never carries a result,
// since it always fires before any call is made. May be configured
// together with ToolCallAuditSentinel, either alone, or neither — see
// validateToolCallAudit.
const ToolRequestAuditSentinel = "publish-tool-request"

// toolCallAuditSentinels is the closed set of recognized ToolCallAudit
// entries. Each names a distinct event type Guard may publish, not an
// invoked tool.
var toolCallAuditSentinels = map[string]bool{
	ToolCallAuditSentinel:    true,
	ToolRequestAuditSentinel: true,
}

// OrchestrationStep names one triggered-tool invocation: which server, and
// which tool on it. Which of the two authorization contracts it must
// implement is implicit from which of Orchestration's CallAuthorization/
// ResultAuthorization fields it appears in.
type OrchestrationStep struct {
	Server string `json:"server"`
	Tool   string `json:"tool"`
	// Parameters is an optional, orchestration-author-supplied execution
	// parameters string attached to this step — passed through verbatim as
	// CallAuthorizationInput.Parameters/ResultAuthorizationInput.Parameters
	// (see contract.go) to whichever triggered tool this step invokes,
	// which may decode it into whatever fields it expects (or ignore it).
	// A plain string, not an embedded JSON value, to keep this step's own
	// shape simple; its *contents*, if set, must decode as a JSON object
	// (checked once at parse time, see validateStepParameters) — not just
	// any valid JSON — since every consumer is expected to read named
	// fields out of it. Empty means no parameters were configured for this
	// step.
	Parameters string `json:"parameters,omitempty"`
}

// Orchestration groups steps by phase, not by an arbitrary flat sequence:
// the order BETWEEN phases is fixed and not configurable (Guard always runs
// CallAuthorization -> the real backend call -> ResultAuthorization ->
// the ToolCallAudit publish switch), but each authorization phase can hold
// more than one step, and the order WITHIN a phase's own slice is
// significant (steps run in listed order, short-circuiting on the first
// call-authorization/result-authorization rejection). ToolCallAudit holds
// 0, 1, or 2 entries, each a distinct recognized sentinel — see
// ToolCallAuditSentinel/ToolRequestAuditSentinel and validateToolCallAudit.
type Orchestration struct {
	CallAuthorization   []OrchestrationStep `json:"callAuthorization,omitempty"`
	ResultAuthorization []OrchestrationStep `json:"resultAuthorization,omitempty"`
	ToolCallAudit       []string            `json:"toolCallAudit,omitempty"`
}

// ToolMapping maps a namespaced-tool-name glob pattern (see
// access_control.GlobMatch) to the orchestration that guards matching
// calls. ToolMappings is evaluated in order; first match wins.
type ToolMapping struct {
	Pattern       string `json:"pattern"`
	Orchestration string `json:"orchestration"`
}

// OrchestrationConfig is the root of the "orchestration" combined-config
// section. A nil *OrchestrationConfig (the section absent from the file
// entirely) means no platform servers are configured at all — NewRuntime
// treats that as a total no-op, so every tool call proceeds unguarded,
// identical to today's behavior.
type OrchestrationConfig struct {
	Orchestrations map[string]Orchestration `json:"orchestrations"`
	ToolMappings   []ToolMapping            `json:"toolMappings"`
}

// MarshalJSON guarantees Orchestrations/ToolMappings serialize as "{}"/"[]",
// never "null", when nil (e.g. an unconfigured OrchestrationConfig's Go
// zero value, as returned by configsync.Writer[OrchestrationConfig].Export
// before any orchestration section has ever been written). Without the
// Orchestrations half of this, the MCP SDK's own output-schema validation
// — which expects an object, derived from this struct's non-pointer map
// field — rejects the tool result outright, since a nil map marshals to
// JSON null; ToolMappings has no such validation failure (a nil slice's
// "null" still unmarshals back to nil, not an error), but is normalized
// the same way purely so an admin export never shows a stray null.
func (c OrchestrationConfig) MarshalJSON() ([]byte, error) {
	if c.Orchestrations == nil {
		c.Orchestrations = map[string]Orchestration{}
	}
	if c.ToolMappings == nil {
		c.ToolMappings = []ToolMapping{}
	}
	type alias OrchestrationConfig
	return json.Marshal(alias(c))
}

// ParseConfig parses already-read orchestration-section JSON bytes and
// structurally validates it: every ToolMapping must reference an
// orchestration that actually exists. This is a config-shape check only —
// it never talks to the network; validating that a referenced tool's
// actual JSON schema matches the contract its phase implies happens
// lazily, on first use, in validate.go.
func ParseConfig(data []byte) (*OrchestrationConfig, error) {
	var cfg OrchestrationConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse orchestration JSON: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (cfg *OrchestrationConfig) validate() error {
	for _, m := range cfg.ToolMappings {
		if _, ok := cfg.Orchestrations[m.Orchestration]; !ok {
			return fmt.Errorf("toolMapping for pattern %q references unknown orchestration %q", m.Pattern, m.Orchestration)
		}
	}
	for name, orch := range cfg.Orchestrations {
		if err := validateToolCallAudit(name, orch.ToolCallAudit); err != nil {
			return err
		}
		if err := validateStepParameters(name, StepTypeCallAuthorization, orch.CallAuthorization); err != nil {
			return err
		}
		if err := validateStepParameters(name, StepTypeResultAuthorization, orch.ResultAuthorization); err != nil {
			return err
		}
	}
	return nil
}

// validateToolCallAudit checks that entries holds only recognized sentinel
// values (see toolCallAuditSentinels), with no duplicates. Both
// ToolCallAuditSentinel and ToolRequestAuditSentinel may be present
// together, either alone, or neither — order doesn't matter, since Guard
// checks membership, not position (see fireAudit/fireRequestAudit in
// orchestrate.go). The upper bound is simply the number of known
// sentinels, so this stays correct without a separate hardcoded limit if a
// third one is ever added.
func validateToolCallAudit(orchName string, entries []string) error {
	if len(entries) > len(toolCallAuditSentinels) {
		return fmt.Errorf("orchestration %q: toolCallAudit holds %d entries, at most %d are allowed", orchName, len(entries), len(toolCallAuditSentinels))
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !toolCallAuditSentinels[entry] {
			return fmt.Errorf("orchestration %q: toolCallAudit entry %q is not a recognized sentinel value", orchName, entry)
		}
		if seen[entry] {
			return fmt.Errorf("orchestration %q: toolCallAudit holds duplicate entry %q", orchName, entry)
		}
		seen[entry] = true
	}
	return nil
}

// validateStepParameters checks that every step's optional Parameters
// string, if set, decodes as a JSON object. Checked once here, at
// orchestration-parse time, so a malformed parameters string fails fast at
// config load rather than surfacing later as a silently-logged warning deep
// inside whichever triggered tool tries to parse it (see
// authorization-mcp-server's logParameters, which expects exactly this
// shape). A JSON object specifically, not "any valid JSON": every triggered
// tool that consumes Parameters is expected to decode it into named fields
// (map[string]any or a concrete struct), so a syntactically-valid but
// non-object value (e.g. "42" or "[1,2,3]") is exactly as unusable to a
// consumer as malformed JSON, and is rejected the same way.
func validateStepParameters(orchName, stepType string, steps []OrchestrationStep) error {
	for _, step := range steps {
		if step.Parameters == "" {
			continue
		}
		var obj map[string]any
		// encoding/json unmarshals the literal `null` into any pointer
		// target, including a map, without error, leaving obj == nil - not
		// a rejection by itself, so it must be checked for explicitly:
		// "null" is valid JSON but not an object, exactly like "42" or
		// "[1,2,3]".
		if err := json.Unmarshal([]byte(step.Parameters), &obj); err != nil || obj == nil {
			if err == nil {
				err = fmt.Errorf("value is JSON null, not an object")
			}
			return fmt.Errorf("orchestration %q: step (server=%q tool=%q type=%q) has invalid parameters: must decode as a JSON object: %w", orchName, step.Server, step.Tool, stepType, err)
		}
	}
	return nil
}
