// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package oauthclient implements a minimal, provider-agnostic OAuth 2.1
// public-client flow: RFC 8414 metadata discovery, RFC 7591 dynamic client
// registration, PKCE, authorization_code exchange, and refresh. It's
// reusable by any CLI that needs to log a human in against a real OAuth
// authorization server — not specific to demo-authorization-server, though
// this package's direct-credential fast path (see credentials.go) only
// works there.
package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Metadata is the subset of RFC 8414 authorization server metadata this
// package understands.
type Metadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint,omitempty"`
	JWKSURI                       string   `json:"jwks_uri,omitempty"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported,omitempty"`
}

// DiscoverMetadata fetches <baseURL>/.well-known/oauth-authorization-server.
// An empty RegistrationEndpoint in the result means the server doesn't
// support dynamic client registration (RFC 8414 convention) — callers must
// then supply a pre-registered client_id instead.
func DiscoverMetadata(ctx context.Context, baseURL, issuer string) (*Metadata, error) {
	url := strings.TrimRight(baseURL, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build metadata request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch authorization server metadata from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("authorization server metadata request to %s returned %d", url, resp.StatusCode)
	}
	var meta Metadata
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		return nil, fmt.Errorf("failed to parse authorization server metadata: %w", err)
	}
	if meta.AuthorizationEndpoint == "" || meta.TokenEndpoint == "" {
		return nil, fmt.Errorf("authorization server metadata from %s is missing authorization_endpoint/token_endpoint", url)
	}
	// RFC 8414 section 3.3: the metadata must name the server it was asked
	// for - baseURL, or issuer when the caller reaches the server at
	// another address than its issuer. Otherwise a server could pass off
	// another's endpoints as its own (an authorization server mix-up),
	// sending the login - and the code - somewhere else.
	if issuer == "" {
		issuer = baseURL
	}
	if strings.TrimRight(meta.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, fmt.Errorf("authorization server metadata from %s names issuer %q, not %s; use the issuer's URL as the auth server, or pass the issuer explicitly (-auth-issuer)", url, meta.Issuer, issuer)
	}
	return &meta, nil
}
