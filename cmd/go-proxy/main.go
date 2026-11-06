// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Command go-proxy is the MCP gateway. Everything it does lives in the
// proxyserver package (proxyserver/); this file is only the binary's entry
// point, in its own directory like every other binary in this module, so
// the module root holds no Go code at all.
package main

import "github.com/pellucon/enterprise-go-mcp-proxy/proxyserver"

func main() {
	proxyserver.Main()
}
