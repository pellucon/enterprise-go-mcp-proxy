// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
)

// DefaultInternalIssuer and DefaultInternalAudience are the iss/aud values
// used for the proxy's internal identity assertions (see Signer). They are
// shared constants — rather than each side hardcoding its own string — so
// the proxy (minting) and a backend's Verifier (checking, via auth.Config's
// Issuer/Audience) can't silently drift apart.
const (
	DefaultInternalIssuer   = "go-proxy"
	DefaultInternalAudience = "go-proxy-backend"
)

// defaultInternalTTLSeconds is used when SignerConfig.TTLSeconds is zero.
// Kept short: this token is minted fresh on every backend call, so it only
// needs to survive the single in-flight request.
const defaultInternalTTLSeconds = 60

// internalKeyBits is the RSA key size used whenever this package generates
// its own internal-signing keypair (GenerateKey) — there's no PEM file
// dictating this anymore, so it has to live somewhere.
const internalKeyBits = 2048

// SignerConfig configures a Signer's minted-assertion claims. It carries no
// key material or key path: the proxy always generates (or, across
// replicas, coordinates via a k8s Secret — see main.go's
// bootstrapInternalSigningKey) its own internal signing key in memory, and
// a backend verifies it via Verifier's JWKSURL against the proxy's own
// /.well-known/jwks-internal.json, never a locally mounted public key file.
// See CLAUDE.md's "Internal identity assertions" section.
type SignerConfig struct {
	// Issuer is embedded as the assertion's "iss" claim. Defaults to
	// DefaultInternalIssuer if empty.
	Issuer string `json:"issuer"`

	// Audience is embedded as the assertion's "aud" claim. Defaults to
	// DefaultInternalAudience if empty.
	Audience string `json:"audience"`

	// TTLSeconds is how long a minted assertion is valid for. Defaults to
	// defaultInternalTTLSeconds if zero.
	TTLSeconds int `json:"ttlSeconds"`

	// MaxRelayHops caps how many relay/loopback hops one client request
	// may go through (e.g. backend A relays to B, which relays to C, is 2).
	// Defaults to DefaultMaxRelayHops if zero.
	MaxRelayHops int `json:"maxRelayHops,omitempty"`

	// Relay lists, per backend server name, the tools/prompts/resources
	// (access_control.GlobMatch patterns over namespaced names/URIs) that
	// server may call back into the proxy for, as the user whose call it is
	// handling. Only a server listed here is ever sent a relay token, and
	// even then, its relayed calls are limited to these targets, never
	// _admin.*, and never more than the user's own role allows. Absent or
	// empty: no backend may relay.
	Relay map[string][]string `json:"relay,omitempty"`
}

// DefaultMaxRelayHops is used when SignerConfig.MaxRelayHops is zero.
const DefaultMaxRelayHops = 3

// Signer mints short-lived RS256 JWTs asserting an already-verified
// caller's identity, for the proxy to send to backend MCP servers. It is
// the mint-side counterpart to Verifier: a backend that wants to check a
// caller's identity builds its own Verifier pointed at a JWKSURL serving
// this Signer's public key (see JWKSHandler).
type Signer struct {
	key      *rsa.PrivateKey
	kid      string
	issuer   string
	audience string
	ttl      time.Duration
}

// GenerateKey returns a fresh RSA private key sized for internal signing —
// the single place that key size is chosen, shared by every caller that
// needs one (SignerFromKey callers, and main.go's
// bootstrapInternalSigningKey when it's the one creating the shared k8s
// Secret for the first time).
func GenerateKey() (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, internalKeyBits)
	if err != nil {
		return nil, fmt.Errorf("failed to generate internal signing key: %w", err)
	}
	return key, nil
}

// EncodePrivateKeyPEM/ParsePrivateKeyPEM round-trip a key through PKCS8 PEM
// bytes — used only to persist a generated key into (and read it back out
// of) the cross-replica k8s Secret main.go's bootstrapInternalSigningKey
// coordinates through; nothing in this package itself touches a file or a
// Secret.
func EncodePrivateKeyPEM(key *rsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func ParsePrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is not an RSA private key")
	}
	return key, nil
}

