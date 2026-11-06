// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package sessioncache

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newBackend starts an MCP backend and returns its URL.
func newBackend(t *testing.T) string {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "1.0.0"}, nil)
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// newHungBackend starts a server that accepts every request and never
// answers, until the client gives up.
func newHungBackend(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http notices the client going away only once the body has
		// been read.
		io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func connectTo(url string) ConnectFunc {
	return func(ctx context.Context) (*mcp.ClientSession, error) {
		return mcp.NewClient(&mcp.Implementation{}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	}
}

// TestGetWaiterGivesUpOnItsOwnContext checks that a caller waiting on a
// hung connect returns when its own context ends, rather than when the
// connect does.
func TestGetWaiterGivesUpOnItsOwnContext(t *testing.T) {
	c := New("")
	url := newHungBackend(t)
	key := Key{Server: "hung", URL: url}
	defer c.CloseServer("hung")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Get(ctx, key, connectTo(url))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get = %v; want the caller's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Get returned after %v; want about the caller's 200ms", elapsed)
	}
}

// TestGetConnectTimesOut checks that a backend that never answers
// initialize fails the connect after ConnectTimeout, and that the failure
// isn't cached.
func TestGetConnectTimesOut(t *testing.T) {
	old := ConnectTimeout
	ConnectTimeout = 300 * time.Millisecond
	defer func() { ConnectTimeout = old }()

	c := New("")
	url := newHungBackend(t)
	key := Key{Server: "hung", URL: url}

	_, err := c.Get(context.Background(), key, connectTo(url))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Get = %v; want a connect timeout", err)
	}
	if _, ok := c.Cached("", "hung"); ok {
		t.Fatal("a failed connect was cached")
	}
}

// TestGetConnectHasNoCallerValues checks that the connect doesn't run on
// the caller's context: the session outlives the request, and the SDK
// keeps the connect context's values for the session's background
// requests.
func TestGetConnectHasNoCallerValues(t *testing.T) {
	type callerKey struct{}
	c := New("")
	defer c.CloseAll()
	url := newBackend(t)

	ctx := context.WithValue(context.Background(), callerKey{}, "caller identity")
	var leaked any
	_, err := c.Get(ctx, Key{Server: "a", URL: url}, func(connectCtx context.Context) (*mcp.ClientSession, error) {
		leaked = connectCtx.Value(callerKey{})
		return connectTo(url)(connectCtx)
	})
	if err != nil {
		t.Fatal(err)
	}
	if leaked != nil {
		t.Errorf("the connect context carried the caller's value %v", leaked)
	}
}

// TestGetKeysOnTarget checks that a session is reused only for the target
// it was opened to: the same server name at another URL gets its own.
func TestGetKeysOnTarget(t *testing.T) {
	c := New("")
	defer c.CloseAll()
	urlA, urlB := newBackend(t), newBackend(t)
	ctx := context.Background()

	a, err := c.Get(ctx, Key{Server: "s", URL: urlA}, connectTo(urlA))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Get(ctx, Key{Server: "s", URL: urlB}, connectTo(urlB))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("the session to the old URL was reused for the new one")
	}
	again, err := c.Get(ctx, Key{Server: "s", URL: urlA}, connectTo(urlA))
	if err != nil || again != a {
		t.Fatalf("Get for the first URL again = %v, %v; want the cached session", again, err)
	}
}

// TestCloseServerAbortsPendingConnect checks that removing a server aborts
// a connect to it in progress, so its waiters return at once.
func TestCloseServerAbortsPendingConnect(t *testing.T) {
	c := New("")
	url := newHungBackend(t)

	errc := make(chan error, 1)
	go func() {
		_, err := c.Get(context.Background(), Key{Server: "hung", URL: url}, connectTo(url))
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond) // let the connect start
	c.CloseServer("hung")

	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "removed while connecting") {
			t.Fatalf("Get = %v; want a removed-while-connecting error", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get didn't return after its server was removed")
	}
}
