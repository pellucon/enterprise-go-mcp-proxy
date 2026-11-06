// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import "context"

// This package is deliberately decoupled from the events package (see
// CLAUDE.md's "NATS event system" section) — neither package imports the
// other. EventPublisher is the seam that lets main.go cross that boundary:
// it builds a real events.Event from a ToolRequestResultEvent and calls
// events.Client.Publish, without triggered_tools_runtime ever needing to
// know events.Event's shape or that NATS is involved at all.
const (
	ToolRequestResultRequestBlocked = "result-blocked"
	ToolRequestResultCallFailed     = "call-failed"
	ToolRequestResultCallSucceeded  = "call-succeeded"
)

// ToolRequest* are the two subtypes a "tool-request" event may carry (see
// ToolRequestEvent) — unlike ToolRequestResult*, there are only two: this
// event fires right after call-authorization's decision, so there's no
// "call-failed"/backend-outcome case to represent yet.
const (
	ToolRequestBlocked = "request-blocked"
	ToolRequestOK      = "request-ok"
)

// ToolRequestResultEvent describes the outcome of one Guard-ed tool call,
// for publishing as a "tool-request-result" event. Subtype is one of the
// ToolRequestResult* constants above and alone encodes whether the call was
// ultimately authorized — there is no separate boolean for that. Reason is
// empty on ToolRequestResultCallSucceeded.
type ToolRequestResultEvent struct {
	Subtype     string
	Caller      string
	Roles       []string
	Tool        string
	CallRequest any
	CallResult  any
	Reason      string
	// ToolCallID is the correlation UUID assigned to the guarded tool call
	// — see CLAUDE.md's "Tool-call correlation ID" section.
	ToolCallID string
}

// EventPublisher publishes e, returning an error if the publish itself
// failed (e.g. NATS unreachable, or e fails dictionary validation). Runtime
// treats a nil EventPublisher as a total no-op (see Runtime.SetEventPublisher)
// and never constructs one itself — main.go is the only place that builds a
// real implementation, closing over its own events.Client.
type EventPublisher func(ctx context.Context, e ToolRequestResultEvent) error

// ToolRequestEvent describes call-authorization's decision for one guarded
// tool call, for publishing as a "tool-request" event — fired immediately
// after that decision is made (rejected or not), before the real backend
// call is even attempted. Subtype is one of the ToolRequest* constants
// above. Unlike ToolRequestResultEvent there is no CallResult field: this
// event always fires before any call is attempted, so there's never a
// result to report.
type ToolRequestEvent struct {
	Subtype     string
	Caller      string
	Roles       []string
	Tool        string
	CallRequest any
	Reason      string
	// ToolCallID mirrors ToolRequestResultEvent.ToolCallID — see CLAUDE.md's
	// "Tool-call correlation ID" section.
	ToolCallID string
}

// RequestEventPublisher publishes e, returning an error if the publish
// itself failed. Mirrors EventPublisher's nil-safe-no-op convention — see
// Runtime.SetRequestEventPublisher.
type RequestEventPublisher func(ctx context.Context, e ToolRequestEvent) error
