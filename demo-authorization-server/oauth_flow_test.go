// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// oauth_flow_test.go is a standalone end-to-end test of the OAuth
// authorization_code + PKCE + refresh_token flow: it spins up a real
// demo-authorization-server and a minimal fake "resource server" (which
// verifies Bearer tokens via go-proxy's own, unmodified auth package
// pointed at the demo server's JWKS endpoint — exactly how a real backend
// would), then drives the whole exchange via plain net/http calls. No
// browser is ever launched: login is a direct POST of admin/admin (or
// another user's own credentials) to /authorize, exactly as instructed.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"

	"github.com/golang-jwt/jwt/v5"
)

func newTestAuthServer(t *testing.T) (*httptest.Server, *State) {
	t.Helper()
	state, err := newState("", "", "")
	if err != nil {
		t.Fatalf("newState failed: %v", err)
	}
	srv := httptest.NewServer(newHandler(state))
	t.Cleanup(srv.Close)
	return srv, state
}

func newFakeResourceServer(t *testing.T, jwksURL string) *httptest.Server {
	t.Helper()
	verifier, err := auth.NewVerifier(context.Background(), auth.Config{JWKSURL: jwksURL})
	if err != nil {
		t.Fatalf("auth.NewVerifier failed: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		token, err := verifier.TokenFromHeader(r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		claims, ok := token.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, "unexpected claims type", http.StatusInternalServerError)
			return
		}
		sub, _ := claims.GetSubject()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"sub": sub, "role": claims["role"]})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newPKCEPair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	v, err := randomOpaqueToken()
	if err != nil {
		t.Fatalf("failed to generate PKCE verifier: %v", err)
	}
	sum := sha256.Sum256([]byte(v))
	return v, base64.RawURLEncoding.EncodeToString(sum[:])
}

// registerTestClient registers a client with a loopback redirect_uri and
// returns its client_id.
func registerTestClient(t *testing.T, authServerURL string) (clientID, redirectURI string) {
	t.Helper()
	redirectURI = "http://127.0.0.1:9999/callback"
	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirectURI}, "client_name": "test-client"})
	resp, err := http.Post(authServerURL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST /register failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 from /register, got %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode /register response: %v", err)
	}
	return out.ClientID, redirectURI
}

// noRedirectClient returns an *http.Client that doesn't follow redirects,
// so a 302 from POST /authorize can be inspected directly.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// login POSTs username/password plus the given authorize params to
// /authorize and returns the raw response — this is the "browser"
// stand-in: no automation, just a direct POST with admin/admin (or another
// user's own creds).
func login(t *testing.T, authServerURL, clientID, redirectURI, challenge, state, username, password string) *http.Response {
	t.Helper()
	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"username":              {username},
		"password":              {password},
	}
	resp, err := noRedirectClient().PostForm(authServerURL+"/authorize", form)
	if err != nil {
		t.Fatalf("POST /authorize failed: %v", err)
	}
	return resp
}

