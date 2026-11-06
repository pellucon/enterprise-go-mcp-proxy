// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package configsync

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/nats-io/nats.go"
)

// ReloadTimeout bounds how long Watcher[T].reload keeps retrying a read of
// its local file after a NATS ping before giving up and keeping the
// last-good value (availability over correctness for a running backend).
// Var, not const, so hermetic tests can shorten it.
//
// 3 minutes, not 30 seconds: verified against a real k3s/kubelet
// deployment (Colima), not just the fake-clientset/local-file unit tests -
// a plain (non-subPath) ConfigMap volume's kubelet-side propagation delay
// is commonly cited as "sync period (default 1 minute) plus the informer
// cache's own TTL (also ~1 minute)", i.e. routinely upwards of a minute in
// practice, sometimes more depending on cluster load. 30s was observed to
// reliably lose the race: reload() gave up and logged "timed out waiting
// for updated content" while the volume file was still mid-sync. Since the
// PollInterval re-check exists, a reload giving up no longer leaves the
// Watcher stale indefinitely (it converges within one PollInterval of the
// file finally syncing), but a ping-driven reload is still the fast path,
// and 3 minutes comfortably covers the common kubelet delay with margin.
var ReloadTimeout = 3 * time.Minute

// ReloadInitialBackoff/ReloadMaxBackoff bound the retry backoff within
// ReloadTimeout — kubelet's ConfigMap-to-volume sync isn't instant, so the
// first read attempt after a ping commonly still sees the old content.
var (
	ReloadInitialBackoff = 200 * time.Millisecond
	ReloadMaxBackoff     = 2 * time.Second
)

// PollInterval is how often every subscribed Watcher re-checks its local
// file even without a NATS ping — the safety net that makes a Watcher
// converge on the file's current content no matter how a ping-driven
// reload and the kubelet's ConfigMap-to-volume sync interleave. Pings stay
// the fast path; see NewWatcher's poll goroutine for why this is needed at
// all. Var, not const, so hermetic tests can shorten it.
var PollInterval = 30 * time.Second

// rawVersionedConfig peels off just the outer {"version", "config"}
// envelope, leaving Config as raw JSON so it can be handed to a section's
// own parse func (e.g. access_control.ParseConfig) unmodified — the same
// json.RawMessage indirection loadProxyConfig already uses per section.
type rawVersionedConfig struct {
	Version int64           `json:"version"`
	Config  json.RawMessage `json:"config"`
}

