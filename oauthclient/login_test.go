// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// newMockAuthServer builds a minimal, real (not stubbed) OAuth AS to
// exercise Login's HTTP mechanics against: metadata discovery, DCR,
// PKCE-verified authorization_code issuance, and code exchange.
// withRegistration controls whether registration_endpoint is advertised
// at all (used by the no-DCR/no-client-id test). The GET /authorize
// handler redirects immediately with no login-form step — this mock
// exists to test the *client's* mechanics (URL construction, callback
// handling, PKCE, state validation), not to re-test
// demo-authorization-server's own login semantics, which
// oauth_flow_test.go already covers.
func newMockAuthServer(t *testing.T, withRegistration bool) *httptest.Server {
	t.Helper()

	type storedCode struct {
		clientID, redirectURI, challenge string
	}
	var mu sync.Mutex
	codes := map[string]storedCode{}
	clients := map[string]bool{}

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		meta := map[string]any{
			"issuer":                 srv.URL,
			"authorization_endpoint": srv.URL + "/authorize",
			"token_endpoint":         srv.URL + "/token",
		}
		if withRegistration {
			meta["registration_endpoint"] = srv.URL + "/register"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(meta)
	})

	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.RedirectURIs) == 0 {
			http.Error(w, "redirect_uris required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		clientID := fmt.Sprintf("client-%d", len(clients))
		clients[clientID] = true
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"client_id": clientID})
	})

	authorize := func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		clientID := r.Form.Get("client_id")
		redirectURI := r.Form.Get("redirect_uri")
		challenge := r.Form.Get("code_challenge")
		state := r.Form.Get("state")

		mu.Lock()
		known := clients[clientID]
		mu.Unlock()
		if !known {
			http.Error(w, "unknown client_id", http.StatusBadRequest)
			return
		}

		if r.Method == http.MethodPost {
			username := r.Form.Get("username")
			password := r.Form.Get("password")
			if username == "" || username != password {
				http.Error(w, "invalid credentials", http.StatusUnauthorized)
				return
			}
		}

		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		mu.Lock()
		codes[code] = storedCode{clientID: clientID, redirectURI: redirectURI, challenge: challenge}
		mu.Unlock()

		http.Redirect(w, r, redirectURI+"?code="+code+"&state="+state, http.StatusFound)
	}
	mux.HandleFunc("GET /authorize", authorize)
	mux.HandleFunc("POST /authorize", authorize)

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		code := r.Form.Get("code")

		mu.Lock()
		rec, ok := codes[code]
		delete(codes, code)
		mu.Unlock()
		if !ok {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != rec.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_id") != rec.clientID || r.Form.Get("redirect_uri") != rec.redirectURI {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-" + code,
			"token_type":    "Bearer",
			"expires_in":    900,
			"refresh_token": "refresh-" + code,
		})
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestLoginCredentialFastPath(t *testing.T) {
	srv := newMockAuthServer(t, true)

	resp, err := Login(context.Background(), LoginOptions{
		AuthServerURL: srv.URL,
		Username:      "alice",
		Password:      "alice",
	})
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if resp.AccessToken == "" {
		t.Fatalf("expected a non-empty access token, got %+v", resp)
	}
}

