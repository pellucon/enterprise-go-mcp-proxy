// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/backendauth"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	"github.com/pellucon/enterprise-go-mcp-proxy/sessioncache"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// backendBaseTransport is the transport under every proxy-to-backend
// connection. Keep-alives are disabled so each connection is a fresh TCP
// dial rather than one reused from Go's default per-host connection pool —
// otherwise a backend Service with multiple pods would keep landing on
// whichever single pod the pooled connection first reached, for as long as
// that connection stayed cached in sessionMap, defeating the load balancing
// a Service is supposed to provide across the backend's pods.
//
// The dial and TLS handshake are bounded, so an unreachable backend fails
// a connect or call quickly instead of waiting on the OS's TCP timeout.
// Responses aren't: a tool call may legitimately run for a long time.
var backendBaseTransport = &http.Transport{
	DisableKeepAlives:   true,
	DialContext:         (&net.Dialer{Timeout: backendDialTimeout}).DialContext,
	TLSHandshakeTimeout: backendDialTimeout,
}

// backendDialTimeout bounds a TCP dial or TLS handshake to a backend.
const backendDialTimeout = 10 * time.Second

// backendHTTPClientFor is the HTTP client for a session to serverName: its
// relayheaders.ServerTransport attaches, per request, the identity
// assertion minted for serverName (and, only if serverName may relay, a
// relay token and the callback URL) from the request context's minter -
// see createMiddleware.
func backendHTTPClientFor(serverName string) *http.Client {
	return &http.Client{Transport: &relayheaders.ServerTransport{Base: backendBaseTransport, Server: serverName}}
}

// backendProgressHandler receives backends' progress notifications on
// go-proxy's client sessions to them and relays each to the client whose
// call it belongs to (dynamic_proxy.Gateway.HandleBackendProgress). Set by
// runProxyServerOn once the gateway exists; nil means progress isn't
// relayed.
var backendProgressHandler func(context.Context, *mcp.ProgressNotificationClientRequest)

type serverDetailMap map[string]*mcpServerDetails

type mcpServerDetails struct {
	serverName        string
	serverUrl         string
	credentialsSecret string // see discover_mcp_servers.ServerAttributes.CredentialsSecret
}

type sessionId string

// sessionMap caches go-proxy's client sessions to backend servers, for
// forwarding client calls. It's used by every concurrent call; see
// sessioncache for how concurrent first calls, removal while connecting,
// and dead connections are handled.
type sessionMap struct {
	cache *sessioncache.Cache
}

func newSessionMap() *sessionMap {
	return &sessionMap{cache: sessioncache.New("")}
}

// sessionFor returns the cached session to details' server for client
// session id, connecting on the first call. ctx (the calling request's)
// only bounds how long this call waits: see sessioncache.Cache.Get.
func (sMap *sessionMap) sessionFor(ctx context.Context, id sessionId, details *mcpServerDetails) (*mcp.ClientSession, error) {
	key := sessioncache.Key{
		Session:           string(id),
		Server:            details.serverName,
		URL:               details.serverUrl,
		CredentialsSecret: details.credentialsSecret,
	}
	return sMap.cache.Get(ctx, key, details.createMCPSession)
}

func (serverDetails *mcpServerDetails) createMCPSession(ctx context.Context) (*mcp.ClientSession, error) {
	// MultiRoundTrip disabled: when a backend tool needs user input (an
	// "input required" result, protocol 2026-07-28+), go-proxy must pass that
	// result back to its own client instead of trying to answer it itself -
	// it can't ask a user. The client answers and calls again with
	// inputResponses/requestState, which registerTool forwards. See
	// dynamic_proxy/gateway.go's registerTool.
	client := mcp.NewClient(&mcp.Implementation{}, &mcp.ClientOptions{
		ProgressNotificationHandler: backendProgressHandler,
		MultiRoundTrip:              &mcp.MultiRoundTripOptions{Disabled: true},
	})
	clientSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   serverDetails.serverUrl,
		HTTPClient: backendauth.Client(backendHTTPClientFor(serverDetails.serverName), serverDetails.credentialsSecret),
	}, nil)
	if err != nil {
		logging.Errorf("Failed to connect to mcp server %v in url %v: %v", serverDetails.serverName, serverDetails.serverUrl, err)
		return nil, fmt.Errorf("connecting to server %s: %w", serverDetails.serverName, err)
	}
	return clientSession, nil
}

// closeServerSessions closes and drops the cached client session for
// serverName across every client session, and aborts connects to it in
// progress. Call this when a backend server is removed, so that a future
// tool call reconnects instead of reusing a session whose transport is
// stuck pointing at a server that's gone.
func (sMap *sessionMap) closeServerSessions(serverName string) {
	sMap.cache.CloseServer(serverName)
}