// Watcher holds the current, live value of one config section in memory,
// updated in reaction to a NATS ping (the fast path) plus a periodic
// re-check of its file every PollInterval (the safety net, see NewWatcher).
// Used by
// every reader: go-proxy's own runtime engines (for accessRules,
// orchestration, discovery) and every backend server consuming
// eventDictionary/declarativeGuardrails.
type Watcher[T any] struct {
	section string
	// path is the local file this section's override ConfigMap is mounted
	// at. Empty means "no override mount configured" (the standalone
	// binary, or a hermetic test not exercising this mechanism) — Current
	// then simply always returns the value NewWatcher was constructed
	// with, exactly as if configsync weren't wired in at all.
	path  string
	parse func([]byte) (*T, error)
	// onChange, if non-nil, is invoked with the new value every time
	// Current's value changes — including once at construction, whether
	// that value came from the override file or from the initial
	// fallback, so a caller with its own downstream engine (e.g.
	// access_control.AccessPolicyEngine.Update, triggered_tools_runtime.
	// Runtime.Update) never special-cases "first load" vs "reload": both
	// go through the same onChange call. Callers with no such engine
	// (discovery, declarative-guardrail) read Current() directly per use
	// instead and pass nil here.
	onChange func(*T)

	sub *nats.Subscription
	// reloadMu is held for the entire duration of any reload() call running
	// on the NATS callback goroutine. Close locks (then immediately unlocks)
	// it after signaling closing, so Close doesn't return until any in-flight
	// reload has actually finished — a caller that gets Close to return can
	// rely on this Watcher being fully quiesced (no goroutine still reading
	// configsync's package-level ReloadTimeout/ReloadInitialBackoff/
	// ReloadMaxBackoff vars) — see CLAUDE.md's "Test lifecycle: graceful
	// teardown". A plain sync.Mutex, not a sync.WaitGroup: a WaitGroup's
	// Add(1) must never race a concurrent Wait when the counter is zero
	// (its own documented misuse case) — since a callback invocation here
	// can start at any time, including concurrently with Close, a WaitGroup
	// can't express "wait for an in-flight call, if any" safely, but a
	// Mutex's Lock/Unlock have no such precondition and are always safe to
	// call concurrently. Paired with closing (below): without a way to
	// interrupt an in-flight reload's own retry loop, Close blocking on this
	// alone could wait out the entire (up to several-minute) ReloadTimeout
	// whenever the override file a reload is retrying against never
	// actually appears (e.g. outside k8s, where an Update only ever mutates
	// the Writer's own in-memory fallback and never touches disk at all).
	reloadMu sync.Mutex
	// closing is closed exactly once, by Close, to cooperatively interrupt
	// any in-flight reload's retry loop immediately rather than letting it
	// run out its own backoff/deadline.
	closing   chan struct{}
	closeOnce sync.Once
	// pollWG tracks the poll goroutine NewWatcher starts alongside the
	// subscription. Its Add(1) happens in NewWatcher, before the Watcher is
	// returned to anyone who could call Close, so Close's Wait can never
	// race it (the misuse case the reloadMu comment above describes).
	pollWG sync.WaitGroup

	mu      sync.RWMutex
	current *T
	lastRaw string
	// version is the envelope version of the override file current was
	// last loaded from - 0 while current is still the initial fallback
	// (no override file yet). See Version.
	version int64
}

// NewWatcher constructs a Watcher for section. initial is the value to use
// when path is empty, or the override file doesn't exist yet (e.g. a fresh
// k8s deployment before its first admin update has ever seeded this
// section, or a standalone/test run with no override file at all) —
// typically whatever was parsed directly from proxy-server.cfg (or a
// backend's own -config default). If path is non-empty and the file
// exists, its content is parsed as a VersionedConfig[T] envelope, validated
// via parse, and used instead — fail-closed: a malformed initial override
// file is a construction error, not silently ignored.
//
// If nc is non-nil and path is non-empty, the Watcher subscribes to this
// section's NATS ping and reloads path on every message. A nil nc (NATS
// not configured) or empty path (no override mount) makes this a total
// no-op: Current() never changes after construction, same as before
// configsync existed.
//
// onChange (may be nil) is invoked once here with the value NewWatcher
// settles on, then again after every successful reload — see the Watcher
// doc comment on the onChange field for why callers with a downstream
// engine want this instead of special-casing initial-load vs reload.
func NewWatcher[T any](section, path string, nc *nats.Conn, initial *T, parse func([]byte) (*T, error), onChange func(*T)) (*Watcher[T], error) {
	w := &Watcher[T]{section: section, path: path, parse: parse, onChange: onChange, current: initial, closing: make(chan struct{})}

	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			var raw rawVersionedConfig
			if err := json.Unmarshal(data, &raw); err != nil {
				return nil, fmt.Errorf("configsync: %s: failed to parse override file %s as a versioned config envelope: %w", section, path, err)
			}
			parsed, err := parse(raw.Config)
			if err != nil {
				return nil, fmt.Errorf("configsync: %s: initial override at %s failed validation: %w", section, path, err)
			}
			w.current = parsed
			w.lastRaw = string(data)
			w.version = raw.Version
		case os.IsNotExist(err):
			// Not seeded yet — initial (parsed directly by the caller,
			// e.g. from proxy-server.cfg) stands.
		default:
			return nil, fmt.Errorf("configsync: %s: failed to read override file %s: %w", section, path, err)
		}
	}

	// Logged only when there's actually a value (nil means the section is
	// genuinely absent/unconfigured — e.g. an optional section with no
	// initial and no override file yet — which is not "relevant
	// configuration" to report).
	if w.current != nil {
		logging.Infof("[configsync] %s: startup configuration: %s", section, formatConfig(w.current))
	}

	if w.onChange != nil {
		w.onChange(w.current)
	}

	if nc != nil && path != "" {
		sub, err := nc.Subscribe(ConfigSubject(section), func(*nats.Msg) {
			w.reloadMu.Lock()
			defer w.reloadMu.Unlock()
			w.reload()
		})
		if err != nil {
			return nil, fmt.Errorf("configsync: %s: failed to subscribe for config change notifications: %w", section, err)
		}
		w.sub = sub

		// Safety net next to the ping: a ping-driven reload stops at the
		// FIRST change it sees in the file, but the kubelet's
		// ConfigMap-to-volume sync can deliver a stale intermediate version
		// first (e.g. a pod that started before its ConfigMap existed gets
		// the kubelet's cached v1 while the ping was for v2), or land only
		// after that reload has given up, or the ping itself can be lost
		// (NATS reconnect). Any of those left the Watcher on stale content
		// until some unrelated later ping. Re-checking the file every
		// PollInterval bounds that to one interval after the file updates.
		w.pollWG.Add(1)
		go w.poll(PollInterval)
	}

	return w, nil
}