func exchangeCode(t *testing.T, authServerURL, code, verifier, clientID, redirectURI string) (status int, body map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	resp, err := http.PostForm(authServerURL+"/token", form)
	if err != nil {
		t.Fatalf("POST /token failed: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func refreshToken(t *testing.T, authServerURL, refreshTok string) (status int, body map[string]any) {
	t.Helper()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshTok}}
	resp, err := http.PostForm(authServerURL+"/token", form)
	if err != nil {
		t.Fatalf("POST /token (refresh) failed: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func whoami(t *testing.T, resourceServerURL, bearer string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, resourceServerURL+"/whoami", nil)
	if err != nil {
		t.Fatalf("failed to build /whoami request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /whoami failed: %v", err)
	}
	return resp
}

func TestFullAuthorizationCodeFlowWithRefresh(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)
	resourceSrv := newFakeResourceServer(t, authSrv.URL+"/.well-known/jwks.json")

	clientID, redirectURI := registerTestClient(t, authSrv.URL)
	verifier, challenge := newPKCEPair(t)

	// GET /authorize renders a login form.
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"xyz"},
	}
	getResp, err := http.Get(authSrv.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatalf("GET /authorize failed: %v", err)
	}
	getBody, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /authorize, got %d", getResp.StatusCode)
	}
	if !strings.Contains(string(getBody), "<form") {
		t.Fatalf("expected a login form in GET /authorize's body, got: %s", getBody)
	}

	loginResp := login(t, authSrv.URL, clientID, redirectURI, challenge, "xyz", "admin", "admin")
	if loginResp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("expected 302 from POST /authorize, got %d: %s", loginResp.StatusCode, body)
	}
	loc, err := loginResp.Location()
	if err != nil {
		t.Fatalf("expected a Location header: %v", err)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("expected a code in the redirect, got %s", loc)
	}
	if got := loc.Query().Get("state"); got != "xyz" {
		t.Fatalf("expected state=xyz to round-trip, got %q", got)
	}

	status, body := exchangeCode(t, authSrv.URL, code, verifier, clientID, redirectURI)
	if status != http.StatusOK {
		t.Fatalf("expected 200 from /token, got %d: %v", status, body)
	}
	accessToken, _ := body["access_token"].(string)
	refreshTok, _ := body["refresh_token"].(string)
	if accessToken == "" || refreshTok == "" {
		t.Fatalf("expected access_token and refresh_token, got %v", body)
	}

	whoamiResp := whoami(t, resourceSrv.URL, accessToken)
	if whoamiResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /whoami, got %d", whoamiResp.StatusCode)
	}
	var who map[string]any
	_ = json.NewDecoder(whoamiResp.Body).Decode(&who)
	if who["sub"] != "admin" {
		t.Fatalf("expected sub=admin, got %v", who)
	}

	// Refresh: new tokens issued, both different from the originals.
	status, refreshed := refreshToken(t, authSrv.URL, refreshTok)
	if status != http.StatusOK {
		t.Fatalf("expected 200 from refresh, got %d: %v", status, refreshed)
	}
	newAccessToken, _ := refreshed["access_token"].(string)
	newRefreshTok, _ := refreshed["refresh_token"].(string)
	if newAccessToken == "" || newRefreshTok == "" {
		t.Fatalf("expected new access_token/refresh_token, got %v", refreshed)
	}
	// Only the refresh token is guaranteed to differ: it's opaque random
	// bytes. The new access token can legitimately be byte-identical to the
	// old one if minted within the same wall-clock second (RS256/PKCS1v15
	// signing is deterministic, and the claims — sub/role/iss/aud/iat/exp —
	// would then match exactly) — that's not a bug, just not something to
	// assert on. Rotation is proven below by the *old* refresh token no
	// longer working, not by the access token's bytes changing.
	if newRefreshTok == refreshTok {
		t.Fatalf("expected the rotated refresh token to differ from the original")
	}

	whoamiResp2 := whoami(t, resourceSrv.URL, newAccessToken)
	if whoamiResp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /whoami with the refreshed token, got %d", whoamiResp2.StatusCode)
	}

	// Replaying the original (now-rotated-away) refresh token must fail.
	status, _ = refreshToken(t, authSrv.URL, refreshTok)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 replaying a rotated-away refresh token, got %d", status)
	}

	// Replaying the original authorization code must also fail.
	status, _ = exchangeCode(t, authSrv.URL, code, verifier, clientID, redirectURI)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 replaying an already-used code, got %d", status)
	}
}

func TestAuthorizePOSTRejectsWrongPassword(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)
	clientID, redirectURI := registerTestClient(t, authSrv.URL)
	_, challenge := newPKCEPair(t)

	resp := login(t, authSrv.URL, clientID, redirectURI, challenge, "", "admin", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for wrong password, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "invalid credentials") {
		t.Fatalf("expected body to mention invalid credentials, got: %s", respBody)
	}
}

