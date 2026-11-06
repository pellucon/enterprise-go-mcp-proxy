// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package main

import "testing"

func TestResolveLLMBaseURL(t *testing.T) {
	cases := []struct {
		name, flag, env, want string
	}{
		{"built-in default when nothing is set", "", "", defaultLLMBaseURL},
		{"environment variable overrides the default", "", "https://api.openai.com/v1", "https://api.openai.com/v1"},
		{"explicit flag wins over the environment", "https://flag.example/v1", "https://api.openai.com/v1", "https://flag.example/v1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveLLMBaseURL(c.flag, c.env); got != c.want {
				t.Errorf("resolveLLMBaseURL(%q, %q) = %q, want %q", c.flag, c.env, got, c.want)
			}
		})
	}
}
