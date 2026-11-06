// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import "net/http"

// backendHTTPClient is used for every poller's short-lived backend
// connection. Keep-alives are disabled so each poll is a fresh TCP dial
// rather than one reused from Go's default per-host connection pool —
// otherwise consecutive polls of a backend Service with multiple pods could
// keep landing on the same pod via a pooled connection instead of getting
// an independent routing decision each time.
var backendHTTPClient = &http.Client{
	Transport: &http.Transport{DisableKeepAlives: true},
}
