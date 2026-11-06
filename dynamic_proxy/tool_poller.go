// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"

	// Using the official Model Context Protocol Go SDK
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/backendauth"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

// ToolsChangedFunc is invoked whenever a poll detects that a backend
// server's tool list has changed since the last poll.
type ToolsChangedFunc = ChangedFunc[*mcp.Tool]

// ToolPoller periodically opens a brand-new, short-lived connection to each
// known backend server, lists its tools, and invokes onChanged when the
// result differs from what was last seen.
type ToolPoller = FeaturePoller[*mcp.Tool]

// NewToolPoller creates a poller that checks each server's tool list every
// frequencySeconds.
func NewToolPoller(frequencySeconds int, onChanged ToolsChangedFunc) *ToolPoller {
	return NewFeaturePoller(frequencySeconds, "ToolPoller", fetchTools, toolName, onChanged)
}

func toolName(t *mcp.Tool) string { return t.Name }

// fetchTools opens a brand-new client connection to the backend server,
// lists its tools, and closes the connection before returning.
func fetchTools(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]*mcp.Tool, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "tool-poller",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   attrs.ServerUrl,
		HTTPClient: backendauth.Client(backendHTTPClient, attrs.CredentialsSecret),
	}, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	return collectAllPages(session.Tools(ctx, nil))
}