func TestAuthorizePOSTRejectsDeactivatedUser(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)
	clientID, redirectURI := registerTestClient(t, authSrv.URL)
	_, challenge := newPKCEPair(t)

	postUsers(t, authSrv.URL, "admin", "admin", []map[string]any{
		{"username": "alice", "roles": []string{"customer", "deactivated"}},
	}, http.StatusOK)

	resp := login(t, authSrv.URL, clientID, redirectURI, challenge, "", "alice", "alice")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a deactivated user's login, got %d", resp.StatusCode)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "invalid credentials") {
		t.Fatalf("expected body to mention invalid credentials, got: %s", respBody)
	}

	// Reactivating (removing the "deactivated" role) must let the same
	// credentials log in again.
	postUsers(t, authSrv.URL, "admin", "admin", []map[string]any{
		{"username": "alice", "roles": []string{"customer"}},
	}, http.StatusOK)
	resp2 := login(t, authSrv.URL, clientID, redirectURI, challenge, "", "alice", "alice")
	if resp2.StatusCode != http.StatusFound {
		t.Fatalf("expected 302 (successful login redirect) after reactivation, got %d", resp2.StatusCode)
	}
}

func TestTokenRejectsBadPKCEVerifier(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)
	clientID, redirectURI := registerTestClient(t, authSrv.URL)
	_, challenge := newPKCEPair(t)

	loginResp := login(t, authSrv.URL, clientID, redirectURI, challenge, "", "admin", "admin")
	loc, err := loginResp.Location()
	if err != nil {
		t.Fatalf("expected a Location header: %v", err)
	}
	code := loc.Query().Get("code")

	status, body := exchangeCode(t, authSrv.URL, code, "wrong-verifier-that-does-not-match-the-challenge", clientID, redirectURI)
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400 for a bad PKCE verifier, got %d", status)
	}
	if _, ok := body["access_token"]; ok {
		t.Fatalf("expected no access_token in the error response, got %v", body)
	}
}

func TestResourceServerRejectsMissingOrInvalidToken(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)
	resourceSrv := newFakeResourceServer(t, authSrv.URL+"/.well-known/jwks.json")

	if resp := whoami(t, resourceSrv.URL, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with no Authorization header, got %d", resp.StatusCode)
	}
	if resp := whoami(t, resourceSrv.URL, "not-a-real-jwt"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 with a garbage token, got %d", resp.StatusCode)
	}

	// A syntactically valid RS256 JWT signed by a *different*, freshly
	// generated key, but claiming the real server's kid, must still be
	// rejected — proves signature verification (not just "well-formed
	// JWT with a recognized kid") is actually enforced.
	forgedKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate forged key: %v", err)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"sub": "admin", "role": []string{"admin"},
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	forged.Header["kid"] = signingKeyID
	forgedString, err := forged.SignedString(forgedKey)
	if err != nil {
		t.Fatalf("failed to sign forged token: %v", err)
	}
	if resp := whoami(t, resourceSrv.URL, forgedString); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for a token signed by the wrong key, got %d", resp.StatusCode)
	}
}

func TestRegisterRejectsNonLoopbackRedirectURI(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)

	body, _ := json.Marshal(map[string]any{"redirect_uris": []string{"https://evil.example.com/callback"}})
	resp, err := http.Post(authSrv.URL+"/register", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST /register failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a non-loopback redirect_uri, got %d", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if _, ok := out["client_id"]; ok {
		t.Fatalf("expected no client_id in the error response, got %v", out)
	}
}

func getUsers(t *testing.T, authServerURL, username, password string) []map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, authServerURL+"/admin/users", nil)
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /admin/users failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from GET /admin/users, got %d", resp.StatusCode)
	}
	var out []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("failed to decode /admin/users response: %v", err)
	}
	return out
}

func postUsers(t *testing.T, authServerURL, username, password string, entries []map[string]any, wantStatus int) {
	t.Helper()
	raw, _ := json.Marshal(entries)
	req, _ := http.NewRequest(http.MethodPost, authServerURL+"/admin/users", strings.NewReader(string(raw)))
	req.SetBasicAuth(username, password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /admin/users failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("POST /admin/users: expected %d, got %d: %s", wantStatus, resp.StatusCode, respBody)
	}
}

