// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// backwardChunkSize bounds how much of a segment file is read into memory
// at once while scanning backward — see readLinesBackward.
const backwardChunkSize = 64 * 1024

// Cursor is list_events' opaque disk-tier pagination position. Callers
// never construct/inspect these fields directly — always through
// EncodeCursor/DecodeCursor's opaque string.
type Cursor struct {
	// Segment is the segment file this cursor resumes within.
	Segment string `json:"segment"`
	// Offset is the exact byte offset of the next (older) line to read,
	// scanning backward from here. A negative value means "start at this
	// segment's own EOF" — used the first time ReadBackward enters a
	// segment, before any line from it has been read.
	Offset int64 `json:"offset"`
}

// EncodeCursor renders c as an opaque, URL-safe string.
func EncodeCursor(c Cursor) (string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("eventlog: failed to encode cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// DecodeCursor parses a string produced by EncodeCursor.
func DecodeCursor(s string) (Cursor, error) {
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("eventlog: invalid cursor: %w", err)
	}
	var c Cursor
	if err := json.Unmarshal(data, &c); err != nil {
		return Cursor{}, fmt.Errorf("eventlog: invalid cursor: %w", err)
	}
	return c, nil
}

// ListSegments returns every segment file in dir, oldest first (filename
// sort == chronological order, per the events-YYYYMMDD[-NNN].jsonl naming
// convention). Returns (nil, nil) if dir doesn't exist yet (nothing has
// ever been logged).
func ListSegments(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, _, ok := parseSegmentFileName(e.Name()); ok {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Result is what ReadBackward returns.
type Result struct {
	// Events is most-recent-first, matching ringBuffer.list's own order.
	Events []*events.Event
	// NextCursor is nil when there's nothing older left on disk to read
	// (either the oldest segment hit BOF, or a `since` cutoff was
	// crossed) — the disk tier is exhausted for this query.
	NextCursor *Cursor
}

// ErrSegmentNotFound means cursor references a segment that's no longer on
// disk — most likely because it was archived (and locally deleted) since
// the cursor was issued. Callers should surface this as a clear "events
// before this point have been archived" error, not retry silently.
type ErrSegmentNotFound struct {
	Segment string
}

func (e *ErrSegmentNotFound) Error() string {
	return fmt.Sprintf("eventlog: segment %q not found (it may have been archived)", e.Segment)
}

// ErrInvalidCursor means a cursor's offset doesn't fit its segment, so it
// wasn't issued by ReadBackward.
var ErrInvalidCursor = errors.New("eventlog: invalid cursor")

// ReadBackward reads up to limit events matching filter from the on-disk
// segment log in dir, most-recent-first, resuming from cursor (nil means
// start at the newest segment's EOF). Stops early once an event's Time is
// before since (a zero since means no lower bound).
func ReadBackward(dir string, cursor *Cursor, limit int, since time.Time, filter events.Filter) (*Result, error) {
	segments, err := ListSegments(dir)
	if err != nil {
		return nil, err
	}
	if len(segments) == 0 {
		return &Result{}, nil
	}

	segIdx := len(segments) - 1
	startOffset := int64(-1)
	if cursor != nil {
		idx := indexOfSegment(segments, cursor.Segment)
		if idx == -1 {
			return nil, &ErrSegmentNotFound{Segment: cursor.Segment}
		}
		segIdx = idx
		startOffset = cursor.Offset
	}

	var out []*events.Event
	var nextCursor *Cursor
	stopped := false

	for segIdx >= 0 && !stopped && len(out) < limit {
		segName := segments[segIdx]
		result, serr := readSegmentBackward(filepath.Join(dir, segName), startOffset, limit-len(out), since, filter)
		startOffset = -1 // only the first segment in this call resumes mid-file
		if serr != nil {
			return nil, fmt.Errorf("eventlog: reading segment %q: %w", segName, serr)
		}
		out = append(out, result.events...)
		if result.sinceCrossed {
			stopped = true
			break
		}
		if result.limitReached {
			nextCursor = &Cursor{Segment: segName, Offset: result.resumeOffset}
			stopped = true
			break
		}
		segIdx--
	}

	return &Result{Events: out, NextCursor: nextCursor}, nil
}

type segmentScanResult struct {
	events       []*events.Event
	limitReached bool
	sinceCrossed bool
	resumeOffset int64
}

// readSegmentBackward scans one segment file backward from `end` (or its
// own EOF, when end<0), collecting up to limit matching events.
func readSegmentBackward(path string, end int64, limit int, since time.Time, filter events.Filter) (*segmentScanResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	switch {
	case end < 0:
		end = info.Size()
	case end > info.Size():
		// Only a forged cursor gets here (a real one never points past
		// what was on disk when it was issued, and segments only grow).
		// Reading backward from past EOF would grow readLinesBackward's
		// buffer by a zero-filled chunk per step, with no newline ever to
		// stop it, until memory runs out.
		return nil, fmt.Errorf("%w: offset %d is past the end of the segment (%d bytes)", ErrInvalidCursor, end, info.Size())
	}

	var res segmentScanResult
	scanErr := readLinesBackward(f, end, func(line []byte, lineStart int64) bool {
		var e events.Event
		if jsonErr := json.Unmarshal(line, &e); jsonErr != nil {
			logging.Errorf("[eventlog] skipping malformed line in %s at offset %d: %v", path, lineStart, jsonErr)
			return true
		}
		if !since.IsZero() && e.Time.Before(since) {
			res.sinceCrossed = true
			return false
		}
		if filter.Matches(&e) {
			res.events = append(res.events, &e)
		}
		if len(res.events) >= limit {
			res.limitReached = true
			res.resumeOffset = lineStart
			return false
		}
		return true
	})
	if scanErr != nil {
		return nil, scanErr
	}
	return &res, nil
}

// readLinesBackward calls yield once per complete line found in f, working
// backward from absolute byte offset end, in chunks of backwardChunkSize.
// yield receives the line's bytes (no trailing newline) and the absolute
// file offset its first byte starts at; returning false stops the scan
// early. Memory use is bounded to roughly one chunk at a time, not the
// whole file — the standard chunked "tail -n" technique.
func readLinesBackward(f *os.File, end int64, yield func(line []byte, lineStart int64) bool) error {
	if end <= 0 {
		return nil
	}
	var buf []byte
	start := end
	for {
		if start > 0 {
			readSize := int64(backwardChunkSize)
			if readSize > start {
				readSize = start
			}
			newStart := start - readSize
			chunk := make([]byte, readSize)
			if _, err := f.ReadAt(chunk, newStart); err != nil && err != io.EOF {
				return err
			}
			buf = append(chunk, buf...)
			start = newStart
		}

		for {
			idx := bytes.LastIndexByte(buf, '\n')
			if idx == -1 {
				break
			}
			line := buf[idx+1:]
			lineStart := start + int64(idx) + 1
			buf = buf[:idx]
			if len(line) == 0 {
				continue
			}
			if !yield(line, lineStart) {
				return nil
			}
		}

		if start == 0 {
			if len(buf) > 0 {
				yield(buf, 0)
			}
			return nil
		}
	}
}

func indexOfSegment(segments []string, name string) int {
	for i, s := range segments {
		if s == name {
			return i
		}
	}
	return -1
}
