// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// internalVerifierHolder lets account-service's tool/resource handlers be
// registered (and its HTTP listener started) before the *auth.Verifier
// they need exists — the verifier now has to fetch go-proxy's real JWKS
// endpoint (auth.Config.JWKSURL), which isn't listening until
// startTestProxy returns, but discovery works better the earlier the
// backend itself is up. set is called once, right after the proxy starts;
// every call before that would see a nil verifier, but no test client
// calls a tool that early.
type internalVerifierHolder struct {
	mu sync.RWMutex
	v  *auth.Verifier
}

func (h *internalVerifierHolder) set(v *auth.Verifier) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.v = v
}

func (h *internalVerifierHolder) get() *auth.Verifier {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.v
}

const (
	ACCOUNT_SERVICE_PORT    = ":8095"
	INTERNAL_AUTH_TEST_PORT = ":8096"
	INTERNAL_AUTH_TEST_URL  = "http://localhost:8096/mcp"
)

// testAccountOwners/testAccountBalances mirror
// sample-mcp-servers/customer-service's accountOwners/accountBalances demo
// data exactly (duplicated here since a package-main test can't import
// another package-main directory — the same constraint already documented
// in platform_servers_test.go for its in-process stand-ins).
var testAccountOwners = map[string]string{
	"acc-100": "alice",
	"acc-200": "bob",
}

var testAccountBalances = map[string]string{
	"acc-100": "$1,000.00",
	"acc-200": "$2,500.00",
}

type getAccountBalanceArgs struct {
	AccountNumber string `json:"accountNumber" jsonschema:"account number"`
}

// verifyInternalCaller extracts and verifies go-proxy's internal identity
// assertion from header using verifier, returning the verified "sub"
// claim. Mirrors customer-service/company-service's verifyCaller, both
// built on relayheaders.VerifyAssertion.
func verifyInternalCaller(verifier *auth.Verifier, header http.Header) (string, error) {
	claims, err := relayheaders.VerifyAssertion(verifier, header)
	if err != nil {
		return "", err
	}
	sub, err := claims.GetSubject()
	if err != nil || sub == "" {
		return "", fmt.Errorf("internal identity assertion missing subject claim")
	}
	return sub, nil
}

// createTestAccountServer is an in-process stand-in for
// sample-mcp-servers/customer-service's get_account_balance tool and
// file:///accounts/{accountNumber} resource template, proving the internal
// assertion mechanism (see main.go's createMiddleware/internalSigner and
// relayheaders.FromContext) reaches both a tool call AND a resource read,
// not just tools.
func createTestAccountServer(verifierHolder *internalVerifierHolder) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "account-service", Version: "1.0.0"}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_account_balance",
		Description: "Get an account's balance (owner-restricted)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"accountNumber": map[string]any{"type": "string", "description": "account number"},
			},
			"required": []string{"accountNumber"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args getAccountBalanceArgs) (*mcp.CallToolResult, any, error) {
		sub, err := verifyInternalCaller(verifierHolder.get(), req.GetExtra().Header)
		if err != nil {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "denied: " + err.Error()}},
			}, nil, nil
		}
		owner, exists := testAccountOwners[args.AccountNumber]
		if !exists || owner != sub {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("denied: %s is not authorized to view account %s", sub, args.AccountNumber)}},
			}, nil, nil
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("Account %s balance: %s", args.AccountNumber, testAccountBalances[args.AccountNumber])}},
		}, nil, nil
	})

	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "file:///accounts/{accountNumber}",
		Name:        "account",
		Description: "An account's details (owner-restricted)",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		accountNumber := strings.TrimPrefix(req.Params.URI, "file:///accounts/")
		sub, err := verifyInternalCaller(verifierHolder.get(), req.GetExtra().Header)
		if err != nil {
			return nil, fmt.Errorf("denied: %w", err)
		}
		owner, exists := testAccountOwners[accountNumber]
		if !exists || owner != sub {
			return nil, fmt.Errorf("denied: %s is not authorized to read account %s", sub, accountNumber)
		}
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, Text: fmt.Sprintf("Account %s balance: %s", accountNumber, testAccountBalances[accountNumber])},
			},
		}, nil
	})

	return server
}

// generateTestRSAKeyPair returns fresh PKCS8-private/PKIX-public PEM bytes,
// for tests that need their own keypair distinct from the one
// mcp_client_test.go's init() generates for external client auth
// (myRsaPrivateKey / ./public.pem).
func generateTestRSAKeyPair(t *testing.T) (privPEM, pubPEM []byte, key *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("failed to generate RSA key: %v", err)
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal private key: %v", err)
	}
	privPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	pubPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	return privPEM, pubPEM, key
}

