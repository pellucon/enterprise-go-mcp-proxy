// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/forwarded"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/nats-io/nats.go"
)

var (
	sessions           *sessionMap = newSessionMap()
	accessPolicyEngine *access_control.AccessPolicyEngine
	mcpServers         serverDetailMap
	// authConfig is populated entirely from the config file's "auth"
	// section in doRun; it has no hardcoded default. If that section is
	// absent, authConfig stays a zero-value auth.Config, and
	// auth.NewVerifier fails fast at startup (empty PublicKeyPath/JWKSURL)
	// rather than silently falling back to a fixed key path.
	authConfig auth.Config
	// triggeredRuntime is populated in doRun from the config file's
	// "orchestration" section, alongside accessPolicyEngine. A nil
	// Orchestration section (triggered_tools_runtime.NewRuntime(nil)) makes
	// it a total no-op: no server is ever classified as a platform server,
	// and no tool call is ever guarded.
	triggeredRuntime *triggered_tools_runtime.Runtime
	// internalSigner is populated in doRun from the config file's
	// "internalAuth" section. A nil internalSigner (section absent) makes
	// the internal-identity-assertion feature a total no-op: no backend
	// ever receives an assertion or relay token, and no relayed call can
	// authenticate. See docs/architecture/auth.md's "Internal identity
	// assertions" section.
	internalSigner *auth.Signer
	// relayTokenVerifier verifies a relay or loopback token presented back
	// to the proxy (see authenticateRequest) - never the identity
	// assertion, whose audience it rejects. It checks signature, issuer
	// and expiry; authenticateRequest checks the audience itself, since
	// it accepts two. Nil unless internalAuth is configured.
	relayTokenVerifier *auth.Verifier
	// internalAuthConfig is proxyCfg.InternalAuth, retained verbatim:
	// createMiddleware reads its Relay permissions and MaxRelayHops, and
	// _admin.export_full_config (admin_tools.go) round-trips it back out
	// unchanged. Nil under the same conditions internalSigner is nil.
	internalAuthConfig *auth.SignerConfig
	// eventsClient is go-proxy's own connection for publishing events (see
	// CLAUDE.md's "NATS event system" section), populated in doRunOn once
	// natsUrl (mandatory, see ProxyConfig.NatsURL) has been used to connect.
	// A package var (not local to doRunOn) because admin_tools.go's
	// registerAdminTools, called from runProxyServerOn, needs to reference
	// it too — mirroring triggeredRuntime/accessPolicyEngine. Also consulted
	// by the /healthz handler (runProxyServerOn) alongside natsConn below.
	eventsClient *events.Client
	// natsConn is configsync's own NATS connection, independent of
	// eventsClient above (see configsync.Connect's doc comment on why the
	// two packages don't share a connection). A package var, unlike a
	// doRunOn-local one, purely so the /healthz handler built in
	// runProxyServerOn can query its IsConnected() state alongside
	// eventsClient's.
	natsConn *nats.Conn
	// selfURL is proxyCfg.SelfURL, copied to a package var so
	// createMiddleware (middleware.go) can read it without proxyCfg being
	// threaded through as a parameter — same reasoning as the other
	// doRunOn-populated vars here. Empty means "not configured": the
	// callback-URL header is simply never attached.
	selfURL string
	// natsURLConfig is proxyCfg.NatsURL, retained verbatim (unlike natsConn/
	// eventsClient, which are connections built from it) purely so
	// _admin.export_full_config (admin_tools.go) can round-trip it back out
	// unchanged.
	natsURLConfig string
	// roleMappingConfig is proxyCfg.RoleMapping, retained verbatim purely
	// so _admin.export_full_config can round-trip it. The value actually
	// enforced lives in accessPolicyEngine (fixed at construction); nothing
	// ever reassigns either after doRunOn sets them.
	roleMappingConfig access_control.RoleMapping

	// discoveryWatcher holds the live discovery config: runProxyServerOn's
	// ServerMonitor reads discoveryWatcher.Current() on every poll tick
	// (rather than closing over proxyCfg.Discovery once), and
	// _admin.update_discovery's handler (admin_tools.go) applies an update
	// immediately via discoveryWatcher.SetLocal, since discovery has no
	// separate engine of its own the way accessRules/orchestration do — this
	// Watcher IS the "engine". See CLAUDE.md's "Unify configuration into a
	// single config-map mechanism" notes.
	discoveryWatcher *configsync.Watcher[discover_mcp_servers.MCPDiscoveryConfig]

	// rateLimitingWatcher holds the live rateLimiting config: rate_limit_gate.go's
	// checkRateLimit reads rateLimitingWatcher.Current() on every tools/call,
	// same "Watcher IS the engine" reasoning as discoveryWatcher above — there's
	// no separate downstream engine on the proxy side, just a config snapshot
	// the gate consults directly (the actual limiting is enforced by the
	// separate rate-limiter server, not here).
	rateLimitingWatcher *configsync.Watcher[ratelimit.Config]

	// accessRulesWriter/orchestrationWriter/discoveryWriter/
	// eventDictionaryWriter/declarativeGuardrailsWriter are populated in
	// doRunOn, one per configsync-managed section, and used only by
	// admin_tools.go's registerSectionAdminTools handlers to persist an
	// update to that section's "<section>-override" ConfigMap and ping
	// every Watcher via NATS. declarativeGuardrailsWriter exists even
	// though the proxy never consumes that section itself (it's
	// backend-only, read by declarative-guardrail) — the proxy is still
	// the sole writer/admin surface for it. eventDictionaryWriter is the
	// same writer/admin surface for eventDictionary, but (unlike
	// declarativeGuardrails) the proxy also reads this section back
	// itself, via a Watcher built alongside eventsClient below.
	accessRulesWriter           *configsync.Writer[access_control.AccessConfig]
	orchestrationWriter         *configsync.Writer[triggered_tools_runtime.OrchestrationConfig]
	discoveryWriter             *configsync.Writer[discover_mcp_servers.MCPDiscoveryConfig]
	eventDictionaryWriter       *configsync.Writer[events.Dictionary]
	declarativeGuardrailsWriter *configsync.Writer[guardrailcfg.RawConfig]
	// eventAggregationWriter has no matching Watcher here either, for the
	// same reason declarativeGuardrailsWriter doesn't: the proxy is only
	// ever this section's admin/write surface — event-aggregator is the
	// sole consumer, via its own configsync.Watcher pointed at the
	// event-aggregation-override ConfigMap.
	eventAggregationWriter *configsync.Writer[events.AggregationConfig]
	// rateLimitingWriter is the admin/write surface for rateLimiting — go-proxy
	// both writes it (admin tools) and reads it back (rateLimitingWatcher
	// above), unlike eventAggregationWriter/declarativeGuardrailsWriter.
	rateLimitingWriter *configsync.Writer[ratelimit.Config]
	// logLevelWriter is the admin/write surface for logLevel — go-proxy both
	// writes it (admin tools) and reads it back via a Watcher constructed
	// alongside it in doRunOn (logging.ApplyConfig as onChange), same dual
	// writer+watcher shape as rateLimitingWriter/rateLimitingWatcher above.
	logLevelWriter *configsync.Writer[logging.LevelConfig]

	// nativeTools collects every tool registerAdminTools/registerUtilityTools
	// add directly to the gateway's *mcp.Server (admin_tools.go/
	// utility_tools.go) — populated once at registration time, alongside
	// each mcp.AddTool call, since the go-sdk's *mcp.Server exposes no
	// exported way to enumerate its own registered tools afterward
	// (Server.listTools is unexported). dynamic_proxy.Gateway.Tools() only
	// covers discovered *backend* tools (its own toolRegistry), so
	// proxy_utility.find_relevant_tools's candidate list (utility_tools.go)
	// unions this slice in too — otherwise "_admin."/"proxy_utility."
	// tools could never be offered as candidates, even to an admin caller
	// who can see (and call) them via an ordinary tools/list.
	nativeTools []*mcp.Tool
)

