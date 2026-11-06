// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrMissingAuthHeader = errors.New("authorization header is missing")
	ErrInvalidAuthFormat = errors.New("authorization header format must be Bearer <token>")
)

// Config controls how a Verifier resolves signing keys and which
// registered claims it enforces.
type Config struct {
	// PublicKeyPath is the path to a PEM-encoded RSA public key, used to
	// verify tokens when JWKSURL is empty.
	PublicKeyPath string `json:"publicKeyPath"`

	// JWKSURL, if set, takes precedence over PublicKeyPath: keys are
	// fetched from this JWKS endpoint and kept fresh by a background
	// refresh goroutine (tied to the context passed to NewVerifier), with
	// per-token key selection by its "kid" header. This is what allows key
	// rotation without restarting the proxy.
	JWKSURL string `json:"jwksUrl"`

	// Issuer, if non-empty, is enforced against the token's "iss" claim.
	Issuer string `json:"issuer"`

	// Audience, if non-empty, is enforced against the token's "aud" claim.
	Audience string `json:"audience"`

	// ResourceURL is this proxy's own externally-reachable canonical MCP
	// URL (e.g. "https://go-proxy.example.com/mcp"), published as RFC 9728
	// Protected Resource Metadata's "resource" field so a generic MCP
	// client (one that doesn't already know which authorization server to
	// use, unlike this repo's own cli) can discover it from an
	// unauthenticated request's 401 response. Empty (default) means
	// Protected Resource Metadata isn't served at all, and an
	// unauthenticated request's 401 carries no resource_metadata hint —
	// today's behavior, unchanged.
	ResourceURL string `json:"resourceUrl,omitempty"`

	// RoleClaim, if non-empty, is the JWT claim name a caller's roles are
	// read from instead of the "role" claim access_control.RolesFromClaims
	// expects by default. Most real-world IdPs don't emit a claim literally
	// named "role" (Auth0 typically uses a namespaced custom claim,
	// Okta/Azure AD use "groups", etc.) — this lets a deployment normalize
	// whatever claim its IdP actually uses without changing any downstream
	// authorization code. Empty (default) preserves today's "role" claim
	// name.
	//
	// Worked example pointing this whole Config at a free Auth0 tenant
	// instead of demo-authorization-server (no code changes needed —
	// Auth0's JWKS endpoint, issuer, and a namespaced custom roles claim
	// added to the access token via an Auth0 Action all just plug in
	// here; see cli/main.go's usage text for the matching CLI-side
	// -audience flag a login needs to get a JWT, not opaque, access
	// token from Auth0):
	//
	//	"auth": {
	//	  "publicKeyPath": "",
	//	  "jwksUrl": "https://YOUR_TENANT.us.auth0.com/.well-known/jwks.json",
	//	  "issuer": "https://YOUR_TENANT.us.auth0.com/",
	//	  "audience": "https://go-proxy-api",
	//	  "roleClaim": "https://go-proxy/roles",
	//	  "resourceUrl": "http://localhost/mcp"
	//	}
	RoleClaim string `json:"roleClaim,omitempty"`
}

// Verifier parses and validates RS256 JWT bearer tokens against a
// configured key source (static public key or JWKS endpoint).
type Verifier struct {
	keyFunc jwt.Keyfunc
	options []jwt.ParserOption
}

// NewVerifier builds a Verifier from cfg. When cfg.JWKSURL is set, the
// initial key set is fetched synchronously so a misconfigured or
// unreachable JWKS endpoint fails fast at startup rather than on the first
// request; ctx also bounds the lifetime of the background refresh
// goroutine keyfunc starts for it.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
	keyFn, err := buildKeyFunc(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Verifier{keyFunc: keyFn, options: parserOptions(cfg.Issuer, cfg.Audience)}, nil
}

// NewVerifierFromKey builds a Verifier directly from an already-resolved
// RSA public key, skipping any file read or JWKS fetch entirely — for a
// caller that already holds the key in memory, namely the proxy verifying
// identity assertions it minted itself (see main.go's
// internalAssertionVerifier, built from internalSigner.PublicKey()).
// issuer/audience are enforced exactly as Config.Issuer/Audience would be;
// pass "" to skip either check.
func NewVerifierFromKey(pub *rsa.PublicKey, issuer, audience string) *Verifier {
	return &Verifier{
		keyFunc: func(*jwt.Token) (interface{}, error) { return pub, nil },
		options: parserOptions(issuer, audience),
	}
}

func parserOptions(issuer, audience string) []jwt.ParserOption {
	options := []jwt.ParserOption{
		jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
		jwt.WithExpirationRequired(),
	}
	if issuer != "" {
		options = append(options, jwt.WithIssuer(issuer))
	}
	if audience != "" {
		options = append(options, jwt.WithAudience(audience))
	}
	return options
}

func buildKeyFunc(ctx context.Context, cfg Config) (jwt.Keyfunc, error) {
	if cfg.JWKSURL != "" {
		k, err := keyfunc.NewDefaultCtx(ctx, []string{cfg.JWKSURL})
		if err != nil {
			return nil, fmt.Errorf("failed to load JWKS from %s: %w", cfg.JWKSURL, err)
		}
		return k.Keyfunc, nil
	}

	pub, err := loadPublicKey(cfg.PublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load public key from %s: %w", cfg.PublicKeyPath, err)
	}
	return func(*jwt.Token) (interface{}, error) { return pub, nil }, nil
}

// TokenFromHeader extracts the bearer token from authHeader and returns it
// only once it has been fully verified: signature, algorithm, expiry, and
// (if configured) issuer/audience.
func (v *Verifier) TokenFromHeader(authHeader string) (*jwt.Token, error) {
	if authHeader == "" {
		return nil, ErrMissingAuthHeader
	}

	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return nil, ErrInvalidAuthFormat
	}

	tokenString := strings.TrimSpace(parts[1])
	return jwt.Parse(tokenString, v.keyFunc, v.options...)
}

func loadPublicKey(path string) (*rsa.PublicKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read key file: %w", err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in %s", path)
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key at %s is not an RSA public key", path)
	}
	return pub, nil
}