// mintExternalTokenForSubRole mints a client-facing JWT (signed with
// myRsaPrivateKey, matching ./public.pem — the same keypair
// mcp_client_test.go's GenerateTestTokenForRole uses) for an arbitrary
// sub/role, unlike GenerateTestTokenForRole which hardcodes sub="udi_r".
// Left as a standalone helper rather than modifying the shared
// GenerateTestTokenForRole, to avoid any risk to existing tests.
func mintExternalTokenForSubRole(sub, role string) (string, error) {
	claims := jwt.MapClaims{
		"sub":  sub,
		"role": role,
		"exp":  time.Now().Add(time.Hour).Unix(),
		"iat":  time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	return token.SignedString(myRsaPrivateKey)
}

// connectAsSubRole is connectAsRole (mcp_client_test.go) parameterized by
// sub as well as role, so this test can demonstrate "alice can see her own
// account, not bob's" against a single shared role ("user").
func connectAsSubRole(ctx context.Context, url, sub, role string) (*mcp.ClientSession, error) {
	t, err := mintExternalTokenForSubRole(sub, role)
	if err != nil {
		return nil, err
	}
	testToken = t
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: newTestTokenClient(),
	}, nil)
}

// forgingAssertionRoundTripper attaches an arbitrary, caller-supplied
// assertion header to every outbound request — used only to simulate an
// attacker forging the internal assertion directly against a backend,
// bypassing go-proxy entirely, to prove the backend's own signature check
// (not just header presence) is what's actually gating access.
func forgingAssertionRoundTripper(assertion string) http.RoundTripper {
	return relayheaders.Set{Assertion: "Bearer " + assertion}.RoundTripper(nil)
}

