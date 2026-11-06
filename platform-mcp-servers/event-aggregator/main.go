// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// event-aggregator is a sample MCP server that subscribes to every event
// published on NATS (see events/) and exposes ordinary tools to
// view/analyze them. Unlike authorization-mcp-server, its tools carry no
// triggered-tool _meta annotation - they're meant to be called by regular
// MCP clients through go-proxy, under normal role-based access control,
// not hidden/invoked only by the orchestration engine.
//
// The subscription starts at process startup and runs continuously in the
// background, independent of any tool call, feeding a fixed-capacity,
// in-memory ring buffer (the "hot" tier). Once the buffer is full, the
// oldest event is evicted and spilled to a local on-disk JSONL segment log
// (the "cold" tier, package eventlog) instead of being dropped - unless
// -event-log-dir is empty, in which case there's no disk tier at all and
// eviction drops the event, exactly like this server's original v1
// no-persistence behavior. A background archiver periodically ships (or,
// by default, discards - see eventlog.DiscardArchiverTransport) rolled log
// segments once they're past a configured age/total-size threshold, so
// local disk usage stays bounded.
//
// The ingestion filter, in-memory buffer size, log age/size thresholds,
// and the archiver's ArchiverTransport (which implementation, and its own
// properties) are all live-reloadable via the eventAggregation
// combined-config section (see CLAUDE.md's "Unify configuration
// into a single config-map mechanism"). Which ArchiverTransport
// implementations exist at all is a Go-level registry, not config — see
// eventlog.RegisterArchiverTransport.
//
// See CLAUDE.md's "NATS event system" section and the
// tiered-event-storage design plan for the full picture.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/mcptrace"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/event-aggregator/eventlog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultListLimit/maxListLimit bound list_events' own per-page result
// size, independent of the ring buffer's own (live-configurable) capacity.
const (
	defaultListLimit = 50
	maxListLimit     = 500
)

// Bootstrap defaults used only until the eventAggregation Watcher's
// constructor-time onChange call (see main) applies the real,
// possibly-mounted config - matching this repo's default published
// defaults (see events.AggregationConfig / event_aggregation_test.json)
// so even that brief transient state is sane.
const (
	bootstrapMaxBufferSize   = 1000
	bootstrapMaxLogAgeDays   = 5
	bootstrapMaxLogSizeBytes = 10_000_000
)

// ringEntry pairs a buffered event with a monotonic sequence number, so a
// list_events page can resume exactly where a previous page left off (see
// pageCursor) even as the buffer keeps evicting from the front.
type ringEntry struct {
	seq   int64
	event *events.Event
}

// ringBuffer is a mutex-guarded, fixed-capacity, most-recent-N buffer of
// received events.
type ringBuffer struct {
	mu       sync.Mutex
	capacity int
	entries  []ringEntry
	nextSeq  int64
}

func newRingBuffer(capacity int) *ringBuffer {
	return &ringBuffer{capacity: capacity}
}

// add appends e, evicting (and returning) the oldest event if now over
// capacity, or nil if nothing was evicted. The caller is responsible for
// persisting a non-nil evicted event to the disk tier *after* add
// returns - add itself never does any I/O under its own lock, matching
// this package's "no slow path holds the ring buffer mutex" concurrency
// rule.
func (b *ringBuffer) add(e *events.Event) *events.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	seq := b.nextSeq
	b.nextSeq++
	b.entries = append(b.entries, ringEntry{seq: seq, event: e})
	if len(b.entries) > b.capacity {
		evicted := b.entries[0].event
		b.entries = b.entries[1:]
		return evicted
	}
	return nil
}

// resize changes the buffer's capacity, evicting from the front (spilling
// via onEvict, called for each evicted event only after the lock is
// released) if the new capacity is smaller than the current occupancy.
// Growing just updates the capacity int.
func (b *ringBuffer) resize(newCapacity int, onEvict func(*events.Event)) {
	b.mu.Lock()
	b.capacity = newCapacity
	var evicted []*events.Event
	for len(b.entries) > b.capacity {
		evicted = append(evicted, b.entries[0].event)
		b.entries = b.entries[1:]
	}
	b.mu.Unlock()
	for _, e := range evicted {
		onEvict(e)
	}
}

