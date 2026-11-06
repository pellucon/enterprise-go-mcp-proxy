// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	RELAY_ECHO_BACKEND_PORT       = ":8106"
	RELAY_RESTRICTED_BACKEND_PORT = ":8107"
	RELAY_TEST_PROXY_PORT         = ":8108"
	RELAY_TEST_PROXY_URL          = "http://localhost:8108/mcp"
)

// echoedHeaders is what a get_headers tool reports back: exactly the relay
// headers it received. This is how the test captures real, validly-minted
// tokens to then present directly back to the proxy itself, the way a
// (malicious or legitimate) backend could, rather than needing a real
// intermediary relay tool.
type echoedHeaders struct {
	Identity   string `json:"identity"`
	RelayToken string `json:"relayToken"`
	Callback   string `json:"callback"`
	ToolCallID string `json:"toolCallId"`
}

func addGetHeadersTool(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{Name: "get_headers", Description: "echoes back the relay headers it received"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			incoming := relayheaders.Extract(req.GetExtra().Header)
			payload, _ := json.Marshal(echoedHeaders{
				Identity:   incoming.Assertion,
				RelayToken: incoming.RelayToken,
				Callback:   incoming.CallbackURL,
				ToolCallID: incoming.ToolCallID,
			})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, nil, nil
		})
}

// createRelayEchoBackend is the backend the test config allows to relay.
func createRelayEchoBackend() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "echo-backend", Version: "1.0.0"}, nil)
	addGetHeadersTool(server)
	return server
}

// createRelayRestrictedBackend may not relay, and hosts a tool only the
// admin role may call.
func createRelayRestrictedBackend() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "restricted-backend", Version: "1.0.0"}, nil)
	addGetHeadersTool(server)
	mcp.AddTool(server, &mcp.Tool{Name: "secret_tool", Description: "a tool the narrow role cannot call directly"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "top secret data"}}}, nil, nil
		})
	return server
}

// relayHeaderTransport sets fixed headers on every request.
type relayHeaderTransport struct {
	headers map[string]string
}

func (t *relayHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func connectAsBackendRelay(ctx context.Context, url string, headers map[string]string) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "backend-relay-test-client", Version: "1.0.0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: &relayHeaderTransport{headers: headers}},
	}, nil)
}

// connectAsClient connects with a role's own client token, fixed on this
// session (unlike connectAsRole, which shares one token var across every
// session it opens - see docs/architecture/testing.md).
func connectAsClient(t *testing.T, ctx context.Context, role string, extra map[string]string) *mcp.ClientSession {
	t.Helper()
	token, err := GenerateTestTokenForRole(role)
	if err != nil {
		t.Fatalf("minting a %s token: %v", role, err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	for k, v := range extra {
		headers[k] = v
	}
	session, err := connectAsBackendRelay(ctx, RELAY_TEST_PROXY_URL, headers)
	if err != nil {
		t.Fatalf("connecting as %s: %v", role, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callExpectingRejection asserts a call with headers is refused, whether at
// connect time or on the call itself (createMiddleware gates every method,
// including the initial handshake), and returns the error.
func callExpectingRejection(t *testing.T, ctx context.Context, what string, headers map[string]string, tool string) error {
	t.Helper()
	session, err := connectAsBackendRelay(ctx, RELAY_TEST_PROXY_URL, headers)
	if err != nil {
		return err
	}
	defer session.Close()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool})
	if err == nil {
		t.Errorf("%s: call to %s succeeded (%v); it should have been rejected", what, tool, res.Content)
		return nil
	}
	return err
}

func getHeaders(t *testing.T, ctx context.Context, session *mcp.ClientSession, tool string) echoedHeaders {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool})
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	if result.IsError {
		t.Fatalf("calling %s returned an error result: %v", tool, result.Content)
	}
	var h echoedHeaders
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &h); err != nil {
		t.Fatalf("parsing %s's echoed headers: %v", tool, err)
	}
	return h
}

func tokenExpiry(t *testing.T, bearer string) time.Time {
	t.Helper()
	parsed, _, err := jwt.NewParser().ParseUnverified(strings.TrimPrefix(bearer, "Bearer "), jwt.MapClaims{})
	if err != nil {
		t.Fatalf("parsing token: %v", err)
	}
	exp, err := parsed.Claims.GetExpirationTime()
	if err != nil || exp == nil {
		t.Fatalf("token has no exp: %v", err)
	}
	return exp.Time
}

