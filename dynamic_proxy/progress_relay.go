// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// Progress relay: when a client asks for progress on a tool call (a progress
// token in the request's _meta), the gateway forwards the call with a token
// of its own and relays every progress notification the backend sends for
// it back to that client, under the client's original token.
//
// Why a gateway-issued token rather than the client's: a backend sees all
// gateway users through one shared session, so two clients could both use
// token 1. Each forwarded call gets a unique token here, mapped back to the
// client session and token that asked for it.
//
// Why the client request's own context: the gateway serves clients
// statelessly, so a notification can only reach the client on the stream of
// the request that's still running, and the SDK finds that stream through
// values on the request's context. createMiddleware strips those values
// before handlers run (so they can't leak into backend calls), but first
// saves the original context with WithClientRequestContext; it's used here
// only to send notifications to the client, never for a backend call.

type clientRequestContextKey struct{}

// WithClientRequestContext stores the client request's original, unstripped
// context on ctx, for sending notifications back to that client.
func WithClientRequestContext(ctx, original context.Context) context.Context {
	return context.WithValue(ctx, clientRequestContextKey{}, original)
}

// clientRequestContext returns the original context saved by
// WithClientRequestContext, or ctx itself when there is none (e.g. a
// gateway used without go-proxy's middleware, whose ctx is still the SDK's).
func clientRequestContext(ctx context.Context) context.Context {
	if original, ok := ctx.Value(clientRequestContextKey{}).(context.Context); ok {
		return original
	}
	return ctx
}

type withoutProgressMessagesKey struct{}

// WithoutProgressMessages marks ctx's tool call as one whose progress is
// relayed to the client without its message text: go-proxy uses it for a
// tool whose results are checked by guardrails, since a backend's message
// is free text no result check sees. The progress values still reach the
// client.
func WithoutProgressMessages(ctx context.Context) context.Context {
	return context.WithValue(ctx, withoutProgressMessagesKey{}, true)
}

func withoutProgressMessages(ctx context.Context) bool {
	drop, _ := ctx.Value(withoutProgressMessagesKey{}).(bool)
	return drop
}

type progressEntry struct {
	session     *mcp.ServerSession
	clientToken any
	ctx         context.Context
	// dropMessage relays the progress without its message text (see
	// WithoutProgressMessages).
	dropMessage bool
}

type progressRelay struct {
	mu      sync.Mutex
	entries map[string]progressEntry
}

func newProgressRelay() *progressRelay {
	return &progressRelay{entries: make(map[string]progressEntry)}
}

func (r *progressRelay) register(e progressEntry) (token string, done func()) {
	token = "gw-progress-" + uuid.NewString()
	r.mu.Lock()
	r.entries[token] = e
	r.mu.Unlock()
	return token, func() {
		r.mu.Lock()
		delete(r.entries, token)
		r.mu.Unlock()
	}
}

func (r *progressRelay) lookup(token any) (progressEntry, bool) {
	s, ok := token.(string)
	if !ok {
		return progressEntry{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[s]
	return e, ok
}

func (r *progressRelay) pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// RelayProgress prepares an outgoing call to relay progress to the client
// whose request ctx and session belong to. If clientToken is nil (the client
// didn't ask for progress) it does nothing. Otherwise it sets a
// gateway-issued token on outgoing and returns a done func that must be
// called once the outgoing call has returned. Used by the gateway's own tool
// forwarding, and by proxy_utility.call_tool for its loopback call.
func (g *Gateway) RelayProgress(ctx context.Context, session *mcp.ServerSession, clientToken any, outgoing interface{ SetProgressToken(any) }) (done func()) {
	if clientToken == nil || session == nil {
		return func() {}
	}
	token, done := g.progress.register(progressEntry{session: session, clientToken: clientToken, ctx: clientRequestContext(ctx), dropMessage: withoutProgressMessages(ctx)})
	outgoing.SetProgressToken(token)
	return done
}

// HandleBackendProgress is the ProgressNotificationHandler for go-proxy's
// connections to backends: it forwards a backend's progress notification to
// the client the call belongs to, under that client's own token. A
// notification for an unknown or already-finished call is dropped.
func (g *Gateway) HandleBackendProgress(_ context.Context, req *mcp.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	entry, ok := g.progress.lookup(req.Params.ProgressToken)
	if !ok {
		logging.Debugf("[Gateway] Dropping progress notification for unknown or finished call (token %v)", req.Params.ProgressToken)
		return
	}
	params := &mcp.ProgressNotificationParams{
		ProgressToken: entry.clientToken,
		Progress:      req.Params.Progress,
		Total:         req.Params.Total,
		Message:       req.Params.Message,
	}
	if entry.dropMessage {
		params.Message = ""
	}
	if err := entry.session.NotifyProgress(entry.ctx, params); err != nil {
		logging.Debugf("[Gateway] Failed to relay progress notification to client: %v", err)
	}
}
