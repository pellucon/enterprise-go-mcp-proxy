// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ensureValidated lazily validates every step in orch against its actual
// target tool's discovered schema, the first time orchestration name is
// used. Only a definitive result is cached in r.validated - valid, or a
// step whose tool is missing or has the wrong schema - until Update clears
// the cache (a config change may reassign which server/tool backs any
// given step). A failure to reach a step's server isn't: that call fails
// closed, and the next one checks again, so a blip (or a caller cancelling
// the very first call) can't leave the orchestration refusing every call
// until the next restart.
//
// Concurrent first calls share one check (r.validating), which runs on
// its own context, bounded by validationTimeout and detached from the
// caller's cancellation: one caller giving up doesn't fail the others
// waiting on it. ctx only bounds how long this caller waits. No lock is
// held during the check's network calls.
func (r *Runtime) ensureValidated(ctx context.Context, name string, orch Orchestration) error {
	r.validationMu.Lock()
	if err, ok := r.validated[name]; ok {
		r.validationMu.Unlock()
		return err
	}
	v, ok := r.validating[name]
	if !ok {
		v = &validation{done: make(chan struct{})}
		r.validating[name] = v
		// WithoutCancel keeps the caller's values: the check's ListTools
		// calls carry this caller's identity, like the call itself would.
		checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), validationTimeout)
		go func(epoch uint64) {
			defer cancel()
			err := r.validateSteps(checkCtx, orch)
			r.validationMu.Lock()
			if r.validating[name] == v {
				delete(r.validating, name)
			}
			var transient *transientValidationError
			if epoch == r.validationEpoch && !errors.As(err, &transient) {
				r.validated[name] = err
			}
			v.err = err
			r.validationMu.Unlock()
			close(v.done)
		}(r.validationEpoch)
	}
	r.validationMu.Unlock()

	select {
	case <-v.done:
		return v.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// validationTimeout bounds one orchestration's signature check.
const validationTimeout = 15 * time.Second

// validation is a signature check in progress; done is closed once err is
// set.
type validation struct {
	done chan struct{}
	err  error
}

// transientValidationError is a check that couldn't be completed (a step's
// server unreachable, its tool list unavailable), as opposed to one that
// found a problem. It isn't cached.
type transientValidationError struct {
	err error
}

func (e *transientValidationError) Error() string { return e.err.Error() }
func (e *transientValidationError) Unwrap() error { return e.err }

// validateSteps validates every step in orch's two authorization phases
// against the contract that phase implies — phase membership (not a
// per-step "type" field) is what determines the expected schema now.
// ToolCallAudit has nothing to validate: it names no tool, just a sentinel
// switch (see ToolCallAuditSentinel), and config.go's validate() already
// checked its shape structurally at parse time.
func (r *Runtime) validateSteps(ctx context.Context, orch Orchestration) error {
	for _, step := range orch.CallAuthorization {
		if err := r.validateStep(ctx, step, StepTypeCallAuthorization); err != nil {
			return err
		}
	}
	for _, step := range orch.ResultAuthorization {
		if err := r.validateStep(ctx, step, StepTypeResultAuthorization); err != nil {
			return err
		}
	}
	return nil
}

// validateStep checks that step.Server's step.Tool actually exists and its
// discovered InputSchema's property-name set matches what stepType expects
// (derived from the shared contract.go structs via jsonschema.For, never
// hand-maintained). This is a structural, property-name-set comparison
// only — not deep type/required equality. A native step (step.Server ==
// dynamic_proxy.UtilityServerName, see native.go) has no remote schema to
// fetch or compare — its Go function signature (NativeCallAuthorizationFunc/
// NativeResultAuthorizationFunc) IS the contract, enforced by the compiler
// at build time — so it's validated separately, by validateNativeStep.
func (r *Runtime) validateStep(ctx context.Context, step OrchestrationStep, stepType string) error {
	if step.Server == dynamic_proxy.UtilityServerName {
		return r.validateNativeStep(step, stepType)
	}

	expected, err := expectedPropertyNames(stepType)
	if err != nil {
		return err
	}

	sess, err := r.session(ctx, step.Server)
	if err != nil {
		return &transientValidationError{fmt.Errorf("validating step (server=%q tool=%q type=%q): %w", step.Server, step.Tool, stepType, err)}
	}

	listResult, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return &transientValidationError{fmt.Errorf("validating step (server=%q tool=%q type=%q): failed to list tools: %w", step.Server, step.Tool, stepType, err)}
	}

	var tool *mcp.Tool
	for _, t := range listResult.Tools {
		if t.Name == step.Tool {
			tool = t
			break
		}
	}
	if tool == nil {
		return fmt.Errorf("validating step (server=%q tool=%q type=%q): tool not found on server", step.Server, step.Tool, stepType)
	}

	actual, err := actualPropertyNames(tool)
	if err != nil {
		return fmt.Errorf("validating step (server=%q tool=%q type=%q): %w", step.Server, step.Tool, stepType, err)
	}

	if !sameSet(expected, actual) {
		return fmt.Errorf("validating step (server=%q tool=%q type=%q): input schema properties %v do not match expected %v",
			step.Server, step.Tool, stepType, sortedKeys(actual), sortedKeys(expected))
	}
	return nil
}

// validateNativeStep is the native-step analog of validateStep: since
// there's no remote schema-drift risk for a native step (see validateStep's
// doc comment), this only checks that the referenced tool name was actually
// registered (RegisterNativeCallAuthorization/RegisterNativeResultAuthorization)
// — the native-step moral equivalent of "tool not found on server".
func (r *Runtime) validateNativeStep(step OrchestrationStep, stepType string) error {
	var ok bool
	switch stepType {
	case StepTypeCallAuthorization:
		_, ok = r.nativeCallAuthorization(step.Tool)
	case StepTypeResultAuthorization:
		_, ok = r.nativeResultAuthorization(step.Tool)
	default:
		return fmt.Errorf("unknown step type %q", stepType)
	}
	if !ok {
		return fmt.Errorf("validating step (server=%q tool=%q type=%q): no native tool registered under %q", step.Server, step.Tool, stepType, step.Server)
	}
	return nil
}

func expectedPropertyNames(stepType string) (map[string]bool, error) {
	schema, err := InputSchemaFor(stepType)
	if err != nil {
		return nil, fmt.Errorf("failed to infer expected schema for step type %q: %w", stepType, err)
	}
	return propertyNameSet(schema.Properties), nil
}

// actualPropertyNames extracts a discovered tool's InputSchema property
// names. Per the SDK's mcp.Tool.InputSchema doc comment, a tool obtained
// via a client-side ListTools call always holds the default JSON
// unmarshaling of the server's schema, i.e. a map[string]any.
func actualPropertyNames(tool *mcp.Tool) (map[string]bool, error) {
	schemaMap, ok := tool.InputSchema.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %q's InputSchema is not a JSON object (got %T)", tool.Name, tool.InputSchema)
	}
	propsRaw, ok := schemaMap["properties"]
	if !ok {
		return map[string]bool{}, nil
	}
	props, ok := propsRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("tool %q's InputSchema.properties is not a JSON object (got %T)", tool.Name, propsRaw)
	}
	names := make(map[string]bool, len(props))
	for k := range props {
		names[k] = true
	}
	return names, nil
}

func propertyNameSet(props map[string]*jsonschema.Schema) map[string]bool {
	names := make(map[string]bool, len(props))
	for k := range props {
		names[k] = true
	}
	return names
}

func sameSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
