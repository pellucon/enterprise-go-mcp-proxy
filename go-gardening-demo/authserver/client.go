// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package authserver is a small HTTP client for the gardening demo's
// authorization server's admin API — today that's always
// demo-authorization-server (see demo-authorization-server/users.go and
// admin_handlers.go), but go-gardening-demo's own code refers to it
// generically as "the auth server" (flags/vars named authServer*, never
// demoAuth*) since the URL is caller-supplied, not hardcoded to that one
// implementation — see garden-customer-admin/garden-staff-admin's
// -auth-server-url flag. Shared by both servers — they mutate the auth
// server's user/role store the same way, so this avoids duplicating the
// same HTTP/Basic-Auth/JSON logic in each (an ordinary shared-package
// extraction, not a "package main can't import package main" workaround —
// demo-authorization-server's own code isn't reused here, this is a client
// of its HTTP API).
package authserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// User mirrors demo-authorization-server's own {"username","roles"} shape.
type User struct {
	Username string   `json:"username"`
	Roles    []string `json:"roles"`
}

// Client is a small HTTP client for demo-authorization-server's admin API,
// authenticating with HTTP Basic Auth (matching demo-authorization-
// server's own username==password demo convention).
type Client struct {
	BaseURL    string
	Username   string
	Password   string
	HTTPClient *http.Client
}

// New returns a Client using http.DefaultClient. baseURL should have no
// trailing slash (e.g. "http://host.docker.internal:8102").
func New(baseURL, username, password string) *Client {
	return &Client{BaseURL: baseURL, Username: username, Password: password, HTTPClient: http.DefaultClient}
}

// UpsertUsers POSTs one or more {username, roles} entries to
// /admin/users — creating or fully replacing (never merging) each entry's
// role list, per demo-authorization-server's own upsertMany semantics.
// The whole batch is rejected atomically if any entry is invalid (e.g.
// names the built-in "admin" user) — see users.go's upsertMany.
func (c *Client) UpsertUsers(ctx context.Context, users []User) error {
	body, err := json.Marshal(users)
	if err != nil {
		return fmt.Errorf("authserver: failed to marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/admin/users", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("authserver: failed to build request: %w", err)
	}
	req.SetBasicAuth(c.Username, c.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("authserver: POST /admin/users request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("authserver: POST /admin/users returned %s: %s", resp.Status, msg)
	}
	return nil
}

// UpsertUser is UpsertUsers for a single entry.
func (c *Client) UpsertUser(ctx context.Context, username string, roles []string) error {
	return c.UpsertUsers(ctx, []User{{Username: username, Roles: roles}})
}
