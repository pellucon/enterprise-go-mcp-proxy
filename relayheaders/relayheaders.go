// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package relayheaders is the single place that knows the X-Go-Proxy-*
// HTTP headers go-proxy uses to carry per-call context from the proxy to a
// backend, and — for a backend relaying a call back into the proxy — back
// again: the internal identity assertion, the relay token, the proxy's own
// callback URL, and the tool-call correlation ID. See
// docs/architecture/auth.md's "Internal identity assertions" and
// "Tool-call correlation ID" sections.
//
// Before this package existed, the header names, the "attach whichever are
// present to an outbound request" RoundTripper, and the
// verify-the-assertion-and-get-claims logic were each hand-duplicated
// across go-proxy itself and every sample backend that participates in the
// relay mechanism (customer-service, company-service, misc-demo-server,
// ai-powered-tools) because those backends are separate package-main
// binaries that can't import go-proxy's own package main. They can all
// import this package instead, since it isn't package main. Every caller
// that reads, forwards, or verifies one of these headers should go through
// here rather than hardcoding the header name or hand-rolling a
// RoundTripper again.
package relayheaders

import (
	"context"
	"fmt"
	"net/http"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// AssertionHeader carries go-proxy's internal identity assertion
	// (see auth.Signer). Formatted as "Bearer <token>", same as the
	// client-facing Authorization header, so a backend can verify it via
	// auth.Verifier.TokenFromHeader unchanged — see VerifyAssertion below.
	// It only tells a backend who the caller is: the proxy never accepts it
	// back as a credential (see RelayTokenHeader).
	AssertionHeader = "X-Go-Proxy-Assertion"

	// RelayTokenHeader carries the token a backend presents back to the
	// proxy to relay a call as the original client ("Bearer <token>", see
	// auth.RelayAudience). The proxy sends it, together with
	// CallbackURLHeader, only to a backend its config allows to relay.
	RelayTokenHeader = "X-Go-Proxy-Relay-Token"

	// CallbackURLHeader carries the proxy's own callable MCP endpoint
	// address, for a backend allowed to relay to dial back into.
	CallbackURLHeader = "X-Go-Proxy-Callback-URL"

	// ToolCallIDHeader carries the per-tool-call correlation UUID assigned
	// (or, for a relayed call, reused) at the first hop, so every backend
	// and triggered-tool call stemming from one client tools/call can be
	// correlated by this one ID.
	ToolCallIDHeader = "X-Go-Proxy-Tool-Call-Id"
)

// Set is the subset of the relay headers present on one call. Assertion
// and RelayToken are the raw header values — already formatted
// "Bearer <token>" — ready to set verbatim on an outbound request or hand
// to VerifyAssertion.
type Set struct {
	Assertion   string
	RelayToken  string
	CallbackURL string
	ToolCallID  string
}

// Extract reads the relay headers off h — an inbound MCP request's
// req.GetExtra().Header, or a plain *http.Request's Header.
func Extract(h http.Header) Set {
	return Set{
		Assertion:   h.Get(AssertionHeader),
		RelayToken:  h.Get(RelayTokenHeader),
		CallbackURL: h.Get(CallbackURLHeader),
		ToolCallID:  h.Get(ToolCallIDHeader),
	}
}

// Empty reports whether none of s's fields are set.
func (s Set) Empty() bool {
	return s.Assertion == "" && s.RelayToken == "" && s.CallbackURL == "" && s.ToolCallID == ""
}

// Apply sets whichever of s's fields are non-empty onto h, leaving h
// untouched for any field left empty.
func (s Set) Apply(h http.Header) {
	if s.Assertion != "" {
		h.Set(AssertionHeader, s.Assertion)
	}
	if s.RelayToken != "" {
		h.Set(RelayTokenHeader, s.RelayToken)
	}
	if s.CallbackURL != "" {
		h.Set(CallbackURLHeader, s.CallbackURL)
	}
	if s.ToolCallID != "" {
		h.Set(ToolCallIDHeader, s.ToolCallID)
	}
}

// SourceFunc produces the Set to attach to one outbound request, given that
// request's context. Transport calls it once per RoundTrip.
type SourceFunc func(ctx context.Context) Set

// Static returns a SourceFunc that always returns s, ignoring ctx — for a
// caller relaying one already-captured Set on every call it makes (e.g. a
// backend tool relaying a call back into the proxy after reading the
// headers it was itself forwarded). go-proxy's own proxy-to-backend
// forwarding uses ServerTransport instead, which mints a Set per request.
func Static(s Set) SourceFunc {
	return func(context.Context) Set { return s }
}

// Transport is an http.RoundTripper that attaches Source(req.Context()) to
// every outbound request, cloning the request only when there's actually
// something to attach. Base defaults to http.DefaultTransport if nil.
type Transport struct {
	Base   http.RoundTripper
	Source SourceFunc
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	s := t.Source(req.Context())
	if !s.Empty() {
		req = req.Clone(req.Context())
		s.Apply(req.Header)
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// RoundTripper returns an http.RoundTripper that attaches s to every
// outbound request it makes — the common case for a backend relaying one
// already-captured Set on a single *http.Client/session, without needing to
// thread it through context (see Static). base defaults to
// http.DefaultTransport if nil.
func (s Set) RoundTripper(base http.RoundTripper) http.RoundTripper {
	return &Transport{Base: base, Source: Static(s)}
}

// Client returns an *http.Client that presents s on every request it
// makes. Convenience wrapper around RoundTripper for the common
// mcp.StreamableClientTransport{HTTPClient: ...} call site.
func (s Set) Client(base http.RoundTripper) *http.Client {
	return &http.Client{Transport: s.RoundTripper(base)}
}

// ClientFor extracts every relay header present on h and returns an
// *http.Client that forwards them all, unchanged, on every request it
// makes — the standard one-call shape for a backend relaying a call back
// into the proxy: present exactly what you were given on the inbound
// request, verbatim, rather than picking out individual fields (Assertion,
// CallbackURL, ToolCallID, and whatever else this Set may hold in the
// future) by hand. A caller that only cares about validating/using one or
// two fields (e.g. reading CallbackURL as the dial target) should still
// call Extract itself for that, but should build its outbound client from
// the same Extract result via .Client(base) — or, when there's no other use
// for the extracted Set, via this shortcut — rather than constructing a new
// Set literal naming only the fields it happens to know about today.
func ClientFor(h http.Header, base http.RoundTripper) *http.Client {
	return Extract(h).Client(base)
}

// VerifyAssertion extracts AssertionHeader from h and verifies it with
// verifier, returning the verified claims. Fails closed: a nil verifier
// (assertion verification not configured on this server) or any
// verification error (missing header, bad signature, wrong
// issuer/audience, expired) is reported as an error, never silently
// treated as "no restriction". Shared by go-proxy's own relay path
// (middleware.go) and every backend that checks the assertion itself to
// enforce its own resource-level authorization. A backend that only wants
// assertions minted for itself verifies with Audience "<audience>/<its
// server name>" (see auth.Signer.SignAssertion).
func VerifyAssertion(verifier *auth.Verifier, h http.Header) (jwt.MapClaims, error) {
	if verifier == nil {
		return nil, fmt.Errorf("internal assertion verification not configured")
	}
	token, err := verifier.TokenFromHeader(h.Get(AssertionHeader))
	if err != nil {
		return nil, fmt.Errorf("invalid internal identity assertion: %w", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, fmt.Errorf("internal identity assertion has unexpected claims type")
	}
	return claims, nil
}
