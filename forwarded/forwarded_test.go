// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package forwarded

import (
	"crypto/tls"
	"net/http/httptest"
	"testing"
)

func TestBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		tls     bool
		headers map[string]string
		want    string
	}{
		{name: "plain http, no headers", want: "http://example.test"},
		{name: "direct TLS, no headers", tls: true, want: "https://example.test"},
		{
			name:    "TLS-terminating proxy",
			headers: map[string]string{"X-Forwarded-Proto": "https", "X-Forwarded-Host": "mcp.acme.com"},
			want:    "https://mcp.acme.com",
		},
		{
			name:    "proxy chain uses the outermost value",
			headers: map[string]string{"X-Forwarded-Proto": "https, http", "X-Forwarded-Host": " mcp.acme.com , inner.local"},
			want:    "https://mcp.acme.com",
		},
		{
			name:    "only proto forwarded keeps the request host",
			headers: map[string]string{"X-Forwarded-Proto": "https"},
			want:    "https://example.test",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "http://example.test/x", nil)
			if tc.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			if got := BaseURL(r); got != tc.want {
				t.Errorf("BaseURL() = %q, want %q", got, tc.want)
			}
		})
	}
}
