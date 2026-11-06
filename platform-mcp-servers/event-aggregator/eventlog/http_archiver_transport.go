// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func init() {
	RegisterArchiverTransport("http", newHTTPArchiverTransport)
}

// httpArchiverTransportProperties is HTTPArchiverTransport's own
// "properties" shape in the eventAggregation config's
// `archiverTransport.properties` field, e.g.
// {"type":"http","properties":{"endpoint":"https://archive.example.com/events"}}.
type httpArchiverTransportProperties struct {
	Endpoint string `json:"endpoint"`
}

func newHTTPArchiverTransport(properties json.RawMessage) (ArchiverTransport, error) {
	var props httpArchiverTransportProperties
	if len(properties) > 0 {
		if err := json.Unmarshal(properties, &props); err != nil {
			return nil, fmt.Errorf("eventlog: invalid http archiver transport properties: %w", err)
		}
	}
	if props.Endpoint == "" {
		return nil, fmt.Errorf(`eventlog: http archiver transport requires a non-empty "endpoint" property`)
	}
	return &HTTPArchiverTransport{Endpoint: strings.TrimRight(props.Endpoint, "/")}, nil
}

// HTTPArchiverTransport uploads a segment file via a plain HTTP PUT to
// Endpoint + "/" + the segment's base filename — no cloud SDK; works
// against an S3/GCS presigned-URL-per-object scheme or a custom receiver
// that accepts `PUT <endpoint>/<filename>`.
type HTTPArchiverTransport struct {
	Endpoint string
	Client   *http.Client
}

// Upload PUTs path's contents to Endpoint/<basename>.
// uploadTimeout bounds one segment upload. A variable so tests can shorten
// it.
var uploadTimeout = 10 * time.Minute

func (t *HTTPArchiverTransport) Upload(ctx context.Context, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("eventlog: failed to open %s for upload: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("eventlog: failed to stat %s: %w", path, err)
	}

	// Bounded: the archiver uploads one segment at a time, so an endpoint
	// that accepts the connection and never answers would otherwise stall
	// archiving for good, and the local log would grow without limit.
	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	url := t.Endpoint + "/" + filepath.Base(path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, f)
	if err != nil {
		return fmt.Errorf("eventlog: failed to build upload request: %w", err)
	}
	req.ContentLength = info.Size()
	req.Header.Set("Content-Type", "application/x-ndjson")

	client := t.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("eventlog: upload request to %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("eventlog: upload to %s returned status %d", url, resp.StatusCode)
	}
	return nil
}
