// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"sync/atomic"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// discoveryReadyTimeout caps how long /healthz reports "not ready" while
// waiting for the first discovery pass. A var so tests can shorten it.
var discoveryReadyTimeout = 30 * time.Second

// newDiscoveryReadiness returns the discovery half of /healthz: false until
// the proxy's first discovery pass is done - the server monitor's initial
// check has run, and every server it found has had its tools, prompts,
// resources and resource templates fetched (or attempted) once - then true
// for good.
//
// Why: the gateway advertises, and serves, whatever it has discovered so
// far. A client connecting in the first seconds after the proxy starts
// would otherwise get empty tool and prompt lists, and a client on a
// pre-2026-07-28 protocol never receives a list-changed notification to
// fetch them again (the stateless handler has no stream to send it on).
// Holding readiness back keeps Kubernetes from routing clients to a new
// replica until it has something to serve; during a rolling update the old
// replica keeps serving meanwhile.
//
// The timeout is a backstop: if the first pass takes longer (a very slow
// backend, a discovery error), the proxy becomes ready anyway rather than
// staying out of service. Once ready it never reports not-ready again for
// discovery reasons - later servers coming and going is normal operation.
func newDiscoveryReadiness(firstCheckDone func() bool, pendingFirstPolls func() int, timeout time.Duration) func() bool {
	start := time.Now()
	var ready atomic.Bool
	return func() bool {
		if ready.Load() {
			return true
		}
		if firstCheckDone() && pendingFirstPolls() == 0 {
			if ready.CompareAndSwap(false, true) {
				logging.Infof("discovery: first pass complete after %s, ready for clients", time.Since(start).Round(time.Millisecond))
			}
			return true
		}
		if time.Since(start) >= timeout {
			if ready.CompareAndSwap(false, true) {
				logging.Warnf("discovery: first pass not complete after %s (%d first polls pending); becoming ready anyway", timeout, pendingFirstPolls())
			}
			return true
		}
		return false
	}
}
