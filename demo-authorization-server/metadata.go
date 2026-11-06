// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// metadata.go serves the two discovery documents an OAuth client needs to
// find this server's endpoints and verify its tokens: RFC 8414
// authorization-server metadata and this server's JWKS.
package main

import (
	"encoding/json"
	"net/http"
)

type authServerMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	JWKSURI                           string   `json:"jwks_uri"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
}

func handleMetadata(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// issuer may be a fixed override, unrelated to how this request
		// actually reached the server; base is always this request's own
		// reachable address, and is what the endpoint URLs below must be
		// built from — see (*State).issuer/baseURL's doc comments.
		issuer := state.issuer(r)
		base := state.baseURL(r)
		meta := authServerMetadata{
			Issuer:                            issuer,
			AuthorizationEndpoint:             base + "/authorize",
			TokenEndpoint:                     base + "/token",
			RegistrationEndpoint:              base + "/register",
			JWKSURI:                           base + "/.well-known/jwks.json",
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			TokenEndpointAuthMethodsSupported: []string{"none"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	}
}

func handleJWKS(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.signer.jwks())
	}
}
