// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"testing"
	"time"
)

func TestDiscoveryReadiness(t *testing.T) {
	t.Run("not ready until the first check is done and no first polls are pending", func(t *testing.T) {
		checked, pending := false, 2
		ready := newDiscoveryReadiness(func() bool { return checked }, func() int { return pending }, time.Minute)
		if ready() {
			t.Fatal("ready before the monitor's first check")
		}
		checked = true
		if ready() {
			t.Fatal("ready with first polls still pending")
		}
		pending = 0
		if !ready() {
			t.Fatal("not ready after the first discovery pass")
		}
		// Once ready, later discovery activity doesn't flip it back.
		pending = 3
		if !ready() {
			t.Fatal("became not-ready again after a new server appeared")
		}
	})

	t.Run("becomes ready after the timeout even if the pass is incomplete", func(t *testing.T) {
		ready := newDiscoveryReadiness(func() bool { return false }, func() int { return 1 }, 20*time.Millisecond)
		if ready() {
			t.Fatal("ready immediately")
		}
		time.Sleep(30 * time.Millisecond)
		if !ready() {
			t.Fatal("not ready after the timeout")
		}
	})
}
