// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

const pollAttemptTimeout = 10 * time.Second

// FetchFunc retrieves the current list of items of type T from a backend
// server, e.g. by opening a short-lived connection and calling
// session.ListTools or session.ListPrompts.
type FetchFunc[T any] func(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]T, error)

// NameFunc extracts an item's stable identifying name, used to sort items
// before fingerprinting so two polls can be compared regardless of the
// order the backend returned them in.
type NameFunc[T any] func(item T) string

// ChangedFunc is invoked whenever a poll detects that a backend server's
// item list has changed since the last poll.
type ChangedFunc[T any] func(serverName string, items []T)

// FeaturePoller periodically opens a brand-new, short-lived connection to
// each known backend server, fetches its items via fetch, and invokes
// onChanged when the result differs from what was last seen. No connection
// is ever held open between polls, so backend servers never need to
// maintain session state on our account. Generic over the feature kind
// (tools, prompts, and in future resources) so this poll-loop/lifecycle/
// fingerprint-diff logic isn't duplicated per kind.
type FeaturePoller[T any] struct {
	mu        sync.Mutex
	frequency time.Duration
	fetch     FetchFunc[T]
	nameOf    NameFunc[T]
	onChanged ChangedFunc[T]
	logPrefix string                        // e.g. "ToolPoller", "PromptPoller" — for log messages only
	cancels   map[string]context.CancelFunc // serverName -> cancel for its polling goroutine
	lastSeen  map[string]string             // serverName -> fingerprint of last-registered item set
	// firstPolled records servers whose first poll has been attempted
	// (successfully or not). See PendingFirstPolls.
	firstPolled map[string]bool
}

// NewFeaturePoller creates a poller that checks each server's item list
// every frequencySeconds, using fetch to retrieve items and nameOf to
// identify them for fingerprinting. logPrefix is used only for log messages.
func NewFeaturePoller[T any](frequencySeconds int, logPrefix string, fetch FetchFunc[T], nameOf NameFunc[T], onChanged ChangedFunc[T]) *FeaturePoller[T] {
	return &FeaturePoller[T]{
		frequency:   time.Duration(frequencySeconds) * time.Second,
		fetch:       fetch,
		nameOf:      nameOf,
		onChanged:   onChanged,
		logPrefix:   logPrefix,
		cancels:     make(map[string]context.CancelFunc),
		lastSeen:    make(map[string]string),
		firstPolled: make(map[string]bool),
	}
}

// Start begins polling attrs.ServerName on the poller's interval, unless a
// poll for that server name is already running.
func (p *FeaturePoller[T]) Start(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) {
	if attrs == nil || attrs.ServerName == "" {
		return
	}

	p.mu.Lock()
	if _, exists := p.cancels[attrs.ServerName]; exists {
		p.mu.Unlock()
		return
	}
	pollCtx, cancel := context.WithCancel(ctx)
	p.cancels[attrs.ServerName] = cancel
	p.mu.Unlock()

	go p.run(pollCtx, attrs)
}

// Stop halts polling for serverName and clears its last-seen fingerprint, so
// a later re-Start always re-registers a fresh baseline instead of being
// suppressed by stale state.
func (p *FeaturePoller[T]) Stop(serverName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if cancel, exists := p.cancels[serverName]; exists {
		cancel()
		delete(p.cancels, serverName)
	}
	delete(p.lastSeen, serverName)
	delete(p.firstPolled, serverName)
}

// PendingFirstPolls returns how many polled servers haven't had their first
// poll attempted yet. A failed attempt counts as done: the point is that
// the gateway has tried each known server once, not that every server is
// healthy - one unreachable backend mustn't hold the proxy back.
func (p *FeaturePoller[T]) PendingFirstPolls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	pending := 0
	for name := range p.cancels {
		if !p.firstPolled[name] {
			pending++
		}
	}
	return pending
}

func (p *FeaturePoller[T]) run(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) {
	p.pollOnce(ctx, attrs)
	// ctx.Err() guards a Stop (and possible re-Start) racing this first
	// poll: a cancelled run must not mark a newer run's server as polled.
	p.mu.Lock()
	if _, polling := p.cancels[attrs.ServerName]; polling && ctx.Err() == nil {
		p.firstPolled[attrs.ServerName] = true
	}
	p.mu.Unlock()

	ticker := time.NewTicker(p.frequency)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.pollOnce(ctx, attrs)
		}
	}
}

func (p *FeaturePoller[T]) pollOnce(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) {
	attemptCtx, cancel := context.WithTimeout(ctx, pollAttemptTimeout)
	defer cancel()

	items, err := p.fetch(attemptCtx, attrs)
	if err != nil {
		// Non-fatal and expected in the ordinary course of things — e.g. a
		// backend pod that hasn't finished starting yet — so this is a
		// Warning, not an Error: the next tick (this poller's own
		// frequencySeconds later) retries automatically, no operator action
		// needed unless it never stops recurring for a given server.
		logging.Warnf("[%s] Failed to list items for server %s: %v — will keep trying", p.logPrefix, attrs.ServerName, err)
		return
	}

	fingerprint, err := p.fingerprint(items)
	if err != nil {
		logging.Warnf("[%s] Failed to fingerprint items for server %s: %v — will keep trying", p.logPrefix, attrs.ServerName, err)
		return
	}

	p.mu.Lock()
	prev, existed := p.lastSeen[attrs.ServerName]
	changed := !existed || prev != fingerprint
	if changed {
		p.lastSeen[attrs.ServerName] = fingerprint
	}
	p.mu.Unlock()

	if changed {
		p.onChanged(attrs.ServerName, items)
	}
}

// fingerprint produces a stable string representation of items so two polls
// can be compared for equality regardless of the order fetch returned them
// in. It must run on the raw items from fetch, before any downstream
// mutation (e.g. Gateway's namespacing) happens.
func (p *FeaturePoller[T]) fingerprint(items []T) (string, error) {
	sorted := slices.Clone(items)
	slices.SortFunc(sorted, func(a, b T) int { return strings.Compare(p.nameOf(a), p.nameOf(b)) })
	b, err := json.Marshal(sorted)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// collectAllPages drains one of the SDK's list iterators (ClientSession.Tools,
// Prompts, Resources, ResourceTemplates), which follow nextCursor page by
// page until the backend's last page. A single List* call would only return
// the first page, silently dropping everything a paginating backend puts on
// later pages. A backend that keeps returning cursors forever is bounded by
// the poll's own pollAttemptTimeout, and one that returns more than
// maxItemsPerServer items is refused (the poll fails, keeping what was last
// registered), so a misbehaving or malicious backend can't make go-proxy
// hold - and register - an unbounded list.
func collectAllPages[T any](items iter.Seq2[T, error]) ([]T, error) {
	var all []T
	for item, err := range items {
		if err != nil {
			return nil, err
		}
		if len(all) == maxItemsPerServer {
			return nil, fmt.Errorf("the server lists more than %d items of this kind", maxItemsPerServer)
		}
		all = append(all, item)
	}
	return all, nil
}

// maxItemsPerServer bounds how many tools (or prompts, resources, resource
// templates) one backend may list - far beyond any real server's.
const maxItemsPerServer = 10000