// TestInternalIdentityAssertion is the plan's end-to-end proof that
// go-proxy mints a proxy-signed internal identity assertion (auth.Signer,
// main.go's createMiddleware, session_proxies.go's
// assertionForwardingTransport) and that a backend verifying it
// (auth.Verifier) can enforce its own resource-level authorization that
// go-proxy's role-based access control alone cannot express — for both a
// tool call and a resource read.
func TestInternalIdentityAssertion(t *testing.T) {
	tmpDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// account-service's own verifier can't be built yet: go-proxy no
	// longer reads its internal signing key from a shared file (see
	// internal_signing_key.go's bootstrapInternalSigningKey), so the only
	// way to verify its assertions is to fetch its real
	// /.well-known/jwks-internal.json — which isn't listening until the
	// proxy actually starts. verifierHolder lets account-service's
	// handlers be registered and its listener started now regardless (so
	// discovery isn't held up waiting on the proxy), with the real
	// verifier plugged in once it's available, below.
	verifierHolder := &internalVerifierHolder{}
	accountServer := createTestAccountServer(verifierHolder)
	startTestBackend(t, ctx, cancel, ACCOUNT_SERVICE_PORT, accountServer)

	natsURL := startTestNATSServer(t)

	cfg := fmt.Sprintf(`{
  "discovery": {
    "discoveryMode": "static",
    "listedTools": ["*"],
    "servers": [
      { "serverName": "account-service", "serverUrl": "http://localhost%s/mcp" }
    ]
  },
  "roleMapping": { "administrator": "admin" },
  "accessRules": {
    "clientAccessRules": [
      { "role": "admin", "allowed_tools": ["*"], "allowed_prompts": ["*"], "allowed_resources": ["*"], "allowed_resource_templates": ["*"] },
      { "role": "user", "allowed_tools": ["account-service.*"], "allowed_prompts": [], "allowed_resources": ["account-service:*"], "allowed_resource_templates": ["account-service:*"] }
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
    "ttlSeconds": 60
  },
  "eventDictionary": { "eventTypes": {} },
  "declarativeGuardrails": { "actions": ["log"], "declarations": [] },
  "natsUrl": %q
}`, ACCOUNT_SERVICE_PORT, natsURL)

	cfgPath := filepath.Join(tmpDir, "internal-auth-test.cfg")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}

	startTestProxy(t, ctx, cancel, cfgPath, INTERNAL_AUTH_TEST_PORT)

	// Now that the proxy is up and serving its real JWKS endpoint, build
	// account-service's actual verifier against it and plug it in — every
	// call below happens only after this point. It requires assertions
	// minted for account-service specifically ("<audience>/<server>"), so
	// an assertion go-proxy minted for any other backend wouldn't pass.
	internalVerifier, err := auth.NewVerifier(ctx, auth.Config{
		JWKSURL:  "http://localhost" + INTERNAL_AUTH_TEST_PORT + "/.well-known/jwks-internal.json",
		Issuer:   auth.DefaultInternalIssuer,
		Audience: auth.DefaultInternalAudience + "/account-service",
	})
	if err != nil {
		t.Fatalf("failed to build internal verifier from go-proxy's JWKS endpoint: %v", err)
	}
	verifierHolder.set(internalVerifier)

	// Scenario 1: Alice calls the tool for her own account -> success.
	aliceSession, err := connectAsSubRole(ctx, INTERNAL_AUTH_TEST_URL, "alice", "user")
	if err != nil {
		t.Fatalf("failed to connect as alice: %v", err)
	}
	defer aliceSession.Close()

	result, err := aliceSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "account-service.get_account_balance",
		Arguments: map[string]any{"accountNumber": "acc-100"},
	})
	if err != nil {
		t.Fatalf("expected alice's own-account call to succeed, got error: %v", err)
	}
	if result.IsError {
		t.Fatalf("expected alice's own-account call to succeed, got denial: %v", result.Content)
	}
	if text, ok := result.Content[0].(*mcp.TextContent); !ok || !strings.Contains(text.Text, "$1,000.00") {
		t.Errorf("unexpected result for alice's own account: %+v", result.Content)
	}

	// Scenario 2: Alice calls the tool for Bob's account -> denied by the
	// backend's own ownership check (not by go-proxy's role-based access
	// control, which already allowed "user" to call account-service.* in
	// general).
	result, err = aliceSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "account-service.get_account_balance",
		Arguments: map[string]any{"accountNumber": "acc-200"},
	})
	if err != nil {
		t.Fatalf("unexpected transport error calling for bob's account: %v", err)
	}
	if !result.IsError {
		t.Errorf("expected alice's call for bob's account to be denied, got: %+v", result.Content)
	}

	// Scenario 3: Alice reads the resource template for Bob's account ->
	// denied the same way, proving the assertion reaches ReadResource too,
	// not just CallTool.
	_, err = aliceSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: "account-service:file:///accounts/acc-200"})
	if err == nil {
		t.Error("expected alice's read of bob's account resource to be denied")
	}

	// And the mirror image: Alice reading her OWN account resource must
	// succeed, confirming scenario 3's denial is ownership-based, not a
	// blanket failure of resource-template propagation.
	readResult, err := aliceSession.ReadResource(ctx, &mcp.ReadResourceParams{URI: "account-service:file:///accounts/acc-100"})
	if err != nil {
		t.Fatalf("expected alice's read of her own account resource to succeed, got error: %v", err)
	}
	if len(readResult.Contents) == 0 || !strings.Contains(readResult.Contents[0].Text, "$1,000.00") {
		t.Errorf("unexpected resource read result for alice's own account: %+v", readResult.Contents)
	}

	// Scenario 4: an assertion signed with a DIFFERENT keypair (simulating
	// an attacker forging the header directly against the backend, bypassing
	// go-proxy entirely) is rejected — proving account-service's
	// internalVerifier actually checks the signature, not just that some
	// value is present in the header.
	_, _, otherKey := generateTestRSAKeyPair(t)
	badClaims := jwt.MapClaims{
		"sub":  "alice",
		"role": "user",
		"iss":  auth.DefaultInternalIssuer,
		"aud":  []string{auth.DefaultInternalAudience, auth.DefaultInternalAudience + "/account-service"},
		"iat":  time.Now().Unix(),
		"exp":  time.Now().Add(time.Minute).Unix(),
	}
	badToken := jwt.NewWithClaims(jwt.SigningMethodRS256, badClaims)
	badAssertion, err := badToken.SignedString(otherKey)
	if err != nil {
		t.Fatalf("failed to mint forged assertion: %v", err)
	}

	directClient := mcp.NewClient(&mcp.Implementation{Name: "attacker", Version: "1.0.0"}, nil)
	directSession, err := directClient.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost" + ACCOUNT_SERVICE_PORT + "/mcp",
		HTTPClient: &http.Client{Transport: forgingAssertionRoundTripper(badAssertion)},
	}, nil)
	if err != nil {
		t.Fatalf("failed to connect directly to account-service: %v", err)
	}
	defer directSession.Close()

	forgedResult, err := directSession.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_account_balance",
		Arguments: map[string]any{"accountNumber": "acc-100"},
	})
	if err != nil {
		t.Fatalf("unexpected transport error calling account-service directly: %v", err)
	}
	if !forgedResult.IsError {
		t.Errorf("expected a forged/mismatched-key assertion to be denied, got: %+v", forgedResult.Content)
	}
}
