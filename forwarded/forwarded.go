// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package forwarded derives the externally visible base URL of an incoming
// request, honoring the de-facto-standard X-Forwarded-Proto/X-Forwarded-Host
// headers a TLS-terminating L7 reverse proxy (ingress-nginx, Traefik) adds.
// Used by go-proxy for its RFC 9728 resource_metadata hint (main.go's
// protectedResourceMetadataURL); a separate package so the logic has its
// own focused unit test and is importable by any other server that needs it.
package forwarded

import (
	"net/http"
	"strings"
)

// BaseURL returns "<scheme>://<host>" for r as the client saw it. Behind an
// L7 proxy r.TLS is nil and r.Host may be rewritten, so X-Forwarded-Proto/
// X-Forwarded-Host take precedence when present (the first value of a
// comma-separated chain: the one the first proxy saw. When proxies append
// to the header rather than replace it, that first value is whatever the
// client itself sent, so it's only as trustworthy as the client - see
// below). When
// they're absent — a direct connection, or an L4 load balancer like k3s's
// klipper — it's exactly the request-derived URL.
//
// The headers are client-controllable, so only use the result for values
// echoed back to that same client (a discovery hint, endpoint URLs in a
// metadata document), never for an authorization decision.
func BaseURL(r *http.Request) string {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	if proto := firstValue(r.Header.Get("X-Forwarded-Proto")); proto != "" {
		scheme = proto
	}
	host := r.Host
	if fwdHost := firstValue(r.Header.Get("X-Forwarded-Host")); fwdHost != "" {
		host = fwdHost
	}
	return scheme + "://" + host
}

// firstValue returns the first entry of a comma-separated X-Forwarded-*
// header value, trimmed; "" if the header is absent.
func firstValue(v string) string {
	first, _, _ := strings.Cut(v, ",")
	return strings.TrimSpace(first)
}
