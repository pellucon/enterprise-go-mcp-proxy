// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package guardrailcfg

import (
	"reflect"
	"sort"
	"testing"
)

func evalExpr(t *testing.T, expr string, roles []string) (bool, []string) {
	t.Helper()
	parsed, err := ParseRoleExpression(expr)
	if err != nil {
		t.Fatalf("ParseRoleExpression(%q) failed: %v", expr, err)
	}
	matched, contributing := parsed.Eval(roles)
	sort.Strings(contributing)
	return matched, contributing
}

func TestRoleExpression_AnyRole(t *testing.T) {
	matched, contributing := evalExpr(t, "_any_role", nil)
	if !matched {
		t.Fatal("_any_role should always match")
	}
	if len(contributing) != 0 {
		t.Fatalf("_any_role should contribute no roles, got %v", contributing)
	}
}

func TestRoleExpression_PlainRole(t *testing.T) {
	matched, contributing := evalExpr(t, "manager", []string{"manager", "developer"})
	if !matched {
		t.Fatal("expected match when role present")
	}
	if !reflect.DeepEqual(contributing, []string{"manager"}) {
		t.Fatalf("expected contributing [manager], got %v", contributing)
	}

	matched, contributing = evalExpr(t, "manager", []string{"developer"})
	if matched {
		t.Fatal("expected no match when role absent")
	}
	if len(contributing) != 0 {
		t.Fatalf("expected no contributing roles on non-match, got %v", contributing)
	}
}

func TestRoleExpression_Negation(t *testing.T) {
	matched, contributing := evalExpr(t, "!guest", []string{"manager"})
	if !matched {
		t.Fatal("expected match: guest absent")
	}
	if len(contributing) != 0 {
		t.Fatalf("negation should contribute no roles, got %v", contributing)
	}

	matched, _ = evalExpr(t, "!guest", []string{"guest"})
	if matched {
		t.Fatal("expected no match: guest present")
	}
}

func TestRoleExpression_And(t *testing.T) {
	// Spec's own worked example: manager & developer, both present -> both returned.
	matched, contributing := evalExpr(t, "manager & developer", []string{"manager", "developer"})
	if !matched {
		t.Fatal("expected match: both roles present")
	}
	if !reflect.DeepEqual(contributing, []string{"developer", "manager"}) {
		t.Fatalf("expected contributing [developer manager], got %v", contributing)
	}

	matched, contributing = evalExpr(t, "manager & developer", []string{"manager"})
	if matched {
		t.Fatal("expected no match: developer absent")
	}
	if len(contributing) != 0 {
		t.Fatalf("expected no contributing roles on non-match, got %v", contributing)
	}
}

func TestRoleExpression_Or(t *testing.T) {
	// Spec's own worked example: manager | developer, both present -> only
	// the short-circuited left side ("manager") need be returned.
	matched, contributing := evalExpr(t, "manager | developer", []string{"manager", "developer"})
	if !matched {
		t.Fatal("expected match: manager present")
	}
	if !reflect.DeepEqual(contributing, []string{"manager"}) {
		t.Fatalf("expected contributing [manager] only (short-circuit), got %v", contributing)
	}

	// Left false, right true: falls through to right's contribution.
	matched, contributing = evalExpr(t, "manager | developer", []string{"developer"})
	if !matched {
		t.Fatal("expected match: developer present")
	}
	if !reflect.DeepEqual(contributing, []string{"developer"}) {
		t.Fatalf("expected contributing [developer], got %v", contributing)
	}

	matched, _ = evalExpr(t, "manager | developer", []string{"guest"})
	if matched {
		t.Fatal("expected no match: neither role present")
	}
}

func TestRoleExpression_Parens(t *testing.T) {
	// '!' binds tighter than '&', which binds tighter than '|'.
	// Without parens: manager & !guest | admin
	//   == (manager & (!guest)) | admin
	matched, _ := evalExpr(t, "manager & !guest | admin", []string{"admin"})
	if !matched {
		t.Fatal("expected match via admin branch")
	}

	matched, _ = evalExpr(t, "manager & !guest | admin", []string{"manager", "guest"})
	if matched {
		t.Fatal("expected no match: manager present but guest present too, and no admin")
	}

	// Explicit grouping changes the result: manager & (!guest | admin)
	matched, _ = evalExpr(t, "manager & (!guest | admin)", []string{"manager", "guest"})
	if matched {
		t.Fatal("expected no match: guest present and admin absent, so grouped OR is false")
	}

	matched, _ = evalExpr(t, "manager & (!guest | admin)", []string{"manager", "guest", "admin"})
	if !matched {
		t.Fatal("expected match: admin present satisfies the grouped OR")
	}
}

func TestRoleExpression_NestedParens(t *testing.T) {
	matched, contributing := evalExpr(t, "((manager | developer) & !guest)", []string{"developer"})
	if !matched {
		t.Fatal("expected match")
	}
	if !reflect.DeepEqual(contributing, []string{"developer"}) {
		t.Fatalf("expected contributing [developer], got %v", contributing)
	}

	matched, _ = evalExpr(t, "((manager | developer) & !guest)", []string{"developer", "guest"})
	if matched {
		t.Fatal("expected no match: guest present")
	}
}

func TestRoleExpression_ParseErrors(t *testing.T) {
	cases := []string{
		"",
		"   ",
		"manager &",
		"| manager",
		"(manager",
		"manager)",
		"manager & & developer",
		"()",
	}
	for _, expr := range cases {
		if _, err := ParseRoleExpression(expr); err == nil {
			t.Errorf("ParseRoleExpression(%q) expected an error, got nil", expr)
		}
	}
}
