// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"sync"

	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// featureRegistry is the non-generic surface Gateway drives each feature
// kind's (tools, prompts, and in future resources) lifecycle through
// uniformly in OnAdded/OnRemoved, without needing to know the concrete item
// type a given kind's genericRegistry[T] is instantiated with.
type featureRegistry interface {
	// Start begins polling attrs.ServerName for this feature kind.
	Start(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes)
	// Stop halts polling for serverName.
	Stop(serverName string)
	// RemoveServer unregisters everything this registry added to the
	// gateway server on behalf of serverName and forgets serverName's
	// bookkeeping.
	RemoveServer(serverName string)
	// PendingFirstPolls is how many servers haven't had their first poll
	// for this feature kind attempted yet.
	PendingFirstPolls() int
}

// registerFunc adds/replaces a single namespaced item T on the gateway
// server (e.g. wiring an AddTool or AddPrompt handler), fully encapsulating
// the per-kind handler closure.
type registerFunc[T any] func(namespacedName string, item T)

// removeFunc unregisters items by namespaced name (e.g.
// gatewayServer.RemoveTools/RemovePrompts).
type removeFunc func(names ...string)

// namespaceFunc rewrites item's name in-place to its namespaced form and
// returns the resulting namespaced name.
type namespaceFunc[T any] func(item T, serverName string) (namespacedName string)

// genericRegistry owns one FeaturePoller[T] plus the bookkeeping of which
// namespaced names are currently registered per backend server, and
// reconciles that against each poll's result. It implements featureRegistry
// so Gateway can drive many kinds uniformly.
type genericRegistry[T any] struct {
	mu         sync.Mutex
	kind       string // "tool", "prompt" — for logging only
	poller     *FeaturePoller[T]
	register   registerFunc[T]
	remove     removeFunc
	namespace  namespaceFunc[T]
	registered map[string][]string // serverName -> namespaced names
	items      map[string]T        // namespaced name -> current item, mirrors registered
}

func newGenericRegistry[T any](
	kind string,
	pollFrequencySeconds int,
	fetch FetchFunc[T],
	nameOf NameFunc[T],
	register registerFunc[T],
	remove removeFunc,
	namespace namespaceFunc[T],
) *genericRegistry[T] {
	r := &genericRegistry[T]{
		kind:       kind,
		register:   register,
		remove:     remove,
		namespace:  namespace,
		registered: make(map[string][]string),
		items:      make(map[string]T),
	}
	r.poller = NewFeaturePoller(pollFrequencySeconds, kind+"Poller", fetch, nameOf, r.reconcile)
	return r
}

func (r *genericRegistry[T]) Start(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) {
	r.poller.Start(ctx, attrs)
}

func (r *genericRegistry[T]) PendingFirstPolls() int {
	return r.poller.PendingFirstPolls()
}

func (r *genericRegistry[T]) Stop(serverName string) {
	r.poller.Stop(serverName)
}

func (r *genericRegistry[T]) RemoveServer(serverName string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if names, exists := r.registered[serverName]; exists {
		logging.Debugf("[Gateway] Removing %ss: %v", r.kind, names)
		r.remove(names...)
		for _, name := range names {
			delete(r.items, name)
		}
		delete(r.registered, serverName)
	}
}

// reconcile is passed as FeaturePoller[T]'s onChanged callback: it
// registers/replaces every item in items under its namespaced name, then
// removes any previously-registered namespaced name no longer present.
func (r *genericRegistry[T]) reconcile(serverName string, items []T) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := make(map[string]bool, len(items))
	for _, item := range items {
		namespacedName := r.namespace(item, serverName)
		current[namespacedName] = true

		logging.Debugf("[Gateway] Registering %s: %s", r.kind, namespacedName)
		r.register(namespacedName, item)
		r.items[namespacedName] = item
	}

	var stale []string
	for _, name := range r.registered[serverName] {
		if !current[name] {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		logging.Debugf("[Gateway] Removing stale %ss for %s: %v", r.kind, serverName, stale)
		r.remove(stale...)
		for _, name := range stale {
			delete(r.items, name)
		}
	}

	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	r.registered[serverName] = names
}

// Item returns the currently-registered item under namespacedName, if any.
func (r *genericRegistry[T]) Item(namespacedName string) (T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[namespacedName]
	return item, ok
}

// Items returns a snapshot of every currently-registered item across all
// backend servers this registry tracks.
func (r *genericRegistry[T]) Items() []T {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]T, 0, len(r.items))
	for _, item := range r.items {
		out = append(out, item)
	}
	return out
}
