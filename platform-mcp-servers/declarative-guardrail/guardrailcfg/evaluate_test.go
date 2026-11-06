// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package guardrailcfg

import (
	"reflect"
	"testing"
	"time"
)

func testGuardrails(t *testing.T) *Guardrails {
	t.Helper()
	raw := &RawConfig{
		Actions: []string{"block", "alert", "log"},
		Declarations: []RawDeclaration{
			{
				Name:           "no-dollar-sums",
				Constraint:     "dollar sums are not allowed",
				RoleExpression: "manager & developer",
				Actions:        []string{"block", "alert"},
			},
			{
				Name:           "guest-warning",
				Constraint:     "warn guests",
				RoleExpression: "guest",
				Actions:        []string{"log"},
			},
			{
				Name:           "business-hours-only",
				Constraint:     "only active during business hours on weekdays",
				RoleExpression: "_any_role",
				Actions:        []string{"alert"},
				EffectiveTime: []RawEffectiveTimeWindow{
					{Days: []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday"}, StartTime: "09:00", EndTime: "17:00"},
				},
			},
			{
				Name:           "future-only",
				Constraint:     "only active starting 2030",
				RoleExpression: "_any_role",
				Actions:        []string{"block"},
				EffectiveDate:  &RawEffectiveDate{Start: mustDate("2030-01-01")},
			},
			{
				Name:           "always-log",
				RoleExpression: "_any_role",
				Actions:        []string{"log"},
				// Constraint deliberately omitted - see TestEvaluate_EmptyConstraintConvention.
			},
		},
	}
	g, err := compileConfig(raw)
	if err != nil {
		t.Fatalf("compileConfig failed: %v", err)
	}
	return g
}

func TestEvaluate_SimpleMatch(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // a Monday

	results, err := Evaluate(g, []string{"manager", "developer"}, []string{"no-dollar-sums"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	want := ActionResult{Roles: []string{"manager", "developer"}, DeclarationName: "no-dollar-sums", Constraint: "dollar sums are not allowed", Actions: []string{"block", "alert"}}
	if !reflect.DeepEqual(results[0], want) {
		t.Fatalf("expected %+v, got %+v", want, results[0])
	}
}

func TestEvaluate_RoleExpressionDoesNotMatch(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	results, err := Evaluate(g, []string{"developer"}, []string{"no-dollar-sums"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %d: %+v", len(results), results)
	}
}

func TestEvaluate_EffectiveDateExcludes(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC) // well before 2030

	results, err := Evaluate(g, []string{"anyone"}, []string{"future-only"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results (effectiveDate not yet active), got %+v", results)
	}

	now = time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	results, err = Evaluate(g, []string{"anyone"}, []string{"future-only"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result once effectiveDate is active, got %+v", results)
	}
}

func TestEvaluate_EffectiveTimeExcludes(t *testing.T) {
	g := testGuardrails(t)

	// Monday at 08:00 - before the 09:00-17:00 window.
	before := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	results, err := Evaluate(g, []string{"anyone"}, []string{"business-hours-only"}, before)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results outside business hours, got %+v", results)
	}

	// Monday at noon - within the window.
	within := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	results, err = Evaluate(g, []string{"anyone"}, []string{"business-hours-only"}, within)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result within business hours, got %+v", results)
	}

	// Saturday at noon - wrong day.
	weekend := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	results, err = Evaluate(g, []string{"anyone"}, []string{"business-hours-only"}, weekend)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results on the weekend, got %+v", results)
	}
}

func TestEvaluate_MultipleDeclarationsMixedResult(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	results, err := Evaluate(g, []string{"manager", "developer"}, []string{"no-dollar-sums", "guest-warning", "business-hours-only"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results (guest-warning shouldn't match), got %d: %+v", len(results), results)
	}
	if results[0].DeclarationName != "no-dollar-sums" || results[1].DeclarationName != "business-hours-only" {
		t.Fatalf("expected results in requested order, got %+v", results)
	}
}

func TestEvaluate_EmptyConstraintConvention(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	results, err := Evaluate(g, []string{"anyone"}, []string{"always-log"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if results[0].Constraint != "" {
		t.Fatalf("expected empty constraint, got %q", results[0].Constraint)
	}
}

func TestEvaluate_UnknownDeclarationNameFailsTheWholeCall(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	_, err := Evaluate(g, []string{"manager", "developer"}, []string{"no-dollar-sums", "does-not-exist"}, now)
	if err == nil {
		t.Fatal("expected error for unknown declaration name")
	}
}

func TestEvaluate_EmptyResultIsEmptySliceNotNil(t *testing.T) {
	g := testGuardrails(t)
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)

	results, err := Evaluate(g, []string{"nobody"}, []string{"no-dollar-sums"}, now)
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if results == nil {
		t.Fatal("expected an empty slice, got nil")
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results, got %+v", results)
	}
}
