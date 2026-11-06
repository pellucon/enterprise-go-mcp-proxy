// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TokenResponse is an RFC 6749 §5.1 access token response.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token,omitempty"`
	Scope        string `json:"scope,omitempty"`
}

type tokenErrorResponse struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func exchangeCode(ctx context.Context, tokenEndpoint, code, verifier, clientID, redirectURI string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	return postTokenRequest(ctx, tokenEndpoint, form)
}

// RefreshToken performs the refresh_token grant. Exported for reuse by
// future CLIs built on this package — the root cli/ tool itself never calls
// this, since it does a fresh Login on every process invocation rather than
// persisting a session across restarts (see cli/main.go's doc comment).
// Note: demo-authorization-
// server's refresh grant does not actually validate clientID against the
// original authorization (most real providers do) — it's still required
// here for provider-agnosticism.
func RefreshToken(ctx context.Context, tokenEndpoint, refreshToken, clientID string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	}
	return postTokenRequest(ctx, tokenEndpoint, form)
}

func postTokenRequest(ctx context.Context, tokenEndpoint string, form url.Values) (*TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token request to %s failed: %w", tokenEndpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var tokenErr tokenErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&tokenErr)
		if tokenErr.Error != "" {
			return nil, fmt.Errorf("token request to %s failed: %s: %s", tokenEndpoint, tokenErr.Error, tokenErr.ErrorDescription)
		}
		return nil, fmt.Errorf("token request to %s returned %d", tokenEndpoint, resp.StatusCode)
	}

	var out TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("token response from %s did not include an access_token", tokenEndpoint)
	}
	return &out, nil
}
