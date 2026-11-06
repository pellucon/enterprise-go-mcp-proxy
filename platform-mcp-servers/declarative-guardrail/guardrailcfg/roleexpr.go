// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Role-expression grammar for declarative-guardrail declarations:
//
//	expr    := orExpr
//	orExpr  := andExpr ( '|' andExpr )*
//	andExpr := unary ( '&' unary )*
//	unary   := '!' unary | atom
//	atom    := '_any_role' | roleName | '(' expr ')'
//
// '!' binds tightest, then '&', then '|'. Role names match exactly (no
// globs). See CLAUDE.md-style reasoning: NOT/AND/OR/parens over
// role-membership predicates form a functionally complete boolean algebra,
// so any boolean combination of roles is expressible.
package guardrailcfg

import (
	"fmt"
	"strings"
)

// anyRoleAtom is the role expression atom that always matches.
const anyRoleAtom = "_any_role"

// exprNode is one node of a parsed role expression's AST.
type exprNode interface {
	// eval reports whether the node matches roles, and which of the
	// caller's roles contributed to that being true (nil/empty when
	// false, or when true for a reason other than a specific role's
	// presence - see roleNode/notNode below).
	eval(roles map[string]bool) (matched bool, contributing []string)
}

type anyRoleNode struct{}

func (anyRoleNode) eval(map[string]bool) (bool, []string) { return true, nil }

type roleNode struct{ name string }

func (n roleNode) eval(roles map[string]bool) (bool, []string) {
	if roles[n.name] {
		return true, []string{n.name}
	}
	return false, nil
}

// notNode negates its operand. A negation's truth comes from a role's
// absence, not presence, so it never contributes roles to the result -
// even when the operand is itself a compound expression.
type notNode struct{ operand exprNode }

func (n notNode) eval(roles map[string]bool) (bool, []string) {
	matched, _ := n.operand.eval(roles)
	return !matched, nil
}

type andNode struct{ left, right exprNode }

// eval requires both sides true; per spec, a true AND's contributing set is
// the union of both sides' contributing roles (e.g. "manager & developer"
// with both roles present returns both).
func (n andNode) eval(roles map[string]bool) (bool, []string) {
	leftMatched, leftRoles := n.left.eval(roles)
	if !leftMatched {
		return false, nil
	}
	rightMatched, rightRoles := n.right.eval(roles)
	if !rightMatched {
		return false, nil
	}
	return true, append(leftRoles, rightRoles...)
}

type orNode struct{ left, right exprNode }

// eval short-circuits on the left side: per spec, "manager | developer"
// with both roles present only needs to report "manager", the side that
// made it true first.
func (n orNode) eval(roles map[string]bool) (bool, []string) {
	if matched, contributing := n.left.eval(roles); matched {
		return true, contributing
	}
	return n.right.eval(roles)
}

// RoleExpression is a parsed, ready-to-evaluate role expression.
type RoleExpression struct {
	source string
	root   exprNode
}

// ParseRoleExpression parses s into a RoleExpression, or returns a
// descriptive error if s isn't valid per the grammar above.
func ParseRoleExpression(s string) (*RoleExpression, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("role expression must not be empty")
	}
	tokens, err := tokenizeRoleExpression(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid role expression %q: %w", s, err)
	}
	p := &roleExprParser{tokens: tokens}
	root, err := p.parseOr()
	if err != nil {
		return nil, fmt.Errorf("invalid role expression %q: %w", s, err)
	}
	if p.pos != len(p.tokens) {
		return nil, fmt.Errorf("invalid role expression %q: unexpected token %q", s, p.peek())
	}
	return &RoleExpression{source: trimmed, root: root}, nil
}

// Eval reports whether roles satisfies the expression, and which of roles
// contributed to a true result (always empty when the result is false).
func (e *RoleExpression) Eval(roles []string) (matched bool, contributing []string) {
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}
	matched, contributing = e.root.eval(roleSet)
	return matched, dedupeStrings(contributing)
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// roleExprParser is a recursive-descent parser over a pre-tokenized role
// expression.
type roleExprParser struct {
	tokens []string
	pos    int
}

func (p *roleExprParser) peek() string {
	if p.pos >= len(p.tokens) {
		return ""
	}
	return p.tokens[p.pos]
}

func (p *roleExprParser) next() string {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *roleExprParser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek() == "|" {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = orNode{left: left, right: right}
	}
	return left, nil
}

func (p *roleExprParser) parseAnd() (exprNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for p.peek() == "&" {
		p.next()
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = andNode{left: left, right: right}
	}
	return left, nil
}

func (p *roleExprParser) parseUnary() (exprNode, error) {
	if p.peek() == "!" {
		p.next()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return notNode{operand: operand}, nil
	}
	return p.parseAtom()
}

func (p *roleExprParser) parseAtom() (exprNode, error) {
	tok := p.peek()
	switch tok {
	case "":
		return nil, fmt.Errorf("unexpected end of expression")
	case "(":
		p.next()
		inner, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek() != ")" {
			return nil, fmt.Errorf("expected ')'")
		}
		p.next()
		return inner, nil
	case ")", "&", "|":
		return nil, fmt.Errorf("unexpected token %q", tok)
	default:
		p.next()
		if tok == anyRoleAtom {
			return anyRoleNode{}, nil
		}
		return roleNode{name: tok}, nil
	}
}

// tokenizeRoleExpression splits s into '(' ')' '!' '&' '|' single-char
// tokens and whitespace-delimited role-name identifiers.
func tokenizeRoleExpression(s string) ([]string, error) {
	var tokens []string
	i := 0
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case isRoleExprSpecial(c):
			tokens = append(tokens, string(c))
			i++
		default:
			j := i
			for j < len(s) && !isRoleExprSpecial(s[j]) && !isRoleExprSpace(s[j]) {
				j++
			}
			tokens = append(tokens, s[i:j])
			i = j
		}
	}
	return tokens, nil
}

func isRoleExprSpecial(c byte) bool {
	return c == '(' || c == ')' || c == '!' || c == '&' || c == '|'
}

func isRoleExprSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
