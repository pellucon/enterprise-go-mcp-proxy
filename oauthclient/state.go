// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package oauthclient

// randomState generates a value for the OAuth "state" parameter, verified
// on the way back to guard against CSRF.
func randomState() (string, error) {
	return randomToken()
}
