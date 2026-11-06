// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package logging

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSizeNeverShowsContent(t *testing.T) {
	for _, v := range []any{json.RawMessage(`{"ssn":"123-45-6789"}`), map[string]any{"ssn": "123-45-6789"}, "123-45-6789"} {
		if got := Size(v); strings.Contains(got, "123") || !strings.HasSuffix(got, " bytes") {
			t.Errorf("Size(%v) = %q; want just a byte count", v, got)
		}
	}
	if got := Size(nil); got != "none" {
		t.Errorf("Size(nil) = %q", got)
	}
}

func TestURLHidesPassword(t *testing.T) {
	if got := URL("nats://user:secret@nats:4222"); strings.Contains(got, "secret") || !strings.Contains(got, "user") {
		t.Errorf("URL = %q; want the password hidden", got)
	}
	if got := URL("nats://nats:4222"); got != "nats://nats:4222" {
		t.Errorf("URL without credentials = %q; want it unchanged", got)
	}
}
