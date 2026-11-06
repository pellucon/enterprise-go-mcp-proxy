// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
)

func toServerDetails(serverAtts *discover_mcp_servers.ServerAttributes) *mcpServerDetails {
	return &mcpServerDetails{
		serverName:        serverAtts.ServerName,
		serverUrl:         serverAtts.ServerUrl,
		credentialsSecret: serverAtts.CredentialsSecret,
	}
}

func getDetailMap(serverMap discover_mcp_servers.ServerMap) serverDetailMap {
	detailsMap := make(serverDetailMap)
	for _, att := range serverMap {
		detailsMap[att.ServerName] = toServerDetails(att)
	}
	return detailsMap
}
