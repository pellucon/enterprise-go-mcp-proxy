// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/backendauth"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	"github.com/pellucon/enterprise-go-mcp-proxy/sessioncache"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// platformBaseTransport has keep-alives disabled so a multi-pod backend
// Service's load balancing isn't defeated by a cached, pinned connection
// (the same reasoning as session_proxies.go's backend transport).
// The dial and TLS handshake are bounded, as there.
var platformBaseTransport = &http.Transport{
	DisableKeepAlives:   true,
	DialContext:         (&net.Dialer{Timeout: platformDialTimeout}).DialContext,
	TLSHandshakeTimeout: platformDialTimeout,
}

// platformDialTimeout bounds a TCP dial or TLS handshake to a platform
// server.
const platformDialTimeout = 10 * time.Second

// platformHTTPClientFor is the HTTP client for a session to serverName. Its
// relayheaders.ServerTransport attaches, per request, the relay headers
// (identity assertion, relay token and callback URL if serverName may
// relay, tool-call ID) the calling ctx's minter produces for serverName -
// the same mechanism go-proxy's ordinary backend calls use - so a
// discovered triggered tool gets the same relay capability an ordinary
// backend tool has (it can call fetchActiveDeals/fetchCustomerType-style
// relay helpers unchanged; see CLAUDE.md's "Business logic validation"
// section). Guard's own ctx (orchestrate.go) already carries the minter by
// the time it reaches session/SessionFor - createMiddleware puts it there
// before ever dispatching to Guard. Read per request from ctx, never cached
// on the shared session: a session in r.sessions is reused across
// concurrent calls, potentially from different callers.
func platformHTTPClientFor(serverName string) *http.Client {
	return &http.Client{Transport: &relayheaders.ServerTransport{Base: platformBaseTransport, Server: serverName}}
}

// Runtime is the access_control.AccessPolicyEngine structural analog for
// orchestration: it holds the current OrchestrationConfig behind a mutex,
// plus a lazily-populated session cache for platform servers. It is
// deliberately not built on dynamic_proxy.genericRegistry/FeaturePoller —
// triggered tools are invoked by a fixed, config-declared (server, tool)
// name, so there's no need to continuously poll tools/list and maintain a
// live registry the way the client-facing Gateway does.
type Runtime struct {
	mu                    sync.RWMutex
	cfg                   *OrchestrationConfig
	eventPublisher        EventPublisher
	requestEventPublisher RequestEventPublisher

	validationMu sync.Mutex
	validated    map[string]error
	validating   map[string]*validation
	// validationEpoch counts Updates, so a check that started under the
	// previous config isn't cached under the new one.
	validationEpoch uint64

	serversMu   sync.Mutex
	serverAttrs map[string]*discover_mcp_servers.ServerAttributes
	// sessions is keyed by server (and target), with the client session
	// always "": these sessions aren't per client.
	sessions *sessioncache.Cache

	// nativeMu guards nativeCallAuthorizations/nativeResultAuthorizations —
	// see native.go. Separate from mu since registration happens once at
	// startup and lookups happen on the hot path; no reason to contend with
	// cfg's own RWMutex.
	nativeMu                   sync.RWMutex
	nativeCallAuthorizations   map[string]NativeCallAuthorizationFunc
	nativeResultAuthorizations map[string]NativeResultAuthorizationFunc
}

// NewRuntime creates a Runtime holding initial as the current config. A nil
// initial (the "orchestration" section absent from the combined config
// file) is a total no-op: MatchOrchestration never matches, so every tool
// call proceeds unguarded.
func NewRuntime(initial *OrchestrationConfig) *Runtime {
	return &Runtime{
		cfg:                        initial,
		validated:                  make(map[string]error),
		validating:                 make(map[string]*validation),
		serverAttrs:                make(map[string]*discover_mcp_servers.ServerAttributes),
		sessions:                   sessioncache.New("[triggered-tools-runtime] "),
		nativeCallAuthorizations:   make(map[string]NativeCallAuthorizationFunc),
		nativeResultAuthorizations: make(map[string]NativeResultAuthorizationFunc),
	}
}

// Update replaces the entire orchestration config, matching
// AccessPolicyEngine.Update's whole-config-replace semantics (no per-rule
// patch/merge). Clears the signature-validation cache, since a config
// change may reassign which server/tool backs any given step.
func (r *Runtime) Update(cfg *OrchestrationConfig) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()

	r.validationMu.Lock()
	r.validated = make(map[string]error)
	r.validating = make(map[string]*validation)
	r.validationEpoch++
	r.validationMu.Unlock()
}

// Snapshot returns the current config, read-only (same convention as
// AccessPolicyEngine.Snapshot — never mutate the result or a pointer passed
// to Update after the call).
func (r *Runtime) Snapshot() *OrchestrationConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg
}

// SetEventPublisher installs the publisher Guard uses for its
// tool-request-result event (see fireAudit in orchestrate.go). Optional and
// nil-safe, matching this codebase's convention for every other optional
// capability (auth, internalAuth, orchestration, natsUrl): a Runtime with no
// publisher set behaves exactly as if a matched orchestration's
// ToolCallAudit were always empty — Guard logs and drops the event rather
// than erroring. Not a NewRuntime parameter, so existing call sites that
// construct a Runtime via NewRuntime(cfg) don't need to change.
func (r *Runtime) SetEventPublisher(p EventPublisher) {
	r.mu.Lock()
	r.eventPublisher = p
	r.mu.Unlock()
}

