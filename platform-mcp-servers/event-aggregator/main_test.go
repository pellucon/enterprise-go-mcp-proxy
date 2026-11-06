// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/event-aggregator/eventlog"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRingBufferAddEvicts(t *testing.T) {
	b := newRingBuffer(2)
	e1 := &events.Event{Type: "alert", Time: time.Now()}
	e2 := &events.Event{Type: "alert", Time: time.Now()}
	e3 := &events.Event{Type: "alert", Time: time.Now()}

	if evicted := b.add(e1); evicted != nil {
		t.Fatalf("expected no eviction under capacity, got %v", evicted)
	}
	if evicted := b.add(e2); evicted != nil {
		t.Fatalf("expected no eviction at exactly capacity, got %v", evicted)
	}
	evicted := b.add(e3)
	if evicted != e1 {
		t.Fatalf("expected the oldest event (e1) to be evicted, got %v", evicted)
	}
}

func TestRingBufferResizeShrinkEvicts(t *testing.T) {
	b := newRingBuffer(5)
	var added []*events.Event
	for i := 0; i < 5; i++ {
		e := &events.Event{Type: "alert", Time: time.Now()}
		added = append(added, e)
		b.add(e)
	}

	var evicted []*events.Event
	b.resize(2, func(e *events.Event) { evicted = append(evicted, e) })

	if len(evicted) != 3 {
		t.Fatalf("expected 3 evictions shrinking 5->2, got %d", len(evicted))
	}
	for i, e := range evicted {
		if e != added[i] {
			t.Fatalf("eviction %d: expected oldest-first order, got mismatch", i)
		}
	}
	if got := b.list(events.Filter{}, 100); len(got) != 2 {
		t.Fatalf("expected 2 events remaining after shrink, got %d", len(got))
	}
}

func TestRingBufferListBackwardPaginationResumes(t *testing.T) {
	b := newRingBuffer(100)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 25
	for i := 0; i < total; i++ {
		b.add(&events.Event{Type: "alert", Title: itoa(i), Time: base.Add(time.Duration(i) * time.Minute)})
	}

	var got []*events.Event
	beforeSeq := int64(-1)
	for {
		matched, resumeSeq, hasResume, sinceCrossed, exhausted := b.listBackward(beforeSeq, 7, time.Time{}, events.Filter{})
		got = append(got, matched...)
		if sinceCrossed {
			t.Fatal("unexpected since cutoff with a zero since")
		}
		if hasResume {
			beforeSeq = resumeSeq
			continue
		}
		if exhausted {
			break
		}
	}

	if len(got) != total {
		t.Fatalf("expected %d events across all pages, got %d", total, len(got))
	}
	for i, e := range got {
		want := itoa(total - 1 - i)
		if e.Title != want {
			t.Fatalf("event %d: want title %s, got %s (not most-recent-first / has gaps)", i, want, e.Title)
		}
	}
}

func TestRingBufferListBackwardSinceCutoff(t *testing.T) {
	b := newRingBuffer(100)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		b.add(&events.Event{Type: "alert", Time: base.Add(time.Duration(i) * time.Hour)})
	}
	since := base.Add(6 * time.Hour)
	matched, _, hasResume, sinceCrossed, _ := b.listBackward(-1, 100, since, events.Filter{})
	if hasResume {
		t.Fatal("since cutoff should not report hasResume")
	}
	if !sinceCrossed {
		t.Fatal("expected sinceCrossed to be true")
	}
	for _, e := range matched {
		if e.Time.Before(since) {
			t.Fatalf("got event at %v before since cutoff %v", e.Time, since)
		}
	}
	if len(matched) != 4 {
		t.Fatalf("expected 4 events at/after the cutoff (hours 6..9), got %d", len(matched))
	}
}

func TestPageCursorRoundTrip(t *testing.T) {
	c := pageCursor{Tier: "mem", Seq: 42}
	s, err := encodeCursor(c)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	got, err := decodeCursor(s)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if got != c {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, c)
	}
}

func TestReadDiskTierNoDirIsNoOp(t *testing.T) {
	matched, cursor, err := readDiskTier("", nil, 10, time.Time{}, events.Filter{})
	if err != nil {
		t.Fatalf("expected no error with an unconfigured disk tier, got %v", err)
	}
	if matched != nil || cursor != "" {
		t.Fatalf("expected no events/cursor with an unconfigured disk tier, got %v / %q", matched, cursor)
	}
}

