// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
)

func mkEvent(typ string, t time.Time) *events.Event {
	return &events.Event{Type: typ, Time: t, Title: typ, Text: typ}
}

func TestWriterSizeRotation(t *testing.T) {
	dir := t.TempDir()
	// Small enough that a handful of events forces multiple rotations.
	w, err := NewWriter(dir, 200)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	now := time.Now().UTC()
	for i := 0; i < 20; i++ {
		if err := w.Append(mkEvent("alert", now)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	segments, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segments) < 2 {
		t.Fatalf("expected size rotation to produce multiple segments, got %v", segments)
	}
	for _, name := range segments {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		// Every rolled (non-active) segment should individually respect
		// the threshold; the active one may hold exactly one line even if
		// that line alone exceeds it (never refused).
		if name != w.Active().Name && info.Size() > 200 {
			t.Errorf("segment %s size %d exceeds threshold and isn't active", name, info.Size())
		}
	}
}

func TestWriterResumesLatestSegmentOnRestart(t *testing.T) {
	dir := t.TempDir()
	w1, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	now := time.Now().UTC()
	if err := w1.Append(mkEvent("alert", now)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	firstSegment := w1.Active().Name
	w1.Close()

	w2, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter (resume): %v", err)
	}
	defer w2.Close()
	if err := w2.Append(mkEvent("alert", now)); err != nil {
		t.Fatalf("Append after resume: %v", err)
	}

	segments, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected the restart to resume the same segment, got %v (first was %s)", segments, firstSegment)
	}
}

func TestBackwardReadAcrossMultipleSegments(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 50
	for i := 0; i < total; i++ {
		e := mkEvent("alert", base.Add(time.Duration(i)*time.Minute))
		e.Title = strconv.Itoa(i)
		if err := w.Append(e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
		// Force a fresh segment every 7 events, to exercise cross-segment
		// traversal without waiting on real size/day rotation.
		if (i+1)%7 == 0 {
			w.Close()
			w2, err := NewWriter(dir, 0)
			if err != nil {
				t.Fatalf("re-NewWriter: %v", err)
			}
			// Force the next segment to be a new file, not a resumed one,
			// by bumping to the next index directly.
			w2.mu.Lock()
			w2.index++
			if err := w2.openLocked(w2.day, w2.index); err != nil {
				w2.mu.Unlock()
				t.Fatalf("openLocked: %v", err)
			}
			w2.mu.Unlock()
			w = w2
			defer w.Close()
		}
	}

	segments, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segments) < 2 {
		t.Fatalf("expected multiple segments, got %v", segments)
	}

	// Page through everything with a small limit and confirm we get every
	// event, most-recent-first, with no duplicates or gaps.
	var got []*events.Event
	var cursor *Cursor
	for {
		res, err := ReadBackward(dir, cursor, 6, time.Time{}, events.Filter{})
		if err != nil {
			t.Fatalf("ReadBackward: %v", err)
		}
		got = append(got, res.Events...)
		if res.NextCursor == nil {
			break
		}
		cursor = res.NextCursor
	}

	if len(got) != total {
		t.Fatalf("expected %d events total, got %d", total, len(got))
	}
	for i, e := range got {
		wantIdx := total - 1 - i
		if e.Title != strconv.Itoa(wantIdx) {
			t.Fatalf("event %d: expected title %s, got %s (order not most-recent-first)", i, strconv.Itoa(wantIdx), e.Title)
		}
	}
}

func TestBackwardReadSinceCutoff(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		if err := w.Append(mkEvent("alert", base.Add(time.Duration(i)*time.Hour))); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	since := base.Add(5 * time.Hour)
	res, err := ReadBackward(dir, nil, 100, since, events.Filter{})
	if err != nil {
		t.Fatalf("ReadBackward: %v", err)
	}
	if res.NextCursor != nil {
		t.Fatalf("expected a since cutoff to leave no further cursor, got %+v", res.NextCursor)
	}
	for _, e := range res.Events {
		if e.Time.Before(since) {
			t.Fatalf("got event at %v, before since cutoff %v", e.Time, since)
		}
	}
	// Events at hours 5..9 satisfy Time >= since, i.e. 5 of them.
	if len(res.Events) != 5 {
		t.Fatalf("expected 5 events at/after the cutoff, got %d", len(res.Events))
	}
}

