// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	testToken       string
	myRsaPrivateKey *rsa.PrivateKey
)

// init generates a fresh RSA keypair for this test binary and writes only
// the public half to ./public.pem, the path every test config in this
// package (integration_test.cfg, orchestration_test.cfg, and the inline
// cfg JSON in several *_test.go files) hardcodes as auth.publicKeyPath.
// No private key material ever touches disk or the repo - this replaces a
// previously committed public.pem plus a private.pem that had to live
// outside the repo entirely (see nokeys.md's "generate keys dynamically on
// startup" approach).
func init() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("failed to generate test RSA key: %v", err)
	}
	myRsaPrivateKey = key

	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		log.Fatalf("failed to marshal test public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile("public.pem", pubPEM, 0o644); err != nil {
		log.Fatalf("failed to write test public.pem: %v", err)
	}
}

// testTokenRoundTripper sends the package-level testToken as a client
// Authorization: Bearer header. It reads testToken at request time, not at
// construction, so whichever token the last GenerateTestToken*/connectAsRole
// call stored is the one every session built on newTestTokenClient sends.
type testTokenRoundTripper struct{}

func (testTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set("Authorization", "Bearer "+testToken)
	return http.DefaultTransport.RoundTrip(cloned)
}

func newTestTokenClient() *http.Client {
	return &http.Client{Transport: testTokenRoundTripper{}}
}

func getTools() (map[string]struct{}, error) {
	t, err := GenerateTestToken()
	testToken = t
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	toolSet := make(map[string]struct{})

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost:8081/mcp",
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		return nil, err
	}
	defer clientSession.Close()

	resultList, err := clientSession.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, err
	}

	for _, tool := range resultList.Tools {
		toolSet[tool.Name] = struct{}{}
	}
	return toolSet, nil
}

func getPrompts() (map[string]struct{}, error) {
	t, err := GenerateTestToken()
	testToken = t
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	promptSet := make(map[string]struct{})

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost:8081/mcp",
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		return nil, err
	}
	defer clientSession.Close()

	resultList, err := clientSession.ListPrompts(ctx, &mcp.ListPromptsParams{})
	if err != nil {
		return nil, err
	}

	for _, prompt := range resultList.Prompts {
		promptSet[prompt.Name] = struct{}{}
	}
	return promptSet, nil
}

func getResources() (map[string]struct{}, error) {
	t, err := GenerateTestToken()
	testToken = t
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	resourceSet := make(map[string]struct{})

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost:8081/mcp",
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		return nil, err
	}
	defer clientSession.Close()

	resultList, err := clientSession.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		return nil, err
	}

	for _, resource := range resultList.Resources {
		resourceSet[resource.URI] = struct{}{}
	}
	return resourceSet, nil
}

func getResourceTemplates() (map[string]struct{}, error) {
	t, err := GenerateTestToken()
	testToken = t
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	templateSet := make(map[string]struct{})

	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   "http://localhost:8081/mcp",
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		return nil, err
	}
	defer clientSession.Close()

	resultList, err := clientSession.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{})
	if err != nil {
		return nil, err
	}

	for _, tmpl := range resultList.ResourceTemplates {
		templateSet[tmpl.URITemplate] = struct{}{}
	}
	return templateSet, nil
}

func callResource(url string, params *mcp.ReadResourceParams) (string, error) {
	t, err := GenerateTestToken()
	testToken = t
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		log.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer clientSession.Close()

	result, err := clientSession.ReadResource(ctx, params)

	if err != nil {
		return "", err
	}
	return result.Contents[0].Text, nil
}

func callPrompt(url string, params *mcp.GetPromptParams) (string, error) {
	t, err := GenerateTestToken()
	testToken = t
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		log.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer clientSession.Close()

	result, err := clientSession.GetPrompt(ctx, params)

	if err != nil {
		return "", err
	}
	return result.Messages[0].Content.(*mcp.TextContent).Text, nil
}

func callTool(url string, params mcp.CallToolParams) (string, error) {
	t, err := GenerateTestToken()
	testToken = t
	ctx := context.Background()
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1.0.0"}, nil)
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: newTestTokenClient(),
	}, nil)
	if err != nil {
		log.Fatalf("Failed to connect to proxy: %v", err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(ctx, &params)

	if err != nil {
		return "", err
	}
	return result.Content[0].(*mcp.TextContent).Text, nil
}

func GenerateTestToken() (string, error) {
	return GenerateTestTokenForRole("admin")
}

// GenerateTestTokenForRole mints a token for an arbitrary role, so tests can
// exercise access-control allow/deny behavior for roles other than "admin"
// (which allows everything and so never exercises deny paths).
func GenerateTestTokenForRole(role string) (string, error) {
	claims := jwt.MapClaims{
		"sub":  "udi_r",                              // Subject (User ID)
		"role": role,                                 // Custom claim
		"exp":  time.Now().Add(time.Hour * 1).Unix(), // Expiration time (1 hour from now)
		"iat":  time.Now().Unix(),                    // Issued at time
	}

	// 1. Change SigningMethodHS256 to SigningMethodRS256
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)

	// 2. Pass the *rsa.PrivateKey object directly
	tokenString, err := token.SignedString(myRsaPrivateKey)
	if err != nil {
		return "", err
	}
	return tokenString, nil
}

// connectAsRole connects to url using a freshly minted token for role, so
// tests can drive a session under a specific role's access-control rules
// (unlike getTools/getPrompts/etc. above, which are hardcoded to "admin").
func connectAsRole(ctx context.Context, url string, role string) (*mcp.ClientSession, error) {
	t, err := GenerateTestTokenForRole(role)
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