// SetRequestEventPublisher installs the publisher Guard uses for its
// "tool-request" event (see fireRequestAudit in orchestrate.go) — the
// ToolRequestAuditSentinel-gated sibling of SetEventPublisher's
// ToolCallAuditSentinel-gated one. Same nil-safe-optional convention: a
// Runtime with no publisher set behaves exactly as if
// ToolRequestAuditSentinel were never configured. Independent of
// SetEventPublisher — either, both, or neither may be installed.
func (r *Runtime) SetRequestEventPublisher(p RequestEventPublisher) {
	r.mu.Lock()
	r.requestEventPublisher = p
	r.mu.Unlock()
}

// MatchOrchestration finds the first ToolMapping whose Pattern matches
// toolName (an already-namespaced tool name, e.g. "customer-service.*"),
// and returns the orchestration it maps to. A matching mapping whose
// orchestration doesn't exist is still a match (fail closed): Guard then
// rejects the call, rather than the tool running unguarded.
func (r *Runtime) MatchOrchestration(toolName string) (string, Orchestration, bool) {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()

	if cfg == nil {
		return "", Orchestration{}, false
	}
	for _, m := range cfg.ToolMappings {
		if access_control.GlobMatch(m.Pattern, toolName) {
			return m.Orchestration, cfg.Orchestrations[m.Orchestration], true
		}
	}
	return "", Orchestration{}, false
}

// OnAdded records URL info for newly-discovered platform servers so a
// future call can lazily dial them. Mirrors dynamic_proxy.Gateway.OnAdded's
// defensive refusal of any reserved server name.
func (r *Runtime) OnAdded(servers discover_mcp_servers.ServerMap) {
	r.serversMu.Lock()
	defer r.serversMu.Unlock()
	for name, attrs := range servers {
		if dynamic_proxy.IsReservedServerName(name) {
			logging.Errorf("[triggered-tools-runtime] refusing to register reserved server name %q", name)
			continue
		}
		r.serverAttrs[name] = attrs
	}
}

// OnRemoved drops URL info and closes/evicts any cached session for
// removed platform servers, so a future re-discovery reconnects instead of
// reusing a session pointing at a server that's gone.
func (r *Runtime) OnRemoved(servers discover_mcp_servers.ServerMap) {
	r.serversMu.Lock()
	for name := range servers {
		delete(r.serverAttrs, name)
	}
	r.serversMu.Unlock()
	for name := range servers {
		r.sessions.CloseServer(name)
	}
}

// session lazily dials serverName on first use and caches the result,
// reusing the connect pattern from session_proxies.go's
// mcpServerDetails.createMCPSession (same DisableKeepAlives rationale).
// ctx (the calling request's) only bounds how long this call waits: the
// connect runs on its own context, outside serversMu, so a slow or hung
// platform server doesn't hold up calls to any other (see
// sessioncache.Cache.Get).
func (r *Runtime) session(ctx context.Context, serverName string) (*mcp.ClientSession, error) {
	r.serversMu.Lock()
	attrs, ok := r.serverAttrs[serverName]
	r.serversMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("platform server %q is not currently discovered", serverName)
	}

	key := sessioncache.Key{Server: serverName, URL: attrs.ServerUrl, CredentialsSecret: attrs.CredentialsSecret}
	return r.sessions.Get(ctx, key, func(connectCtx context.Context) (*mcp.ClientSession, error) {
		client := mcp.NewClient(&mcp.Implementation{}, nil)
		sess, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{
			Endpoint:   attrs.ServerUrl,
			HTTPClient: backendauth.Client(platformHTTPClientFor(serverName), attrs.CredentialsSecret),
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to connect to platform server %q at %v: %w", serverName, attrs.ServerUrl, err)
		}
		return sess, nil
	})
}

// SessionFor exposes Runtime's own lazily-dialed, cached backend session —
// the same mechanism invokeCallAuthorization/invokeResultAuthorization use
// internally for a step naming a real discovered server — to any other
// go-proxy code that needs to call a real discovered platform server
// directly by name, independent of whether orchestration is even
// configured (OnAdded/OnRemoved track every discovered server unconditionally,
// not just ones an orchestration references). Two current callers, neither
// aware of the other: an in-process native step (see native.go, e.g.
// go-proxy's constraints_validation calling declarative-guardrail's
// evaluate_declarations), and go-proxy's rate-limiting gate
// (rate_limit_gate.go's checkRateLimit, root package, calling
// rate-limiter's check_permission) — this method has no orchestration-
// specific behavior itself, it's just a connection cache keyed by server
// name. serverName must name a real discovered server (see OnAdded);
// dynamic_proxy.UtilityServerName itself is never valid here — OnAdded
// already refuses to record it.
func (r *Runtime) SessionFor(ctx context.Context, serverName string) (*mcp.ClientSession, error) {
	return r.session(ctx, serverName)
}