// poll re-checks the file every interval until Close. It skips a tick
// while a ping-driven reload() holds reloadMu (that reload is already
// watching the file), and otherwise does one tryReload — which only
// applies genuinely changed, valid content, so an unchanged file is a
// cheap no-op read.
func (w *Watcher[T]) poll(interval time.Duration) {
	defer w.pollWG.Done()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.closing:
			return
		case <-ticker.C:
		}
		if !w.reloadMu.TryLock() {
			continue
		}
		if w.tryReload() {
			logging.Infof("[configsync] %s: the update above was picked up by the periodic re-check (no change ping arrived for it)", w.section)
		}
		w.reloadMu.Unlock()
	}
}

// Current returns the section's current value. Callers must treat the
// result as read-only (same convention as access_control.AccessPolicyEngine.
// Snapshot/triggered_tools_runtime.Runtime.Snapshot) — Watcher never mutates
// a value in place after publishing it, always swapping in a wholly new one.
func (w *Watcher[T]) Current() *T {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.current
}

// Version returns the configsync version of the override file Current's
// value was loaded from - the same number the Writer stamped on that
// write - or 0 while Current is still the initial fallback (no override
// file seen yet). SetLocal doesn't change it: a local admin update has no
// file behind it. A backend exposes this so go-proxy can confirm a write
// actually reached it (see the declarative-guardrail server's
// get_config_version tool), since the kubelet's ConfigMap-to-volume sync
// lags the Writer by up to a couple of minutes.
func (w *Watcher[T]) Version() int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.version
}

// SetLocal immediately swaps Current to v and fires onChange, without
// touching the override file or NATS. For a section with no downstream
// engine of its own (e.g. discovery, whose only "engine" is this Watcher),
// the admin-tool handler that just wrote v to the section's override
// ConfigMap calls this so the replica that served the call reflects it
// immediately, rather than waiting on its own NATS round trip — mirroring
// access_control.AccessPolicyEngine.Update/triggered_tools_runtime.Runtime.
// Update's existing "always apply in-memory immediately" behavior.
func (w *Watcher[T]) SetLocal(v *T) {
	w.mu.Lock()
	w.current = v
	w.mu.Unlock()
	logging.Infof("[configsync] %s: applied updated configuration (local admin update): %s", w.section, formatConfig(v))
	if w.onChange != nil {
		w.onChange(v)
	}
}

