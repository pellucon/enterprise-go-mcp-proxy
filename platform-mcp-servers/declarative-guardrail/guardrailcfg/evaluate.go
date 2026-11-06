// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Evaluate ties a Guardrails (loaded once at startup, see config.go)
// together with a specific call's roles/declarationNames/now to produce
// the tool's output.
package guardrailcfg

import (
	"fmt"
	"time"
)

// ActionResult is one triggered declaration's contribution to the tool's
// output. The intended caller workflow: take some user input (or another
// MCP call's result), call evaluate_declarations, and for each returned
// result, validate that input/result against Constraint - e.g. as a JSON
// Schema fragment via a schema-based validator (see go-proxy's
// constraints_validation), or as free-form text via an LLM-based validator -
// if it violates the restriction Constraint states, perform Actions. An
// empty Constraint is a convention meaning every input/result should be
// treated as violating it, skipping that validation step entirely.
type ActionResult struct {
	Roles           []string `json:"roles" jsonschema:"the caller's roles that caused this declaration to trigger"`
	DeclarationName string   `json:"declarationName" jsonschema:"the name of the triggered declaration"`
	Constraint      string   `json:"constraint" jsonschema:"the restriction to validate the caller's actual input or result against; an empty value means every input/result violates it, no validation needed"`
	Actions         []string `json:"actions" jsonschema:"the action(s) to take (e.g. block, alert, log) if the input/result violates the constraint"`
}

// Evaluate checks each name in declarationNames (every name must exist in
// guardrails - an unknown name fails the whole call, a caller mistake
// rather than a config problem) against roles and now, returning one
// ActionResult per declaration that both matches its role expression and
// falls within its effective-date/time windows (if configured). A
// declaration that doesn't trigger is simply omitted from the result -
// never returned as a false/empty entry.
func Evaluate(guardrails *Guardrails, roles []string, declarationNames []string, now time.Time) ([]ActionResult, error) {
	results := []ActionResult{}
	for _, name := range declarationNames {
		decl, ok := guardrails.declarations[name]
		if !ok {
			return nil, fmt.Errorf("unknown declaration %q", name)
		}

		matched, contributing := decl.roleExpr.Eval(roles)
		if !matched {
			continue
		}
		if decl.effectiveDate != nil && !decl.effectiveDate.Active(now) {
			continue
		}
		if len(decl.effectiveTime) > 0 && !decl.effectiveTime.Active(now) {
			continue
		}

		results = append(results, ActionResult{
			Roles:           contributing,
			DeclarationName: decl.name,
			Constraint:      decl.constraint,
			Actions:         append([]string{}, decl.actions...),
		})
	}
	return results, nil
}
