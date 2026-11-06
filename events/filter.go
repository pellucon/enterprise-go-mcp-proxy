// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import "github.com/pellucon/enterprise-go-mcp-proxy/access_control"

// Filter holds optional match constraints against an Event; a zero-value
// field means "no constraint on this field". All set fields are AND'd
// together. Shared by two independent uses: event-aggregator's list_events
// query filter, and AggregationConfig's ingestion-time filter (which gates
// what gets aggregated at all, before an event ever reaches the ring buffer
// or the on-disk log).
type Filter struct {
	Type       string   `json:"type,omitempty"`
	Subtype    string   `json:"subtype,omitempty"`
	User       string   `json:"user,omitempty"`
	Roles      []string `json:"roles,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	ToolCallID string   `json:"toolCallId,omitempty"`
}

// metadataString type-asserts e.Metadata[key] to a string, returning "" on
// any mismatch (missing key, nil, non-string value) — this is filtering,
// not authorization, so a silent non-match is the right failure mode.
func metadataString(e *Event, key string) string {
	s, _ := e.Metadata[key].(string)
	return s
}

// metadataRoles coerces e.Metadata["roles"] into a []string. Roles is
// published as a real []string, but after a JSON round-trip (Metadata is
// map[string]any) it decodes as []any of strings.
func metadataRoles(e *Event) []string {
	switch v := e.Metadata["roles"].(type) {
	case []string:
		return v
	case []any:
		roles := make([]string, 0, len(v))
		for _, r := range v {
			if s, ok := r.(string); ok {
				roles = append(roles, s)
			}
		}
		return roles
	default:
		return nil
	}
}

// Matches reports whether e satisfies every set field of f. A zero-value
// Filter matches every event.
func (f Filter) Matches(e *Event) bool {
	if f.Type != "" && e.Type != f.Type {
		return false
	}
	if f.Subtype != "" && e.Subtype != f.Subtype {
		return false
	}
	if f.User != "" && metadataString(e, "user") != f.User {
		return false
	}
	if len(f.Roles) > 0 {
		eventRoles := metadataRoles(e)
		matched := false
		for _, want := range f.Roles {
			for _, got := range eventRoles {
				if want == got {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	if f.Tool != "" && !access_control.GlobMatch(f.Tool, metadataString(e, "tool")) {
		return false
	}
	if f.Reason != "" && !access_control.GlobMatch(f.Reason, metadataString(e, "reason")) {
		return false
	}
	if f.ToolCallID != "" && metadataString(e, "toolCallId") != f.ToolCallID {
		return false
	}
	return true
}
