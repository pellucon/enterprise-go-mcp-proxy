// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type credentialLoginParams struct {
	AuthorizationEndpoint string
	ClientID              string
	RedirectURI           string
	Challenge             string
	State                 string
	Username              string
	Password              string
}

// loginViaCredentials POSTs credentials directly to AuthorizationEndpoint,
// skipping the browser entirely. This only works against
// demo-authorization-server's deliberate direct-POST login shortcut (see
// its authorize.go) — real OAuth providers require the actual browser
// redirect flow, and there's no way to automate around that by design:
// avoiding exactly this (a third party handling raw credentials) is the
// whole point of OAuth's redirect-based login.
func loginViaCredentials(ctx context.Context, p credentialLoginParams) (string, error) {
	form := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.ClientID},
		"redirect_uri":          {p.RedirectURI},
		"code_challenge":        {p.Challenge},
		"code_challenge_method": {"S256"},
		"state":                 {p.State},
		"username":              {p.Username},
		"password":              {p.Password},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.AuthorizationEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("failed to build credential login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("credential login request to %s failed: %w", p.AuthorizationEndpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("credential login to %s failed (status %d) — check username/password", p.AuthorizationEndpoint, resp.StatusCode)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("credential login succeeded but no redirect Location was returned: %w", err)
	}
	if got := loc.Query().Get("state"); got != p.State {
		return "", fmt.Errorf("state mismatch in credential login response — possible CSRF, aborting")
	}
	code := loc.Query().Get("code")
	if code == "" {
		return "", fmt.Errorf("credential login redirect did not include a code")
	}
	return code, nil
}
