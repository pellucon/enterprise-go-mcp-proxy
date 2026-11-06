// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"strings"
	"time"
)

// Outcome is the three-way classification of one call's result, derived
// purely from the CLI's own captured stdout/stderr for that call — never
// from pod logs, since garden-online-store's business-rule rejections
// (and the equivalent across the other gardening servers) don't log
// anything server-side at all (see README.md's "flagged code gaps"
// section). This is the confirmed, primary design decision behind this
// whole driver.
type Outcome string

const (
	// OutcomeSuccess: the call produced no "Error: " line at all — the
	// tool ran and returned a clean (non-IsError) result.
	OutcomeSuccess Outcome = "success"

	// OutcomeBusinessError: the RPC itself succeeded but the tool's own
	// result was IsError:true (e.g. insufficient stock, unknown id,
	// already-resolved). The go-sdk auto-wraps any plain `error` a typed
	// tool handler returns into exactly this shape (verified directly
	// against mcp/server.go), so this also covers ordinary validation
	// failures like "unknown order %q", not just explicit business-rule
	// checks. Expected and common — the whole point of this stress test
	// is to surface and categorize these, not eliminate them.
	OutcomeBusinessError Outcome = "business-error"

	// OutcomeGuardrailRejected: the call never reached its backend at
	// all — go-proxy's orchestration/guardrail engine blocked it via a
	// call-authorization or result-authorization step (verified directly
	// against triggered-tools-runtime/orchestrate.go's own
	// fmt.Sprintf("tool call rejected by %s-authorization step ...")
	// wording). Per CLAUDE.md, this rejection "surfaces to the client as
	// a Go error ... not a clean CallToolResult{IsError:true}" — which is
	// exactly why it produces empty stdout (see OutcomeTransportError's
	// own history below) despite being conceptually much closer to a
	// business rejection than a transport failure: it's the guardrail
	// machinery working exactly as designed, expected and common, same
	// as OutcomeBusinessError.
	OutcomeGuardrailRejected Outcome = "guardrail-rejected"

	// OutcomeAuthzDenied: the caller's role isn't granted this tool at
	// all (middleware.go's isToolRoleAuthorized), matched on its
	// "doesn't have a role that allows it" wording. Should be
	// rare-to-nonexistent in a normal run — the test always authenticates
	// as "admin", which is granted every tool in every committed *.cfg —
	// so any occurrence is worth a second look, same as
	// OutcomeTransportError, but it's a distinct category from it: this
	// is go-proxy correctly enforcing a role rule, not a sign of an
	// actual connectivity problem.
	OutcomeAuthzDenied Outcome = "authz-denied"

	// OutcomeTransportError: everything left over once the two categories
	// above are ruled out — a genuine network/connection failure, or
	// anything else that makes the RPC itself fail before a tool ever
	// runs. This USED to be the catch-all for every "empty stdout, RPC
	// failed" case, including guardrail rejections and authz denials —
	// which was misleading (see git history/PR discussion): "blocked by
	// constraint(s)" is not a transport failure, it's the system working
	// correctly, and lumping it in here made a perfectly healthy result
	// look like a red flag alongside genuinely rare, worth-investigating
	// failures. Should still be rare-to-nonexistent in a normal run.
	OutcomeTransportError Outcome = "transport-error"
)

// classifyOutcome implements the rules verified against
// cli/commands.go's runCallTool and the two RPC-level rejection message
// formats identified above:
//   - stderr empty                                    -> success
//   - stderr has "Error: ..." and stdout has content  -> business-error
//   - stderr has "Error: ..." and stdout is empty, and the message names
//     a call-authorization/result-authorization step  -> guardrail-rejected
//   - ...and the message says the caller's role isn't allowed the tool
//     -> authz-denied
//   - ...anything else                                -> transport-error
func classifyOutcome(stdout, stderr string) Outcome {
	if strings.TrimSpace(stderr) == "" {
		return OutcomeSuccess
	}
	if strings.TrimSpace(stdout) != "" {
		return OutcomeBusinessError
	}
	switch {
	case strings.Contains(stderr, "rejected by call-authorization step"),
		strings.Contains(stderr, "rejected by result-authorization step"):
		return OutcomeGuardrailRejected
	case strings.Contains(stderr, "doesn't have a role that allows it"):
		return OutcomeAuthzDenied
	default:
		return OutcomeTransportError
	}
}

// maxStoredOutputBytes caps how much of a call's stdout/stderr gets
// written into its CallRecord. Some gardening reads return large lists
// (e.g. garden-online-store.get_customer_orders: 500 rows,
// garden-customer-admin.get_customers: up to 300) and the CLI prints each
// result twice (a compact block, then a pretty-printed one) — uncapped,
// a multi-thousand-call run's JSONL files can reach the tens-to-hundreds
// of MB, well past GitHub's push size limits (verified in practice: a
// 5-minute/3-client run produced a 109 MB client1.jsonl, averaging ~53 KB
// per record). Applied only for STORAGE, after classifyOutcome has
// already run on the untruncated text — classification only ever needs
// "is stdout/stderr empty" and a short leading substring match, both
// unaffected by truncating from the end.
const maxStoredOutputBytes = 4096

// truncateForStorage caps s at maxStoredOutputBytes, appending a marker
// noting how much was cut so a truncated record is never mistaken for a
// genuinely short one.
func truncateForStorage(s string) string {
	if len(s) <= maxStoredOutputBytes {
		return s
	}
	return fmt.Sprintf("%s...[truncated, %d more bytes]", s[:maxStoredOutputBytes], len(s)-maxStoredOutputBytes)
}

// CallRecord is one line of a client's <client>.jsonl output file — see
// README.md's "Output format" section for the on-disk shape.
type CallRecord struct {
	Client       int       `json:"client"`
	File         string    `json:"file"`
	LineIndex    int       `json:"lineIndex"`
	Cycle        int       `json:"cycle"`
	Tool         string    `json:"tool"`
	Line         string    `json:"line"`
	DispatchedAt time.Time `json:"dispatchedAt"`
	CompletedAt  time.Time `json:"completedAt"`
	LatencyMs    int64     `json:"latencyMs"`
	Outcome      Outcome   `json:"outcome"`
	Stdout       string    `json:"stdout"`
	Stderr       string    `json:"stderr"`
}
