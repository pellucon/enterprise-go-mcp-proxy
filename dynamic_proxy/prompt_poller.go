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

// PromptsChangedFunc is invoked whenever a poll detects that a backend
// server's prompt list has changed since the last poll.
type PromptsChangedFunc = ChangedFunc[*mcp.Prompt]

// PromptPoller periodically opens a brand-new, short-lived connection to
// each known backend server, lists its prompts, and invokes onChanged when
// the result differs from what was last seen.
type PromptPoller = FeaturePoller[*mcp.Prompt]

// NewPromptPoller creates a poller that checks each server's prompt list
// every frequencySeconds.
func NewPromptPoller(frequencySeconds int, onChanged PromptsChangedFunc) *PromptPoller {
	return NewFeaturePoller(frequencySeconds, "PromptPoller", fetchPrompts, promptName, onChanged)
}

func promptName(p *mcp.Prompt) string { return p.Name }

// fetchPrompts opens a brand-new client connection to the backend server,
// lists its prompts, and closes the connection before returning.
func fetchPrompts(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]*mcp.Prompt, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "prompt-poller",
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

	return collectAllPages(session.Prompts(ctx, nil))
}
