// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const INIT_SESSION_ID = sessionId("someSessionId")
const TEST_SESSION_ID = sessionId("someTestSessionId")

// closeAllSessions closes every cached session, for test cleanup.
func closeAllSessions(sm *sessionMap) {
	sm.cache.CloseAll()
}

func cachedSession(sm *sessionMap, id sessionId, server string) (*mcp.ClientSession, bool) {
	return sm.cache.Cached(string(id), server)
}

func verifyProxy(t *testing.T, session *mcp.ClientSession, serverName string) {
	t.Helper()
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Errorf("session for MCP server %v doesn't work: %v", serverName, err)
	}
}

// TestProxyCreation checks that sessionFor connects once per (client
// session, server) and reuses the session afterwards.
func TestProxyCreation(t *testing.T) {
	sm := newSessionMap()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer closeAllSessions(sm)

	// Hosts its own backends on ports 8082/8083 (matching
	// integration_test.go's MCP_SERVER1_PORT/MCP_SERVER2_PORT) rather than
	// depending on another test's servers still being up.
	server1, server2 := createServers()
	startTestBackend(t, ctx, cancel, MCP_SERVER1_PORT, server1)
	startTestBackend(t, ctx, cancel, MCP_SERVER2_PORT, server2)

	mcp1 := &mcpServerDetails{serverName: "mcp1", serverUrl: "http://localhost:8082/mcp"}
	mcp2 := &mcpServerDetails{serverName: "mcp2", serverUrl: "http://localhost:8083/mcp"}

	for _, id := range []sessionId{INIT_SESSION_ID, TEST_SESSION_ID} {
		first1, err := sm.sessionFor(ctx, id, mcp1)
		if err != nil {
			t.Fatalf("%s/mcp1: %v", id, err)
		}
		first2, err := sm.sessionFor(ctx, id, mcp2)
		if err != nil {
			t.Fatalf("%s/mcp2: %v", id, err)
		}
		verifyProxy(t, first1, "mcp1")
		verifyProxy(t, first2, "mcp2")
		if again, _ := sm.sessionFor(ctx, id, mcp1); again != first1 {
			t.Errorf("%s/mcp1: a second call made a new session instead of reusing it", id)
		}
	}
	a, _ := cachedSession(sm, INIT_SESSION_ID, "mcp1")
	b, _ := cachedSession(sm, TEST_SESSION_ID, "mcp1")
	if a == b {
		t.Error("two client sessions share one backend session")
	}
}

// initializeCounter wraps an MCP handler, counting "initialize" requests
// (one per new backend session). If gate is non-nil, the first initialize
// waits for it to be closed, after signalling started.
type initializeCounter struct {
	handler http.Handler
	count   atomic.Int32
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func (c *initializeCounter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	if strings.Contains(string(body), `"method":"initialize"`) {
		if c.count.Add(1) == 1 && c.gate != nil {
			c.once.Do(func() { close(c.started) })
			<-c.gate
		}
	}
	c.handler.ServeHTTP(w, r)
}

func newCountingBackend(t *testing.T, gated bool) (*initializeCounter, string) {
	t.Helper()
	backend := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "1.0.0"}, nil)
	counter := &initializeCounter{handler: mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, nil)}
	if gated {
		counter.gate, counter.started = make(chan struct{}), make(chan struct{})
	}
	srv := httptest.NewServer(counter)
	t.Cleanup(srv.Close)
	return counter, srv.URL
}

// TestSessionForConcurrentFirstCalls is the regression test for concurrent
// first calls: every client shares the "" session, so many calls can ask
// for a not-yet-connected backend at once. They must not race on the
// session map (run with -race), and must share one connection per backend.
func TestSessionForConcurrentFirstCalls(t *testing.T) {
	sm := newSessionMap()
	defer closeAllSessions(sm)
	counterA, urlA := newCountingBackend(t, false)
	counterB, urlB := newCountingBackend(t, false)
	servers := []*mcpServerDetails{{serverName: "a", serverUrl: urlA}, {serverName: "b", serverUrl: urlB}}

	var wg sync.WaitGroup
	got := make([]*mcp.ClientSession, 40)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session, err := sm.sessionFor(context.Background(), "", servers[i%2])
			if err != nil {
				t.Errorf("call %d: %v", i, err)
			}
			got[i] = session
		}(i)
	}
	wg.Wait()

	for i := range got {
		if got[i] != got[i%2] {
			t.Fatalf("call %d got a different session from the other calls to server %s", i, servers[i%2].serverName)
		}
	}
	if a, b := counterA.count.Load(), counterB.count.Load(); a != 1 || b != 1 {
		t.Errorf("connections made: a=%d b=%d; want 1 each", a, b)
	}
}

// TestSessionForServerRemovedWhileConnecting checks that a session that
// finishes connecting after its server was removed isn't cached - the
// next call reconnects instead of reusing a session to a removed server.
func TestSessionForServerRemovedWhileConnecting(t *testing.T) {
	sm := newSessionMap()
	defer closeAllSessions(sm)
	counter, url := newCountingBackend(t, true)
	details := &mcpServerDetails{serverName: "mcp1", serverUrl: url}

	errc := make(chan error, 1)
	go func() {
		_, err := sm.sessionFor(context.Background(), "", details)
		errc <- err
	}()
	<-counter.started
	sm.closeServerSessions("mcp1")
	close(counter.gate)

	if err := <-errc; err == nil || !strings.Contains(err.Error(), "removed while connecting") {
		t.Fatalf("sessionFor = %v; want a removed-while-connecting error", err)
	}
	if _, ok := cachedSession(sm, "", "mcp1"); ok {
		t.Fatal("a session to a removed server was cached")
	}
	session, err := sm.sessionFor(context.Background(), "", details)
	if err != nil {
		t.Fatalf("reconnecting after removal: %v", err)
	}
	verifyProxy(t, session, "mcp1")
}