// connectInProcessEventAggregator builds server (via createServer) and
// returns a connected client session to it over an in-memory transport —
// no real network socket, no HTTP - the same lightweight pattern
// constraints_validation_test.go's startInProcessGuardrailSession uses.
func connectInProcessEventAggregator(t *testing.T, ctx context.Context, buffer *ringBuffer, stats *eventStats, writer *eventlog.Writer, logDir string) *mcp.ClientSession {
	t.Helper()
	server := createServer(buffer, stats, writer, logDir)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("failed to connect in-memory server: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0.0"}, nil)
	sess, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("failed to connect in-memory client: %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess
}

// callListEvents invokes the real list_events tool over sess and decodes
// its manually-marshaled JSON response (see listEventsResult) — the same
// shape a real client has to parse, since nextCursor is embedded in the
// tool's TextContent, not a structured CallToolResult field.
func callListEvents(t *testing.T, ctx context.Context, sess *mcp.ClientSession, args ListEventsArgs) listEventsResult {
	t.Helper()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "list_events", Arguments: args})
	if err != nil {
		t.Fatalf("list_events call failed: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_events returned an error result: %+v", res.Content)
	}
	if len(res.Content) != 1 {
		t.Fatalf("expected exactly one content block, got %d", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected a TextContent block, got %T", res.Content[0])
	}
	var out listEventsResult
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("failed to decode list_events response %q: %v", text.Text, err)
	}
	return out
}

// TestListEventsToolTierHandoff drives the real list_events MCP tool (not
// ringBuffer.listBackward/eventlog.ReadBackward directly) across a real
// memory-to-disk tier handoff, following each page's nextCursor exactly as
// a real client would - the piece the rest of this file's cursor tests
// (TestRingBufferListBackwardPaginationResumes, TestPageCursorRoundTrip,
// and eventlog's own TestBackwardReadAcrossMultipleSegments/
// TestCursorRoundTrip) only cover indirectly, one layer below the actual
// tool handler.
func TestListEventsToolTierHandoff(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	writer, err := eventlog.NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("eventlog.NewWriter: %v", err)
	}
	defer writer.Close()

	const capacity = 5
	const total = 15 // capacity=5 in memory, 10 evicted to disk
	buffer := newRingBuffer(capacity)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < total; i++ {
		e := &events.Event{Type: "alert", Title: itoa(i), Time: base.Add(time.Duration(i) * time.Minute)}
		if evicted := buffer.add(e); evicted != nil {
			if err := writer.Append(evicted); err != nil {
				t.Fatalf("writer.Append: %v", err)
			}
		}
	}

	sess := connectInProcessEventAggregator(t, ctx, buffer, newEventStats(), writer, dir)

	var got []*events.Event
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatalf("too many pages, cursor likely never terminates (last cursor %q)", cursor)
		}
		page := callListEvents(t, ctx, sess, ListEventsArgs{Limit: 4, Cursor: cursor})
		got = append(got, page.Events...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(got) != total {
		t.Fatalf("expected %d events across all pages, got %d", total, len(got))
	}
	for i, e := range got {
		want := itoa(total - 1 - i)
		if e.Title != want {
			t.Fatalf("event %d: want title %s, got %s (not most-recent-first, or has gaps/dupes across the mem->disk handoff)", i, want, e.Title)
		}
	}
}

// TestEventStatsRecordAndSnapshot covers record's "most specific key,
// never both" rule: an event with a subtype is counted only under
// "type.subtype", never also under the bare "type" (no redundant
// "alert": 3 alongside "alert.tool-call-blocked": 2 / "alert.result-blocked": 1).
func TestEventStatsRecordAndSnapshot(t *testing.T) {
	s := newEventStats()
	s.record(&events.Event{Type: "alert", Subtype: "tool-call-blocked"})
	s.record(&events.Event{Type: "alert", Subtype: "tool-call-blocked"})
	s.record(&events.Event{Type: "alert", Subtype: "result-blocked"})
	s.record(&events.Event{Type: "tool-request"})

	got := s.snapshot()
	want := map[string]int{
		"alert.tool-call-blocked": 2,
		"alert.result-blocked":    1,
		"tool-request":            1,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d distinct keys, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("counts[%q] = %d, want %d (full: %v)", k, got[k], v, got)
		}
	}
}

func TestEventStatsSnapshotIsACopy(t *testing.T) {
	s := newEventStats()
	s.record(&events.Event{Type: "alert"})
	snap := s.snapshot()
	snap["alert"] = 999 // mutating the returned copy must not affect s
	if got := s.snapshot()["alert"]; got != 1 {
		t.Fatalf("expected snapshot() to return an independent copy, got alert=%d after mutating a prior snapshot", got)
	}
}