// list returns up to limit events, most-recent-first, matching f (a
// zero-value events.Filter matches everything). Used by get_event_stats'
// sibling tools that don't need pagination; list_events itself uses
// listBackward below.
func (b *ringBuffer) list(f events.Filter, limit int) []*events.Event {
	matched, _, _, _, _ := b.listBackward(-1, limit, time.Time{}, f)
	return matched
}

// listBackward returns up to limit events matching filter, most-recent-
// first, resuming strictly before beforeSeq (beforeSeq<0 means start at
// the newest buffered event). Stops early once an event's Time is before
// since (sinceCrossed=true; a zero since never crosses). hasResume=true
// means limit was reached mid-buffer and resumeSeq is where the next page
// should resume; memExhausted=true means every buffered event was
// scanned without filling limit or crossing since - the caller should
// hand off to the disk tier.
func (b *ringBuffer) listBackward(beforeSeq int64, limit int, since time.Time, filter events.Filter) (matched []*events.Event, resumeSeq int64, hasResume bool, sinceCrossed bool, memExhausted bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	start := len(b.entries) - 1
	if beforeSeq >= 0 {
		start = -1
		for i, ent := range b.entries {
			if ent.seq == beforeSeq {
				start = i - 1
				break
			}
		}
		if start == -1 {
			// beforeSeq isn't in the buffer any more (evicted since the
			// cursor was issued, e.g. by a live capacity shrink) - resume
			// from the newest entry still strictly older than beforeSeq.
			for i := len(b.entries) - 1; i >= 0; i-- {
				if b.entries[i].seq < beforeSeq {
					start = i
					break
				}
			}
		}
	}

	for i := start; i >= 0; i-- {
		e := b.entries[i].event
		if !since.IsZero() && e.Time.Before(since) {
			return matched, 0, false, true, false
		}
		if filter.Matches(e) {
			matched = append(matched, e)
		}
		if len(matched) >= limit {
			return matched, b.entries[i].seq, true, false, false
		}
	}
	return matched, 0, false, false, true
}

// reset clears every buffered event. Only affects this process's own
// in-memory buffer — it doesn't touch NATS, any other subscriber, the
// disk log, or events published before this call.
func (b *ringBuffer) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entries = nil
}

// eventStats is a mutex-guarded, monotonically-increasing count of every
// event this process has ever been given, one counter per event: keyed
// by "type.subtype" when the event has a subtype, or just "type" when it
// doesn't — never both, so an event with a subtype is never counted
// twice under two different keys (e.g. "tool-request-result.call-succeeded"
// only, not also a redundant "tool-request-result" carrying the same
// total). Independent of the ring buffer's own capacity/eviction and the
// disk tier entirely, so
// get_event_stats stays O(1) regardless of how much history exists on
// disk (a full disk scan would cost O(disk size) per call, unlike
// list_events' limit-bounded tier-handoff walk). Both real NATS ingestion
// (see main's onEvent, gated by the ingestion filter) and load_events'
// bulk synthetic load (which bypasses that filter, same as it bypasses
// NATS/the event dictionary) call record - a caller seeding canned demo
// data via load_events (e.g. go-gardening-demo's own sample-events.json)
// expects get_event_stats to reflect it exactly as if those events had
// arrived for real, not report empty until real traffic shows up.
// reset_events is the one exception: it only clears the ring buffer's
// displayable contents, never these counts, since "how many events has
// this process ever seen" shouldn't un-happen just because memory was
// cleared. Like the ring buffer itself, this resets to zero on a process
// restart — no persistence, matching this server's existing
// no-persistence-for-in-memory-state convention.
type eventStats struct {
	mu     sync.Mutex
	counts map[string]int
}

func newEventStats() *eventStats {
	return &eventStats{counts: make(map[string]int)}
}

// record increments e's single counter: "type.subtype" when e has a
// subtype, or just "type" when it doesn't - never both, so an event with
// a subtype is never counted twice under two different keys. Called once
// per event ingested for real (after it passes the ingestion filter - see
// main's onEvent) and once per event loaded via load_events (see
// createServer's load_events handler) - see this type's own doc comment
// for why both call it.
func (s *eventStats) record(e *events.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := e.Type
	if e.Subtype != "" {
		key = e.Type + "." + e.Subtype
	}
	s.counts[key]++
}

// snapshot returns a copy of the current counts, safe for the caller to
// serialize without racing further record calls.
func (s *eventStats) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.counts))
	for k, v := range s.counts {
		out[k] = v
	}
	return out
}

