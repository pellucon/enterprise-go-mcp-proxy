// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package events

import (
	"encoding/json"
	"fmt"
)

// ArchiverTransportConfig selects and configures event-aggregator's
// ArchiverTransport implementation (see
// eventlog.RegisterArchiverTransport/eventlog.NewArchiverTransport): Type
// is the string key an implementation registered itself under (e.g.
// "discard", "http"), and Properties is that implementation's own
// arbitrary JSON object of settings (empty/nil for an implementation
// like DiscardArchiverTransport that needs none). This package only
// knows the wire shape — it deliberately never imports eventlog (which
// itself imports this package for events.Event), so it has no notion of
// which Type values actually exist; that's resolved only where an
// ArchiverTransport is actually constructed.
type ArchiverTransportConfig struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties,omitempty"`
}

// AggregationConfig is the root of the "eventAggregation" combined-config
// section (CLAUDE.md's "Unify configuration into a single
// config-map mechanism"): event-aggregator's ingestion filter plus its
// tiered-storage thresholds. A nil *AggregationConfig (the section absent
// from the file entirely) means event-aggregator falls back to its own
// bundled local default (see its event_aggregation_test.json) rather than
// this section's zero value — mirroring how OrchestrationConfig's absence
// is a total no-op rather than an all-zero orchestration.
type AggregationConfig struct {
	// Filter gates which events are aggregated at all: a non-matching
	// event is dropped before it ever reaches the in-memory ring buffer,
	// the on-disk log, or archival. A zero-value Filter (every field
	// unset) matches every event.
	Filter Filter `json:"filter"`

	// MaxBufferSize bounds how many recent events are kept in memory; the
	// oldest is evicted (spilled to the on-disk log, not dropped) once
	// this many have been received.
	MaxBufferSize int `json:"maxBufferSize"`

	// MaxLogAgeDays bounds how long an on-disk log segment is kept before
	// the archiver rolls it out of the cluster (or discards it — see
	// ArchiverTransport/eventlog.DiscardArchiverTransport).
	MaxLogAgeDays int `json:"maxLogAgeDays"`

	// MaxLogSizeBytes bounds the on-disk log's total size; once exceeded,
	// the archiver works oldest-segment-first until back under the limit.
	MaxLogSizeBytes int64 `json:"maxLogSizeBytes"`

	// ArchiverTransport selects how the archiver ships a rolled segment
	// out of the cluster. `{"type":"discard"}` (the default in every
	// committed config) discards it locally instead — see
	// eventlog.DiscardArchiverTransport.
	ArchiverTransport ArchiverTransportConfig `json:"archiverTransport"`
}

// ParseAggregationConfig parses already-read eventAggregation-section JSON
// bytes and structurally validates it: MaxBufferSize/MaxLogAgeDays must be
// positive and MaxLogSizeBytes must be non-negative, since a zero/negative
// threshold would either wedge event-aggregator (never buffering/rotating
// anything) or be silently meaningless. ArchiverTransport.Type must be
// non-empty and, if ArchiverTransport.Properties is set, it must decode as
// a JSON object — the same "syntactically valid JSON isn't enough, it must
// be a usable object" convention triggered-tools-runtime's own per-step
// Parameters validation already applies. Which Type values are actually
// valid is not checked here — that's resolved by eventlog.NewArchiverTransport,
// only known once every ArchiverTransport implementation has registered
// itself.
func ParseAggregationConfig(data []byte) (*AggregationConfig, error) {
	var cfg AggregationConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse eventAggregation JSON: %w", err)
	}
	if cfg.MaxBufferSize <= 0 {
		return nil, fmt.Errorf("eventAggregation.maxBufferSize must be positive, got %d", cfg.MaxBufferSize)
	}
	if cfg.MaxLogAgeDays <= 0 {
		return nil, fmt.Errorf("eventAggregation.maxLogAgeDays must be positive, got %d", cfg.MaxLogAgeDays)
	}
	if cfg.MaxLogSizeBytes < 0 {
		return nil, fmt.Errorf("eventAggregation.maxLogSizeBytes must not be negative, got %d", cfg.MaxLogSizeBytes)
	}
	if cfg.ArchiverTransport.Type == "" {
		return nil, fmt.Errorf("eventAggregation.archiverTransport.type must not be empty")
	}
	if len(cfg.ArchiverTransport.Properties) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(cfg.ArchiverTransport.Properties, &obj); err != nil {
			return nil, fmt.Errorf("eventAggregation.archiverTransport.properties must be a JSON object: %w", err)
		}
	}
	return &cfg, nil
}