// TestGetEventStatsToolCountsLoadEvents proves load_events' bulk-loaded
// events count toward get_event_stats exactly as if they'd been ingested
// for real (see eventStats' own doc comment) - a caller seeding canned
// demo data (e.g. go-gardening-demo's own sample-events.json) expects
// get_event_stats to reflect it immediately, not stay empty until real
// NATS traffic arrives. Also confirms a subsequent reset_events call
// (which only clears the buffer's displayable contents) leaves those
// counts untouched, and that real record calls still add on top.
func TestGetEventStatsToolCountsLoadEvents(t *testing.T) {
	ctx := context.Background()
	buffer := newRingBuffer(10)
	stats := newEventStats()
	sess := connectInProcessEventAggregator(t, ctx, buffer, stats, nil, "")

	getStats := func() map[string]int {
		t.Helper()
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "get_event_stats", Arguments: map[string]any{}})
		if err != nil {
			t.Fatalf("get_event_stats call failed: %v", err)
		}
		text, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatalf("expected a TextContent block, got %T", res.Content[0])
		}
		var counts map[string]int
		if err := json.Unmarshal([]byte(text.Text), &counts); err != nil {
			t.Fatalf("failed to decode get_event_stats response %q: %v", text.Text, err)
		}
		return counts
	}

	loadResult, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "load_events",
		Arguments: map[string]any{
			"events": []map[string]any{
				{"type": "alert", "subtype": "tool-call-alert", "time": time.Now().UTC().Format(time.RFC3339), "title": "t", "text": "t"},
				{"type": "alert", "subtype": "tool-call-alert", "time": time.Now().UTC().Format(time.RFC3339), "title": "t", "text": "t"},
			},
		},
	})
	if err != nil {
		t.Fatalf("load_events call failed: %v", err)
	}
	if loadResult.IsError {
		t.Fatalf("load_events returned an error result: %+v", loadResult.Content)
	}

	counts := getStats()
	// Both loaded events have a subtype, so they're counted only under
	// the specific "alert.tool-call-alert" key - no redundant bare
	// "alert" key alongside it (see eventStats.record's own doc comment).
	if counts["alert.tool-call-alert"] != 2 {
		t.Fatalf("expected load_events' 2 events to be reflected in get_event_stats, got %v", counts)
	}
	if _, ok := counts["alert"]; ok {
		t.Fatalf("expected no redundant bare \"alert\" key alongside \"alert.tool-call-alert\", got %v", counts)
	}

	// reset_events clears the buffer, but must not clear the stats.
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "reset_events", Arguments: map[string]any{}}); err != nil {
		t.Fatalf("reset_events call failed: %v", err)
	}
	if counts := getStats(); counts["alert.tool-call-alert"] != 2 {
		t.Fatalf("expected reset_events to leave get_event_stats' counters untouched, got %v", counts)
	}

	// Real ingestion of a subtype-less alert adds a separate "alert" key,
	// independent of the subtype-keyed count above.
	stats.record(&events.Event{Type: "alert"})
	if counts := getStats(); counts["alert"] != 1 || counts["alert.tool-call-alert"] != 2 {
		t.Fatalf("expected a real subtype-less record to add its own \"alert\" key without touching \"alert.tool-call-alert\", got %v", counts)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// TestLoadEventsMarksImported checks that every event load_events adds is
// marked imported - even one claiming otherwise - so a loaded event can
// never pass as a real audit record.
func TestLoadEventsMarksImported(t *testing.T) {
	ctx := context.Background()
	buffer := newRingBuffer(10)
	sess := connectInProcessEventAggregator(t, ctx, buffer, newEventStats(), nil, "")

	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := sess.CallTool(ctx, &mcp.CallToolParams{
		Name: "load_events",
		Arguments: map[string]any{"events": []map[string]any{
			{"type": "tool-request-result", "subtype": "call-succeeded", "time": now, "title": "t", "text": "t", "metadata": map[string]any{"user": "alice", "imported": false}},
			{"type": "alert", "time": now, "title": "t", "text": "t"},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded := buffer.list(events.Filter{}, 10)
	if len(loaded) != 2 {
		t.Fatalf("buffer holds %d events, want 2", len(loaded))
	}
	for _, e := range loaded {
		if e.Metadata[importedKey] != true {
			t.Errorf("loaded %s event isn't marked imported: %v", e.Type, e.Metadata)
		}
	}
}