// pageCursor is list_events' opaque pagination position, spanning both
// the in-memory ring-buffer tier and the on-disk log tier - see the
// tiered-event-storage design plan's Q1. Callers only ever see it
// base64-encoded (encodeCursor/decodeCursor); the JSON shape is an
// internal implementation detail.
type pageCursor struct {
	Tier    string `json:"tier"` // "mem" or "disk"
	Seq     int64  `json:"seq,omitempty"`
	Segment string `json:"segment,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
}

func encodeCursor(c pageCursor) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("failed to encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCursor(s string) (pageCursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	var c pageCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return pageCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return c, nil
}

// ListEventsArgs is list_events' input. Every filter field is optional and
// AND'd together with the rest; an empty/absent field means "no constraint
// on this field".
type ListEventsArgs struct {
	Type       string   `json:"type,omitempty" jsonschema:"event type to filter by; empty means every type"`
	Subtype    string   `json:"subtype,omitempty" jsonschema:"event subtype to filter by; empty means every subtype"`
	User       string   `json:"user,omitempty" jsonschema:"exact metadata.user to filter by"`
	Roles      []string `json:"roles,omitempty" jsonschema:"metadata.roles to filter by; an event matches if it has at least one of these roles"`
	Tool       string   `json:"tool,omitempty" jsonschema:"wildcard pattern ('*' matches any characters) to match against metadata.tool"`
	Reason     string   `json:"reason,omitempty" jsonschema:"wildcard pattern ('*' matches any characters) to match against metadata.reason"`
	ToolCallID string   `json:"toolCallId,omitempty" jsonschema:"exact metadata.toolCallId to filter by"`
	Since      string   `json:"since,omitempty" jsonschema:"optional RFC3339 lower time bound; resolved once on the first page of a query so a multi-page fetch doesn't drift as time passes"`
	Cursor     string   `json:"cursor,omitempty" jsonschema:"opaque continuation token from a previous list_events call's nextCursor; absent means start from the newest event"`
	Limit      int      `json:"limit,omitempty" jsonschema:"maximum number of events to return in this page, most-recent-first (default 50, max 500)"`
}

// listEventsResult is list_events' manually-marshaled output shape (see
// createServer's own doc comment on why list_events builds its
// *mcp.CallToolResult by hand rather than declaring a typed Out).
type listEventsResult struct {
	Events     []*events.Event `json:"events"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// LoadEventsArgs is load_events' input.
type LoadEventsArgs struct {
	Events []events.Event `json:"events" jsonschema:"events to load, replacing the current buffer contents; oldest first, same order normal NATS ingestion assumes"`
}

// readDiskTier wraps eventlog.ReadBackward, translating its disk-only
// eventlog.Cursor to/from this server's own combined pageCursor encoding.
// A nil writer (disk tier not configured, see -event-log-dir) always
// reports no data and no cursor - the disk tier is simply absent, exactly
// like this server's original no-persistence behavior.
func readDiskTier(logDir string, cursor *eventlog.Cursor, limit int, since time.Time, filter events.Filter) ([]*events.Event, string, error) {
	if logDir == "" {
		return nil, "", nil
	}
	res, err := eventlog.ReadBackward(logDir, cursor, limit, since, filter)
	if err != nil {
		var notFound *eventlog.ErrSegmentNotFound
		if errors.As(err, &notFound) {
			return nil, "", fmt.Errorf("events before this point have been archived and are no longer available on disk: %w", err)
		}
		return nil, "", err
	}
	if res.NextCursor == nil {
		return res.Events, "", nil
	}
	enc, err := encodeCursor(pageCursor{Tier: "disk", Segment: res.NextCursor.Segment, Offset: res.NextCursor.Offset})
	if err != nil {
		return nil, "", err
	}
	return res.Events, enc, nil
}

// createServer builds the MCP server. writer/logDir may be their zero
// values (nil, "") when the disk tier isn't configured (-event-log-dir
// empty) - list_events then only ever serves from the in-memory buffer,
// same as this server's original v1 behavior.
func createServer(buffer *ringBuffer, stats *eventStats, writer *eventlog.Writer, logDir string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "event-aggregator", Version: "1.0.0"}, nil)
	server.AddReceivingMiddleware(mcptrace.Middleware())

	// list_events builds its *mcp.CallToolResult manually (Out declared
	// bare `any`) rather than via a typed Out struct - this predates the
	// tiered-storage/pagination work and is kept deliberately: this repo
	// hit real MCP-client-rejected-schema bugs from jsonschema-go's
	// inference on struct fields (see CLAUDE.md), and the manual
	// pattern sidesteps all of them. listEventsResult above exists only
	// as this handler's own JSON marshaling shape, never as a tool Out
	// type.
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_events",
		Description: "List received events, most-recent-first, optionally filtered by type/subtype/user/roles/tool/reason/toolCallId, optionally bounded by since, and paginated via cursor/nextCursor across both the in-memory buffer and the on-disk log.",
		// Note: an earlier version of this tool supported an optional
		// toolCallIdClosure flag expanding a page to every other
		// in-memory buffered event sharing a matched event's
		// toolCallId. Removed - it only ever worked within the ring
		// buffer (never the disk tier, an acknowledged limitation at
		// the time) and added real complexity for a niche need; a
		// caller wanting a whole toolCallId family can just filter by
		// -tool-call-id directly.
	}, func(ctx context.Context, req *mcp.CallToolRequest, args ListEventsArgs) (*mcp.CallToolResult, any, error) {
		limit := args.Limit
		if limit <= 0 || limit > maxListLimit {
			limit = defaultListLimit
		}
		filter := events.Filter{
			Type:       args.Type,
			Subtype:    args.Subtype,
			User:       args.User,
			Roles:      args.Roles,
			Tool:       args.Tool,
			Reason:     args.Reason,
			ToolCallID: args.ToolCallID,
		}

		var since time.Time
		if args.Since != "" {
			parsed, err := time.Parse(time.RFC3339, args.Since)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid since %q: %w", args.Since, err)
			}
			since = parsed
		}

		tier := "mem"
		var cur pageCursor
		if args.Cursor != "" {
			parsed, err := decodeCursor(args.Cursor)
			if err != nil {
				return nil, nil, err
			}
			cur = parsed
			tier = cur.Tier
		}

		var matched []*events.Event
		var nextCursor string

		if tier == "mem" {
			beforeSeq := int64(-1)
			if args.Cursor != "" {
				beforeSeq = cur.Seq
			}
			memMatched, resumeSeq, hasResume, sinceCrossed, memExhausted := buffer.listBackward(beforeSeq, limit, since, filter)
			matched = memMatched
			switch {
			case hasResume:
				enc, err := encodeCursor(pageCursor{Tier: "mem", Seq: resumeSeq})
				if err != nil {
					return nil, nil, err
				}
				nextCursor = enc
			case sinceCrossed:
				// Done: the caller doesn't want anything older, on disk
				// or otherwise.
			case memExhausted:
				diskMatched, diskCursor, err := readDiskTier(logDir, nil, limit-len(matched), since, filter)
				if err != nil {
					return nil, nil, err
				}
				matched = append(matched, diskMatched...)
				nextCursor = diskCursor
			}
		} else {
			diskCursor := &eventlog.Cursor{Segment: cur.Segment, Offset: cur.Offset}
			diskMatched, nc, err := readDiskTier(logDir, diskCursor, limit, since, filter)
			if err != nil {
				return nil, nil, err
			}
			matched = diskMatched
			nextCursor = nc
		}

		data, err := json.Marshal(listEventsResult{Events: matched, NextCursor: nextCursor})
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal events: %w", err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_event_stats",
		Description: "Get cumulative counts of every event this process has ever been given (since its own last restart), one count per event under its most specific key - \"type.subtype\" if it has a subtype, otherwise just \"type\" - so an event is never counted under two keys at once. Both real NATS ingestion and load_events' bulk loads count, independent of the in-memory buffer's capacity/eviction and the on-disk log entirely, so this reflects everything ever aggregated, not just what's currently retrievable via list_events.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		data, err := json.Marshal(stats.snapshot())
		if err != nil {
			return nil, nil, fmt.Errorf("failed to marshal stats: %w", err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "reset_events",
		Description: "Clear every event currently held in memory. Does not affect NATS, any other subscriber, the on-disk log, or events published before this call — they're simply erased from memory, not archived.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		buffer.reset()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "events cleared"}}}, nil, nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "load_events",
		Description: "Replace every buffered event with the given events, for testing/demo purposes. Bypasses the event dictionary/NATS/ingestion filter entirely — the buffer's own in-memory contents only (any overflow past the current buffer capacity still spills to the on-disk log, if configured) — but still counts toward get_event_stats, exactly as if each event had been ingested for real. Every loaded event is marked with metadata imported=true, so it can't pass as a real audit record.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args LoadEventsArgs) (*mcp.CallToolResult, any, error) {
		buffer.reset()
		for i := range args.Events {
			markImported(&args.Events[i])
			stats.record(&args.Events[i])
			if evicted := buffer.add(&args.Events[i]); evicted != nil {
				persistEvicted(writer, evicted)
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("loaded %d events", len(args.Events))}}}, nil, nil
	})

	return server
}

