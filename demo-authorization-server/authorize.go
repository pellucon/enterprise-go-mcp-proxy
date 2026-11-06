// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// authorize.go implements the /authorize step of the flow: rendering a
// login form and, on submission, authenticating the user and issuing a
// one-time authorization code bound to the PKCE challenge.
package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sync"
	"time"
)

const authCodeTTL = 60 * time.Second

type authCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Subject       string
	Roles         []string
	ExpiresAt     time.Time
}

type authCodeStore struct {
	mu    sync.Mutex
	codes map[string]authCode
}

func newAuthCodeStore() *authCodeStore {
	return &authCodeStore{codes: map[string]authCode{}}
}

func (s *authCodeStore) issue(rec authCode) (string, error) {
	code, err := randomOpaqueToken()
	if err != nil {
		return "", err
	}
	rec.ExpiresAt = time.Now().Add(authCodeTTL)
	s.mu.Lock()
	s.codes[code] = rec
	s.mu.Unlock()
	return code, nil
}

// consume looks up and deletes code in one step — a code is single-use, so
// even a successful lookup removes it, making any replay of the same code
// (this function called again with it) always miss, regardless of whether
// the first call succeeded or the code had merely expired.
func (s *authCodeStore) consume(code string) (authCode, bool) {
	s.mu.Lock()
	rec, ok := s.codes[code]
	delete(s.codes, code)
	s.mu.Unlock()
	if !ok || time.Now().After(rec.ExpiresAt) {
		return authCode{}, false
	}
	return rec, true
}

// randomOpaqueToken generates a 32-byte, base64url-encoded random string —
// used for authorization codes and refresh tokens alike (see token.go),
// neither of which need to be self-verifying JWTs since both are always
// looked up server-side by this same server.
func randomOpaqueToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("failed to generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

var loginFormTemplate = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html><head><title>demo-authorization-server login</title></head>
<body>
<h1>Log in</h1>
{{if .Error}}<p style="color:red">{{.Error}}</p>{{end}}
<form method="POST" action="/authorize">
  <input type="hidden" name="response_type" value="{{.ResponseType}}">
  <input type="hidden" name="client_id" value="{{.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.CodeChallengeMethod}}">
  <input type="hidden" name="state" value="{{.State}}">
  <label>Username <input type="text" name="username"></label><br>
  <label>Password <input type="password" name="password"></label><br>
  <button type="submit">Log in</button>
</form>
</body></html>
`))

type loginFormData struct {
	ResponseType         string
	ClientID             string
	RedirectURI          string
	CodeChallenge        string
	CodeChallengeMethod  string
	State                string
	Error                string
}

// authorizeParams holds the OAuth query/form parameters this server
// understands, whether they arrived via GET's query string or POST's form
// body.
type authorizeParams struct {
	ResponseType        string
	ClientID             string
	RedirectURI          string
	CodeChallenge        string
	CodeChallengeMethod  string
	State                string
}

func parseAuthorizeParams(values interface{ Get(string) string }) authorizeParams {
	return authorizeParams{
		ResponseType:        values.Get("response_type"),
		ClientID:            values.Get("client_id"),
		RedirectURI:         values.Get("redirect_uri"),
		CodeChallenge:       values.Get("code_challenge"),
		CodeChallengeMethod: values.Get("code_challenge_method"),
		State:               values.Get("state"),
	}
}

// validateAuthorizeParams re-checks p against the client store regardless
// of whether it arrived from a fresh GET or was echoed back via POST's
// hidden fields — a malicious client controls what it renders, so this
// server never trusts its own hidden fields blindly.
func validateAuthorizeParams(clients *clientStore, p authorizeParams) (oauthClient, error) {
	if p.ResponseType != "code" {
		return oauthClient{}, fmt.Errorf("response_type must be \"code\"")
	}
	if p.ClientID == "" || p.RedirectURI == "" || p.CodeChallenge == "" {
		return oauthClient{}, fmt.Errorf("client_id, redirect_uri, and code_challenge are required")
	}
	if p.CodeChallengeMethod != "S256" {
		return oauthClient{}, fmt.Errorf("code_challenge_method must be \"S256\"")
	}
	client, ok := clients.lookup(p.ClientID)
	if !ok {
		return oauthClient{}, fmt.Errorf("unknown client_id %q", p.ClientID)
	}
	found := false
	for _, uri := range client.RedirectURIs {
		if uri == p.RedirectURI {
			found = true
			break
		}
	}
	if !found {
		return oauthClient{}, fmt.Errorf("redirect_uri %q is not registered for this client", p.RedirectURI)
	}
	return client, nil
}

func handleAuthorizeGet(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := parseAuthorizeParams(r.URL.Query())
		if _, err := validateAuthorizeParams(state.clients, p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		renderLoginForm(w, p, "")
	}
}

func handleAuthorizePost(state *State) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form body", http.StatusBadRequest)
			return
		}
		p := parseAuthorizeParams(r.PostForm)
		if _, err := validateAuthorizeParams(state.clients, p); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		username := r.PostForm.Get("username")
		password := r.PostForm.Get("password")
		u, ok := state.users.authenticate(username, password)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			renderLoginForm(w, p, "invalid credentials")
			return
		}

		code, err := state.authCodes.issue(authCode{
			ClientID:      p.ClientID,
			RedirectURI:   p.RedirectURI,
			CodeChallenge: p.CodeChallenge,
			Subject:       u.Username,
			Roles:         u.Roles,
		})
		if err != nil {
			http.Error(w, "failed to issue authorization code", http.StatusInternalServerError)
			return
		}

		redirectURL := p.RedirectURI + "?code=" + url.QueryEscape(code)
		if p.State != "" {
			redirectURL += "&state=" + url.QueryEscape(p.State)
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
	}
}

func renderLoginForm(w http.ResponseWriter, p authorizeParams, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = loginFormTemplate.Execute(w, loginFormData{
		ResponseType:        p.ResponseType,
		ClientID:            p.ClientID,
		RedirectURI:         p.RedirectURI,
		CodeChallenge:       p.CodeChallenge,
		CodeChallengeMethod: p.CodeChallengeMethod,
		State:               p.State,
		Error:               errMsg,
	})
}