func TestBackwardReadFilterMatches(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	now := time.Now().UTC()
	if err := w.Append(mkEvent("alert", now)); err != nil {
		t.Fatal(err)
	}
	if err := w.Append(mkEvent("tool-request", now)); err != nil {
		t.Fatal(err)
	}

	res, err := ReadBackward(dir, nil, 100, time.Time{}, events.Filter{Type: "alert"})
	if err != nil {
		t.Fatalf("ReadBackward: %v", err)
	}
	if len(res.Events) != 1 || res.Events[0].Type != "alert" {
		t.Fatalf("expected exactly one alert event, got %+v", res.Events)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := Cursor{Segment: "events-20260101.jsonl", Offset: 1234}
	s, err := EncodeCursor(c)
	if err != nil {
		t.Fatalf("EncodeCursor: %v", err)
	}
	got, err := DecodeCursor(s)
	if err != nil {
		t.Fatalf("DecodeCursor: %v", err)
	}
	if got != c {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, c)
	}
}

func TestReadBackwardStaleCursor(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	w.Close()

	_, err = ReadBackward(dir, &Cursor{Segment: "events-19990101.jsonl", Offset: -1}, 10, time.Time{}, events.Filter{})
	if err == nil {
		t.Fatal("expected an error for a cursor referencing a missing segment")
	}
}

// TestReadBackwardRejectsOffsetPastEOF checks that a forged cursor
// pointing past a segment's end is refused rather than read, which used to
// grow the read buffer without bound.
func TestReadBackwardRejectsOffsetPastEOF(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	w.Close()
	segments, err := ListSegments(dir)
	if err != nil || len(segments) != 1 {
		t.Fatalf("ListSegments = %v, %v", segments, err)
	}

	_, err = ReadBackward(dir, &Cursor{Segment: segments[0], Offset: 1 << 60}, 10, time.Time{}, events.Filter{})
	if !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("ReadBackward = %v; want ErrInvalidCursor", err)
	}
}

func TestConcurrentAppendAndRead(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = w.Append(mkEvent("alert", time.Now().UTC()))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, _ = ReadBackward(dir, nil, 20, time.Time{}, events.Filter{})
		}
	}()
	wg.Wait()
}

func TestArchiverDiscardDefaultBoundsDiskUsage(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 50)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()

	// Force several rolled segments via size rotation.
	for i := 0; i < 10; i++ {
		if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := ListSegments(dir)
	if len(before) < 2 {
		t.Fatalf("expected multiple segments before archiving, got %v", before)
	}

	a := NewArchiver(dir, w.Active, DiscardArchiverTransport{}, 0, 1) // maxSizeBytes=1: archive everything not active
	a.sweep(context.Background())

	after, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	// Only the active segment should remain; everything else was
	// discarded (deleted locally, never uploaded anywhere).
	if len(after) != 1 || after[0] != w.Active().Name {
		t.Fatalf("expected only the active segment to remain, got %v (active=%s)", after, w.Active().Name)
	}
}

func TestArchiverSkipsActiveSegment(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	a := NewArchiver(dir, w.Active, DiscardArchiverTransport{}, 0, 1)
	a.sweep(context.Background())

	segments, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("active segment must never be archived, got %v", segments)
	}
}

func TestArchiverAgeThreshold(t *testing.T) {
	dir := t.TempDir()
	// A segment file dated far in the past, plus a fresh active one.
	old := segmentFileName("20200101", 0)
	if err := os.WriteFile(filepath.Join(dir, old), []byte(`{"type":"alert","time":"2020-01-01T00:00:00Z","title":"t","text":"t"}`+"\n"), 0o644); err != nil {
		t.Fatalf("seed old segment: %v", err)
	}
	w, err := NewWriter(dir, 0)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}

	a := NewArchiver(dir, w.Active, DiscardArchiverTransport{}, 5, 0) // maxAgeDays=5, no size cap
	a.sweep(context.Background())

	segments, err := ListSegments(dir)
	if err != nil {
		t.Fatalf("ListSegments: %v", err)
	}
	for _, s := range segments {
		if s == old {
			t.Fatalf("expected the old segment to be archived by age, still present: %v", segments)
		}
	}
	if len(segments) != 1 || segments[0] != w.Active().Name {
		t.Fatalf("expected only the active segment to remain, got %v", segments)
	}
}
