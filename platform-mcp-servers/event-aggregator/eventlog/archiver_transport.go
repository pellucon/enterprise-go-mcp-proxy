// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// ArchiverTransport ships an already-rolled, immutable segment file out of
// the cluster. Upload's error, if any, aborts that segment's archival for
// this sweep (retried on the next one) — the local file is only deleted
// after a successful Upload.
type ArchiverTransport interface {
	Upload(ctx context.Context, path string) error
}

// ArchiverTransportFactory builds an ArchiverTransport from its
// "properties" JSON object — the eventAggregation config's
// `archiverTransport.properties` field, verbatim and unvalidated beyond
// "is a JSON object" (see events.ParseAggregationConfig). nil/empty
// properties is valid for an implementation that needs no configuration
// of its own (e.g. DiscardArchiverTransport).
type ArchiverTransportFactory func(properties json.RawMessage) (ArchiverTransport, error)

var (
	archiverTransportFactoriesMu sync.RWMutex
	archiverTransportFactories   = map[string]ArchiverTransportFactory{}
)

// RegisterArchiverTransport makes an ArchiverTransport implementation
// selectable by typeName via the eventAggregation config's
// `"archiverTransport":{"type":...}` field (see NewArchiverTransport).
// Every implementation in this package calls this from its own init()
// (see discard_archiver_transport.go/http_archiver_transport.go) — this
// is the only piece adding a *new* implementation needs: drop a new .go
// file in this package with its own type + init(), nothing else in the
// package needs editing, since Go compiles every file in a package
// together and init() runs before main() regardless of which file it's
// in. Panics on a duplicate typeName — a programming error (two files
// registering the same name), not a runtime/config condition, so it's
// caught at process startup rather than surfacing as a confusing "wrong
// implementation selected" bug later.
func RegisterArchiverTransport(typeName string, factory ArchiverTransportFactory) {
	archiverTransportFactoriesMu.Lock()
	defer archiverTransportFactoriesMu.Unlock()
	if _, exists := archiverTransportFactories[typeName]; exists {
		panic("eventlog: archiver transport type already registered: " + typeName)
	}
	archiverTransportFactories[typeName] = factory
}

// NewArchiverTransport builds the ArchiverTransport registered under
// typeName (see RegisterArchiverTransport), or an error if no
// implementation has registered that name — e.g. a config typo, or a
// type this binary simply doesn't have the implementation file for.
func NewArchiverTransport(typeName string, properties json.RawMessage) (ArchiverTransport, error) {
	archiverTransportFactoriesMu.RLock()
	factory, ok := archiverTransportFactories[typeName]
	archiverTransportFactoriesMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("eventlog: unknown archiver transport type %q", typeName)
	}
	return factory(properties)
}
