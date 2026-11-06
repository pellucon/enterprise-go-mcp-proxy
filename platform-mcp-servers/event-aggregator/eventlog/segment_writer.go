// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package eventlog implements event-aggregator's on-disk "cold" tier: an
// append-only JSONL segment log that receives events evicted from the
// in-memory ring buffer, plus a periodic archiver that ships (or discards)
// rolled segments once they're past a configured age/total-size threshold.
// See CLAUDE.md and the tiered-event-storage design plan for the
// full picture; this package only knows about files on disk, never NATS,
// the ring buffer, or MCP.
package eventlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
)

const (
	segmentFilePrefix = "events-"
	segmentFileSuffix = ".jsonl"
)

// segmentMeta describes one segment: its filename (relative to the log
// directory) and current size. Writer publishes its active segment's
// segmentMeta via a lock-free atomic.Pointer so readers (list_events' disk
// fallback, the Archiver) can learn it without taking Writer's own append
// mutex.
type segmentMeta struct {
	Name string
	Size int64
}

// Writer is an append-only JSONL segment writer: one events.Event per
// line, rolled to a new segment file when the calendar day changes or the
// active segment reaches maxSizeBytes. Safe for concurrent Append (guarded
// by its own mutex) and concurrent Active() reads (lock-free).
type Writer struct {
	dir string

	mu      sync.Mutex
	f       *os.File
	day     string // YYYYMMDD the active segment covers
	index   int    // 0 = no "-NNN" suffix, >=1 = a mid-day rollover count
	written int64  // bytes written to the active segment so far

	maxSizeBytes atomic.Int64
	active       atomic.Pointer[segmentMeta]
}

// NewWriter creates dir if needed and opens a segment writer for it,
// resuming the latest already-on-disk segment for today (if any) rather
// than always starting a fresh one — so a process restart continues
// appending to where it left off instead of leaving a small orphaned
// segment behind. maxSizeBytes<=0 means never roll on size (day-only
// rotation).
func NewWriter(dir string, maxSizeBytes int64) (*Writer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("eventlog: failed to create %s: %w", dir, err)
	}
	w := &Writer{dir: dir}
	w.maxSizeBytes.Store(maxSizeBytes)

	today := time.Now().UTC().Format("20060102")
	index, err := latestExistingIndex(dir, today)
	if err != nil {
		return nil, err
	}
	if err := w.openLocked(today, index); err != nil {
		return nil, err
	}
	return w, nil
}

// SetMaxSizeBytes updates the rotation threshold live (see
// events.AggregationConfig.MaxLogSizeBytes) — takes effect on the next
// Append that would otherwise exceed it; never truncates or rotates the
// currently-open segment just because the threshold shrank.
func (w *Writer) SetMaxSizeBytes(n int64) {
	w.maxSizeBytes.Store(n)
}

// Active returns the writer's current segment name/size, safe to call
// concurrently with Append without blocking on its mutex.
func (w *Writer) Active() segmentMeta {
	if m := w.active.Load(); m != nil {
		return *m
	}
	return segmentMeta{}
}

// Append serializes e as one JSON line and appends it to the active
// segment, first rolling to a new segment if the calendar day has changed
// or appending this line would exceed the size threshold.
func (w *Writer) Append(e *events.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("eventlog: failed to marshal event: %w", err)
	}
	line := append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()

	today := time.Now().UTC().Format("20060102")
	maxSize := w.maxSizeBytes.Load()
	switch {
	case today != w.day:
		if err := w.openLocked(today, 0); err != nil {
			return err
		}
	case maxSize > 0 && w.written+int64(len(line)) > maxSize:
		if err := w.openLocked(today, w.index+1); err != nil {
			return err
		}
	}

	n, err := w.f.Write(line)
	if err != nil {
		return fmt.Errorf("eventlog: failed to append to %s: %w", w.f.Name(), err)
	}
	w.written += int64(n)
	w.active.Store(&segmentMeta{Name: filepath.Base(w.f.Name()), Size: w.written})
	return nil
}

// Close closes the active segment file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

// openLocked closes the current segment file (if any) and opens
// day/index's, creating it if it doesn't exist yet. Caller must hold w.mu.
func (w *Writer) openLocked(day string, index int) error {
	if w.f != nil {
		w.f.Close()
	}
	name := segmentFileName(day, index)
	path := filepath.Join(w.dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("eventlog: failed to open segment %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("eventlog: failed to stat segment %s: %w", path, err)
	}
	w.f = f
	w.day = day
	w.index = index
	w.written = info.Size()
	w.active.Store(&segmentMeta{Name: name, Size: w.written})
	return nil
}

// latestExistingIndex returns the highest segment index already on disk
// for day (0 if none exist yet).
func latestExistingIndex(dir, day string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	highest := -1
	for _, e := range entries {
		idx, d, ok := parseSegmentFileName(e.Name())
		if ok && d == day && idx > highest {
			highest = idx
		}
	}
	if highest == -1 {
		return 0, nil
	}
	return highest, nil
}

// segmentFileName always includes the zero-padded index suffix, even for
// index 0 — an omitted suffix for index 0 (e.g. "events-20260902.jsonl")
// would sort *after* "events-20260902-001.jsonl" in a plain lexicographic
// filename sort, since '-' (0x2D) sorts before '.' (0x2E): that would
// invert ListSegments' chronological-via-filename-sort invariant between
// a day's first segment and its later rollovers. Always suffixing keeps
// every segment's filename comparable the same way.
func segmentFileName(day string, index int) string {
	return fmt.Sprintf("%s%s-%03d%s", segmentFilePrefix, day, index, segmentFileSuffix)
}

// parseSegmentFileName parses "events-YYYYMMDD-NNN.jsonl"; ok is false for
// any name not matching this shape (e.g. an unrelated file that ended up
// in the log directory).
func parseSegmentFileName(name string) (index int, day string, ok bool) {
	if !strings.HasPrefix(name, segmentFilePrefix) || !strings.HasSuffix(name, segmentFileSuffix) {
		return 0, "", false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, segmentFilePrefix), segmentFileSuffix)
	parts := strings.SplitN(mid, "-", 2)
	if len(parts) != 2 || len(parts[0]) != 8 {
		return 0, "", false
	}
	day = parts[0]
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, "", false
	}
	return n, day, true
}