// importedKey marks, in an event's metadata, one that load_events added
// rather than one that arrived over NATS from go-proxy - so a loaded event
// (demo data, or a forgery by whoever may call load_events) can never pass
// as a real audit record. Set on every loaded event, overwriting any value
// the caller supplied.
const importedKey = "imported"

func markImported(e *events.Event) {
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	e.Metadata[importedKey] = true
}

// persistEvicted spills an event evicted from the ring buffer to the disk
// tier, if configured (writer is nil when -event-log-dir is empty, in
// which case an evicted event is simply dropped - this server's original
// v1 behavior). A write failure is logged, not fatal or propagated: disk
// persistence is best-effort, strictly better than the unconditional drop
// it replaces, never allowed to block ingestion.
func persistEvicted(writer *eventlog.Writer, e *events.Event) {
	if writer == nil || e == nil {
		return
	}
	if err := writer.Append(e); err != nil {
		logging.Errorf("event-aggregator: failed to persist evicted event to disk log: %v", err)
	}
}

func main() {
	var port, natsURL, eventsDictionary, eventAggregationConfig, logLevelConfig string
	var logDir string
	var archiveCheckInterval time.Duration
	flag.StringVar(&port, "port", ":8080", "Port to listen on, e.g. :8080")
	flag.StringVar(&natsURL, "nats-url", "nats://nats:4222", "NATS server URL")
	flag.StringVar(&eventsDictionary, "events-dictionary", "./event_dictionary_test.json", `Path to the event dictionary config file, as a {"version":N,"config":{"eventTypes":{...}}} envelope — also what a go-proxy event-dictionary-override ConfigMap is mounted at in k8s, live-reloaded via the same NATS connection used for subscribing`)
	flag.StringVar(&eventAggregationConfig, "event-aggregation-config", "./event_aggregation_test.json", `Path to the event-aggregation config file, as a {"version":N,"config":{"filter":{...},"maxBufferSize":N,"maxLogAgeDays":N,"maxLogSizeBytes":N,"archiverTransport":{"type":"discard"}}} envelope — also what a go-proxy event-aggregation-override ConfigMap is mounted at in k8s, live-reloaded via the same NATS connection used for subscribing. The archiver's ArchiverTransport (see eventlog.RegisterArchiverTransport) is selected by "archiverTransport.type" here, not a flag`)
	flag.StringVar(&logDir, "event-log-dir", "", "Directory for the on-disk event log (the 'cold' tier); empty disables the disk tier entirely — evicted events are then simply dropped, matching this server's original no-persistence behavior")
	flag.DurationVar(&archiveCheckInterval, "archive-check-interval", time.Hour, "How often the archiver sweeps the disk log for segments past their age/size threshold")
	flag.StringVar(&logLevelConfig, "log-level-config", "./log_level_test.json", `Path to the log-level config file, as a {"version":N,"config":{"level":"error|info|debug"}} envelope — also what a go-proxy log-level-override ConfigMap is mounted at in k8s, live-reloaded via the same NATS connection used for subscribing; a missing/not-yet-seeded file is not fatal, the process just keeps its default ("info") level`)
	flag.Parse()

	buffer := newRingBuffer(bootstrapMaxBufferSize)
	stats := newEventStats()

	var writer *eventlog.Writer
	var archiver *eventlog.Archiver
	if logDir != "" {
		var err error
		writer, err = eventlog.NewWriter(logDir, bootstrapMaxLogSizeBytes)
		if err != nil {
			logging.Fatalf("Failed to open event log directory %q: %v", logDir, err)
		}
		defer writer.Close()

		// DiscardArchiverTransport until the eventAggregation Watcher's
		// constructor-time onChange call (below) applies the real,
		// config-selected ArchiverTransport — matching every other
		// field's bootstrap-then-real-config sequencing in this
		// function.
		archiver = eventlog.NewArchiver(logDir, writer.Active, eventlog.DiscardArchiverTransport{}, bootstrapMaxLogAgeDays, bootstrapMaxLogSizeBytes)
		logging.Infof("startup configuration: eventLogDir=%s archiveCheckInterval=%s", logDir, archiveCheckInterval)
	}

	eventsClient, err := events.NewClient(natsURL)
	if err != nil {
		logging.Fatalf("Failed to initialize events client: %v", err)
	}

	if _, err := os.Stat(eventsDictionary); err != nil {
		logging.Fatalf("Event dictionary file %q not found: %v", eventsDictionary, err)
	}
	if _, err := os.Stat(eventAggregationConfig); err != nil {
		logging.Fatalf("Event aggregation config file %q not found: %v", eventAggregationConfig, err)
	}
	configNC, err := configsync.Connect(natsURL)
	if err != nil {
		logging.Fatalf("Failed to connect to NATS at %q: %v", natsURL, err)
	}
	if _, err := configsync.NewWatcher("event-dictionary", eventsDictionary, configNC, (*events.Dictionary)(nil), events.ParseDictionary, eventsClient.SetDictionary); err != nil {
		logging.Fatalf("Failed to load event dictionary %q: %v", eventsDictionary, err)
	}

	// A missing/not-yet-seeded log-level-override ConfigMap must not block
	// startup — unlike eventsDictionary/eventAggregationConfig above, no
	// os.Stat pre-check: a non-nil initial lets configsync.NewWatcher fall
	// back to it when logLevelConfig doesn't exist yet.
	if _, err := configsync.NewWatcher("log-level", logLevelConfig, configNC, &logging.LevelConfig{Level: "info"}, logging.ParseConfig, logging.ApplyConfig); err != nil {
		logging.Fatalf("Failed to load log level config %q: %v", logLevelConfig, err)
	}

	var filterPtr atomic.Pointer[events.Filter]
	applyAggregationConfig := func(cfg *events.AggregationConfig) {
		if cfg == nil {
			return
		}
		f := cfg.Filter
		filterPtr.Store(&f)
		buffer.resize(cfg.MaxBufferSize, func(e *events.Event) { persistEvicted(writer, e) })
		if writer != nil {
			writer.SetMaxSizeBytes(cfg.MaxLogSizeBytes)
		}
		if archiver != nil {
			archiver.SetMaxAgeDays(cfg.MaxLogAgeDays)
			archiver.SetMaxSizeBytes(cfg.MaxLogSizeBytes)
			// A bad/unknown archiverTransport.type is never fatal here,
			// on either the first load or a later live update - it's
			// logged and the archiver keeps whichever ArchiverTransport
			// it had (the DiscardArchiverTransport bootstrap, or the
			// last one that validated), matching configsync.Watcher's
			// own availability-over-correctness convention for an
			// already-running process.
			t, err := eventlog.NewArchiverTransport(cfg.ArchiverTransport.Type, cfg.ArchiverTransport.Properties)
			if err != nil {
				logging.Errorf("event-aggregator: failed to build archiver transport %q, keeping the previous one: %v", cfg.ArchiverTransport.Type, err)
			} else {
				archiver.SetArchiverTransport(t)
			}
		}
	}
	if _, err := configsync.NewWatcher("event-aggregation", eventAggregationConfig, configNC, (*events.AggregationConfig)(nil), events.ParseAggregationConfig, applyAggregationConfig); err != nil {
		logging.Fatalf("Failed to load event aggregation config %q: %v", eventAggregationConfig, err)
	}

	if archiver != nil {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go archiver.Run(ctx, archiveCheckInterval)
	}

	onEvent := func(e *events.Event) {
		if f := filterPtr.Load(); f != nil && !f.Matches(e) {
			return
		}
		stats.record(e)
		if evicted := buffer.add(e); evicted != nil {
			persistEvicted(writer, evicted)
		}
	}
	if _, err := eventsClient.Subscribe("events.>", onEvent); err != nil {
		logging.Fatalf("Failed to subscribe to events: %v", err)
	}

	server := createServer(buffer, stats, writer, logDir)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true})

	mux := http.NewServeMux()
	mux.Handle("/", mcpHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if eventsClient.IsConnected() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	logging.Debugf("Starting event-aggregator on port %s", port)
	if err := http.ListenAndServe(port, mux); err != nil {
		logging.Fatal(err)
	}
}
