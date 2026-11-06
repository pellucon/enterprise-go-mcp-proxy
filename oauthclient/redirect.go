// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

import (
	"fmt"
	"net"
)

// bindLoopbackListener binds 127.0.0.1:port (port 0 = OS-assigned
// ephemeral port).
func bindLoopbackListener(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("failed to bind loopback listener on port %d: %w", port, err)
	}
	return ln, nil
}

// redirectURIFromListener builds the redirect_uri a loopback listener
// implies, per RFC 8252 ("OAuth 2.0 for Native Apps"): native/CLI clients
// use a loopback interface redirect URI, exact-matched against whatever
// was registered for the client.
func redirectURIFromListener(ln net.Listener) string {
	return fmt.Sprintf("http://%s/callback", ln.Addr().String())
}