// TestCloseServerSessionsRemovesDeadSessionAcrossClients is a regression
// test: when a backend server is killed, closeServerSessions must drop the
// cached client session for it for every client session, not just one -
// otherwise a client's next tool call reuses a session whose transport is
// stuck pointing at the now-dead server, instead of reconnecting once the
// server (or a replacement) comes back up.
func TestCloseServerSessionsRemovesDeadSessionAcrossClients(t *testing.T) {
	backend := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "1.0.0"}, nil)
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend
	}, nil))

	details := &mcpServerDetails{serverName: "mcp1", serverUrl: httpServer.URL}

	sm := newSessionMap()
	ctx := context.Background()

	const clientA = sessionId("client-a")
	const clientB = sessionId("client-b")

	for _, id := range []sessionId{clientA, clientB} {
		if _, err := sm.sessionFor(ctx, id, details); err != nil {
			t.Fatalf("expected a session for %s/mcp1: %v", id, err)
		}
	}

	// Simulate the backend process being killed: sever the live connection
	// first so Close() doesn't hang, then shut down the listener.
	httpServer.CloseClientConnections()
	httpServer.Close()

	sm.closeServerSessions("mcp1")

	for _, id := range []sessionId{clientA, clientB} {
		if _, ok := cachedSession(sm, id, "mcp1"); ok {
			t.Errorf("expected %s's dead session for mcp1 to be removed", id)
		}
	}

	// A restarted server should now be usable: since the dead entry was
	// removed, the next call must create a fresh session instead of reusing
	// the old (dead) one.
	backend2 := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "1.0.0"}, nil)
	httpServer2 := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return backend2
	}, nil))
	defer httpServer2.Close()
	// Deferred after httpServer2.Close, so it runs first: an httptest
	// server's Close waits for the session's open stream to end.
	defer closeAllSessions(sm)

	clientSession, err := sm.sessionFor(ctx, clientA, &mcpServerDetails{serverName: "mcp1", serverUrl: httpServer2.URL})
	if err != nil {
		t.Fatalf("expected clientA to reconnect to the restarted server: %v", err)
	}
	verifyProxy(t, clientSession, "mcp1")
}

// swappableHandler serves whatever handler was set last, standing in for
// a backend that restarts (or briefly fails) at the same address.
type swappableHandler struct{ h atomic.Value }

func (s *swappableHandler) set(h http.Handler) { s.h.Store(&h) }
func (s *swappableHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	(*s.h.Load().(*http.Handler)).ServeHTTP(w, r)
}

func mcpBackendHandler(stateless bool) http.Handler {
	backend := mcp.NewServer(&mcp.Implementation{Name: "test-backend", Version: "1.0.0"}, nil)
	mcp.AddTool(backend, &mcp.Tool{Name: "ping"}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
	})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return backend }, &mcp.StreamableHTTPOptions{Stateless: stateless})
}

// waitUntilDropped waits for sm to drop its cached session to server.
func waitUntilDropped(t *testing.T, sm *sessionMap, server string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := cachedSession(sm, "", server); !ok {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the dead session to %s was never dropped", server)
}

// TestSessionRecoversFromDeadConnection checks that a cached session whose
// connection the SDK ended for good is dropped, so the next call
// reconnects: a stateful backend that restarted (its sessions are gone, so
// it answers "session not found").
func TestSessionRecoversFromDeadConnection(t *testing.T) {
	backend := &swappableHandler{}
	backend.set(mcpBackendHandler(false))
	srv := httptest.NewServer(backend)
	defer srv.Close()
	sm := newSessionMap()
	defer closeAllSessions(sm)
	details := &mcpServerDetails{serverName: "mcp1", serverUrl: srv.URL}
	ctx := context.Background()

	first, err := sm.sessionFor(ctx, "", details)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err != nil {
		t.Fatalf("call before the restart: %v", err)
	}

	backend.set(mcpBackendHandler(false)) // restarted: the old session is unknown
	if _, err := first.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err == nil {
		t.Fatal("a call on the old session succeeded after the restart")
	}
	waitUntilDropped(t, sm, "mcp1")

	second, err := sm.sessionFor(ctx, "", details)
	if err != nil {
		t.Fatalf("reconnecting: %v", err)
	}
	if second == first {
		t.Fatal("got the dead session back")
	}
	if _, err := second.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err != nil {
		t.Errorf("call after reconnecting: %v", err)
	}
}

// TestSessionSurvivesTransientFailure checks that a failure the SDK treats
// as transient (a 503, a refused connection) doesn't drop the session: it
// keeps working once the backend answers again.
func TestSessionSurvivesTransientFailure(t *testing.T) {
	backend := &swappableHandler{}
	backend.set(mcpBackendHandler(true))
	srv := httptest.NewServer(backend)
	defer srv.Close()
	sm := newSessionMap()
	defer closeAllSessions(sm)
	ctx := context.Background()

	session, err := sm.sessionFor(ctx, "", &mcpServerDetails{serverName: "mcp1", serverUrl: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	backend.set(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err == nil {
		t.Fatal("the call during the outage succeeded")
	}
	backend.set(mcpBackendHandler(true))
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "ping"}); err != nil {
		t.Fatalf("call after the outage: %v", err)
	}
	if cached, ok := cachedSession(sm, "", "mcp1"); !ok || cached != session {
		t.Error("a transient failure dropped the session")
	}
}
