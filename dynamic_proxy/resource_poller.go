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

// ResourcesChangedFunc is invoked whenever a poll detects that a backend
// server's resource list has changed since the last poll.
type ResourcesChangedFunc = ChangedFunc[*mcp.Resource]

// ResourcePoller periodically opens a brand-new, short-lived connection to
// each known backend server, lists its resources, and invokes onChanged when
// the result differs from what was last seen.
type ResourcePoller = FeaturePoller[*mcp.Resource]

// NewResourcePoller creates a poller that checks each server's resource list
// every frequencySeconds.
func NewResourcePoller(frequencySeconds int, onChanged ResourcesChangedFunc) *ResourcePoller {
	return NewFeaturePoller(frequencySeconds, "ResourcePoller", fetchResources, resourceURI, onChanged)
}

// resourceURI is used as the fingerprinting/identifying name for a resource.
// Unlike tools/prompts, resources are identified by URI rather than Name,
// since Name need not be unique per the MCP spec.
func resourceURI(r *mcp.Resource) string { return r.URI }

// fetchResources opens a brand-new client connection to the backend server,
// lists its resources, and closes the connection before returning.
func fetchResources(ctx context.Context, attrs *discover_mcp_servers.ServerAttributes) ([]*mcp.Resource, error) {
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "resource-poller",
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

	return collectAllPages(session.Resources(ctx, nil))
}
