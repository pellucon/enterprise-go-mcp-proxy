// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// clients.go implements RFC 7591 dynamic client registration, restricted
// to loopback-only redirect URIs (127.0.0.1/::1/localhost, http scheme
// only) as a deliberate anti-phishing mitigation: even a maliciously
// self-registered client can't have an authorization code delivered
// anywhere but the browser's own machine — see CLAUDE.md-style
// reasoning discussed for this feature (RFC 8252, "OAuth for Native
// Apps"): a remote attacker cannot receive traffic sent to a loopback
// address on someone else's machine.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"

	"github.com/google/uuid"
)

type oauthClient struct {
	ClientID     string
	ClientName   string
	RedirectURIs []string
}

type clientStore struct {
	mu      sync.Mutex
	clients map[string]oauthClient
}

func newClientStore() *clientStore {
	return &clientStore{clients: map[string]oauthClient{}}
}

func (s *clientStore) register(name string, redirectURIs []string) (oauthClient, error) {
	if len(redirectURIs) == 0 {
		return oauthClient{}, fmt.Errorf("redirect_uris must not be empty")
	}
	for _, raw := range redirectURIs {
		if err := validateLoopbackRedirectURI(raw); err != nil {
			return oauthClient{}, err
		}
	}

	c := oauthClient{
		ClientID:     uuid.NewString(),
		ClientName:   name,
		RedirectURIs: append([]string{}, redirectURIs...),
	}
	s.mu.Lock()
	s.clients[c.ClientID] = c
	s.mu.Unlock()
	return c, nil
}

func (s *clientStore) lookup(clientID string) (oauthClient, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[clientID]
	return c, ok
}

// validateLoopbackRedirectURI enforces http-scheme, loopback-host-only
// redirect URIs. This is the entire mitigation that makes open dynamic
// client registration safe here: a code delivered to a loopback address
// can only ever reach a process already running on the same machine as the
// browser that logged in, so a remotely-registered malicious client gains
// nothing by registering at all.
func validateLoopbackRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid redirect_uri %q: %w", raw, err)
	}
	if u.Scheme != "http" {
		return fmt.Errorf("redirect_uri %q must use the http scheme (loopback only)", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("redirect_uri %q must resolve to a loopback address (127.0.0.1, ::1, or localhost)", raw)
	}
	return nil
}

type registerRequest struct {
	RedirectURIs []string `json:"redirect_uris"`
	ClientName   string   `json:"client_name,omitempty"`
}

type registerErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

type registerResponse struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

func handleRegister(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req registerRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeRegisterError(w, "invalid_client_metadata", "invalid JSON body")
			return
		}
		if len(req.RedirectURIs) == 0 {
			writeRegisterError(w, "invalid_client_metadata", "redirect_uris must not be empty")
			return
		}
		client, err := state.clients.register(req.ClientName, req.RedirectURIs)
		if err != nil {
			writeRegisterError(w, "invalid_redirect_uri", err.Error())
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(registerResponse{
			ClientID:                client.ClientID,
			ClientName:              client.ClientName,
			RedirectURIs:            client.RedirectURIs,
			TokenEndpointAuthMethod: "none",
		})
	}
}

func writeRegisterError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(registerErrorResponse{Error: code, ErrorDescription: description})
}
