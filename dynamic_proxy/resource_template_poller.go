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

// ResourceTemplatesChangedFunc is invoked whenever a poll detects that a
// backend server's resource template list has changed since the last poll.
type ResourceTemplatesChangedFunc = ChangedFunc[*mcp.ResourceTemplate]

// ResourceTemplatePoller periodically opens a brand-new, short-lived
// connection to each known backend server, lists its resource templates, and
// invokes onChanged when the result differs from what was last seen.
type ResourceTemplatePoller = FeaturePoller[*mcp.ResourceTemplate]

// NewResourceTemplatePoller creates a poller that checks each server's
// resource template list every frequencySeconds.
func NewResourceTemplatePoller(frequencySeconds int, onChanged ResourceTemplatesChangedFunc) *ResourceTemplatePoller {
	return NewFeaturePoller(frequencySeconds, "ResourceTemplatePoller", fetchResourceTemplates, resourceTemplateURI, onChanged)
}

// resourceTemplateURI is used as the fingerprinting/identifying name for a
// resource template: its URI template string (e.g. "file:///{path}").
func resourceTemplateURI(t *mcp.ResourceTemplate) string { return t.URITemplate }

// fetchResourceTemplates opens a brand-new client connection to the backend
// server, lists its resource templates, and closes the connection before
// returning.
func fetchResourceTemplates(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]*mcp.ResourceTemplate, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "resource-template-poller",
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

	return collectAllPages(session.ResourceTemplates(ctx, nil))
}
