// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type registerRequest struct {
	RedirectURIs []string `json:"redirect_uris"`
	ClientName   string   `json:"client_name,omitempty"`
}

type registerResponse struct {
	ClientID string `json:"client_id"`
}

// registerClient performs RFC 7591 dynamic client registration, returning
// the assigned client_id.
func registerClient(ctx context.Context, registrationEndpoint, redirectURI, clientName string) (string, error) {
	body, err := json.Marshal(registerRequest{RedirectURIs: []string{redirectURI}, ClientName: clientName})
	if err != nil {
		return "", fmt.Errorf("failed to encode registration request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to build registration request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("client registration request to %s failed: %w", registrationEndpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("client registration at %s returned %d", registrationEndpoint, resp.StatusCode)
	}
	var out registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("failed to parse registration response: %w", err)
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("registration response from %s did not include a client_id", registrationEndpoint)
	}
	return out.ClientID, nil
}
