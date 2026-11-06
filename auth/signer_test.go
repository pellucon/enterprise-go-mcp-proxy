// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/auth"

	"github.com/golang-jwt/jwt/v5"
)

// generateTestKeyPair generates a fresh RSA keypair, writing only the
// public half (PKIX PEM) to dir for auth.NewVerifier's PublicKeyPath —
// the private half is returned directly for auth.SignerFromKey, since
// Signer no longer reads a key from disk at all.
func generateTestKeyPair(t *testing.T, dir string) (key *rsa.PrivateKey, pubPath string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubPath = filepath.Join(dir, "public.pem")
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(pubPath, pubPEM, 0o600); err != nil {
		t.Fatalf("failed to write public key: %v", err)
	}
	return key, pubPath
}

// TestSigner_SignRoundTripsMultipleRoles proves the fix end-to-end, the
// same way a real backend would consume it: mint via Signer.Sign with
// multiple roles, verify the resulting token via a Verifier (never
// ParseUnverified), and confirm access_control.RolesFromClaims recovers
// every role - not a single comma-joined string, which is what
// middleware.go used to produce before this fix (see CLAUDE.md's
// "Internal identity assertions" section).
func TestSigner_SignRoundTripsMultipleRoles(t *testing.T) {
	dir := t.TempDir()
	key, pubPath := generateTestKeyPair(t, dir)

	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	verifier, err := auth.NewVerifier(context.Background(), auth.Config{PublicKeyPath: pubPath})
	if err != nil {
		t.Fatalf("NewVerifier failed: %v", err)
	}

	wantRoles := []string{"manager", "developer", "on-call"}
	tokenString, err := signer.Sign("alice", wantRoles)
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}

	token, err := verifier.TokenFromHeader("Bearer " + tokenString)
	if err != nil {
		t.Fatalf("TokenFromHeader failed to verify minted token: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("unexpected claims type %T", token.Claims)
	}

	sub, err := claims.GetSubject()
	if err != nil || sub != "alice" {
		t.Fatalf("expected sub \"alice\", got %q (err %v)", sub, err)
	}

	roles := access_control.RolesFromClaims(claims)
	if len(roles) != len(wantRoles) {
		t.Fatalf("expected %d roles, got %d: %v", len(wantRoles), len(roles), roles)
	}
	for i, want := range wantRoles {
		if roles[i] != want {
			t.Fatalf("role[%d]: expected %q, got %q (full: %v)", i, want, roles[i], roles)
		}
	}
}

// TestSigner_SignSingleRoleStillWorks confirms the single-role case (the
// common path) is unaffected by the array-based encoding.
func TestSigner_SignSingleRoleStillWorks(t *testing.T) {
	dir := t.TempDir()
	key, pubPath := generateTestKeyPair(t, dir)

	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	verifier, err := auth.NewVerifier(context.Background(), auth.Config{PublicKeyPath: pubPath})
	if err != nil {
		t.Fatalf("NewVerifier failed: %v", err)
	}

	tokenString, err := signer.Sign("bob", []string{"admin"})
	if err != nil {
		t.Fatalf("Sign failed: %v", err)
	}
	token, err := verifier.TokenFromHeader("Bearer " + tokenString)
	if err != nil {
		t.Fatalf("TokenFromHeader failed to verify minted token: %v", err)
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("unexpected claims type %T", token.Claims)
	}

	roles := access_control.RolesFromClaims(claims)
	if len(roles) != 1 || roles[0] != "admin" {
		t.Fatalf("expected [admin], got %v", roles)
	}
}

func parseWith(t *testing.T, key *rsa.PrivateKey, token, issuer, audience string) jwt.MapClaims {
	t.Helper()
	parsed, err := auth.NewVerifierFromKey(&key.PublicKey, issuer, audience).TokenFromHeader("Bearer " + token)
	if err != nil {
		t.Fatalf("verifying token (aud %q): %v", audience, err)
	}
	return parsed.Claims.(jwt.MapClaims)
}

