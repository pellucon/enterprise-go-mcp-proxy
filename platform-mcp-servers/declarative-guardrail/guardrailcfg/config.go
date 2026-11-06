// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Config file loading for declarative-guardrail: both the catalog of valid
// actions and the list of guardrail declarations live in one JSON file,
// loaded and validated exactly once, at server startup - not per call, and
// never re-validated per call. See LoadConfig.
package guardrailcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// RawConfig is the raw, on-disk JSON shape.
type RawConfig struct {
	Actions      []string         `json:"actions"`
	Declarations []RawDeclaration `json:"declarations"`
}

type RawDeclaration struct {
	Name string `json:"name"`
	// Constraint is a restriction statement, not a warning message
	// describing this declaration - it's returned verbatim to the caller
	// (see ActionResult.Constraint in evaluate.go) to validate their actual
	// input/result against (e.g. as a JSON Schema fragment for a
	// schema-based validator, or free-form text for an LLM-based one).
	// Constraint must describe only what content is or isn't allowed -
	// never restate why the declaration triggered (the roles that matched
	// RoleExpression, or the EffectiveDate/EffectiveTime window it's
	// currently active during - those are already guaranteed true by the
	// time Constraint is returned) and never include rationale/explanation
	// for the restriction. E.g. for a declaration whose RoleExpression is "guest",
	// write "No customer, financial, or otherwise non-public data.", not
	// "Guest users must not be shown ..." - the role is implicit in
	// having triggered at all. Optional: an absent/empty Constraint is a
	// convention meaning every input/result should be treated as
	// violating it - no content validation needed, e.g. for declarations
	// that should unconditionally log/block/alert whenever they trigger
	// at all.
	Constraint     string   `json:"constraint,omitempty"`
	RoleExpression string   `json:"roleExpression"`
	Actions        []string `json:"actions"`
	// Timezone is an optional IANA time zone name (e.g. "America/New_York",
	// or a fixed-offset zone like "EST" - note "EST" is a fixed -05:00
	// with no DST, unlike "America/New_York" which observes it) that
	// EffectiveDate/EffectiveTime below are interpreted in. Empty/absent
	// defaults to UTC, matching this package's original UTC-only
	// behavior.
	Timezone      string                   `json:"timezone,omitempty"`
	EffectiveDate *RawEffectiveDate        `json:"effectiveDate,omitempty"`
	EffectiveTime []RawEffectiveTimeWindow `json:"effectiveTime,omitempty"`
}

type RawEffectiveDate struct {
	Start *string `json:"start,omitempty"`
	End   *string `json:"end,omitempty"`
}

type RawEffectiveTimeWindow struct {
	Days      []string `json:"days,omitempty"`
	StartTime string   `json:"startTime"`
	EndTime   string   `json:"endTime"`
}

// compiledDeclaration is a RawDeclaration after validation, with its
// role expression and effective-date/time fields parsed into their typed,
// ready-to-evaluate forms exactly once.
type compiledDeclaration struct {
	name          string
	constraint    string
	roleExpr      *RoleExpression
	actions       []string
	effectiveDate *effectiveDate       // nil = no date gating
	effectiveTime effectiveTimeWindows // nil/empty = no time-of-day gating
}

// Guardrails is a fully loaded and validated configuration, ready for
// repeated per-call evaluation (see evaluate.go).
type Guardrails struct {
	actions      map[string]bool
	declarations map[string]*compiledDeclaration
}

// LoadConfig reads path and parses it. See ParseConfig for the parsing
// logic on already-read bytes.
func LoadConfig(path string) (*Guardrails, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %q: %w", path, err)
	}
	return ParseConfig(data)
}

// ParseConfig parses already-read guardrails-config JSON bytes and
// validates them. Any problem - malformed JSON, missing mandatory fields,
// an action name not in the actions catalog, a malformed role expression,
// malformed effective-date/time fields, or a duplicate declaration/action
// name - is returned as a single descriptive error. Split out of LoadConfig
// so a caller extracting this section from a larger combined config file
// (go-proxy's own combined proxy-server.cfg, via its "declarativeGuardrails"
// section) or reloading it via configsync.Watcher can parse it directly,
// without writing it back out to a temporary file first.
func ParseConfig(data []byte) (*Guardrails, error) {
	var raw RawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	return compileConfig(&raw)
}

