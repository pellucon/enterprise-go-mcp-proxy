// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// token.go implements POST /token: the authorization_code exchange (with
// PKCE verification) and refresh_token rotation.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

const refreshTokenTTL = 24 * time.Hour

type refreshTokenRecord struct {
	Subject   string
	Roles     []string
	ClientID  string
	ExpiresAt time.Time
}

type refreshTokenStore struct {
	mu     sync.Mutex
	tokens map[string]refreshTokenRecord
}

func newRefreshTokenStore() *refreshTokenStore {
	return &refreshTokenStore{tokens: map[string]refreshTokenRecord{}}
}

func (s *refreshTokenStore) issue(rec refreshTokenRecord) (string, error) {
	token, err := randomOpaqueToken()
	if err != nil {
		return "", err
	}
	rec.ExpiresAt = time.Now().Add(refreshTokenTTL)
	s.mu.Lock()
	s.tokens[token] = rec
	s.mu.Unlock()
	return token, nil
}

// consume looks up and deletes token in one step, enforcing rotation: once
// a refresh token has been used, only the newly-issued replacement is
// valid — a leaked-then-replayed old token always misses afterward.
func (s *refreshTokenStore) consume(token string) (refreshTokenRecord, bool) {
	s.mu.Lock()
	rec, ok := s.tokens[token]
	delete(s.tokens, token)
	s.mu.Unlock()
	if !ok || time.Now().After(rec.ExpiresAt) {
		return refreshTokenRecord{}, false
	}
	return rec, true
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type tokenErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
}

func writeTokenError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(tokenErrorResponse{Error: code, ErrorDescription: description})
}

func handleToken(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			writeTokenError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
			return
		}

		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			handleAuthorizationCodeGrant(state, w, r)
		case "refresh_token":
			handleRefreshTokenGrant(state, w, r)
		default:
			writeTokenError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		}
	}
}

func handleAuthorizationCodeGrant(state *State, w http.ResponseWriter, r *http.Request) {
	code := r.PostForm.Get("code")
	redirectURI := r.PostForm.Get("redirect_uri")
	clientID := r.PostForm.Get("client_id")
	verifier := r.PostForm.Get("code_verifier")

	rec, ok := state.authCodes.consume(code)
	if !ok {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "unknown, expired, or already-used code")
		return
	}
	if rec.ClientID != clientID || rec.RedirectURI != redirectURI {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "client_id/redirect_uri do not match the authorization request")
		return
	}
	if !verifyPKCE(verifier, rec.CodeChallenge) {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match code_challenge")
		return
	}

	issueTokenPair(state, w, r, rec.Subject, rec.Roles, rec.ClientID)
}

func handleRefreshTokenGrant(state *State, w http.ResponseWriter, r *http.Request) {
	refreshToken := r.PostForm.Get("refresh_token")

	rec, ok := state.refreshTokens.consume(refreshToken)
	if !ok {
		writeTokenError(w, http.StatusBadRequest, "invalid_grant", "unknown, expired, or already-used refresh token")
		return
	}

	issueTokenPair(state, w, r, rec.Subject, rec.Roles, rec.ClientID)
}

func issueTokenPair(state *State, w http.ResponseWriter, r *http.Request, subject string, roles []string, clientID string) {
	issuer := state.issuer(r)
	audience := state.audience(r)
	accessToken, err := state.signer.mintAccessToken(subject, roles, issuer, audience)
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	refreshTok, err := state.refreshTokens.issue(refreshTokenRecord{Subject: subject, Roles: roles, ClientID: clientID})
	if err != nil {
		writeTokenError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		AccessToken:  accessToken,
		TokenType:    "Bearer",
		ExpiresIn:    int(accessTokenTTL.Seconds()),
		RefreshToken: refreshTok,
		Scope:        "",
	})
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}
