// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// keys.go mints and verifies demo-authorization-server's own access
// tokens. It deliberately doesn't reuse go-proxy's auth.Signer: Signer
// never sets a JWT "kid" header (needed here so JWKS-based verification
// can select this server's key), and auth.NewSigner only loads a private
// key from a PEM file path, not an in-memory-generated one — this server
// generates a fresh RSA keypair every time it starts, with no key
// material ever written to disk (every restart invalidates every
// previously-issued token, matching its "demo" framing).
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"fmt"
	"math/big"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// signingKeyID is fixed since this server only ever holds one signing key
// at a time (regenerated, never rotated, on each restart).
const signingKeyID = "demo-authorization-server-key-1"

const accessTokenTTL = 12 * time.Hour

type tokenSigner struct {
	privateKey *rsa.PrivateKey
	kid        string
	ttl        time.Duration
	// roleClaim is the JWT claim name minted tokens carry their roles
	// under — "role" unless overridden (see newTokenSigner), matching
	// go-proxy's auth.Config.RoleClaim: this server is meant to stand in
	// for a real third-party IdP, and most of those don't emit roles
	// under a claim literally named "role", so this lets a deployment
	// exercise go-proxy's roleClaim remapping end to end instead of only
	// against a synthetic test token.
	roleClaim string
}

func newTokenSigner(roleClaim string) (*tokenSigner, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("failed to generate signing key: %w", err)
	}
	if roleClaim == "" {
		roleClaim = "role"
	}
	return &tokenSigner{privateKey: key, kid: signingKeyID, ttl: accessTokenTTL, roleClaim: roleClaim}, nil
}

// mintAccessToken signs an RS256 JWT asserting sub/roles, valid for the
// signer's TTL. issuer/audience are passed in per-call (rather than fixed
// at construction) so they always match the request's own Host — see
// (*State).issuer — regardless of which port the server ends up listening
// on. Claim shape mirrors auth.Signer.Sign's exactly (sub, role as a JSON
// array under s.roleClaim, iss, aud, iat, exp) so a resource server already
// configured to accept auth.Signer-minted tokens needs no changes to also
// accept these when s.roleClaim is left at its "role" default — see
// access_control.RolesFromClaims, which parses "role" as either a bare
// string or a JSON array.
func (s *tokenSigner) mintAccessToken(sub string, roles []string, issuer, audience string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":       sub,
		s.roleClaim: roles,
		"iss":       issuer,
		"aud":       audience,
		"iat":       now.Unix(),
		"exp":       now.Add(s.ttl).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.kid
	return token.SignedString(s.privateKey)
}

// jwk is one RSA public key in RFC 7518 §6.3.1 JSON Web Key shape.
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
}

func (s *tokenSigner) jwks() jwksDocument {
	pub := s.privateKey.PublicKey
	return jwksDocument{
		Keys: []jwk{{
			Kty: "RSA",
			Use: "sig",
			Alg: "RS256",
			Kid: s.kid,
			N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}},
	}
}
