// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	RESOURCE_METADATA_BACKEND_PORT = ":8111"
	RESOURCE_METADATA_PROXY_PORT   = ":8112"
	RESOURCE_METADATA_PROXY_URL    = "http://localhost:8112/mcp"
	resourceMetadataTestIssuer     = "https://idp.example.com"
	resourceMetadataTestResource   = "https://go-proxy.example.com/mcp"
)

// mintExternalToken mints a client-facing JWT signed with privKey, carrying
// resourceMetadataTestIssuer as "iss" plus whatever extra claims are given
// (e.g. a custom role-claim name instead of "role") — a hermetic keypair
// distinct from mcp_client_test.go's myRsaPrivateKey/./public.pem, following
// internal_auth_test.go's generateTestRSAKeyPair pattern rather than
// depending on repo state.
func mintExternalToken(t *testing.T, privKey *rsa.PrivateKey, sub string, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": sub,
		"iss": resourceMetadataTestIssuer,
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	signed, err := token.SignedString(privKey)
	if err != nil {
		t.Fatalf("failed to mint test token: %v", err)
	}
	return signed
}

// bearerRoundTripper attaches a plain client Authorization: Bearer header to
// every outbound request — a standalone, package-var-free equivalent of
// mcp_client_test.go's testTokenRoundTripper (which reads from the
// shared testToken var), so this test's subtests can each use their own
// token without risk of clobbering that shared var.
type bearerRoundTripper string

func (b bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(cloned)
}