// TestSigner_SignAssertionAudiences checks that an identity assertion is
// minted for the shared audience and for its server specifically, so a
// backend may require either - and that it's never valid as a relay or
// loopback token.
func TestSigner_SignAssertionAudiences(t *testing.T) {
	key, _ := generateTestKeyPair(t, t.TempDir())
	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	token, err := signer.SignAssertion(auth.Identity{Subject: "alice", Roles: []string{"user"}}, "crm")
	if err != nil {
		t.Fatal(err)
	}
	parseWith(t, key, token, auth.DefaultInternalIssuer, auth.DefaultInternalAudience)
	parseWith(t, key, token, auth.DefaultInternalIssuer, auth.DefaultInternalAudience+"/crm")
	for _, aud := range []string{auth.DefaultInternalAudience + "/other", auth.RelayAudience, auth.LoopbackAudience} {
		if _, err := auth.NewVerifierFromKey(&key.PublicKey, auth.DefaultInternalIssuer, aud).TokenFromHeader("Bearer " + token); err == nil {
			t.Errorf("assertion verified with audience %q", aud)
		}
	}
}

func TestSigner_SignRelayAndLoopbackClaims(t *testing.T) {
	key, _ := generateTestKeyPair(t, t.TempDir())
	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	id := auth.Identity{Subject: "alice", Roles: []string{"user"}, Hop: 2, ToolCallID: "call-1", RelayFrom: "origin"}

	relay, err := signer.SignRelay(id, "crm")
	if err != nil {
		t.Fatal(err)
	}
	claims := parseWith(t, key, relay, auth.DefaultInternalIssuer, auth.RelayAudience)
	if claims[auth.ClaimHop] != float64(2) || claims[auth.ClaimToolCallID] != "call-1" || claims[auth.ClaimRelayFrom] != "crm" {
		t.Errorf("relay token claims = %v; want hop 2, tcid call-1, relay_from crm (the server it was sent to)", claims)
	}

	loopback, err := signer.SignLoopback(id)
	if err != nil {
		t.Fatal(err)
	}
	claims = parseWith(t, key, loopback, auth.DefaultInternalIssuer, auth.LoopbackAudience)
	if claims[auth.ClaimRelayFrom] != "origin" {
		t.Errorf("loopback token relay_from = %v, want the identity's own %q", claims[auth.ClaimRelayFrom], "origin")
	}
	id.RelayFrom = ""
	loopback, _ = signer.SignLoopback(id)
	if _, ok := parseWith(t, key, loopback, auth.DefaultInternalIssuer, auth.LoopbackAudience)[auth.ClaimRelayFrom]; ok {
		t.Error("loopback token for a non-relayed call carries relay_from")
	}
}

// TestSigner_ExpiryNeverExceedsIdentity checks that no token outlives the
// credential its identity came from, so relaying can't extend it.
func TestSigner_ExpiryNeverExceedsIdentity(t *testing.T) {
	key, _ := generateTestKeyPair(t, t.TempDir())
	signer := auth.SignerFromKey(key, auth.SignerConfig{TTLSeconds: 600})
	cap := time.Now().Add(30 * time.Second).Truncate(time.Second)
	id := auth.Identity{Subject: "alice", Expiry: cap, Hop: 1}

	for name, mint := range map[string]func() (string, error){
		"assertion": func() (string, error) { return signer.SignAssertion(id, "crm") },
		"relay":     func() (string, error) { return signer.SignRelay(id, "crm") },
		"loopback":  func() (string, error) { return signer.SignLoopback(id) },
	} {
		token, err := mint()
		if err != nil {
			t.Fatal(err)
		}
		parsed, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
		if err != nil {
			t.Fatal(err)
		}
		exp, _ := parsed.Claims.GetExpirationTime()
		if !exp.Time.Equal(cap) {
			t.Errorf("%s exp = %v, want the identity's expiry %v", name, exp.Time, cap)
		}
	}

	// Without a cap, the TTL applies.
	token, _ := signer.SignRelay(auth.Identity{Subject: "alice", Hop: 1}, "crm")
	parsed, _, _ := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{})
	exp, _ := parsed.Claims.GetExpirationTime()
	if d := time.Until(exp.Time); d < 590*time.Second || d > 610*time.Second {
		t.Errorf("uncapped exp is %v away, want about the 600s TTL", d)
	}
}
