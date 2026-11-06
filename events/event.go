// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package events implements a small NATS-based publish/subscribe layer for
// go-proxy's triggered tools and backend MCP servers: triggered tools publish
// events (e.g. an alert when a call is rejected, a record of every tool
// call), and a dedicated MCP server ("aggregator") subscribes and exposes
// tools to view/analyze them. See CLAUDE.md's "NATS event system" section.
package events

import (
	"fmt"
	"time"
)

// Event is the wire shape published to and received from NATS. Type, Time,
// Title, and Text are mandatory; Subtype and Metadata are optional (Subtype
// may be empty, Metadata may be nil/empty).
type Event struct {
	Type     string         `json:"type"`
	Subtype  string         `json:"subtype,omitempty"`
	Time     time.Time      `json:"time"`
	Title    string         `json:"title"`
	Text     string         `json:"text"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// unsetSubtypeToken fills the subtype token of Subject() when Subtype is
// empty, so every event's subject has the same number of tokens and both
// "events.>" and "events.<type>.>" wildcards match uniformly.
const unsetSubtypeToken = "_"

// Subject derives the NATS subject an Event is published/matched on:
// "events.<type>.<subtype>" (or the placeholder token in place of subtype
// when Subtype is empty).
func (e *Event) Subject() string {
	subtype := e.Subtype
	if subtype == "" {
		subtype = unsetSubtypeToken
	}
	return fmt.Sprintf("events.%s.%s", e.Type, subtype)
}