// SignerFromKey builds a Signer around an already-resolved key — the only
// constructor this package exposes, since key resolution (generate fresh
// vs. read back from a shared k8s Secret) is main.go's concern, not this
// package's.
func SignerFromKey(key *rsa.PrivateKey, cfg SignerConfig) *Signer {
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = DefaultInternalIssuer
	}
	audience := cfg.Audience
	if audience == "" {
		audience = DefaultInternalAudience
	}
	ttlSeconds := cfg.TTLSeconds
	if ttlSeconds == 0 {
		ttlSeconds = defaultInternalTTLSeconds
	}

	return &Signer{
		key:      key,
		kid:      keyID(&key.PublicKey),
		issuer:   issuer,
		audience: audience,
		ttl:      time.Duration(ttlSeconds) * time.Second,
	}
}

// keyID derives a stable identifier for pub, used both as the "kid" header
// on every token this Signer mints and as the matching JWK's own "kid" in
// JWKSHandler's output — the two have to agree for a JWKS-based Verifier
// (github.com/MicahParks/keyfunc) to find the right key at all.
func keyID(pub *rsa.PublicKey) string {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		// Unreachable for a key this package itself generated or parsed
		// from a well-formed PKCS8 blob; MarshalPKIXPublicKey only fails
		// on a key type it doesn't recognize, and *rsa.PublicKey is always
		// supported.
		return ""
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])[:16]
}

// PublicKey returns the public half of s's signing key — used both to
// build the proxy's own internalAssertionVerifier (main.go, via
// NewVerifierFromKey) and by JWKSHandler.
func (s *Signer) PublicKey() *rsa.PublicKey {
	return &s.key.PublicKey
}

// KID returns the key ID embedded in every token s mints (see keyID).
func (s *Signer) KID() string {
	return s.kid
}

// JWKSHandler returns an http.HandlerFunc serving s's public key as a
// standard JWKS document ({"keys":[...]}) — what main.go mounts at
// /.well-known/jwks-internal.json so a backend's Verifier (JWKSURL mode)
// can fetch it, exactly the same shape/library (github.com/MicahParks/
// jwkset, keyfunc's own server-side counterpart) as any real IdP's JWKS
// endpoint. Deliberately unauthenticated, like /healthz and
// /.well-known/oauth-protected-resource: a backend needs to fetch this
// before it can verify anything, so there's no credential to gate it on.
func (s *Signer) JWKSHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jwk, err := jwkset.NewJWKFromKey(s.PublicKey(), jwkset.JWKOptions{
			Metadata: jwkset.JWKMetadataOptions{
				KID: s.kid,
				ALG: jwkset.AlgRS256,
				USE: jwkset.UseSig,
			},
		})
		if err != nil {
			http.Error(w, "failed to marshal internal signing key as JWK", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwkset.JWKSMarshal{Keys: []jwkset.JWKMarshal{jwk.Marshal()}})
	}
}

// Sign mints an RS256 JWT asserting sub/roles, valid for the Signer's TTL.
// Callers should pass already-verified values (e.g. claims extracted from
// the client's own token, checked by a Verifier), since this is presented
// to backends as an assertion of caller identity.
//
// roles is embedded as a JSON array (via jwt.MapClaims's own
// encoding/json-based marshaling), the same shape RFC 7519's own reserved
// "aud" claim allows (a single string or an array of strings) and the
// shape multi-valued role/group claims take in every major IdP (Auth0,
// Keycloak, Okta, Azure AD, Cognito) - never a delimited string packed
// into one claim value. access_control.RolesFromClaims already parses
// both a bare string and a JSON array back out, so a single-element roles
// slice round-trips identically to how it always has.
func (s *Signer) Sign(sub string, roles []string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":  sub,
		"role": roles,
		"iss":  s.issuer,
		"aud":  s.audience,
		"iat":  now.Unix(),
		"exp":  now.Add(s.ttl).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	// A "kid" header is required for a JWKS-based Verifier (keyfunc) to
	// select the right key from the set — see keyID's doc comment.
	token.Header["kid"] = s.kid
	return token.SignedString(s.key)
}