// HTTPMiddleware is an HTTP-layer gate in front of the MCP handler,
// distinct from and running before createMiddleware's per-MCP-method
// checks (middleware.go). It exists so a generic MCP client (one that
// doesn't already know which authorization server to use out-of-band, the
// way this repo's own cli does) gets a real HTTP 401 with a
// WWW-Authenticate header on its very first unauthenticated request — the
// signal the MCP Authorization spec (and the go-sdk's own client-side
// AuthorizationCodeHandler) relies on to start OAuth discovery at all.
// Without this, an unauthenticated request used to reach all the way into
// JSON-RPC dispatch and fail there instead, as an error inside an HTTP 200
// — invisible to a client that isn't already expecting to be logged in.
//
// Reuses authenticateRequest (middleware.go), the same two-path check
// (client Authorization header, or a backend's relayed identity assertion)
// createMiddleware itself performs — deliberately not re-derived here, so
// the relay path (see CLAUDE.md's "Internal identity assertions" section)
// keeps working through this gate for free rather than needing its own
// carve-out. createMiddleware still re-validates independently afterward;
// this is purely an additional, earlier signal, not a replacement.
func HTTPMiddleware(verifier *auth.Verifier, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, _, err := authenticateRequest(verifier, r.Header); err != nil {
			wwwAuthenticate := "Bearer"
			if authConfig.ResourceURL != "" {
				wwwAuthenticate += fmt.Sprintf(` resource_metadata=%q`, protectedResourceMetadataURL(r))
			}
			w.Header().Set("WWW-Authenticate", wwwAuthenticate)
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// protectedResourceMetadataURL returns the absolute URL of this proxy's own
// RFC 9728 Protected Resource Metadata document, derived from the incoming
// request rather than a separately-configured base URL — only meaningful
// when authConfig.ResourceURL is set (see HTTPMiddleware).
//
// Behind a TLS-terminating L7 reverse proxy (e.g. ingress-nginx in front of
// https://mcp.acme.com), the base URL comes from X-Forwarded-Proto/
// X-Forwarded-Host (see forwarded.BaseURL); without them — a direct
// connection, or an L4 load balancer like k3s's klipper — it's exactly the
// request-derived URL. A client spoofing these headers only changes the
// hint in its own 401 response, never anything another caller sees.
func protectedResourceMetadataURL(r *http.Request) string {
	return forwarded.BaseURL(r) + "/.well-known/oauth-protected-resource"
}

// protectedResourceMetadataPaths returns every path the RFC 9728 document
// is served at: the root well-known path (what HTTPMiddleware's 401 hint
// points to) plus, when resourceURL has a path component (e.g. "/mcp"), the
// path-inserted variant RFC 9728 §3.1 defines
// ("/.well-known/oauth-protected-resource/mcp"), which spec-compliant MCP
// clients try first.
func protectedResourceMetadataPaths(resourceURL string) []string {
	paths := []string{"/.well-known/oauth-protected-resource"}
	if u, err := url.Parse(resourceURL); err == nil {
		if p := strings.TrimRight(u.Path, "/"); p != "" {
			paths = append(paths, "/.well-known/oauth-protected-resource"+p)
		}
	}
	return paths
}

// Proxy Server: Forwards requests to the backend with headers.
// proxyCfg is parsed once by doRun at startup — the config file is not
// re-read afterward. It only changes on a redeploy/restart, so both the
// poll frequencies below and the discovery config handed to the
// ServerMonitor come from this single parse, not from disk on every tick.
func runProxyServer(ctx context.Context, proxyCfg *ProxyConfig) error {
	return runProxyServerOn(ctx, proxyCfg, ":8081")
}

// runProxyServerOn is runProxyServer parameterized by listen address, so
// platform_servers_test.go can run a second proxy instance in the same test
// binary as integration_test.go's TestProxyClient (which binds :8081
// forever, no teardown) without a port collision. Unlike the original
// inline body, a bind failure is returned rather than passed to log.Fatal —
// a second doRunOn call failing to bind must not kill the whole test
// binary via os.Exit.
func runProxyServerOn(ctx context.Context, proxyCfg *ProxyConfig, addr string) error {
	// Built once at startup: fails fast if a configured JWKS endpoint is
	// unreachable/misconfigured, rather than on the first incoming request.
	verifier, err := auth.NewVerifier(ctx, authConfig)
	if err != nil {
		return fmt.Errorf("failed to initialize JWT verifier: %w", err)
	}

	frequency := 1                      // default discovery-scan frequency, in seconds
	toolsPollFrequency := 5             // default tool-list poll frequency, in seconds
	promptsPollFrequency := 5           // default prompt-list poll frequency, in seconds
	resourcesPollFrequency := 5         // default resource-list poll frequency, in seconds
	resourceTemplatesPollFrequency := 5 // default resource-template-list poll frequency, in seconds
	config := proxyCfg.Discovery
	if config.Mode != discover_mcp_servers.ModeStatic && config.FrequencySeconds > 0 {
		frequency = config.FrequencySeconds
	}
	if config.ToolsPollFrequencySeconds > 0 {
		toolsPollFrequency = config.ToolsPollFrequencySeconds
	}
	if config.PromptsPollFrequencySeconds > 0 {
		promptsPollFrequency = config.PromptsPollFrequencySeconds
	}
	if config.ResourcesPollFrequencySeconds > 0 {
		resourcesPollFrequency = config.ResourcesPollFrequencySeconds
	}
	if config.ResourceTemplatesPollFrequencySeconds > 0 {
		resourceTemplatesPollFrequency = config.ResourceTemplatesPollFrequencySeconds
	}

	gateway := dynamic_proxy.NewGateway(func(callCtx context.Context, sessId string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error) {
		return sessions.sessionFor(callCtx, sessionId(sessId), toServerDetails(server))
	}, triggered_tools_runtime.IsTriggeredTool, toolsPollFrequency, promptsPollFrequency, resourcesPollFrequency, resourceTemplatesPollFrequency)

	backendProgressHandler = gateway.HandleBackendProgress

	registerAdminTools(gateway.Server())
	registerUtilityTools(gateway)

	monitor := discover_mcp_servers.NewServerMonitor(
		frequency,
		func() (discover_mcp_servers.ServerMap, error) {
			// Re-discovers the live server set (e.g. "k8s"/"scan" mode
			// process/Service churn) on every poll tick, against
			// discoveryWatcher's *current* config — not proxyCfg.Discovery
			// captured once — so an _admin.update_discovery call takes
			// effect on the very next tick, no restart needed.
			return discover_mcp_servers.DiscoverMCPsFromConfig(discoveryWatcher.Current(), currentMustPin())
		},
		func(added discover_mcp_servers.ServerMap) {
			// Every discovered server goes to both: the gateway registers
			// whichever of its tools aren't hidden triggered tools (see
			// dynamic_proxy.ToolVisibilityFilter/triggered_tools_runtime.IsTriggeredTool
			// above), and the runtime records connection info for any
			// server that might host a triggered tool an orchestration step
			// references — a single server can freely host both kinds.
			gateway.OnAdded(ctx, added)
			triggeredRuntime.OnAdded(added)
		},
		func(removed discover_mcp_servers.ServerMap) {
			gateway.OnRemoved(removed)
			for name := range removed {
				sessions.closeServerSessions(name)
			}
			triggeredRuntime.OnRemoved(removed)
		},
	)
	// Tracked so this function doesn't return until the background loop has
	// actually stopped reading/writing shared state, not merely been asked
	// to via ctx cancellation — see CLAUDE.md's "Test lifecycle: graceful
	// teardown".
	var backgroundWG sync.WaitGroup
	backgroundWG.Add(1)
	go func() { defer backgroundWG.Done(); monitor.Start(ctx) }()

	gateway.Server().AddReceivingMiddleware(createMiddleware(verifier, gateway))

	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return gateway.Server()
	}, &mcp.StreamableHTTPOptions{Stateless: true})

	protectedHandler := HTTPMiddleware(verifier, mcpHandler)

	// /healthz is a sibling mux route, not wrapped by protectedHandler — a
	// k8s readiness probe has no JWT and shouldn't need one; every real
	// auth happens inside the MCP layer, which this bypasses entirely. "/"
	// keeps the MCP handler reachable with no path suffix, matching every
	// discovered backend server URL's own shape (see
	// discover_mcp_servers/k8s_discovery_test.go) and preserving exactly
	// today's routing for every path other than /healthz.
	mux := http.NewServeMux()
	mux.Handle("/", protectedHandler)
	discoveryReady := newDiscoveryReadiness(monitor.FirstCheckDone, gateway.InitialPollsPending, discoveryReadyTimeout)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if natsConn.IsConnected() && eventsClient.IsConnected() && discoveryReady() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	// /.well-known/oauth-protected-resource (RFC 9728) is only served when
	// authConfig.ResourceURL is configured — see auth.Config.ResourceURL's
	// doc comment. Deliberately unauthenticated, like /healthz: a generic
	// MCP client fetches this before it has any credential at all, in
	// response to HTTPMiddleware's 401. authConfig.Issuer doubles as the
	// document's authorization_servers entry — for a real IdP this is
	// already its real, discoverable base URL by construction; for
	// demo-authorization-server it must be pointed at a real fetchable URL
	// (its own -issuer flag) rather than the placeholder string
	// proxy-server.cfg uses today for this to be meaningful.
	if authConfig.ResourceURL != "" {
		prmHandler := sdkauth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:               authConfig.ResourceURL,
			AuthorizationServers:   []string{authConfig.Issuer},
			BearerMethodsSupported: []string{"header"},
		})
		for _, p := range protectedResourceMetadataPaths(authConfig.ResourceURL) {
			mux.Handle(p, prmHandler)
		}
	}

	// /.well-known/jwks-internal.json serves internalSigner's public key
	// as a standard JWKS document, only when internalAuth is configured
	// (internalSigner != nil) — this is what lets a backend verify the
	// proxy's internal identity assertions via a URL (auth.Config.JWKSURL)
	// instead of a locally mounted public key file, and lets that work
	// across multiple go-proxy replicas/hosts (see CLAUDE.md's "Internal
	// identity assertions" section and bootstrapInternalSigningKey).
	// Deliberately unauthenticated, like /healthz and
	// oauth-protected-resource above: a backend needs to fetch this before
	// it can verify anything.
	if internalSigner != nil {
		mux.HandleFunc("/.well-known/jwks-internal.json", internalSigner.JWKSHandler())
	}

	// An explicit *http.Server (rather than the package-level
	// http.ListenAndServe) so ctx cancellation can gracefully Shutdown it.
	// In production ctx is canceled by SIGTERM/SIGINT (see Main); in tests,
	// by the test's own cleanup.
	//
	// ReadHeaderTimeout and IdleTimeout stop a client from holding
	// connections open forever by sending headers slowly or never sending
	// another request (before any credential is checked). There's no
	// ReadTimeout or WriteTimeout: a tool call's response, streamed over
	// SSE, may legitimately take a long time.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	logging.Debugf("Starting Gateway Server on %s", addr)
	serveErr := srv.ListenAndServe()

	backgroundWG.Wait()

	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// Main is the go-proxy command's entry point (cmd/go-proxy/main.go calls