// Close unsubscribes from NATS (best effort — stops most, not necessarily
// every, future message from being dispatched: a message already pulled off
// the wire and queued for delivery when Unsubscribe runs is still delivered
// regardless), signals any reload() already in flight — and the poll
// goroutine — to stop immediately (rather than running out its own
// backoff/deadline), waits for the poll goroutine to exit, and then
// blocks until reloadMu can be acquired — i.e. until no reload() call, past
// or newly-dispatched, is still running. A caller that gets Close to return
// can rely on this Watcher being fully quiesced. Safe to call more than
// once, and safe to call on a Watcher that never subscribed (nc or path was
// empty at construction).
func (w *Watcher[T]) Close() error {
	if w.sub == nil {
		return nil
	}
	w.closeOnce.Do(func() { close(w.closing) })
	err := w.sub.Unsubscribe()
	w.pollWG.Wait()
	w.reloadMu.Lock()
	w.reloadMu.Unlock()
	return err
}

// reload re-reads w.path with bounded retry/backoff until its content
// changes from what's already loaded (kubelet's ConfigMap-to-volume sync
// isn't instant, so the first attempt right after a ping commonly still
// sees the old content), ReloadTimeout elapses (in which case the last-good
// value is kept and the timeout is logged rather than treated as fatal —
// availability over correctness for an already-running process), or Close
// signals via w.closing that this Watcher is shutting down and the retry
// loop should stop immediately instead of running out its own timeout.
func (w *Watcher[T]) reload() {
	deadline := time.Now().Add(ReloadTimeout)
	backoff := ReloadInitialBackoff
	for {
		if w.tryReload() {
			return
		}
		select {
		case <-w.closing:
			return
		default:
		}
		if time.Now().After(deadline) {
			// Warn, not Error: this reload may simply have been chased by a
			// later one — tryReload only checks "did the file's bytes change
			// since I last saw them," not "did it reach a specific version,"
			// so when writes to the same section land close together (e.g.
			// several automated updates in quick succession), an earlier
			// ping's reload can absorb the change a later ping's reload was
			// itself waiting for, leaving the later one waiting for a further
			// change that never comes. The process still ends up on the
			// correct current content either way (via whichever reload did
			// catch it, or else the PollInterval re-check — which is what
			// makes this true even when only ONE ping was sent and an
			// intermediate version absorbed it) — this path only means THIS
			// particular reload gave up
			// on stale-but-still-valid-at-the-time content, not that anything
			// is actually wrong or out of sync.
			logging.Warnf("[configsync] %s: timed out waiting for updated content at %s, keeping last-good value", w.section, w.path)
			return
		}
		select {
		case <-time.After(backoff):
		case <-w.closing:
			return
		}
		if backoff < ReloadMaxBackoff {
			backoff *= 2
		}
	}
}

// tryReload attempts one read+parse+swap. It returns true only when it
// applied genuinely new content; false (read error, unchanged content, or
// a parse/validation failure) means "caller should retry" — a malformed
// write is retried within the bounded window rather than poisoning
// lastRaw.
func (w *Watcher[T]) tryReload() bool {
	data, err := os.ReadFile(w.path)
	if err != nil {
		return false
	}

	w.mu.RLock()
	unchanged := string(data) == w.lastRaw
	w.mu.RUnlock()
	if unchanged {
		return false
	}

	var raw rawVersionedConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		logging.Errorf("[configsync] %s: reload failed to parse envelope at %s, retrying: %v", w.section, w.path, err)
		return false
	}
	parsed, err := w.parse(raw.Config)
	if err != nil {
		logging.Errorf("[configsync] %s: reload validation failed, retrying: %v", w.section, err)
		return false
	}

	w.mu.Lock()
	w.current = parsed
	w.lastRaw = string(data)
	w.version = raw.Version
	w.mu.Unlock()
	logging.Infof("[configsync] %s: applied updated configuration (version %d): %s", w.section, raw.Version, formatConfig(parsed))
	if w.onChange != nil {
		w.onChange(parsed)
	}
	return true
}

// formatConfig renders v as compact JSON for a log line — never itself a
// source of failure for the log call it backs: if v doesn't marshal for
// some reason, it falls back to a plain Go-syntax representation rather
// than dropping the message entirely.
func formatConfig[T any](v *T) string {
	if v == nil {
		return "<none>"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%+v", *v)
	}
	return string(raw)
}