// ParseRawConfig parses already-read guardrails-config JSON bytes into
// RawConfig - the plain, JSON-roundtrippable shape - rather than compiling
// them into a Guardrails ready for per-call evaluation. It still runs the
// same structural validation ParseConfig does (via compileConfig, whose
// result is discarded here), so a malformed config is still rejected
// fail-closed, just without paying for or keeping the compiled form.
//
// This is what go-proxy's own config.go/main.go use for the
// declarativeGuardrails section: go-proxy is only ever this section's admin
// surface (see CLAUDE.md's "Unify configuration into a single config-map
// mechanism") - it persists the section's ConfigMap via configsync.Writer[T]
// but never evaluates a guardrail itself, so it has no use for the compiled
// form, and using it there was a real bug: Guardrails' fields are
// unexported (there's nothing for json.Marshal/json.Unmarshal to see), so a
// configsync.Writer[Guardrails] would export "{}" no matter what the
// ConfigMap actually held, and an update would silently persist "{}" back
// to it - wiping the config any real declarative-guardrail backend depends
// on. declarative-guardrail's own configsync.Watcher, which does need the
// compiled form for Current() to be evaluation-ready, is unaffected by any
// of this: it only ever calls a parse func to deserialize bytes, never
// marshals T, so it keeps using ParseConfig exactly as before.
func ParseRawConfig(data []byte) (*RawConfig, error) {
	var raw RawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}
	if _, err := compileConfig(&raw); err != nil {
		return nil, err
	}
	return &raw, nil
}

func compileConfig(raw *RawConfig) (*Guardrails, error) {
	if len(raw.Actions) == 0 {
		return nil, fmt.Errorf("config must define at least one action")
	}
	actionSet := make(map[string]bool, len(raw.Actions))
	for _, a := range raw.Actions {
		if strings.TrimSpace(a) == "" {
			return nil, fmt.Errorf("actions must not contain an empty name")
		}
		if actionSet[a] {
			return nil, fmt.Errorf("duplicate action name %q", a)
		}
		actionSet[a] = true
	}

	// An empty declarations list is valid - it's proxy-server.cfg's shipped
	// default for the declarativeGuardrails section, meaning "no guardrails
	// configured yet". A tool call naming any declaration then simply fails
	// with "unknown declaration" (see evaluate.go), same as naming one that
	// was never configured.
	declarations := make(map[string]*compiledDeclaration, len(raw.Declarations))
	for i, d := range raw.Declarations {
		compiled, err := compileDeclaration(d, actionSet)
		if err != nil {
			return nil, fmt.Errorf("declarations[%d]: %w", i, err)
		}
		if _, exists := declarations[compiled.name]; exists {
			return nil, fmt.Errorf("declarations[%d]: duplicate declaration name %q", i, compiled.name)
		}
		declarations[compiled.name] = compiled
	}

	return &Guardrails{actions: actionSet, declarations: declarations}, nil
}

func compileDeclaration(d RawDeclaration, validActions map[string]bool) (*compiledDeclaration, error) {
	if strings.TrimSpace(d.Name) == "" {
		return nil, fmt.Errorf("name is required")
	}
	if strings.TrimSpace(d.RoleExpression) == "" {
		return nil, fmt.Errorf("declaration %q: roleExpression is required", d.Name)
	}
	roleExpr, err := ParseRoleExpression(d.RoleExpression)
	if err != nil {
		return nil, fmt.Errorf("declaration %q: %w", d.Name, err)
	}
	if len(d.Actions) == 0 {
		return nil, fmt.Errorf("declaration %q: actions must not be empty", d.Name)
	}
	for _, a := range d.Actions {
		if !validActions[a] {
			return nil, fmt.Errorf("declaration %q: action %q is not in the configured actions catalog", d.Name, a)
		}
	}

	loc := time.UTC
	if strings.TrimSpace(d.Timezone) != "" {
		loc, err = time.LoadLocation(d.Timezone)
		if err != nil {
			return nil, fmt.Errorf("declaration %q: invalid timezone %q: %w", d.Name, d.Timezone, err)
		}
	}

	var ed *effectiveDate
	if d.EffectiveDate != nil {
		ed, err = parseEffectiveDate(d.EffectiveDate.Start, d.EffectiveDate.End, loc)
		if err != nil {
			return nil, fmt.Errorf("declaration %q: %w", d.Name, err)
		}
	}

	var windows effectiveTimeWindows
	for i, w := range d.EffectiveTime {
		compiled, err := parseEffectiveTimeWindow(w.Days, w.StartTime, w.EndTime, loc)
		if err != nil {
			return nil, fmt.Errorf("declaration %q: effectiveTime[%d]: %w", d.Name, i, err)
		}
		windows = append(windows, compiled)
	}

	return &compiledDeclaration{
		name:          d.Name,
		constraint:    d.Constraint,
		roleExpr:      roleExpr,
		actions:       append([]string{}, d.Actions...),
		effectiveDate: ed,
		effectiveTime: windows,
	}, nil
}