// it): parses -config and runs the gateway until it exits.
//
// SIGTERM (what Kubernetes sends to stop a pod) or SIGINT (Ctrl-C) cancels
// the context doRun runs under, which is the same graceful shutdown the
// tests use: the HTTP server stops accepting connections and finishes
// in-flight requests (see runProxyServerOn), background loops stop, and the
// process exits 0. A second signal kills it immediately.
func Main() {
	var configPath string
	flag.StringVar(&configPath, "config", "proxy-server.cfg", "Path to the combined proxy configuration JSON file (discovery, access rules, and auth settings)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	signaled := make(chan struct{})
	go func() {
		<-ctx.Done()
		// Restores the default signal behavior, so a second signal
		// terminates at once instead of waiting out the shutdown.
		stop()
		logging.Info("received shutdown signal, shutting down")
		close(signaled)
	}()

	if err := doRun(ctx, configPath); err != nil {
		logging.Fatal(err)
	}
	if ctx.Err() != nil {
		<-signaled
		logging.Info("shutdown complete")
	}
}

func doRun(ctx context.Context, configPath string) error {
	return doRunOn(ctx, configPath, ":8081")
}

// overrideMountPath returns the local file a "<section>-override" ConfigMap
// is mounted at in k8s: a sibling directory of configPath itself (e.g.
// configPath "/etc/proxy-server/proxy-server.cfg" ⇒ "/etc/proxy-server/
// access-rules-override/config.json" — see k8s/go-proxy-deployment.yaml's
// volumeMounts, which mount each override ConfigMap at exactly that
// sibling directory). Deriving it from configPath rather than a hardcoded
// absolute path means a hermetic test pointing configPath at a t.TempDir()
// automatically gets a writable, sandboxed override path too, with no
// separate flag/config field needed — the same code path production and
// tests both exercise. Outside k8s (or before any admin update has ever
// seeded a section) the file simply doesn't exist yet, so every
// configsync.Watcher built against it falls back to its caller-supplied
// initial value instead — see configsync.NewWatcher's doc comment.
func overrideMountPath(configPath, section string) string {
	return filepath.Join(filepath.Dir(configPath), section+"-override", "config.json")
}

// doRunOn is doRun parameterized by listen address — see runProxyServerOn.
func doRunOn(ctx context.Context, configPath string, addr string) error {
	logging.Debugf("Loading proxy configuration from: %s", configPath)
	proxyCfg, err := loadProxyConfig(configPath)
	if err != nil {
		panic(err)
	}
	logging.ApplyConfig(proxyCfg.LogLevel)

	// natsUrl is mandatory (see ProxyConfig.NatsURL): both configsync's
	// change-notification pings and go-proxy's own event publishing depend
	// on a real NATS connection, and neither RetryOnFailedConnect(true)
	// below nor /healthz's readiness gating (runProxyServerOn) are a
	// substitute for catching a missing URL here, at startup, rather than
	// leaving the proxy running with a permanently no-op NATS integration.
	if proxyCfg.NatsURL == "" {
		return fmt.Errorf("natsUrl is required")
	}
	natsURLConfig = proxyCfg.NatsURL

	// natsConn is configsync's own connection (see configsync.Connect's doc
	// comment on why it's independent of eventsClient's connection below).
	// Deferred-closed here so it (and every subscription on it) is torn
	// down before doRunOn returns, matching CLAUDE.md's "Test lifecycle:
	// graceful teardown" expectations for every other resource this
	// function builds.
	natsConn, err = configsync.Connect(proxyCfg.NatsURL)
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}
	defer natsConn.Close()

	// watchersToClose collects every configsync.Watcher this function
	// constructs below, so they can all be explicitly unsubscribed before
	// natsConn itself closes (registered after natsConn's own defer, so it
	// runs first — LIFO). Without this, a Watcher's NATS subscription
	// callback goroutine can still be mid-reload when doRunOn returns,
	// racing against anything that mutates configsync's package-level
	// ReloadTimeout/ReloadInitialBackoff/ReloadMaxBackoff vars (as hermetic
	// tests do) — see CLAUDE.md's "Test lifecycle: graceful teardown".
	var watchersToClose []io.Closer
	defer func() {
		for _, w := range watchersToClose {
			w.Close()
		}
	}()

	// eventsClient is go-proxy's own events.Client, independent of natsConn
	// above (see natsConn's doc comment).
	eventsClient, err = events.NewClient(proxyCfg.NatsURL)
	if err != nil {
		return fmt.Errorf("failed to connect events client to NATS: %w", err)
	}
	defer eventsClient.Close()

	// The administrator role is fixed here for the life of the process:
	// accessRulesWriter/Watcher below only ever swap the engine's rules,
	// never this (see access_control.RoleMapping on why).
	roleMappingConfig = proxyCfg.RoleMapping
	accessPolicyEngine = access_control.NewAccessPolicyEngine(proxyCfg.AccessRules, proxyCfg.RoleMapping.Administrator)
	logging.Infof("administrator role (authorizes _admin.* tools): %q", proxyCfg.RoleMapping.Administrator)
	accessRulesWriter = configsync.NewWriter[access_control.AccessConfig]("access-rules", natsConn, proxyCfg.AccessRules)
	if w, err := configsync.NewWatcher("access-rules", overrideMountPath(configPath, "access-rules"), natsConn, proxyCfg.AccessRules, access_control.ParseConfig, accessPolicyEngine.Update); err != nil {
		return fmt.Errorf("failed to initialize access-rules watcher: %w", err)
	} else {
		watchersToClose = append(watchersToClose, w)
	}

	triggeredRuntime = triggered_tools_runtime.NewRuntime(proxyCfg.Orchestration)
	triggeredRuntime.SetEventPublisher(publishToolRequestResult)
	triggeredRuntime.SetRequestEventPublisher(publishToolRequest)
	triggeredRuntime.RegisterNativeCallAuthorization("request_constraints_validation", requestConstraintsValidation)
	triggeredRuntime.RegisterNativeCallAuthorization("business_logic_constraints_validation", businessLogicConstraintsValidation)
	triggeredRuntime.RegisterNativeCallAuthorization("request_ai_constraints_validation", requestAIConstraintsValidation)
	triggeredRuntime.RegisterNativeResultAuthorization("result_constraints_validation", resultConstraintsValidation)
	triggeredRuntime.RegisterNativeResultAuthorization("result_ai_constraints_validation", resultAIConstraintsValidation)
	orchestrationWriter = configsync.NewWriter[triggered_tools_runtime.OrchestrationConfig]("orchestration", natsConn, proxyCfg.Orchestration)
	if w, err := configsync.NewWatcher("orchestration", overrideMountPath(configPath, "orchestration"), natsConn, proxyCfg.Orchestration, triggered_tools_runtime.ParseConfig, triggeredRuntime.Update); err != nil {
		return fmt.Errorf("failed to initialize orchestration watcher: %w", err)
	} else {
		watchersToClose = append(watchersToClose, w)
	}

	discoveryWriter = configsync.NewWriter[discover_mcp_servers.MCPDiscoveryConfig]("discovery", natsConn, proxyCfg.Discovery)
	discoveryWatcher, err = configsync.NewWatcher("discovery", overrideMountPath(configPath, "discovery"), natsConn, proxyCfg.Discovery, discover_mcp_servers.ParseConfig, nil)
	if err != nil {
		return fmt.Errorf("failed to initialize discovery watcher: %w", err)
	}
	watchersToClose = append(watchersToClose, discoveryWatcher)

	// eventsClient.SetDictionary is what lets go-proxy's own
	// publishToolRequestResult (below) call eventsClient.Publish at all —
	// Publish fails closed until a dictionary has been set (see
	// events.Client.getDictionary).
	eventDictionaryWriter = configsync.NewWriter[events.Dictionary]("event-dictionary", natsConn, proxyCfg.EventDictionary)
	if w, err := configsync.NewWatcher("event-dictionary", overrideMountPath(configPath, "event-dictionary"), natsConn, proxyCfg.EventDictionary, events.ParseDictionary, eventsClient.SetDictionary); err != nil {
		return fmt.Errorf("failed to initialize event-dictionary watcher: %w", err)
	} else {
		watchersToClose = append(watchersToClose, w)
	}

	// declarativeGuardrailsWriter has no matching Watcher here — the proxy
	// never consumes this section itself (see this var's doc comment),
	// it's purely the write/admin side for declarative-guardrail.
	declarativeGuardrailsWriter = configsync.NewWriter[guardrailcfg.RawConfig]("declarative-guardrails", natsConn, proxyCfg.DeclarativeGuardrails)

	// eventAggregationWriter likewise has no matching Watcher — the proxy
	// never consumes this section itself, it's purely the write/admin
	// side for event-aggregator. proxyCfg.EventAggregation may be nil
	// (section absent from the config file); NewWriter handles a nil
	// initial fine (Export then just reports version 0 until the first
	// admin update), same as orchestrationWriter above when Orchestration
	// is absent.
	eventAggregationWriter = configsync.NewWriter[events.AggregationConfig]("event-aggregation", natsConn, proxyCfg.EventAggregation)

	// rateLimitingWriter/rateLimitingWatcher: unlike eventAggregationWriter
	// above, the proxy IS a genuine consumer here (rate_limit_gate.go reads
	// rateLimitingWatcher.Current() on every tools/call) — same dual
	// writer+watcher shape as orchestration/discovery/event-dictionary.
	rateLimitingWriter = configsync.NewWriter[ratelimit.Config]("rate-limiting", natsConn, proxyCfg.RateLimiting)
	rateLimitingWatcher, err = configsync.NewWatcher("rate-limiting", overrideMountPath(configPath, "rate-limiting"), natsConn, proxyCfg.RateLimiting, ratelimit.ParseConfig, nil)
	if err != nil {
		return fmt.Errorf("failed to initialize rate-limiting watcher: %w", err)
	}
	watchersToClose = append(watchersToClose, rateLimitingWatcher)

	// logLevelWriter/its Watcher: unlike rateLimitingWatcher above (whose
	// onChange is nil), logging.ApplyConfig IS the downstream engine here —
	// there's no separate object to update, just the package-level
	// slog.LevelVar. This is what makes _admin.update_log_level take effect
	// with no restart, on every replica, once each has processed the
	// resulting NATS ping (see CLAUDE.md's "Unify configuration into a
	// single config-map mechanism").
	logLevelWriter = configsync.NewWriter[logging.LevelConfig]("log-level", natsConn, proxyCfg.LogLevel)
	if w, err := configsync.NewWatcher("log-level", overrideMountPath(configPath, "log-level"), natsConn, proxyCfg.LogLevel, logging.ParseConfig, logging.ApplyConfig); err != nil {
		return fmt.Errorf("failed to initialize log-level watcher: %w", err)
	} else {
		watchersToClose = append(watchersToClose, w)
	}

	// The cfg seeds each section's override ConfigMap the first time any
	// replica starts against it; from then on the override is the live
	// config, changed through the _admin.update_<section> tools. Reconcile
	// (see its doc comment) checks the deployed cfg against it: a cfg left
	// behind by admin updates is fine (a warning), but a cfg that was
	// changed and differs from the live config is refused, since applying
	// it is never what happens - an edit or revocation made in the cfg
	// would otherwise be silently ignored. A no-op outside k8s.
	var drifted []string
	for _, reconcile := range []func(context.Context) error{
		func(ctx context.Context) error { return accessRulesWriter.Reconcile(ctx, access_control.ParseConfig) },
		func(ctx context.Context) error {
			return orchestrationWriter.Reconcile(ctx, triggered_tools_runtime.ParseConfig)
		},
		func(ctx context.Context) error {
			return discoveryWriter.Reconcile(ctx, discover_mcp_servers.ParseConfig)
		},
		func(ctx context.Context) error { return eventDictionaryWriter.Reconcile(ctx, events.ParseDictionary) },
		func(ctx context.Context) error {
			return declarativeGuardrailsWriter.Reconcile(ctx, guardrailcfg.ParseRawConfig)
		},
		func(ctx context.Context) error {
			return eventAggregationWriter.Reconcile(ctx, events.ParseAggregationConfig)
		},
		func(ctx context.Context) error { return rateLimitingWriter.Reconcile(ctx, ratelimit.ParseConfig) },
		func(ctx context.Context) error { return logLevelWriter.Reconcile(ctx, logging.ParseConfig) },
	} {
		var drift *configsync.DriftError
		switch err := reconcile(ctx); {
		case errors.As(err, &drift):
			drifted = append(drifted, drift.Section)
		case err != nil:
			return fmt.Errorf("failed to reconcile override config map: %w", err)
		}
	}
	if len(drifted) > 0 {
		return driftError(drifted)
	}

	// In k8s mode every server go-proxy trusts by name for a security
	// decision must be pinned (see pinning.go). Discovery would refuse an
	// unpinned one anyway, failing its guardrail or relay closed; refusing
	// to start says why, instead of leaving it to a log line.
	mustPin := trustedServerNames(triggeredRuntime.Snapshot(), relayOf(proxyCfg.InternalAuth))
	if err := discover_mcp_servers.UnpinnedNames(discoveryWatcher.Current(), mustPin); err != nil {
		return fmt.Errorf("discovery: %w", err)
	}
	servers, err := discover_mcp_servers.DiscoverMCPsFromConfig(discoveryWatcher.Current(), mustPin)
	if err != nil {
		panic(err)
	}
	mcpServers = getDetailMap(servers)

	if proxyCfg.Auth != nil {
		authConfig = *proxyCfg.Auth
		if raw, err := json.Marshal(authConfig); err == nil {
			logging.Infof("startup configuration: auth: %s", raw)
		}
		// Without these, any token the identity provider's keys signed is
		// accepted - on a shared tenant (Okta, Entra ID, Auth0) that includes
		// tokens issued for every other application there.
		if authConfig.Audience == "" {
			logging.Warnf("auth.audience is empty: tokens issued for any application by the same identity provider are accepted. Set it to the audience your IdP puts in tokens for go-proxy.")
		}
		if authConfig.Issuer == "" {
			logging.Warnf("auth.issuer is empty: tokens from any issuer signing with these keys are accepted. Set it to your IdP's issuer.")
		}
	}

	// Reset first: doRunOn runs more than once per process in tests, and a
	// config without internalAuth must not inherit a previous run's signer.
	internalSigner, relayTokenVerifier, internalAuthConfig = nil, nil, nil
	if proxyCfg.InternalAuth != nil {
		internalAuthConfig = proxyCfg.InternalAuth
		if raw, err := json.Marshal(internalAuthConfig); err == nil {
			logging.Infof("startup configuration: internalAuth: %s", raw)
		}
		internalSigningKey, err := bootstrapInternalSigningKey(ctx)
		if err != nil {
			return fmt.Errorf("failed to bootstrap internal signing key: %w", err)
		}
		internalSigner = auth.SignerFromKey(internalSigningKey, *proxyCfg.InternalAuth)

		// effectiveIssuer mirrors the defaulting auth.SignerFromKey already
		// applies internally: if internalAuth.issuer is unset,
		// internalSigner mints "iss" as auth.DefaultInternalIssuer, so the
		// verifier below must expect that value explicitly — an empty
		// issuer would make auth.Verifier skip the check entirely.
		effectiveIssuer := proxyCfg.InternalAuth.Issuer
		if effectiveIssuer == "" {
			effectiveIssuer = auth.DefaultInternalIssuer
		}

		// Built directly from the in-memory key, not a JWKS fetch: the
		// proxy already holds its own key, so there's no reason to round-
		// trip through its own HTTP endpoint (which also isn't listening
		// yet at this point in startup) to verify its own tokens. No
		// audience here: authenticateRequest accepts exactly
		// auth.RelayAudience or auth.LoopbackAudience and checks that
		// itself.
		relayTokenVerifier = auth.NewVerifierFromKey(internalSigner.PublicKey(), effectiveIssuer, "")
	}

	selfURL = proxyCfg.SelfURL
	if selfURL != "" {
		logging.Infof("startup configuration: selfUrl: %s", selfURL)
	}
	logging.Infof("startup configuration: natsUrl: %s", logging.URL(proxyCfg.NatsURL))

	errChan := make(chan error, 1)
	go func() {
		errChan <- runProxyServerOn(ctx, proxyCfg, addr)
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		logging.Debug("proxy server context canceled, shutting down")
		// Wait for runProxyServerOn to actually finish tearing down (HTTP
		// listener closed, monitor goroutine returned) before this
		// function returns.
		<-errChan
	}
	return nil
}