// TestBackendRelay covers what a backend can and can't do with the tokens
// it's given: the identity assertion is never a credential; only a server
// internalAuth.relay lists gets a relay token; a relayed call is limited to
// that server's targets and the user's own role, never reaches _admin.*,
// can't outlive or out-hop the original call, and keeps its tool-call ID.
func TestBackendRelay(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startTestBackend(t, ctx, cancel, RELAY_ECHO_BACKEND_PORT, createRelayEchoBackend())
	startTestBackend(t, ctx, cancel, RELAY_RESTRICTED_BACKEND_PORT, createRelayRestrictedBackend())

	tmpDir := t.TempDir()

	natsURL := startTestNATSServer(t)

	cfg := fmt.Sprintf(`{
  "discovery": {
    "discoveryMode": "static",
    "listedTools": ["*"],
    "servers": [
      { "serverName": "echo-backend", "serverUrl": "http://localhost%s/mcp" },
      { "serverName": "restricted-backend", "serverUrl": "http://localhost%s/mcp" }
    ]
  },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "narrow", "allowed_tools": ["echo-backend.*"], "allowed_prompts": [], "allowed_resources": [], "allowed_resource_templates": [] },
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": [], "allowed_resources": [], "allowed_resource_templates": [] }
    ]
  },
  "auth": {
    "publicKeyPath": "./public.pem",
    "jwksUrl": "",
    "issuer": "",
    "audience": ""
  },
  "internalAuth": {
    "issuer": "go-proxy",
    "audience": "go-proxy-backend",
    "ttlSeconds": 60,
    "maxRelayHops": 3,
    "relay": {
      "echo-backend": ["echo-backend.*", "restricted-backend.get_headers", "proxy_utility.call_tool"]
    }
  },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "selfUrl": "%s",
  "natsUrl": %q
}`, RELAY_ECHO_BACKEND_PORT, RELAY_RESTRICTED_BACKEND_PORT, RELAY_TEST_PROXY_URL, natsURL)

	cfgPath := tmpDir + "/backend-relay-test.cfg"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	startTestProxy(t, ctx, cancel, cfgPath, RELAY_TEST_PROXY_PORT)

	narrow := connectAsClient(t, ctx, "narrow", nil)
	admin := connectAsClient(t, ctx, "admin", nil)

	// A backend allowed to relay gets an identity assertion, a relay token,
	// the callback URL and a tool-call ID; each direct call gets its own ID.
	first := getHeaders(t, ctx, narrow, "echo-backend.get_headers")
	if first.Identity == "" || first.RelayToken == "" || first.Callback == "" || first.ToolCallID == "" {
		t.Fatalf("relaying backend got headers %+v; want all four set", first)
	}
	if second := getHeaders(t, ctx, narrow, "echo-backend.get_headers"); second.ToolCallID == first.ToolCallID {
		t.Error("two independent direct calls got the same tool-call ID")
	}

	// A backend not allowed to relay gets only the identity assertion (and
	// the tool-call ID): no relay token, no callback URL.
	restricted := getHeaders(t, ctx, admin, "restricted-backend.get_headers")
	if restricted.Identity == "" || restricted.RelayToken != "" || restricted.Callback != "" {
		t.Errorf("non-relaying backend got headers %+v; want only the identity assertion", restricted)
	}

	// A client can't choose its own tool-call ID.
	forged := getHeaders(t, ctx, connectAsClient(t, ctx, "narrow", map[string]string{relayheaders.ToolCallIDHeader: "forged-id"}), "echo-backend.get_headers")
	if forged.ToolCallID == "forged-id" {
		t.Error("a client-supplied tool-call ID header was used")
	}

	// The identity assertion is never a credential - not for an ordinary
	// tool, not for an admin's assertion replayed at _admin.*.
	adminEcho := getHeaders(t, ctx, admin, "echo-backend.get_headers")
	for what, assertion := range map[string]string{"narrow's assertion": first.Identity, "admin's assertion": adminEcho.Identity} {
		for _, tool := range []string{"echo-backend.get_headers", "_admin.export_role_permissions"} {
			callExpectingRejection(t, ctx, what+" replayed", map[string]string{relayheaders.AssertionHeader: assertion}, tool)
		}
	}

	// The relay token works for an allowed target, keeps the original
	// tool-call ID (from the token, not a header), and the tokens minted
	// for the next hop don't outlive the first ones.
	relaySession, err := connectAsBackendRelay(ctx, RELAY_TEST_PROXY_URL, map[string]string{relayheaders.RelayTokenHeader: first.RelayToken})
	if err != nil {
		t.Fatalf("connecting with a relay token: %v", err)
	}
	defer relaySession.Close()
	relayed := getHeaders(t, ctx, relaySession, "echo-backend.get_headers")
	if relayed.ToolCallID != first.ToolCallID {
		t.Errorf("relayed call got tool-call ID %q, want the original %q", relayed.ToolCallID, first.ToolCallID)
	}
	if got, limit := tokenExpiry(t, relayed.RelayToken), tokenExpiry(t, first.RelayToken); got.After(limit) {
		t.Errorf("relayed call's relay token expires %v, after the original's %v: relaying extended it", got, limit)
	}

	// Limits: the user's own role (narrow can't call restricted-backend.*,
	// though echo-backend may relay to restricted-backend.get_headers), the
	// relaying server's targets (admin may call secret_tool, but
	// echo-backend may not relay to it), and never _admin.*.
	callExpectingRejection(t, ctx, "narrow relay beyond its role", map[string]string{relayheaders.RelayTokenHeader: first.RelayToken}, "restricted-backend.get_headers")
	adminRelay := map[string]string{relayheaders.RelayTokenHeader: adminEcho.RelayToken}
	callExpectingRejection(t, ctx, "admin relay outside the server's targets", adminRelay, "restricted-backend.secret_tool")
	callExpectingRejection(t, ctx, "admin relay to _admin.*", adminRelay, "_admin.export_role_permissions")
	adminRelaySession, err := connectAsBackendRelay(ctx, RELAY_TEST_PROXY_URL, adminRelay)
	if err != nil {
		t.Fatalf("connecting with admin's relay token: %v", err)
	}
	defer adminRelaySession.Close()
	getHeaders(t, ctx, adminRelaySession, "restricted-backend.get_headers") // in the targets, and admin's role allows it

	// A relay's restrictions follow it through proxy_utility.call_tool:
	// echo-backend may relay to call_tool, but the loopback call_tool makes
	// is still limited to echo-backend's targets and never reaches _admin.*.
	viaCallTool := func(target string) (*mcp.CallToolResult, error) {
		return adminRelaySession.CallTool(ctx, &mcp.CallToolParams{
			Name:      "proxy_utility.call_tool",
			Arguments: map[string]any{"tool": target, "arguments": map[string]any{}},
		})
	}
	if res, err := viaCallTool("restricted-backend.get_headers"); err != nil || res.IsError {
		t.Errorf("relay through call_tool to an allowed target failed: err=%v result=%+v", err, res)
	}
	for _, target := range []string{"_admin.export_role_permissions", "restricted-backend.secret_tool"} {
		if res, err := viaCallTool(target); err == nil && !res.IsError {
			t.Errorf("relay through call_tool reached %s", target)
		}
	}

	// Tokens the proxy would refuse: too many hops, expired, from a server
	// not allowed to relay, and a loopback token claiming a relay origin
	// that isn't allowed to relay.
	id := auth.Identity{Subject: "narrow-user", Roles: []string{"narrow"}, Hop: 1}
	mint := func(what string, f func() (string, error)) map[string]string {
		token, err := f()
		if err != nil {
			t.Fatalf("minting %s: %v", what, err)
		}
		return map[string]string{relayheaders.RelayTokenHeader: "Bearer " + token}
	}
	tooDeep := id
	tooDeep.Hop = 4
	expired := id
	expired.Expiry = time.Now().Add(-time.Minute)
	strayLoopback := id
	strayLoopback.RelayFrom = "restricted-backend"
	for what, headers := range map[string]map[string]string{
		"past maxRelayHops":           mint("deep token", func() (string, error) { return internalSigner.SignRelay(tooDeep, "echo-backend") }),
		"expired":                     mint("expired token", func() (string, error) { return internalSigner.SignRelay(expired, "echo-backend") }),
		"from a non-relaying server":  mint("stray token", func() (string, error) { return internalSigner.SignRelay(id, "restricted-backend") }),
		"loopback from a non-relayer": mint("stray loopback", func() (string, error) { return internalSigner.SignLoopback(strayLoopback) }),
	} {
		callExpectingRejection(t, ctx, "relay token "+what, headers, "echo-backend.get_headers")
	}

	// No credential at all.
	callExpectingRejection(t, ctx, "no credential", map[string]string{}, "echo-backend.get_headers")
}

