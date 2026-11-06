// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package eventlog

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNewArchiverTransportDiscard(t *testing.T) {
	tr, err := NewArchiverTransport("discard", nil)
	if err != nil {
		t.Fatalf("NewArchiverTransport(discard): %v", err)
	}
	if _, ok := tr.(DiscardArchiverTransport); !ok {
		t.Fatalf("expected a DiscardArchiverTransport, got %T", tr)
	}
	if err := tr.Upload(context.Background(), "/does/not/exist"); err != nil {
		t.Fatalf("DiscardArchiverTransport.Upload should never fail, got %v", err)
	}
}

func TestNewArchiverTransportHTTP(t *testing.T) {
	props, _ := json.Marshal(map[string]string{"endpoint": "https://example.com/archive"})
	tr, err := NewArchiverTransport("http", props)
	if err != nil {
		t.Fatalf("NewArchiverTransport(http): %v", err)
	}
	httpTransport, ok := tr.(*HTTPArchiverTransport)
	if !ok {
		t.Fatalf("expected an *HTTPArchiverTransport, got %T", tr)
	}
	if httpTransport.Endpoint != "https://example.com/archive" {
		t.Fatalf("expected trimmed endpoint, got %q", httpTransport.Endpoint)
	}
}

func TestNewArchiverTransportHTTPRequiresEndpoint(t *testing.T) {
	if _, err := NewArchiverTransport("http", nil); err == nil {
		t.Fatal("expected an error for a missing endpoint property")
	}
}

func TestNewArchiverTransportUnknownType(t *testing.T) {
	if _, err := NewArchiverTransport("does-not-exist", nil); err == nil {
		t.Fatal("expected an error for an unregistered archiver transport type")
	}
}

func TestRegisterArchiverTransportDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected RegisterArchiverTransport to panic on a duplicate type name")
		}
	}()
	RegisterArchiverTransport("discard", func(json.RawMessage) (ArchiverTransport, error) { return DiscardArchiverTransport{}, nil })
}

// stubArchiverTransport lets TestArchiverSetArchiverTransportSwapsLive
// observe which ArchiverTransport instance a sweep actually used.
type stubArchiverTransport struct{ uploaded *[]string }

func (s stubArchiverTransport) Upload(ctx context.Context, path string) error {
	*s.uploaded = append(*s.uploaded, path)
	return nil
}

func TestArchiverSetArchiverTransportSwapsLive(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, 50)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	defer w.Close()
	for i := 0; i < 5; i++ {
		if err := w.Append(mkEvent("alert", time.Now().UTC())); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	var uploaded []string
	a := NewArchiver(dir, w.Active, DiscardArchiverTransport{}, 0, 1)
	a.SetArchiverTransport(stubArchiverTransport{uploaded: &uploaded})
	a.sweep(context.Background())

	if len(uploaded) == 0 {
		t.Fatal("expected the swapped-in stub archiver transport to receive at least one upload")
	}
}

// TestHTTPArchiverUploadTimesOut checks that an endpoint that never answers
// fails the upload instead of stalling archiving forever.
func TestHTTPArchiverUploadTimesOut(t *testing.T) {
	old := uploadTimeout
	uploadTimeout = 200 * time.Millisecond
	defer func() { uploadTimeout = old }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "events-20261104.jsonl")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- (&HTTPArchiverTransport{Endpoint: srv.URL}).Upload(context.Background(), path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an upload that was never answered succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the upload didn't time out")
	}
}