// publishToolRequestResult is the triggered_tools_runtime.EventPublisher
// installed on triggeredRuntime in doRunOn — it's what backs an
// orchestration's toolCallAudit publish switch
// (triggered_tools_runtime.ToolCallAuditSentinel), turning the runtime's
// decoupled ToolRequestResultEvent into a real "tool-request-result"
// events.Event and publishing it. See CLAUDE.md's "NATS event system" and
// "Triggered tools / orchestration" sections.
//
// A result-blocked event carries no "result": a result a guardrail kept from
// the client mustn't reach whoever reads the audit trail instead. The
// request and result are capped by auditPayload.
func publishToolRequestResult(ctx context.Context, e triggered_tools_runtime.ToolRequestResultEvent) error {
	metadata := map[string]any{
		"user":       e.Caller,
		"roles":      e.Roles,
		"tool":       e.Tool,
		"request":    auditPayload(e.CallRequest),
		"toolCallId": e.ToolCallID,
	}
	if e.CallResult != nil && e.Subtype != triggered_tools_runtime.ToolRequestResultRequestBlocked {
		metadata["result"] = auditPayload(e.CallResult)
	}
	if e.Reason != "" {
		metadata["reason"] = e.Reason
	}

	var title, text string
	switch e.Subtype {
	case triggered_tools_runtime.ToolRequestResultRequestBlocked:
		title, text = "Tool call blocked", e.Reason
	case triggered_tools_runtime.ToolRequestResultCallFailed:
		title, text = "Tool call failed", e.Reason
	default: // triggered_tools_runtime.ToolRequestResultCallSucceeded
		title, text = "Tool call succeeded", fmt.Sprintf("tool %q call succeeded", e.Tool)
	}

	return eventsClient.Publish(&events.Event{
		Type:     "tool-request-result",
		Subtype:  e.Subtype,
		Time:     time.Now().UTC(),
		Title:    title,
		Text:     text,
		Metadata: metadata,
	})
}

