// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

// Adapted from the root cli/ tool's client.go: commonFlags/connect/
// authRoundTripper/printContent/printStructuredContent. Duplicated rather
// than imported: both this package and the root cli/ are package main, and
// "package main can't cross-import another package main" is an established
// constraint in this repo (see CLAUDE.md's note on platform_servers_test.go's
// stand-ins) — oauthclient itself, an ordinary importable package,
// is reused directly rather than duplicated.

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/pellucon/enterprise-go-mcp-proxy/oauthclient"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// commonFlags are accepted once, before any section/tool command — how to
// reach the proxy and how to log in against the OAuth authorization
// server. Same flags/defaults as the root cli/ tool's own commonFlags.
type commonFlags struct {
	endpoint     string
	authServer   string
	authIssuer   string
	clientID     string
	redirectPort int
	username     string
	password     string
}

func registerCommonFlags(fs *flag.FlagSet) *commonFlags {
	cf := &commonFlags{}
	fs.StringVar(&cf.endpoint, "endpoint", "http://localhost/mcp",
		"Proxy MCP endpoint URL (default targets the k8s deployment's stable LoadBalancer port; "+
			"\":80\" is the default HTTP port so it's omitted from the URL — http://localhost/mcp "+
			"and http://localhost:80/mcp are identical)")
	fs.StringVar(&cf.authServer, "auth-server", "http://localhost:8102", "Base URL of the OAuth authorization server to log in against")
	fs.StringVar(&cf.authIssuer, "auth-issuer", "", "The issuer the authorization server's metadata must name, when it differs from -auth-server (a server reached at another address than its issuer). Default: -auth-server")
	fs.StringVar(&cf.clientID, "client-id", "", "Pre-registered OAuth client_id, skipping dynamic client registration")
	fs.IntVar(&cf.redirectPort, "redirect-port", 0, "Fixed local port to bind for the OAuth redirect_uri (0 = ephemeral); only meaningful together with -client-id")
	fs.StringVar(&cf.username, "username", "", "Username for a direct credential login, skipping the browser (must be paired with -password; demo-authorization-server only)")
	fs.StringVar(&cf.password, "password", "", "Password for a direct credential login (see -username)")
	return cf
}

// connect logs in once (browser flow by default, or a direct credential
// POST if cf.username/cf.password are both set) and opens the single MCP
// session the CLI's whole run — one-shot or REPL — is driven through. One
// session suffices for every section: the gateway already namespaces
// every backend's tools under "garden-*.", so there's no per-backend
// session to manage here.
func (cf *commonFlags) connect(ctx context.Context) (*mcp.ClientSession, error) {
	resp, err := oauthclient.Login(ctx, oauthclient.LoginOptions{
		AuthServerURL: cf.authServer,
		Issuer:        cf.authIssuer,
		ClientID:      cf.clientID,
		ClientName:    "go-gardening-demo-cli",
		RedirectPort:  cf.redirectPort,
		Username:      cf.username,
		Password:      cf.password,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to log in: %w", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "go-gardening-demo-cli", Version: "1.0.0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   cf.endpoint,
		HTTPClient: &http.Client{Transport: &authRoundTripper{token: resp.AccessToken}},
	}, nil)
}

type authRoundTripper struct {
	token string
}

func (a *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(cloned)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

// printContent renders MCP content blocks; text content (the common case)
// is printed as plain text, anything else falls back to a JSON dump.
func printContent(content []mcp.Content) {
	for _, c := range content {
		if tc, ok := c.(*mcp.TextContent); ok {
			fmt.Println(tc.Text)
			continue
		}
		raw, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			fmt.Printf("%v\n", c)
			continue
		}
		fmt.Println(string(raw))
	}
}

// printStructuredContent prints a tool's typed StructuredContent as
// indented JSON, if it set one — a no-op otherwise.
func printStructuredContent(structuredContent any) {
	if structuredContent == nil {
		return
	}
	raw, err := json.MarshalIndent(structuredContent, "", "  ")
	if err != nil {
		fmt.Printf("%v\n", structuredContent)
		return
	}
	fmt.Println(string(raw))
}
