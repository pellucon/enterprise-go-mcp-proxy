// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package sessioncache caches go-proxy's MCP client sessions to backend
// servers. Both of go-proxy's backend session caches use it: the one for
// forwarding client calls (proxyserver's sessionMap) and the
// triggered-tools runtime's one for platform servers. Every concurrent
// caller shares it, so it never blocks one caller on another's network
// call: connects run outside its lock, and a slow or hung backend only
// delays the callers waiting for that backend, each up to its own context.
package sessioncache

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ConnectTimeout bounds a connect (the MCP initialize handshake), so a
// backend that accepts the connection but never answers can't leave a
// connect, and every caller waiting on it, pending forever. A variable
// only so tests can shorten it.
var ConnectTimeout = 10 * time.Second

// Key identifies one cached session: a client session (all clients share
// the "" session, since the gateway serves statelessly), a server, and the
// exact target the session was opened to. The target is part of the key so
// a session is only ever reused for the URL and credentials it was opened
// with: a call that looked a server up just before its removal can't leave
// a session cached that a re-added server under the same name, at another
// URL, would then reuse.
type Key struct {
	Session           string
	Server            string
	URL               string
	CredentialsSecret string
}

// ConnectFunc opens a session. Its context carries no caller's values and
// expires after ConnectTimeout.
type ConnectFunc func(ctx context.Context) (*mcp.ClientSession, error)

// Cache is a set of cached sessions; the zero value isn't usable, use New.
type Cache struct {
	logPrefix string

	mu       sync.Mutex
	sessions map[Key]*mcp.ClientSession
	pending  map[Key]*pendingSession
	// generation counts CloseServer calls per server, so a session that
	// finishes connecting after its server was removed is closed instead
	// of cached.
	generation map[string]uint64
}

// pendingSession is a session being created; done is closed once session
// or err is set. cancel aborts the connect (CloseServer).
type pendingSession struct {
	done    chan struct{}
	cancel  context.CancelFunc
	session *mcp.ClientSession
	err     error
}

// New returns an empty Cache whose log lines start with logPrefix.
func New(logPrefix string) *Cache {
	return &Cache{
		logPrefix:  logPrefix,
		sessions:   make(map[Key]*mcp.ClientSession),
		pending:    make(map[Key]*pendingSession),
		generation: make(map[string]uint64),
	}
}

// Get returns the cached session for key, connecting with connect on the
// first call. Concurrent first calls share one connect and its result.
//
// ctx only bounds how long this caller waits. The connect itself runs on a
// fresh context (ConnectTimeout, no values), never on ctx, for two reasons:
// one caller giving up must not fail the others waiting on the same
// connect, and the session outlives the request and is shared by every
// caller, while the SDK keeps the connect context's values for the
// session's background requests - so a caller's per-request identity
// (relayheaders.WithMinter) must never reach it.
//
// A session that finishes connecting after CloseServer removed its server
// is closed and reported as an error, never cached. A cached session is
// dropped as soon as its connection ends (see dropWhenClosed), so the next
// call reconnects.
func (c *Cache) Get(ctx context.Context, key Key, connect ConnectFunc) (*mcp.ClientSession, error) {
	c.mu.Lock()
	if session, ok := c.sessions[key]; ok {
		c.mu.Unlock()
		return session, nil
	}
	p, ok := c.pending[key]
	if !ok {
		connectCtx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
		p = &pendingSession{done: make(chan struct{}), cancel: cancel}
		c.pending[key] = p
		go c.connect(connectCtx, key, p, c.generation[key.Server], connect)
	}
	c.mu.Unlock()

	select {
	case <-p.done:
		return p.session, p.err
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for a session to %s: %w", key.Server, ctx.Err())
	}
}

func (c *Cache) connect(ctx context.Context, key Key, p *pendingSession, generation uint64, connect ConnectFunc) {
	// Cancelling after the connect returns is safe: the SDK detaches an
	// established session from the connect context.
	defer p.cancel()
	type outcome struct {
		session *mcp.ClientSession
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		session, err := connect(ctx)
		done <- outcome{session, err}
	}()
	var session *mcp.ClientSession
	var err error
	select {
	case o := <-done:
		session, err = o.session, o.err
	case <-ctx.Done():
		// The SDK can take seconds more to return from an abandoned
		// connect (it tries to tell the server about the cancellation, and
		// a hung server doesn't answer that either), so the waiters don't
		// wait for it. A session that still turns up is closed.
		err = ctx.Err()
		go func() {
			if late := <-done; late.session != nil {
				late.session.Close()
			}
		}()
	}
	if err != nil && ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("connecting to %s timed out after %v: %w", key.Server, ConnectTimeout, err)
	}

	var stale *mcp.ClientSession
	c.mu.Lock()
	if c.pending[key] == p {
		delete(c.pending, key)
	}
	switch {
	case c.generation[key.Server] != generation:
		stale, session = session, nil
		err = fmt.Errorf("server %q was removed while connecting to it", key.Server)
	case err != nil:
	default:
		c.sessions[key] = session
		go c.dropWhenClosed(key, session)
	}
	p.session, p.err = session, err
	c.mu.Unlock()
	close(p.done)

	if stale != nil {
		if cerr := stale.Close(); cerr != nil {
			logging.Errorf("%sFailed to close session for removed server %v: %v", c.logPrefix, key.Server, cerr)
		}
	}
}

// dropWhenClosed waits for session's connection to end and then drops it
// from the cache, if it's still the cached one. The SDK ends a connection
// for good on a non-transient failure - a stateful backend that restarted
// or a backend replica that doesn't know the session ("session not
// found"), a backend that couldn't be reached - so without this every
// later call would fail on the dead session until discovery removed and
// re-added the server. The call that hit the failure still fails (it may
// have reached the backend, so it isn't retried); the next one reconnects.
func (c *Cache) dropWhenClosed(key Key, session *mcp.ClientSession) {
	err := session.Wait()
	c.mu.Lock()
	current := c.sessions[key] == session
	if current {
		delete(c.sessions, key)
	}
	c.mu.Unlock()
	if current {
		logging.Warnf("%sBackend session to %v ended (%v); the next call reconnects", c.logPrefix, key.Server, err)
	}
}

// CloseServer closes and drops every cached session to server, and aborts
// connects to it in progress. Call it when a backend server is removed, so
// that a future call reconnects instead of reusing a session whose
// transport is stuck pointing at a server that's gone. Each entry is
// dropped even if its Close fails, and the sessions are closed after the
// lock is released, since closing is a network call.
func (c *Cache) CloseServer(server string) {
	c.mu.Lock()
	c.generation[server]++
	for key, p := range c.pending {
		if key.Server == server {
			p.cancel()
		}
	}
	var closing []*mcp.ClientSession
	for key, session := range c.sessions {
		if key.Server == server {
			closing = append(closing, session)
			delete(c.sessions, key)
		}
	}
	c.mu.Unlock()

	for _, session := range closing {
		if err := session.Close(); err != nil {
			logging.Errorf("%sFailed to close session for server %v: %v", c.logPrefix, server, err)
		}
	}
}

// Cached returns a session cached for client session id and server, at any
// target, if there is one (for tests).
func (c *Cache) Cached(id, server string) (*mcp.ClientSession, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, session := range c.sessions {
		if key.Session == id && key.Server == server {
			return session, true
		}
	}
	return nil, false
}

// CloseAll closes and drops every cached session (for test cleanup).
func (c *Cache) CloseAll() {
	c.mu.Lock()
	sessions := c.sessions
	c.sessions = make(map[Key]*mcp.ClientSession)
	c.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}
