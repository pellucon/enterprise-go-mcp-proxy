// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"encoding/json"
)

func init() {
	RegisterArchiverTransport("discard", func(json.RawMessage) (ArchiverTransport, error) {
		return DiscardArchiverTransport{}, nil
	})
}

// DiscardArchiverTransport is the default ArchiverTransport
// (`"archiverTransport":{"type":"discard"}` in every committed
// eventAggregation config) when no real archive destination is
// configured: it ships nothing anywhere, but still reports success, so
// Archiver's normal upload-then-delete step still deletes the local
// segment — the on-disk log's total size stays bounded out-of-the-box
// even with no external destination set up. See the tiered-event-storage
// design plan's "Default archive destination: discard, not disabled".
// Takes no properties.
type DiscardArchiverTransport struct{}

// Upload is a no-op: it performs no I/O and always succeeds.
func (DiscardArchiverTransport) Upload(ctx context.Context, path string) error { return nil }