// maxAuditPayloadBytes caps the request or result (as JSON) an audit event
// carries. A larger one is replaced by a summary (see auditPayload), which
// keeps every audit event well under NATS's 1 MB message limit - an event
// over it isn't delivered at all - and keeps bulk data out of the audit
// trail.
const maxAuditPayloadBytes = 16 << 10

// auditPayload returns v for an audit event's request/result, or, if it's
// larger than maxAuditPayloadBytes as JSON (or can't be encoded), a summary
// recording that it was there: {"truncated": true, "bytes": N,
// "sha256": "..."} - its size and fingerprint, without its content.
func auditPayload(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"truncated": true, "error": "not encodable as JSON"}
	}
	if len(raw) <= maxAuditPayloadBytes {
		return v
	}
	sum := sha256.Sum256(raw)
	return map[string]any{"truncated": true, "bytes": len(raw), "sha256": hex.EncodeToString(sum[:])}
}

// publishToolRequest is the triggered_tools_runtime.RequestEventPublisher
// installed on triggeredRuntime in doRunOn — it's what backs an
// orchestration's independent toolCallAudit publish switch
// (triggered_tools_runtime.ToolRequestAuditSentinel), turning the
// runtime's decoupled ToolRequestEvent into a real "tool-request"
// events.Event and publishing it. Unlike publishToolRequestResult, there's
// no "result" metadata key: this fires right after call-authorization's
// own decision, before any backend call is attempted, so there's never a
// result to report. See CLAUDE.md's "NATS event system" and "Triggered
// tools / orchestration" sections.
func publishToolRequest(ctx context.Context, e triggered_tools_runtime.ToolRequestEvent) error {
	metadata := map[string]any{
		"user":       e.Caller,
		"roles":      e.Roles,
		"tool":       e.Tool,
		"request":    auditPayload(e.CallRequest),
		"toolCallId": e.ToolCallID,
	}
	if e.Reason != "" {
		metadata["reason"] = e.Reason
	}

	var title, text string
	switch e.Subtype {
	case triggered_tools_runtime.ToolRequestBlocked:
		title, text = "Tool request blocked", e.Reason
	default: // triggered_tools_runtime.ToolRequestOK
		title, text = "Tool request accepted", fmt.Sprintf("tool %q request accepted", e.Tool)
	}

	return eventsClient.Publish(&events.Event{
		Type:     "tool-request",
		Subtype:  e.Subtype,
		Time:     time.Now().UTC(),
		Title:    title,
		Text:     text,
		Metadata: metadata,
	})
}
