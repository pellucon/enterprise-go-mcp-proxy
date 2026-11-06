// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// Archiver periodically scans a log directory for rolled (non-active)
// segments past a configured max age or contributing to the directory's
// total size exceeding a configured max, and archives them oldest-first
// via an ArchiverTransport: Upload, then delete the local file only on
// success. Never touches the writer's currently-active segment (learned
// via active, a lock-free read of Writer's own atomic.Pointer) and never
// takes Writer's append mutex — see the design plan's Concurrency
// section for why the two are independent.
type Archiver struct {
	dir    string
	active func() segmentMeta

	archiverTransportMu sync.RWMutex
	archiverTransport   ArchiverTransport

	maxAgeDays   atomic.Int64
	maxSizeBytes atomic.Int64
}

// NewArchiver builds an Archiver for dir. active is typically a Writer's
// own Active method, so the archiver always skips whatever segment is
// currently being appended to.
func NewArchiver(dir string, active func() segmentMeta, transport ArchiverTransport, maxAgeDays int, maxSizeBytes int64) *Archiver {
	a := &Archiver{dir: dir, active: active, archiverTransport: transport}
	a.maxAgeDays.Store(int64(maxAgeDays))
	a.maxSizeBytes.Store(maxSizeBytes)
	return a
}

// SetArchiverTransport swaps the ArchiverTransport used by every sweep
// from this point on (e.g. on a live eventAggregation config change —
// see events.ArchiverTransportConfig). A sweep already in progress
// finishes with whichever ArchiverTransport it read at its own start;
// it's never interrupted mid-upload.
func (a *Archiver) SetArchiverTransport(t ArchiverTransport) {
	a.archiverTransportMu.Lock()
	a.archiverTransport = t
	a.archiverTransportMu.Unlock()
}

func (a *Archiver) currentArchiverTransport() ArchiverTransport {
	a.archiverTransportMu.RLock()
	defer a.archiverTransportMu.RUnlock()
	return a.archiverTransport
}

// SetMaxAgeDays updates the age threshold live.
func (a *Archiver) SetMaxAgeDays(n int) { a.maxAgeDays.Store(int64(n)) }

// SetMaxSizeBytes updates the total-size threshold live.
func (a *Archiver) SetMaxSizeBytes(n int64) { a.maxSizeBytes.Store(n) }

// Run sweeps immediately (so a freshly-started process doesn't wait a
// full interval before its first sweep), then again every interval,
// until ctx is done.
func (a *Archiver) Run(ctx context.Context, interval time.Duration) {
	a.sweep(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.sweep(ctx)
		}
	}
}

// sweep archives every rolled segment past the age threshold, then — if
// the directory's total size still exceeds the size threshold — archives
// further rolled segments oldest-first until back under it (or nothing
// rolled is left to archive).
func (a *Archiver) sweep(ctx context.Context) {
	activeName := a.active().Name
	segments, err := ListSegments(a.dir)
	if err != nil {
		logging.Errorf("[eventlog] archiver: failed to list %s: %v", a.dir, err)
		return
	}

	type entry struct {
		name string
		size int64
		day  time.Time
	}
	var rolled []entry
	var total int64
	for _, name := range segments {
		info, err := os.Stat(filepath.Join(a.dir, name))
		if err != nil {
			continue
		}
		total += info.Size()
		if name == activeName {
			continue // never archive the segment still being written
		}
		rolled = append(rolled, entry{name: name, size: info.Size(), day: segmentDay(name)})
	}

	maxAge := time.Duration(a.maxAgeDays.Load()) * 24 * time.Hour
	maxSize := a.maxSizeBytes.Load()
	now := time.Now().UTC()

	archived := make(map[string]bool)
	if maxAge > 0 {
		for _, e := range rolled {
			if !e.day.IsZero() && now.Sub(e.day) > maxAge && a.archiveOne(ctx, e.name) {
				archived[e.name] = true
				total -= e.size
			}
		}
	}

	if maxSize > 0 && total > maxSize {
		// rolled is already oldest-first (ListSegments' filename sort).
		for _, e := range rolled {
			if total <= maxSize {
				break
			}
			if archived[e.name] {
				continue
			}
			if a.archiveOne(ctx, e.name) {
				total -= e.size
			}
		}
	}
}

func (a *Archiver) archiveOne(ctx context.Context, name string) bool {
	path := filepath.Join(a.dir, name)
	if err := a.currentArchiverTransport().Upload(ctx, path); err != nil {
		logging.Errorf("[eventlog] archiver: failed to upload %s, will retry next sweep: %v", name, err)
		return false
	}
	if err := os.Remove(path); err != nil {
		logging.Errorf("[eventlog] archiver: uploaded %s but failed to remove local copy: %v", name, err)
		return false
	}
	logging.Debugf("[eventlog] archived %s", name)
	return true
}

// segmentDay parses the YYYYMMDD embedded in a segment filename as a UTC
// midnight timestamp — used for age comparisons, deliberately not the
// file's own mtime (which would drift if the file were ever copied or
// touched by anything outside this package).
func segmentDay(name string) time.Time {
	_, day, ok := parseSegmentFileName(name)
	if !ok {
		return time.Time{}
	}
	t, err := time.Parse("20060102", day)
	if err != nil {
		return time.Time{}
	}
	return t
}
