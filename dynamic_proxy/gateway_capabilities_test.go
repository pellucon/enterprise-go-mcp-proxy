// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestGatewayAdvertisesCapabilitiesBeforeDiscovery checks the capabilities a
// client sees when it connects before any backend has been discovered: tools,
// prompts and resources must all be advertised (with listChanged), and
// logging must not be - otherwise an early client would be told the gateway
// has no prompts or resources for its whole session.
func TestGatewayAdvertisesCapabilitiesBeforeDiscovery(t *testing.T) {
	gateway := NewGateway(nil, nil, 5, 5, 5, 5)

	gatewayHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return gateway.Server()
	}, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer gatewayHTTP.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "capabilities-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: gatewayHTTP.URL}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	caps := session.InitializeResult().Capabilities
	if caps == nil {
		t.Fatal("no capabilities advertised")
	}
	if caps.Tools == nil || !caps.Tools.ListChanged {
		t.Errorf("tools capability = %+v, want advertised with listChanged", caps.Tools)
	}
	if caps.Prompts == nil || !caps.Prompts.ListChanged {
		t.Errorf("prompts capability = %+v, want advertised with listChanged, even with no prompts registered", caps.Prompts)
	}
	if caps.Resources == nil || !caps.Resources.ListChanged {
		t.Errorf("resources capability = %+v, want advertised with listChanged, even with no resources registered", caps.Resources)
	}
	if caps.Resources != nil && caps.Resources.Subscribe {
		t.Error("resource subscriptions advertised, but the gateway doesn't relay them")
	}
	if caps.Logging != nil {
		t.Error("logging advertised, but the gateway never sends log messages to clients")
	}

	// An empty list is a valid answer before discovery.
	if _, err := session.ListPrompts(ctx, nil); err != nil {
		t.Errorf("prompts/list before discovery: %v", err)
	}
	if _, err := session.ListResources(ctx, nil); err != nil {
		t.Errorf("resources/list before discovery: %v", err)
	}
}
