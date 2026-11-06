// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// DefaultLoginTimeout bounds how long loginViaBrowser waits for the user
// to complete login in their browser before giving up.
const DefaultLoginTimeout = 5 * time.Minute

type browserLoginParams struct {
	Listener              net.Listener
	AuthorizationEndpoint string
	ClientID              string
	RedirectURI           string
	Challenge             string
	State                 string
	Audience              string
	Scope                 string
	Timeout               time.Duration
	Output                io.Writer
}

// loginViaBrowser prints the authorization URL for the user to open
// themselves — this package never launches a browser process — and waits
// for exactly one callback request on p.Listener, validating the returned
// state before returning the authorization code.
func loginViaBrowser(ctx context.Context, p browserLoginParams) (string, error) {
	out := p.Output
	if out == nil {
		out = os.Stderr
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = DefaultLoginTimeout
	}

	authURL, err := buildAuthorizeURL(p.AuthorizationEndpoint, p.ClientID, p.RedirectURI, p.Challenge, p.State, p.Audience, p.Scope)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(out, "Open this URL in a browser to log in:\n\n  %s\n\n", authURL)

	type result struct {
		code string
		err  error
	}
	resultCh := make(chan result, 1)
	// report delivers the first callback's outcome; any later one is
	// dropped instead of blocking its handler forever.
	report := func(res result) {
		select {
		case resultCh <- res:
		default:
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// state first: a callback without this login's state didn't come
		// from its authorization server, so it can't even end the login
		// (any local process or web page can request this URL). It's
		// answered and otherwise ignored, and the login keeps waiting.
		if q.Get("state") != p.State {
			http.Error(w, "Login failed (state mismatch). You can close this tab.", http.StatusBadRequest)
			return
		}
		if errParam := q.Get("error"); errParam != "" {
			http.Error(w, "Authorization failed. You can close this tab.", http.StatusOK)
			report(result{err: fmt.Errorf("authorization failed: %s: %s", errParam, q.Get("error_description"))})
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "Login failed (no code). You can close this tab.", http.StatusBadRequest)
			report(result{err: fmt.Errorf("callback did not include a code")})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><p>Login successful. You can close this tab.</p></body></html>")
		report(result{code: code})
	})
	srv := &http.Server{Handler: mux}

	go func() { _ = srv.Serve(p.Listener) }()
	// srv.Shutdown waits for the in-flight /callback handler (which has
	// already written its full response by the time resultCh is read
	// below) to finish before returning, so the browser always gets a
	// complete response rather than a reset connection — unlike Close,
	// which would tear the connection down immediately.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	select {
	case res := <-resultCh:
		if res.err != nil {
			return "", res.err
		}
		return res.code, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out after %s waiting for browser login", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func buildAuthorizeURL(authorizationEndpoint, clientID, redirectURI, challenge, state, audience, scope string) (string, error) {
	u, err := url.Parse(authorizationEndpoint)
	if err != nil {
		return "", fmt.Errorf("invalid authorization_endpoint %q: %w", authorizationEndpoint, err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if audience != "" {
		q.Set("audience", audience)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}