// Audiences of the two tokens the proxy accepts back as a credential. They
// are fixed (not configurable) and must differ from SignerConfig.Audience,
// the identity assertion's audience, so a backend can never present the
// identity assertion it was given as one of these.
const (
	// RelayAudience is a relay token's "aud": sent only to a backend
	// SignerConfig.Relay allows to relay, which presents it back to the
	// proxy to call another tool as the same user.
	RelayAudience = "go-proxy-relay"
	// LoopbackAudience is a loopback token's "aud": minted by the proxy for
	// its own proxy_utility.call_tool/render_prompt, and only ever sent to
	// the proxy's own address, never to a backend.
	LoopbackAudience = "go-proxy-loopback"
)

// Claim names the relay and loopback tokens add to the standard ones.
const (
	// ClaimHop counts relay/loopback hops: a token minted while handling a
	// direct client call carries 1, one minted while handling a call that
	// itself arrived with hop h carries h+1.
	ClaimHop = "hop"
	// ClaimToolCallID binds the token to the tool call that caused it (the
	// tool-call correlation ID), so a relayed call keeps that ID.
	ClaimToolCallID = "tcid"
	// ClaimRelayFrom names the backend the relay chain started from. On a
	// relay token it's the server the token was sent to; a loopback token
	// minted while handling a relayed call keeps the relaying server's
	// name, so its restrictions follow the call through call_tool.
	ClaimRelayFrom = "relay_from"
)

// Identity is a caller's already-verified identity, as carried through a
// request (and on to backend calls and relays it causes).
type Identity struct {
	Subject string
	Roles   []string
	// Expiry caps every token minted for this identity: a token never
	// outlives the credential the request arrived with (the client's JWT,
	// or the relay/loopback token). Zero means no cap beyond the TTL.
	Expiry time.Time
	// Hop is the hop number tokens minted for this identity carry (see
	// ClaimHop).
	Hop int
	// ToolCallID is the tool call this identity is acting within, if any.
	ToolCallID string
	// RelayFrom is the backend whose relay this request is part of, if
	// any (see ClaimRelayFrom).
	RelayFrom string
}

// expiry is the "exp" for a token minted now for id: the TTL, capped at
// id.Expiry.
func (s *Signer) expiry(now time.Time, id Identity) time.Time {
	exp := now.Add(s.ttl)
	if !id.Expiry.IsZero() && id.Expiry.Before(exp) {
		exp = id.Expiry
	}
	return exp
}

func (s *Signer) signClaims(claims jwt.MapClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = s.kid
	return token.SignedString(s.key)
}

// SignAssertion mints the identity assertion sent to backend server: who
// the caller is, for the backend's own checks. Its "aud" is both the
// configured audience and "<audience>/<server>", so a backend may require
// an assertion minted for itself specifically. The proxy never accepts it
// back as a credential.
func (s *Signer) SignAssertion(id Identity, server string) (string, error) {
	now := time.Now()
	return s.signClaims(jwt.MapClaims{
		"sub":  id.Subject,
		"role": id.Roles,
		"iss":  s.issuer,
		"aud":  []string{s.audience, s.audience + "/" + server},
		"iat":  now.Unix(),
		"exp":  s.expiry(now, id).Unix(),
	})
}

// SignRelay mints the relay token sent to backend server, which may
// present it back to the proxy to call another tool as the same user (see
// SignerConfig.Relay).
func (s *Signer) SignRelay(id Identity, server string) (string, error) {
	return s.signHop(RelayAudience, id, server)
}

// SignLoopback mints the token the proxy's own proxy_utility tools use to
// call back into the proxy as the same user. id.RelayFrom, if set, is kept,
// so a loopback made while handling a relayed call stays under that
// relay's restrictions.
func (s *Signer) SignLoopback(id Identity) (string, error) {
	return s.signHop(LoopbackAudience, id, id.RelayFrom)
}

func (s *Signer) signHop(audience string, id Identity, relayFrom string) (string, error) {
	now := time.Now()
	claims := jwt.MapClaims{
		"sub":    id.Subject,
		"role":   id.Roles,
		"iss":    s.issuer,
		"aud":    audience,
		"iat":    now.Unix(),
		"exp":    s.expiry(now, id).Unix(),
		ClaimHop: id.Hop,
	}
	if id.ToolCallID != "" {
		claims[ClaimToolCallID] = id.ToolCallID
	}
	if relayFrom != "" {
		claims[ClaimRelayFrom] = relayFrom
	}
	return s.signClaims(claims)
}