// TestCheckRelayTargetNeverAllowsAdmin checks that _admin.* is out of reach
// of a relay even when the relaying server's targets would match it.
func TestCheckRelayTargetNeverAllowsAdmin(t *testing.T) {
	old := internalAuthConfig
	t.Cleanup(func() { internalAuthConfig = old })
	internalAuthConfig = &auth.SignerConfig{Relay: map[string][]string{"wide": {"*"}}}

	ra := requestAuth{kind: authRelay, relayFrom: "wide"}
	if err := checkRelayTarget(ra, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "_admin.update_role_permissions"}}); err == nil {
		t.Error("a relay reached an _admin.* tool")
	}
	if err := checkRelayTarget(ra, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "crm.get_customer"}}); err != nil {
		t.Errorf("a relay to a matching target was refused: %v", err)
	}
	// A call that isn't part of a relay isn't restricted here.
	if err := checkRelayTarget(requestAuth{kind: authLoopback}, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "_admin.update_role_permissions"}}); err != nil {
		t.Errorf("a non-relayed loopback was restricted: %v", err)
	}
}

// TestAuthenticateRequestRelayTokens checks why authenticateRequest refuses
// each kind of token a backend might present (TestBackendRelay only sees
// the resulting 401).
func TestAuthenticateRequestRelayTokens(t *testing.T) {
	key, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	oldSigner, oldVerifier, oldCfg := internalSigner, relayTokenVerifier, internalAuthConfig
	t.Cleanup(func() { internalSigner, relayTokenVerifier, internalAuthConfig = oldSigner, oldVerifier, oldCfg })
	internalAuthConfig = &auth.SignerConfig{MaxRelayHops: 2, Relay: map[string][]string{"echo": {"*"}}}
	internalSigner = auth.SignerFromKey(key, *internalAuthConfig)
	relayTokenVerifier = auth.NewVerifierFromKey(&key.PublicKey, auth.DefaultInternalIssuer, "")

	id := auth.Identity{Subject: "alice", Roles: []string{"user"}, Hop: 1, ToolCallID: "call-1"}
	header := func(name string, mint func() (string, error)) http.Header {
		token, err := mint()
		if err != nil {
			t.Fatal(err)
		}
		return http.Header{name: {"Bearer " + token}}
	}
	deep := id
	deep.Hop = 3

	for name, tc := range map[string]struct {
		h       http.Header
		wantErr string
	}{
		"relay token":         {header(relayheaders.RelayTokenHeader, func() (string, error) { return internalSigner.SignRelay(id, "echo") }), ""},
		"loopback token":      {header(relayheaders.RelayTokenHeader, func() (string, error) { return internalSigner.SignLoopback(id) }), ""},
		"assertion as relay":  {header(relayheaders.RelayTokenHeader, func() (string, error) { return internalSigner.SignAssertion(id, "echo") }), "invalid audience"},
		"assertion header":    {header(relayheaders.AssertionHeader, func() (string, error) { return internalSigner.SignAssertion(id, "echo") }), "an identity assertion is not a credential"},
		"too many hops":       {header(relayheaders.RelayTokenHeader, func() (string, error) { return internalSigner.SignRelay(deep, "echo") }), "too many relay hops"},
		"non-relaying server": {header(relayheaders.RelayTokenHeader, func() (string, error) { return internalSigner.SignRelay(id, "other") }), `"other" isn't allowed to relay`},
		"other key": {func() http.Header {
			k, _ := auth.GenerateKey()
			s := auth.SignerFromKey(k, auth.SignerConfig{})
			tok, _ := s.SignRelay(id, "echo")
			return http.Header{relayheaders.RelayTokenHeader: {"Bearer " + tok}}
		}(), "invalid relay token"},
	} {
		t.Run(name, func(t *testing.T) {
			_, ra, err := authenticateRequest(nil, tc.h)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if ra.hop != 1 || ra.toolCallID != "call-1" {
					t.Errorf("requestAuth = %+v; want hop 1, tool-call ID call-1", ra)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v; want one containing %q", err, tc.wantErr)
			}
		})
	}
}