// createPingServer is a trivial in-process backend: no ownership logic, its
// only job is to exist as something an authenticated/authorized caller can
// call, so tests here can focus purely on the HTTP-layer auth gate, PRM
// document, and role-claim remapping rather than backend business logic.
// It also returns an accessor for the real go-proxy-minted relay headers
// ping's handler most recently received, letting a subtest capture a
// genuinely valid relay token (via one ordinary authenticated call) rather
// than needing to independently mint one from shared key material — the
// proxy no longer reads its internal signing key from a file at all, so
// there's no shared file left to mint against.
func createPingServer() (*mcp.Server, func() relayheaders.Set) {
	server := mcp.NewServer(&mcp.Implementation{Name: "demo-service", Version: "1.0.0"}, nil)
	var mu sync.Mutex
	var last relayheaders.Set
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ping",
		Description: "Replies pong",
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		mu.Lock()
		last = relayheaders.Extract(req.GetExtra().Header)
		mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	return server, func() relayheaders.Set {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// TestProtectedResourceMetadataAndRoleClaim exercises the plan implemented
// this session (see CLAUDE.md's "Auth and access control" section /
// auth.Config's ResourceURL and RoleClaim fields): a generic MCP client
// (unlike this repo's own cli, which always logs in out-of-band)
// gets a real HTTP 401 with a WWW-Authenticate: Bearer resource_metadata="..."
// hint on an unauthenticated request, can fetch that RFC 9728 document, and
// a caller whose IdP puts roles under a non-"role" claim is still
// authorized correctly.
func TestProtectedResourceMetadataAndRoleClaim(t *testing.T) {
	tmpDir := t.TempDir()

	_, externalPubPEM, externalPrivKey := generateTestRSAKeyPair(t)
	if err := os.WriteFile(filepath.Join(tmpDir, "external-public.pem"), externalPubPEM, 0o644); err != nil {
		t.Fatalf("failed to write external public key: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pingServer, lastPingHeaders := createPingServer()
	startTestBackend(t, ctx, cancel, RESOURCE_METADATA_BACKEND_PORT, pingServer)

	natsURL := startTestNATSServer(t)

	cfg := `{
  "discovery": {
    "discoveryMode": "static",
    "listedTools": ["*"],
    "servers": [
      { "serverName": "demo-service", "serverUrl": "http://localhost` + RESOURCE_METADATA_BACKEND_PORT + `/mcp" }
    ]
  },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "user", "allowed_tools": ["demo-service.*"], "allowed_prompts": [], "allowed_resources": [], "allowed_resource_templates": [] }
    ]
  },
  "auth": {
    "publicKeyPath": "` + filepath.Join(tmpDir, "external-public.pem") + `",
    "jwksUrl": "",
    "issuer": "` + resourceMetadataTestIssuer + `",
    "audience": "",
    "resourceUrl": "` + resourceMetadataTestResource + `",
    "roleClaim": "custom_role"
  },
  "internalAuth": {
    "issuer": "go-proxy",
    "audience": "go-proxy-backend",
    "ttlSeconds": 60,
    "relay": { "demo-service": ["demo-service.ping"] }
  },
  "selfUrl": "` + RESOURCE_METADATA_PROXY_URL + `",
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": "` + natsURL + `"
}`

	cfgPath := filepath.Join(tmpDir, "resource-metadata-test.cfg")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	startTestProxy(t, ctx, cancel, cfgPath, RESOURCE_METADATA_PROXY_PORT)

	t.Run("unauthenticated request gets 401 with a resource_metadata hint", func(t *testing.T) {
		resp, err := http.Post(RESOURCE_METADATA_PROXY_URL, "application/json", nil)
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
		want := `Bearer resource_metadata="http://localhost` + RESOURCE_METADATA_PROXY_PORT + `/.well-known/oauth-protected-resource"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("WWW-Authenticate = %q, want %q", got, want)
		}
	})

	t.Run("behind a TLS-terminating reverse proxy the hint uses X-Forwarded-Proto/Host", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, RESOURCE_METADATA_PROXY_URL, nil)
		if err != nil {
			t.Fatalf("failed to build request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-Proto", "https")
		req.Header.Set("X-Forwarded-Host", "mcp.acme.com, inner.proxy.local")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("unexpected transport error: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
		want := `Bearer resource_metadata="https://mcp.acme.com/.well-known/oauth-protected-resource"`
		if got := resp.Header.Get("WWW-Authenticate"); got != want {
			t.Errorf("WWW-Authenticate = %q, want %q", got, want)
		}
	})

	// Both the root well-known path and RFC 9728's path-inserted variant
	// (resourceMetadataTestResource's path is "/mcp") serve the same document.
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		t.Run("protected resource metadata document is served at "+path, func(t *testing.T) {
			resp, err := http.Get("http://localhost" + RESOURCE_METADATA_PROXY_PORT + path)
			if err != nil {
				t.Fatalf("unexpected transport error: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("expected 200, got %d", resp.StatusCode)
			}
			var doc struct {
				Resource             string   `json:"resource"`
				AuthorizationServers []string `json:"authorization_servers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
				t.Fatalf("failed to decode metadata document: %v", err)
			}
			if doc.Resource != resourceMetadataTestResource {
				t.Errorf("resource = %q, want %q", doc.Resource, resourceMetadataTestResource)
			}
			if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != resourceMetadataTestIssuer {
				t.Errorf("authorization_servers = %v, want [%q]", doc.AuthorizationServers, resourceMetadataTestIssuer)
			}
		})
	}

	t.Run("a role under a configured custom claim name is honored", func(t *testing.T) {
		token := mintExternalToken(t, externalPrivKey, "carol", map[string]any{"custom_role": "user"})
		client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   RESOURCE_METADATA_PROXY_URL,
			HTTPClient: &http.Client{Transport: bearerRoundTripper(token)},
		}, nil)
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer session.Close()

		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "demo-service.ping"})
		if err != nil {
			t.Fatalf("expected call to succeed with role read from custom_role claim, got error: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected call to succeed, got denial: %v", result.Content)
		}
	})

	t.Run("no role at all under the default role claim is denied", func(t *testing.T) {
		// Sanity check that the remap in the previous subtest is actually
		// doing something: the same token shape but without custom_role
		// (or with it absent) must still be denied, proving demo-service.*
		// isn't just universally allowed.
		token := mintExternalToken(t, externalPrivKey, "dave", nil)
		client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   RESOURCE_METADATA_PROXY_URL,
			HTTPClient: &http.Client{Transport: bearerRoundTripper(token)},
		}, nil)
		if err != nil {
			t.Fatalf("failed to connect: %v", err)
		}
		defer session.Close()

		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "demo-service.ping"}); err == nil {
			t.Error("expected call with no role to be denied")
		}
	})

	t.Run("a relay token passes the HTTP-layer gate, an identity assertion doesn't", func(t *testing.T) {
		// go-proxy no longer reads its internal signing key from a shared
		// file (it generates one in memory - see
		// internal_signing_key.go's bootstrapInternalSigningKey), so a
		// genuinely valid assertion can't be minted independently anymore.
		// Instead, capture a real relay token the same way TestBackendRelay
		// does: make one ordinary authenticated call and read the headers
		// go-proxy actually attached to it off createPingServer's capture
		// hook, then present the relay token back with no Authorization
		// header at all. The identity assertion from the same call must not
		// get through.
		sourceToken := mintExternalToken(t, externalPrivKey, "relay-source", map[string]any{"custom_role": "user"})
		sourceClient := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
		sourceSession, err := sourceClient.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   RESOURCE_METADATA_PROXY_URL,
			HTTPClient: &http.Client{Transport: bearerRoundTripper(sourceToken)},
		}, nil)
		if err != nil {
			t.Fatalf("failed to connect to capture a real assertion: %v", err)
		}
		if _, err := sourceSession.CallTool(ctx, &mcp.CallToolParams{Name: "demo-service.ping"}); err != nil {
			t.Fatalf("failed to make the call that captures a real assertion: %v", err)
		}
		sourceSession.Close()

		captured := lastPingHeaders()
		if captured.Assertion == "" || captured.RelayToken == "" {
			t.Fatalf("expected a real identity assertion and relay token to have been captured, got %+v", captured)
		}

		assertionClient := mcp.NewClient(&mcp.Implementation{Name: "replaying-backend", Version: "1.0.0"}, nil)
		if s, err := assertionClient.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   RESOURCE_METADATA_PROXY_URL,
			HTTPClient: &http.Client{Transport: relayheaders.Set{Assertion: captured.Assertion}.RoundTripper(nil)},
		}, nil); err == nil {
			s.Close()
			t.Error("an identity assertion alone passed the HTTP-layer 401 gate")
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "relaying-backend", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   RESOURCE_METADATA_PROXY_URL,
			HTTPClient: &http.Client{Transport: relayheaders.Set{RelayToken: captured.RelayToken}.RoundTripper(nil)},
		}, nil)
		if err != nil {
			t.Fatalf("expected a relay token to pass the HTTP-layer 401 gate, got connect error: %v", err)
		}
		defer session.Close()

		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "demo-service.ping"})
		if err != nil {
			t.Fatalf("expected relayed call to succeed, got error: %v", err)
		}
		if result.IsError {
			t.Fatalf("expected relayed call to succeed, got denial: %v", result.Content)
		}
	})
}
