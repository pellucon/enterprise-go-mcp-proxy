// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// server.go assembles demo-authorization-server's State (every in-memory
// store plus the token signer) and its HTTP mux. See main.go for flag
// parsing and startup.
package main

import "net/http"

// State bundles everything every handler needs. By default issuer is
// computed per-request from the request's own Host (see (*State).issuer)
// rather than fixed at construction time, so it's always correct
// regardless of which port the server ends up listening on — this matters
// for oauth_flow_test.go, which uses httptest.NewServer's OS-assigned
// port. issuerOverride pins a stable value instead, for deployments where
// a resource server's config checks a fixed auth.issuer. audienceOverride
// is separate and optional: unset, minted tokens' "aud" claim just equals
// whatever "iss" resolves to (this server's original, single-value
// behavior); set, it names the actual resource server a token is intended
// for (e.g. "go-proxy"), distinct from this authorization server's own
// identity — see (*State).audience.
type State struct {
	users         *userStore
	clients       *clientStore
	authCodes     *authCodeStore
	refreshTokens *refreshTokenStore
	signer        *tokenSigner

	issuerOverride   string
	audienceOverride string
}

// roleClaim is passed straight through to newTokenSigner — empty defaults
// to "role" there, matching this server's original behavior.
func newState(issuerOverride, audienceOverride, roleClaim string) (*State, error) {
	signer, err := newTokenSigner(roleClaim)
	if err != nil {
		return nil, err
	}
	return &State{
		users:            newUserStore(),
		clients:          newClientStore(),
		authCodes:        newAuthCodeStore(),
		refreshTokens:    newRefreshTokenStore(),
		signer:           signer,
		issuerOverride:   issuerOverride,
		audienceOverride: audienceOverride,
	}, nil
}

// issuer returns issuerOverride if set, else the dynamic per-request base
// URL. Used for the metadata document's "issuer" field and for the "iss"
// claim baked into minted tokens (token.go's issueTokenPair) — never for
// the endpoint URLs a caller actually has to dereference, which must
// always come from baseURL below regardless of any override, since
// different callers reach this server at different addresses (e.g. a
// host-side caller at localhost:8102, an in-cluster pod at
// host.docker.internal:8102) while only the issuer identity string itself
// needs to be one value both sides agree to check — it doesn't need to be
// a fetchable URL.
func (s *State) issuer(r *http.Request) string {
	if s.issuerOverride != "" {
		return s.issuerOverride
	}
	return s.baseURL(r)
}

// audience returns audienceOverride if set, else falls back to issuer(r)
// — matching this server's original behavior, where "aud" was never a
// distinct concept from "iss". Used only for the "aud" claim baked into
// minted tokens (token.go's issueTokenPair). Real OAuth resource-server
// hygiene wants this to name the resource server the token is for (e.g.
// "go-proxy"), not this authorization server itself — an "aud" that
// always equals "iss" gives the audience check no actual ability to
// distinguish which resource server a token was meant for, defeating its
// anti-replay purpose the moment more than one resource server trusts
// this authorization server's keys.
func (s *State) audience(r *http.Request) string {
	if s.audienceOverride != "" {
		return s.audienceOverride
	}
	return s.issuer(r)
}

// baseURL is always derived from the incoming request's own Host, never
// overridden — used to build the metadata document's actual
// dereferenceable endpoint URLs (authorization_endpoint, token_endpoint,
// registration_endpoint, jwks_uri).
func (s *State) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func newHandler(state *State) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", handleMetadata(state))
	mux.HandleFunc("GET /.well-known/jwks.json", handleJWKS(state))
	mux.HandleFunc("POST /register", handleRegister(state))
	mux.HandleFunc("GET /authorize", handleAuthorizeGet(state))
	mux.HandleFunc("POST /authorize", handleAuthorizePost(state))
	mux.HandleFunc("POST /token", handleToken(state))
	mux.HandleFunc("POST /admin/users", handleAdminUsersPost(state))
	mux.HandleFunc("GET /admin/users", handleAdminUsersGet(state))
	mux.HandleFunc("DELETE /admin/users/{username}", handleAdminUsersDelete(state))

	return mux
}
