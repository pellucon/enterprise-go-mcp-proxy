// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// demo-authorization-server is a minimal OAuth 2.1 authorization server
// (authorization_code + PKCE, refresh tokens, dynamic client registration
// restricted to loopback redirect URIs, JWKS) used to demonstrate/exercise
// a real browser-login flow against go-proxy-style RS256-JWT-verifying
// resource servers. It mints its own ephemeral RSA keypair at startup — no
// key material is ever written to disk — so every restart invalidates all
// previously-issued tokens/codes/sessions, matching its "demo" framing.
//
// Every user's password is always identical to its username (e.g.
// admin/admin) — there is no real credential store. The built-in "admin"
// user (role "admin") always exists and can't be added, edited, or deleted
// via the admin API. See users.go/clients.go/authorize.go/token.go for the
// individual pieces, and demo-authorization-server/client and
// demo-authorization-server/scripts/manage-users.sh for two independent
// ways to drive the user-management API.
//
// Deployed to k8s like any other server in this repo, via scripts/deploy.sh
// --deploy-auth-server/-das (see k8s/ in this directory), at a fixed
// replicas: 1 — every bit of its state above is in-process memory with no
// cross-replica coordination, so a second replica would mint tokens the
// other replica couldn't verify. Its k8s/service.yaml is a
// type: LoadBalancer on port 8102, the same port and URL
// (http://localhost:8102) it answered at as a host process, so nothing
// that depends on that address (proxy-server.cfg's auth.issuer, both
// CLIs' -auth-server default) needs to change. Unlike claude-bridge/
// main.go (which stays host-only — no `claude` CLI or authenticated
// session exists inside a container), this server has no such
// constraint.
package main

import (
	"flag"
	"net/http"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

func main() {
	var port, issuer, audience, roleClaim, logLevel string
	flag.StringVar(&port, "port", ":8102", "Port to listen on, e.g. :8102")
	flag.StringVar(&issuer, "issuer", "", "Stable issuer identity string embedded in minted tokens' \"iss\" claim and the metadata document's \"issuer\" field, overriding the default per-request-Host-derived value. Needed whenever a deployer's config pins a fixed auth.issuer to check against (e.g. via scripts/deploy.sh --start-demo-auth) — leave empty for ad hoc local/manual use, which keeps working exactly as before.")
	flag.StringVar(&audience, "audience", "", "Stable audience identity string embedded in minted tokens' \"aud\" claim, identifying the resource server the token is intended for (e.g. \"go-proxy\") — distinct from -issuer, which identifies this authorization server itself. Empty (default) falls back to whatever -issuer resolves to, matching this server's original single-value behavior; set this explicitly whenever a deployer's config checks a fixed auth.audience.")
	flag.StringVar(&roleClaim, "role-claim", "", "JWT claim name minted tokens carry their roles under, instead of the default \"role\" — this server stands in for a real third-party IdP, and most of those don't emit roles under a claim literally named \"role\", so this lets a deployment's auth.roleClaim (CLAUDE.md's \"Auth and access control\" section) be exercised against a real token instead of only a synthetic test one. Empty (default) keeps minting under \"role\", matching this server's original behavior.")
	flag.StringVar(&logLevel, "log-level", "info", "Log severity: error, info, or debug. Set once at startup — this server has no live-reload mechanism.")
	flag.Parse()
	logging.Init(logLevel)

	if issuer != "" || audience != "" || roleClaim != "" {
		logging.Infof("startup configuration: issuer=%q audience=%q roleClaim=%q", issuer, audience, roleClaim)
	}

	state, err := newState(issuer, audience, roleClaim)
	if err != nil {
		logging.Fatalf("failed to initialize server state: %v", err)
	}

	handler := newHandler(state)

	logging.Debugf("Starting demo-authorization-server on port %s", port)
	if err := http.ListenAndServe(port, handler); err != nil {
		logging.Fatal(err)
	}
}
