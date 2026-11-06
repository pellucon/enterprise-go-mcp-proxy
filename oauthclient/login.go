// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"context"
	"fmt"
	"io"
	"time"
)

// LoginOptions configures Login.
type LoginOptions struct {
	// AuthServerURL is the OAuth authorization server's base URL (its
	// RFC 8414 metadata is fetched from
	// <AuthServerURL>/.well-known/oauth-authorization-server).
	AuthServerURL string

	// ClientID, if set, skips dynamic client registration and uses this
	// pre-registered client_id directly — required for providers that
	// don't support RFC 7591 DCR (most real-world providers). If set,
	// RedirectPort should also be set to whatever fixed port that
	// client's redirect_uri was registered with.
	ClientID string

	// Issuer, if set, is the issuer the server's metadata must name, when
	// that differs from AuthServerURL: a server reached at another address
	// than its public one, e.g. demo-authorization-server through its
	// in-cluster Service from the gardening demo's seed Job, while its
	// issuer stays http://localhost:8102. Default: AuthServerURL itself.
	Issuer string

	// Audience, if set, is sent as the "audience" query parameter on the
	// authorization request — required by Auth0 (and any other provider
	// using the same convention) to receive a signed JWT access token
	// instead of an opaque one scoped to the provider's own userinfo
	// endpoint. Omit for providers that don't use it (e.g.
	// demo-authorization-server, which ignores it). Only meaningful for
	// the browser login path — the credential fast path only works
	// against demo-authorization-server anyway.
	Audience string

	// Scope, if set, is sent as the space-separated "scope" query
	// parameter on the authorization request — required by providers
	// that reject a scope-less request, e.g. an Okta custom authorization
	// server (typically "openid" plus a custom scope its access policy
	// grants). Empty (default) sends no scope parameter at all, exactly
	// as before. Like Audience, only meaningful for the browser login path.
	Scope string

	// ClientName is used only when dynamically registering a new client
	// (ClientID empty). Defaults to "oauthclient" if empty.
	ClientName string

	// RedirectPort is the local loopback port to bind for the OAuth
	// redirect. 0 (default) picks an OS-assigned ephemeral port —
	// appropriate whenever a new client is being dynamically registered,
	// since the exact port is only decided at registration time. Set
	// this only alongside a pre-existing ClientID that was registered
	// against one specific, fixed redirect_uri.
	RedirectPort int

	// Username/Password, if both set, select the direct-credential login
	// path instead of the browser — see loginViaCredentials's doc
	// comment for why this only works against demo-authorization-server.
	Username string
	Password string

	// Timeout bounds how long the browser login path waits for the user
	// to complete login. 0 defaults to DefaultLoginTimeout. Unused by
	// the credential path.
	Timeout time.Duration

	// Output is where the browser login path prints the authorization
	// URL. nil defaults to os.Stderr.
	Output io.Writer
}

// Login performs a full OAuth 2.1 authorization_code + PKCE login against
// the authorization server at opts.AuthServerURL, returning the resulting
// token pair. See LoginOptions for the two login paths (browser vs. direct
// credentials).
func Login(ctx context.Context, opts LoginOptions) (*TokenResponse, error) {
	meta, err := DiscoverMetadata(ctx, opts.AuthServerURL, opts.Issuer)
	if err != nil {
		return nil, err
	}

	pkce, err := newPKCEPair()
	if err != nil {
		return nil, err
	}
	state, err := randomState()
	if err != nil {
		return nil, err
	}

	ln, err := bindLoopbackListener(opts.RedirectPort)
	if err != nil {
		return nil, err
	}
	// Always closed on the way out: the credential path never actually
	// accepts a connection on it (no real callback arrives there — the
	// code comes back directly in the credential POST's redirect
	// response instead, see loginViaCredentials), and the browser path's
	// own graceful Shutdown already closes it before returning, making
	// this a harmless redundant close in that case.
	defer func() { _ = ln.Close() }()
	redirectURI := redirectURIFromListener(ln)

	clientID := opts.ClientID
	if clientID == "" {
		if meta.RegistrationEndpoint == "" {
			return nil, fmt.Errorf("no -client-id given and %s does not support dynamic client registration (no registration_endpoint in its metadata)", opts.AuthServerURL)
		}
		clientName := opts.ClientName
		if clientName == "" {
			clientName = "oauthclient"
		}
		clientID, err = registerClient(ctx, meta.RegistrationEndpoint, redirectURI, clientName)
		if err != nil {
			return nil, err
		}
	}

	var code string
	if opts.Username != "" && opts.Password != "" {
		code, err = loginViaCredentials(ctx, credentialLoginParams{
			AuthorizationEndpoint: meta.AuthorizationEndpoint,
			ClientID:              clientID,
			RedirectURI:           redirectURI,
			Challenge:             pkce.challenge,
			State:                 state,
			Username:              opts.Username,
			Password:              opts.Password,
		})
	} else {
		code, err = loginViaBrowser(ctx, browserLoginParams{
			Listener:              ln,
			AuthorizationEndpoint: meta.AuthorizationEndpoint,
			ClientID:              clientID,
			RedirectURI:           redirectURI,
			Challenge:             pkce.challenge,
			State:                 state,
			Audience:              opts.Audience,
			Scope:                 opts.Scope,
			Timeout:               opts.Timeout,
			Output:                opts.Output,
		})
	}
	if err != nil {
		return nil, err
	}

	return exchangeCode(ctx, meta.TokenEndpoint, code, pkce.verifier, clientID, redirectURI)
}