func deleteUser(t *testing.T, authServerURL, username, password, target string, wantStatus int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, authServerURL+"/admin/users/"+target, nil)
	req.SetBasicAuth(username, password)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /admin/users/%s failed: %v", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("DELETE /admin/users/%s: expected %d, got %d", target, wantStatus, resp.StatusCode)
	}
}

func containsUser(users []map[string]any, username string, roles []string) bool {
	for _, u := range users {
		if u["username"] != username {
			continue
		}
		if roles == nil {
			return true
		}
		gotRoles, _ := u["roles"].([]any)
		if len(gotRoles) != len(roles) {
			return false
		}
		for i, r := range roles {
			if gotRoles[i] != r {
				return false
			}
		}
		return true
	}
	return false
}

func TestAdminUsersAPI(t *testing.T) {
	authSrv, _ := newTestAuthServer(t)

	// Unauthenticated.
	resp, err := http.Get(authSrv.URL + "/admin/users")
	if err != nil {
		t.Fatalf("GET /admin/users failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no Basic Auth, got %d", resp.StatusCode)
	}

	// Authenticated as admin: lists the built-in admin user.
	users := getUsers(t, authSrv.URL, "admin", "admin")
	if !containsUser(users, "admin", []string{"admin"}) {
		t.Fatalf("expected built-in admin user in list, got %v", users)
	}

	// Add alice/finance.
	postUsers(t, authSrv.URL, "admin", "admin", []map[string]any{
		{"username": "alice", "roles": []string{"finance"}},
	}, http.StatusOK)
	users = getUsers(t, authSrv.URL, "admin", "admin")
	if !containsUser(users, "alice", []string{"finance"}) {
		t.Fatalf("expected alice/finance in list, got %v", users)
	}

	// Re-add alice with different roles: replace, not merge.
	postUsers(t, authSrv.URL, "admin", "admin", []map[string]any{
		{"username": "alice", "roles": []string{"manager"}},
	}, http.StatusOK)
	users = getUsers(t, authSrv.URL, "admin", "admin")
	if !containsUser(users, "alice", []string{"manager"}) {
		t.Fatalf("expected alice's roles replaced with [manager], got %v", users)
	}

	// Alice can log in herself and gets her current role in the token.
	clientID, redirectURI := registerTestClient(t, authSrv.URL)
	verifier, challenge := newPKCEPair(t)
	loginResp := login(t, authSrv.URL, clientID, redirectURI, challenge, "", "alice", "alice")
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("expected alice/alice to log in successfully, got %d", loginResp.StatusCode)
	}
	loc, _ := loginResp.Location()
	code := loc.Query().Get("code")
	status, body := exchangeCode(t, authSrv.URL, code, verifier, clientID, redirectURI)
	if status != http.StatusOK {
		t.Fatalf("expected 200 exchanging alice's code, got %d: %v", status, body)
	}
	aliceToken, _ := body["access_token"].(string)
	parsed, _, err := new(jwt.Parser).ParseUnverified(aliceToken, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("failed to parse alice's token: %v", err)
	}
	parsedClaims := parsed.Claims.(jwt.MapClaims)
	gotRoles, _ := parsedClaims["role"].([]any)
	if len(gotRoles) != 1 || gotRoles[0] != "manager" {
		t.Fatalf("expected alice's token role claim to be [manager], got %v", parsedClaims["role"])
	}

	// admin cannot be deleted.
	deleteUser(t, authSrv.URL, "admin", "admin", "admin", http.StatusForbidden)

	// alice can be deleted.
	deleteUser(t, authSrv.URL, "admin", "admin", "alice", http.StatusNoContent)
	users = getUsers(t, authSrv.URL, "admin", "admin")
	if containsUser(users, "alice", nil) {
		t.Fatalf("expected alice to be gone after delete, got %v", users)
	}

	// A non-admin user cannot call the admin API.
	postUsers(t, authSrv.URL, "admin", "admin", []map[string]any{
		{"username": "bob", "roles": []string{"finance"}},
	}, http.StatusOK)
	postUsers(t, authSrv.URL, "bob", "bob", []map[string]any{
		{"username": "carol", "roles": []string{"finance"}},
	}, http.StatusForbidden)
}
