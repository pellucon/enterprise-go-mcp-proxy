// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package discover_mcp_servers

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// DiscoveryFunc matches a function that queries current servers (like your DiscoverMCPs wrapper)
type DiscoveryFunc func() (ServerMap, error)

// OnChangeFunc handles subsets of diffed servers
type OnChangeFunc func(ServerMap)

// ServerMonitor coordinates state tracking and periodic polling of MCP servers
type ServerMonitor struct {
	frequency time.Duration
	numCycles int
	discover  DiscoveryFunc
	onAdded   OnChangeFunc
	onRemoved OnChangeFunc
	lastSeen  ServerMap
	// failures logs a failed discovery once rather than on every tick
	// while it keeps failing.
	failures problemLog
	// firstCheckDone is set once the initial baseline check has run (and
	// fired onAdded for every server it found), whether or not discovery
	// itself succeeded. See FirstCheckDone.
	firstCheckDone atomic.Bool
}

// FirstCheckDone reports whether Start's initial baseline check has
// completed. go-proxy's readiness check uses it, together with the
// gateway's first polls of the servers that check found, so the proxy only
// takes client traffic once its first discovery pass is done.
func (sm *ServerMonitor) FirstCheckDone() bool {
	return sm.firstCheckDone.Load()
}

// NewServerMonitor initializes the monitor struct with requirements
func NewServerMonitor(frequencySeconds int, discover DiscoveryFunc, onAdded OnChangeFunc, onRemoved OnChangeFunc) *ServerMonitor {
	return &ServerMonitor{
		frequency: time.Duration(frequencySeconds) * time.Second,
		discover:  discover,
		onAdded:   onAdded,
		onRemoved: onRemoved,
		lastSeen:  make(ServerMap),
	}
}

// Start runs the monitoring loop. It blocks until the passed context is canceled.
func (sm *ServerMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(sm.frequency)
	defer ticker.Stop()

	// Execute an initial baseline check immediately upon starting
	sm.checkAndDiff()
	sm.firstCheckDone.Store(true)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sm.checkAndDiff()
		}
	}
}

// checkAndDiff executes the discovery function and computes mutations
func (sm *ServerMonitor) checkAndDiff() {
	current, err := sm.discover()
	if err != nil {
		// Keep the last-seen set (and every server in it) rather than
		// treating a failed discovery as "everything was removed", but say
		// so: e.g. a namespace in k8sNamespaces go-proxy has no RBAC for
		// fails every tick until it's fixed.
		var problems problemList
		problems.errorf("discovery failed, keeping the last known servers: %v", err)
		sm.failures.report(problems)
		return
	}
	if sm.failures.active() {
		sm.failures.report(nil)
		logging.Info("discovery recovered")
	}

	// 1. Identify Added Servers: present in current but missing from
	// lastSeen. A server whose attributes changed (a new URL or credentials
	// Secret under the same name - e.g. a discovery config change, or a
	// different Service winning the name) counts as both removed and added,
	// so its old connections are torn down and new ones made to the new
	// address; diffing by name alone would keep using the old ones.
	added := make(ServerMap)
	removed := make(ServerMap)
	for name, attr := range current {
		prev, exists := sm.lastSeen[name]
		if !exists {
			added[name] = attr
		} else if *prev != *attr {
			removed[name] = prev
			added[name] = attr
		}
	}

	// 2. Identify Removed Servers: Present in lastSeen, but missing from current
	for name, attr := range sm.lastSeen {
		if _, exists := current[name]; !exists {
			removed[name] = attr
		}
	}

	// 3. Fire callbacks if state changes occurred. Removals go first, so a
	// changed server is fully torn down before it's added back.
	if len(removed) > 0 && sm.onRemoved != nil {
		sm.onRemoved(removed)
	}
	if len(added) > 0 && sm.onAdded != nil {
		sm.onAdded(added)
	}

	// 4. Update the state baseline for the next clock cycle
	sm.lastSeen = current
}