// syncWriter is a trivial mutex-guarded io.Writer — needed because the
// test goroutine below polls the same buffer Login's browser path writes
// the authorization URL to, from a different goroutine.
type syncWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestLoginBrowserPath(t *testing.T) {
	srv := newMockAuthServer(t, true)

	var out bytes.Buffer
	var outMu sync.Mutex
	type loginResult struct {
		resp *TokenResponse
		err  error
	}
	resultCh := make(chan loginResult, 1)

	go func() {
		resp, err := Login(context.Background(), LoginOptions{
			AuthServerURL: srv.URL,
			Output:        &syncWriter{mu: &outMu, w: &out},
			Timeout:       10 * time.Second,
		})
		resultCh <- loginResult{resp, err}
	}()

	// Poll for the printed authorization URL, then simulate the browser
	// by GETting it directly: the mock AS's GET /authorize responds with
	// a 302 to the client's own loopback listener, which Go's default
	// redirect-following http.Get then hits for real, completing the
	// flow exactly as an actual browser would.
	var authURL string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		outMu.Lock()
		text := out.String()
		outMu.Unlock()
		if idx := strings.Index(text, srv.URL+"/authorize?"); idx >= 0 {
			end := strings.IndexAny(text[idx:], "\n ")
			if end < 0 {
				end = len(text) - idx
			}
			authURL = text[idx : idx+end]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if authURL == "" {
		t.Fatalf("timed out waiting for an authorization URL to be printed; output so far: %s", out.String())
	}

	getResp, err := http.Get(authURL)
	if err != nil {
		t.Fatalf("simulated browser GET failed: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected the final redirect chain to land on a 200 from the callback, got %d", getResp.StatusCode)
	}

	select {
	case res := <-resultCh:
		if res.err != nil {
			t.Fatalf("Login failed: %v", res.err)
		}
		if res.resp.AccessToken == "" {
			t.Fatalf("expected a non-empty access token, got %+v", res.resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Login did not return after the simulated browser completed login")
	}
}

func TestBuildAuthorizeURLAudience(t *testing.T) {
	withAudience, err := buildAuthorizeURL("https://example.com/authorize", "client-1", "http://127.0.0.1:8765/callback", "challenge", "state", "https://go-proxy-api", "")
	if err != nil {
		t.Fatalf("buildAuthorizeURL failed: %v", err)
	}
	u, err := url.Parse(withAudience)
	if err != nil {
		t.Fatalf("failed to parse built URL: %v", err)
	}
	if got := u.Query().Get("audience"); got != "https://go-proxy-api" {
		t.Fatalf("expected audience=https://go-proxy-api in the URL, got %q (full URL: %s)", got, withAudience)
	}

	withoutAudience, err := buildAuthorizeURL("https://example.com/authorize", "client-1", "http://127.0.0.1:8765/callback", "challenge", "state", "", "")
	if err != nil {
		t.Fatalf("buildAuthorizeURL failed: %v", err)
	}
	u, err = url.Parse(withoutAudience)
	if err != nil {
		t.Fatalf("failed to parse built URL: %v", err)
	}
	if _, ok := u.Query()["audience"]; ok {
		t.Fatalf("expected no audience parameter when Audience is empty, got URL: %s", withoutAudience)
	}
}

func TestBuildAuthorizeURLScope(t *testing.T) {
	withScope, err := buildAuthorizeURL("https://example.com/authorize", "client-1", "http://127.0.0.1:8765/callback", "challenge", "state", "", "openid mcp.access")
	if err != nil {
		t.Fatalf("buildAuthorizeURL failed: %v", err)
	}
	u, err := url.Parse(withScope)
	if err != nil {
		t.Fatalf("failed to parse built URL: %v", err)
	}
	if got := u.Query().Get("scope"); got != "openid mcp.access" {
		t.Fatalf("expected scope=\"openid mcp.access\" in the URL, got %q (full URL: %s)", got, withScope)
	}

	withoutScope, err := buildAuthorizeURL("https://example.com/authorize", "client-1", "http://127.0.0.1:8765/callback", "challenge", "state", "", "")
	if err != nil {
		t.Fatalf("buildAuthorizeURL failed: %v", err)
	}
	u, err = url.Parse(withoutScope)
	if err != nil {
		t.Fatalf("failed to parse built URL: %v", err)
	}
	if _, ok := u.Query()["scope"]; ok {
		t.Fatalf("expected no scope parameter when Scope is empty, got URL: %s", withoutScope)
	}
}

func TestLoginNoClientIDNoRegistrationEndpoint(t *testing.T) {
	srv := newMockAuthServer(t, false)

	_, err := Login(context.Background(), LoginOptions{
		AuthServerURL: srv.URL,
		Username:      "alice",
		Password:      "alice",
	})
	if err == nil {
		t.Fatalf("expected an error when no client_id is given and the server doesn't support DCR")
	}
}

// TestDiscoverMetadataRefusesOtherIssuer checks RFC 8414 section 3.3: metadata
// naming an issuer other than the server asked is refused.
func TestDiscoverMetadataRefusesOtherIssuer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 "https://other.example.com",
			"authorization_endpoint": "https://other.example.com/authorize",
			"token_endpoint":         "https://other.example.com/token",
		})
	}))
	defer srv.Close()
	if _, err := DiscoverMetadata(context.Background(), srv.URL, ""); err == nil || !strings.Contains(err.Error(), "names issuer") {
		t.Fatalf("DiscoverMetadata = %v; want an issuer mismatch error", err)
	}
}

// TestLoginIgnoresForgedCallback checks that a callback without the
// login's state - here an error=access_denied any local page could send -
// can't end the login, which then completes normally.
func TestLoginIgnoresForgedCallback(t *testing.T) {
	srv := newMockAuthServer(t, true)

	var out bytes.Buffer
	var outMu sync.Mutex
	errCh := make(chan error, 1)
	go func() {
		_, err := Login(context.Background(), LoginOptions{
			AuthServerURL: srv.URL,
			Output:        &syncWriter{mu: &outMu, w: &out},
			Timeout:       10 * time.Second,
		})
		errCh <- err
	}()

	var authURL string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && authURL == ""; time.Sleep(10 * time.Millisecond) {
		outMu.Lock()
		text := out.String()
		outMu.Unlock()
		if idx := strings.Index(text, srv.URL+"/authorize?"); idx >= 0 {
			end := strings.IndexAny(text[idx:], "\n ")
			authURL = text[idx : idx+end]
		}
	}
	if authURL == "" {
		t.Fatal("no authorization URL was printed")
	}
	parsed, _ := url.Parse(authURL)
	callback := parsed.Query().Get("redirect_uri")

	forged, err := http.Get(callback + "?error=access_denied&state=not-the-login-state")
	if err != nil {
		t.Fatalf("forged callback: %v", err)
	}
	forged.Body.Close()
	select {
	case err := <-errCh:
		t.Fatalf("a forged callback ended the login: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	real, err := http.Get(authURL)
	if err != nil {
		t.Fatalf("real login: %v", err)
	}
	real.Body.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Login failed after a forged callback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Login didn't finish")
	}
}

// TestDiscoverMetadataAcceptsExplicitIssuer checks the Issuer override: a
// server reached at another address than its issuer is accepted when the
// caller names that issuer, and still refused when the issuer differs.
func TestDiscoverMetadataAcceptsExplicitIssuer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 "http://localhost:8102",
			"authorization_endpoint": "http://localhost:8102/authorize",
			"token_endpoint":         "http://localhost:8102/token",
		})
	}))
	defer srv.Close()
	if _, err := DiscoverMetadata(context.Background(), srv.URL, "http://localhost:8102"); err != nil {
		t.Errorf("the named issuer was refused: %v", err)
	}
	if _, err := DiscoverMetadata(context.Background(), srv.URL, "https://other.example.com"); err == nil {
		t.Error("an issuer other than the named one was accepted")
	}
}
