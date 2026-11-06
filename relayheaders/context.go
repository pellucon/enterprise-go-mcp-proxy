// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package relayheaders

import (
	"context"
	"fmt"
	"net/http"
)

// MintFunc returns the relay headers for one outbound request from the
// proxy to backend server: tokens minted for that server specifically,
// carrying the identity of the caller whose request it is part of. go-proxy
// puts one on each request's context (WithMinter); ServerTransport calls it.
type MintFunc func(server string) (Set, error)

type minterContextKey struct{}

// WithMinter returns a copy of ctx carrying mint, for ServerTransport to
// call on every outbound request made with that context. The minter is
// per request, never stored on a shared client or session: a cached
// backend session is reused by concurrent calls from different callers.
func WithMinter(ctx context.Context, mint MintFunc) context.Context {
	return context.WithValue(ctx, minterContextKey{}, mint)
}

// minterFrom returns the MintFunc on ctx, or nil if there is none (e.g. a
// discovery poller's request, which acts for no caller).
func minterFrom(ctx context.Context) MintFunc {
	mint, _ := ctx.Value(minterContextKey{}).(MintFunc)
	return mint
}

// ServerTransport is the http.RoundTripper for go-proxy's connections to
// one backend server: on every request it attaches the relay headers the
// request context's MintFunc produces for Server, so each backend gets
// tokens minted for it (and only a backend allowed to relay gets a relay
// token). A request whose context has no MintFunc goes out without relay
// headers; a MintFunc error fails the request rather than sending it
// without the caller's identity. Base defaults to http.DefaultTransport.
type ServerTransport struct {
	Base   http.RoundTripper
	Server string
}

func (t *ServerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if mint := minterFrom(req.Context()); mint != nil {
		s, err := mint(t.Server)
		if err != nil {
			// A RoundTripper must close the request body, even on error.
			if req.Body != nil {
				req.Body.Close()
			}
			return nil, fmt.Errorf("relay headers for server %q: %w", t.Server, err)
		}
		if !s.Empty() {
			req = req.Clone(req.Context())
			s.Apply(req.Header)
		}
	}
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}
