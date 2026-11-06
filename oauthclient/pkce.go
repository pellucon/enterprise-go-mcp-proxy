// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// pkcePair is a PKCE (RFC 7636) code_verifier/code_challenge pair. S256
// only — this package never falls back to the "plain" method.
type pkcePair struct {
	verifier  string
	challenge string
}

func newPKCEPair() (pkcePair, error) {
	verifier, err := randomToken()
	if err != nil {
		return pkcePair{}, fmt.Errorf("failed to generate PKCE verifier: %w", err)
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkcePair{verifier: verifier, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}, nil
}

// randomToken returns 32 random bytes, base64url-encoded — used both as a
// PKCE verifier and (see state.go) as the CSRF "state" value.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
