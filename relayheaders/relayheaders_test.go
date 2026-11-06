// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package relayheaders

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
)

func TestExtract(t *testing.T) {
	h := http.Header{}
	h.Set(AssertionHeader, "Bearer abc")
	h.Set(RelayTokenHeader, "Bearer relay")
	h.Set(CallbackURLHeader, "http://proxy/mcp")
	h.Set(ToolCallIDHeader, "call-1")

	got := Extract(h)
	want := Set{Assertion: "Bearer abc", RelayToken: "Bearer relay", CallbackURL: "http://proxy/mcp", ToolCallID: "call-1"}
	if got != want {
		t.Errorf("Extract() = %+v, want %+v", got, want)
	}
}

func TestExtractMissingHeadersYieldsEmptySet(t *testing.T) {
	got := Extract(http.Header{})
	if !got.Empty() {
		t.Errorf("expected an empty Set from headers with nothing set, got %+v", got)
	}
}

func TestSetApplyOnlySetsNonEmptyFields(t *testing.T) {
	h := http.Header{}
	h.Set(CallbackURLHeader, "preexisting")

	Set{Assertion: "Bearer abc", ToolCallID: "call-1"}.Apply(h)

	if got := h.Get(AssertionHeader); got != "Bearer abc" {
		t.Errorf("AssertionHeader = %q, want %q", got, "Bearer abc")
	}
	if got := h.Get(ToolCallIDHeader); got != "call-1" {
		t.Errorf("ToolCallIDHeader = %q, want %q", got, "call-1")
	}
	// CallbackURL was left empty on the Set, so Apply must not touch the
	// header that was already there.
	if got := h.Get(CallbackURLHeader); got != "preexisting" {
		t.Errorf("CallbackURLHeader = %q, want untouched %q", got, "preexisting")
	}
}

// recordingRoundTripper captures the last request it saw (specifically its
// headers) without making a real network call.
type recordingRoundTripper struct {
	lastReq *http.Request
}

func (r *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r.lastReq = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: http.Header{}}, nil
}

func TestTransportAttachesHeadersFromSource(t *testing.T) {
	rec := &recordingRoundTripper{}
	transport := &Transport{
		Base: rec,
		Source: func(ctx context.Context) Set {
			return Set{Assertion: "Bearer xyz", ToolCallID: "call-42"}
		},
	}

	req, _ := http.NewRequest(http.MethodPost, "http://backend/mcp", nil)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}

	if got := rec.lastReq.Header.Get(AssertionHeader); got != "Bearer xyz" {
		t.Errorf("AssertionHeader = %q, want %q", got, "Bearer xyz")
	}
	if got := rec.lastReq.Header.Get(ToolCallIDHeader); got != "call-42" {
		t.Errorf("ToolCallIDHeader = %q, want %q", got, "call-42")
	}
	// The original request object must not have been mutated in place —
	// RoundTrip clones before attaching, so concurrent callers sharing one
	// *http.Request-building call site never race on headers.
	if got := req.Header.Get(AssertionHeader); got != "" {
		t.Errorf("expected the original request to be left untouched, got AssertionHeader=%q", got)
	}
}

func TestTransportSkipsCloneWhenSourceIsEmpty(t *testing.T) {
	rec := &recordingRoundTripper{}
	transport := &Transport{Base: rec, Source: Static(Set{})}

	req, _ := http.NewRequest(http.MethodPost, "http://backend/mcp", nil)
	if _, err := transport.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip failed: %v", err)
	}

	// An empty Set means RoundTrip forwards the exact same *http.Request
	// pointer rather than cloning — verifiable since our recorder captured
	// whatever pointer it was actually handed.
	if rec.lastReq != req {
		t.Error("expected RoundTrip to pass the original request through unmodified when Source returns an empty Set")
	}
}

func TestSetRoundTripperAttachesFixedHeaders(t *testing.T) {
	rec := &recordingRoundTripper{}
	client := &http.Client{Transport: Set{Assertion: "Bearer static-token"}.RoundTripper(rec)}

	req, _ := http.NewRequest(http.MethodGet, "http://backend/mcp", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := rec.lastReq.Header.Get(AssertionHeader); got != "Bearer static-token" {
		t.Errorf("AssertionHeader = %q, want %q", got, "Bearer static-token")
	}
}

func TestClientForRelaysEverythingFromHeaderVerbatim(t *testing.T) {
	rec := &recordingRoundTripper{}
	inbound := http.Header{}
	inbound.Set(AssertionHeader, "Bearer relayed-token")
	inbound.Set(RelayTokenHeader, "Bearer relay-token")
	inbound.Set(CallbackURLHeader, "http://proxy/mcp")
	inbound.Set(ToolCallIDHeader, "call-99")

	client := ClientFor(inbound, rec)
	req, _ := http.NewRequest(http.MethodGet, "http://backend/mcp", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}

	// ClientFor must forward every field present on the inbound headers
	// (not a hand-picked subset) — a caller relaying a call back into the
	// proxy shouldn't need to know which individual X-Go-Proxy-* fields
	// exist to pass them all along.
	got := Extract(rec.lastReq.Header)
	want := Set{Assertion: "Bearer relayed-token", RelayToken: "Bearer relay-token", CallbackURL: "http://proxy/mcp", ToolCallID: "call-99"}
	if got != want {
		t.Errorf("relayed headers = %+v, want %+v", got, want)
	}
}

func TestSetClientMatchesRoundTripper(t *testing.T) {
	rec := &recordingRoundTripper{}
	s := Set{Assertion: "Bearer abc"}
	client := s.Client(rec)

	req, _ := http.NewRequest(http.MethodGet, "http://backend/mcp", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := rec.lastReq.Header.Get(AssertionHeader); got != "Bearer abc" {
		t.Errorf("AssertionHeader = %q, want %q", got, "Bearer abc")
	}
}

// generateTestKeyPair generates a fresh RSA keypair, writing only the
// public half to dir (for auth.NewVerifier's PublicKeyPath) and returning
// the private half directly (for auth.SignerFromKey, which no longer
// reads a key from disk) — mirroring the same pattern used by
// auth/signer_test.go (duplicated here since this package can't import a
// package-main test helper, and auth_test is a different package to
// auth/signer_test.go's own).
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
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})

	pubPath = filepath.Join(dir, "public.pem")
	if err := os.WriteFile(pubPath, pubPEM, 0o644); err != nil {
		t.Fatalf("failed to write public key: %v", err)
	}
	return key, pubPath
}

func TestVerifyAssertionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key, pubPath := generateTestKeyPair(t, dir)

	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	verifier, err := auth.NewVerifier(context.Background(), auth.Config{
		PublicKeyPath: pubPath,
		Issuer:        auth.DefaultInternalIssuer,
		Audience:      auth.DefaultInternalAudience,
	})
	if err != nil {
		t.Fatalf("failed to build verifier: %v", err)
	}

	token, err := signer.Sign("alice", []string{"user"})
	if err != nil {
		t.Fatalf("failed to sign assertion: %v", err)
	}

	h := http.Header{}
	h.Set(AssertionHeader, "Bearer "+token)

	claims, err := VerifyAssertion(verifier, h)
	if err != nil {
		t.Fatalf("VerifyAssertion failed: %v", err)
	}
	sub, err := claims.GetSubject()
	if err != nil || sub != "alice" {
		t.Errorf("expected subject %q, got %q (err=%v)", "alice", sub, err)
	}
}

func TestVerifyAssertionFailsClosed(t *testing.T) {
	dir := t.TempDir()
	key, pubPath := generateTestKeyPair(t, dir)
	otherDir := filepath.Join(dir, "other")
	if err := os.Mkdir(otherDir, 0o755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}
	_, otherPubPath := generateTestKeyPair(t, otherDir)

	signer := auth.SignerFromKey(key, auth.SignerConfig{})
	token, err := signer.Sign("alice", []string{"user"})
	if err != nil {
		t.Fatalf("failed to sign assertion: %v", err)
	}
	validHeader := http.Header{}
	validHeader.Set(AssertionHeader, "Bearer "+token)

	verifier, err := auth.NewVerifier(context.Background(), auth.Config{
		PublicKeyPath: pubPath,
		Issuer:        auth.DefaultInternalIssuer,
		Audience:      auth.DefaultInternalAudience,
	})
	if err != nil {
		t.Fatalf("failed to build verifier: %v", err)
	}
	mismatchedVerifier, err := auth.NewVerifier(context.Background(), auth.Config{
		PublicKeyPath: otherPubPath,
		Issuer:        auth.DefaultInternalIssuer,
		Audience:      auth.DefaultInternalAudience,
	})
	if err != nil {
		t.Fatalf("failed to build mismatched verifier: %v", err)
	}

	if _, err := VerifyAssertion(nil, validHeader); err == nil {
		t.Error("expected a nil verifier to fail closed")
	}
	if _, err := VerifyAssertion(verifier, http.Header{}); err == nil {
		t.Error("expected a missing assertion header to fail closed")
	}
	if _, err := VerifyAssertion(mismatchedVerifier, validHeader); err == nil {
		t.Error("expected an assertion signed by a different key to fail closed")
	}
}

// TestServerTransportMintsForItsServer checks that ServerTransport asks
// the request context's minter for headers for its own server, per
// request, and attaches what it returns.
func TestServerTransportMintsForItsServer(t *testing.T) {
	rec := &recordingRoundTripper{}
	var asked []string
	ctx := WithMinter(context.Background(), func(server string) (Set, error) {
		asked = append(asked, server)
		return Set{Assertion: "Bearer for-" + server, ToolCallID: "call-1"}, nil
	})
	client := &http.Client{Transport: &ServerTransport{Base: rec, Server: "crm"}}
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://crm/mcp", nil)
		if _, err := client.Do(req); err != nil {
			t.Fatalf("request failed: %v", err)
		}
	}
	if len(asked) != 2 || asked[0] != "crm" || asked[1] != "crm" {
		t.Errorf("minter asked for %v, want [crm crm]", asked)
	}
	if got := Extract(rec.lastReq.Header); got != (Set{Assertion: "Bearer for-crm", ToolCallID: "call-1"}) {
		t.Errorf("attached headers = %+v", got)
	}
}

func TestServerTransportWithoutMinterAttachesNothing(t *testing.T) {
	rec := &recordingRoundTripper{}
	client := &http.Client{Transport: &ServerTransport{Base: rec, Server: "crm"}}
	req, _ := http.NewRequest(http.MethodGet, "http://crm/mcp", nil)
	if _, err := client.Do(req); err != nil {
		t.Fatalf("request failed: %v", err)
	}
	if got := Extract(rec.lastReq.Header); !got.Empty() {
		t.Errorf("headers attached without a minter: %+v", got)
	}
}

// TestServerTransportFailsWhenMintingFails checks that a request isn't sent
// without the caller's identity when its headers can't be minted.
func TestServerTransportFailsWhenMintingFails(t *testing.T) {
	rec := &recordingRoundTripper{}
	ctx := WithMinter(context.Background(), func(string) (Set, error) { return Set{}, errors.New("boom") })
	client := &http.Client{Transport: &ServerTransport{Base: rec, Server: "crm"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://crm/mcp", nil)
	if _, err := client.Do(req); err == nil {
		t.Fatal("request succeeded although minting failed")
	}
	if rec.lastReq != nil {
		t.Error("request was sent although minting failed")
	}
}
