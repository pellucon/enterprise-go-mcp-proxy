// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// createMiddleware puts the caller's verified identity on ctx twice: as
// an auth.Identity (callerIdentityFromContext, for the proxy_utility
// loopback) and as a relayheaders.MintFunc (relayheaders.WithMinter) that
// mints, per outbound request, tokens for the backend server that request
// goes to. Both go-proxy's own proxy-to-backend sessions
// (session_proxies.go's backendHTTPClientFor) and triggered-tools-runtime's
// platform-server sessions (runtime.go's platformHTTPClientFor) use that
// minter through relayheaders.ServerTransport, so a triggered tool gets
// the identical relay capability an ordinary backend call has — see
// docs/architecture/auth.md's "Internal identity assertions" section.

// callerClaimsContextKey is the context key createMiddleware uses to stash
// the caller's already-verified claims (see authenticateRequest) for a
// native tool handler running downstream of next(...) — a "utility" tool
// (main.go's registerUtilityTools) — to read back directly, without
// re-deriving them. See callerClaimsFromContext.
type callerClaimsContextKey struct{}

// callerClaimsFromContext returns the claims createMiddleware stashed via
// callerClaimsContextKey, or nil if ctx carries none (e.g. called outside
// createMiddleware's dispatch, which shouldn't happen for any native tool
// handler registered on the gateway's own *mcp.Server).
func callerClaimsFromContext(ctx context.Context) jwt.MapClaims {
	claims, _ := ctx.Value(callerClaimsContextKey{}).(jwt.MapClaims)
	return claims
}

// detachedContext wraps ctx, preserving its cancellation signal and deadline
// but not its value chain. See detachContextValues.
type detachedContext struct {
	context.Context
}

func (detachedContext) Value(key any) any { return nil }

// detachContextValues strips every value already attached to ctx (e.g. by
// the SDK's own incoming-request handling) while still tying the result to
// ctx's cancellation/deadline, so a client disconnect still cancels
// everything downstream. This exists specifically to stop the SDK's own
// per-request protocol-version tracking (an unexported context key set on
// every incoming request — see [github.com/modelcontextprotocol/go-sdk]'s
// streamable transport) from silently riding along onto go-proxy's own
// outgoing calls to backend MCP servers. Go's context.Context has no way to
// unset one specific ancestor value (only to shadow it by re-adding the same
// key, which isn't possible for another package's unexported key type), so
// a full detach-and-selectively-reattach is the only reliable option.
// Without this, a client that negotiates the newest MCP protocol version
// with go-proxy would cause go-proxy to request that same version against
// every backend it forwards calls to — even a backend that, on its own,
// negotiated (and only supports) an older version — which a non-stateless
// backend then rejects outright. See CLAUDE.md's "Backend protocol version
// isolation" section.
// methodSubscriptionsListen is the MCP method (protocol 2026-07-28+) a
// client uses to open a stream of list-changed notifications.
const methodSubscriptionsListen = "subscriptions/listen"

func detachContextValues(ctx context.Context) context.Context {
	return detachedContext{ctx}
}

// authKind is how a request authenticated.
type authKind int

const (
	// authClient: an ordinary client's Authorization header.
	authClient authKind = iota
	// authRelay: a backend's relay token (auth.RelayAudience).
	authRelay
	// authLoopback: the proxy's own proxy_utility loopback token
	// (auth.LoopbackAudience).
	authLoopback
)

// requestAuth is what authenticateRequest learned about a request beyond
// its claims.
type requestAuth struct {
	kind authKind
	// expiry is when the credential the request arrived with expires; no
	// token minted while handling it may outlive that.
	expiry time.Time
	// hop is the incoming hop number: 0 for a client, the token's hop
	// claim for a relay/loopback token.
	hop int
	// toolCallID is the relay/loopback token's tool-call ID, if any.
	toolCallID string
	// relayFrom is the backend whose relay this request is part of: set
	// for every relay token, and for a loopback token minted while
	// handling a relayed call. Its relay restrictions apply to the request.
	relayFrom string
}

// authenticateRequest implements the two authentication paths shared by
// createMiddleware (per-MCP-method) and the HTTP-layer gate in main.go's
// HTTPMiddleware (per-HTTP-request, ahead of MCP dispatch entirely): an
// ordinary client presenting its own Authorization header, or a call back
// into the proxy presenting a relay token (a backend allowed to relay) or a
// loopback token (the proxy's own proxy_utility tools) on
// relayheaders.RelayTokenHeader. Presence of Authorization always selects
// the client path, even if it turns out invalid — never silently falls
// back to the relay path, which would otherwise be a downgrade vector
// (present a broken client token alongside a forged relay header, hoping
// for a fallback).
//
// The identity assertion every backend receives (relayheaders.
// AssertionHeader) is never a credential here: its audience isn't one this
// accepts, so a backend can't turn the identity it was told about into
// the authority to act as that user.
//
// On the client path, if authConfig.RoleClaim names a claim other than
// "role", its value is copied into claims["role"] so every downstream
// consumer of RolesFromClaims (access rules, orchestration, audit events)
// keeps reading the one normalized key regardless of what a real IdP calls
// its roles/groups claim. The relay path never needs this: relay and
// loopback tokens are always minted by this proxy's own auth.Signer with a
// literal "role" claim.
func authenticateRequest(verifier *auth.Verifier, h http.Header) (jwt.MapClaims, requestAuth, error) {
	if authHeader := h.Get("Authorization"); authHeader != "" {
		parsedToken, err := verifier.TokenFromHeader(authHeader)
		if err != nil {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: %w", err)
		}
		claims, ok := parsedToken.Claims.(jwt.MapClaims)
		if !ok {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: invalid token claims")
		}
		if rc := authConfig.RoleClaim; rc != "" && rc != "role" {
			claims["role"] = claims[rc]
		}
		ra := requestAuth{kind: authClient}
		if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
			ra.expiry = exp.Time
		}
		return claims, ra, nil
	}

	if token := h.Get(relayheaders.RelayTokenHeader); token != "" {
		if relayTokenVerifier == nil {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: relay tokens aren't accepted (internalAuth isn't configured)")
		}
		parsed, err := relayTokenVerifier.TokenFromHeader(token)
		if err != nil {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: invalid relay token: %w", err)
		}
		claims, ok := parsed.Claims.(jwt.MapClaims)
		if !ok {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: invalid relay token claims")
		}
		return relayAuthFromClaims(claims)
	}

	if h.Get(relayheaders.AssertionHeader) != "" {
		return nil, requestAuth{}, fmt.Errorf("unauthorized: no credential presented (an identity assertion is not a credential; a backend relays with the relay token it was given)")
	}
	return nil, requestAuth{}, fmt.Errorf("unauthorized: no credential presented")
}

// relayAuthFromClaims checks an already signature-verified relay or
// loopback token's own claims: exactly one accepted audience, a hop count
// within internalAuth.maxRelayHops, and, for a relay token, a relaying
// server the config allows to relay.
func relayAuthFromClaims(claims jwt.MapClaims) (jwt.MapClaims, requestAuth, error) {
	aud, err := claims.GetAudience()
	if err != nil || len(aud) != 1 {
		return nil, requestAuth{}, fmt.Errorf("unauthorized: relay token has an invalid audience")
	}
	var ra requestAuth
	switch aud[0] {
	case auth.RelayAudience:
		ra.kind = authRelay
	case auth.LoopbackAudience:
		ra.kind = authLoopback
	default:
		return nil, requestAuth{}, fmt.Errorf("unauthorized: token audience %q isn't accepted as a credential", aud[0])
	}

	hop, ok := claims[auth.ClaimHop].(float64)
	if !ok || hop < 1 || hop != float64(int(hop)) {
		return nil, requestAuth{}, fmt.Errorf("unauthorized: relay token has an invalid hop count")
	}
	ra.hop = int(hop)
	if max := maxRelayHops(); ra.hop > max {
		return nil, requestAuth{}, fmt.Errorf("unauthorized: too many relay hops (%d, at most %d)", ra.hop, max)
	}

	ra.toolCallID, _ = claims[auth.ClaimToolCallID].(string)
	ra.relayFrom, _ = claims[auth.ClaimRelayFrom].(string)
	if ra.kind == authRelay {
		if ra.relayFrom == "" {
			return nil, requestAuth{}, fmt.Errorf("unauthorized: relay token names no relaying server")
		}
	}
	if ra.relayFrom != "" && len(relayTargets(ra.relayFrom)) == 0 {
		return nil, requestAuth{}, fmt.Errorf("unauthorized: server %q isn't allowed to relay", ra.relayFrom)
	}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		ra.expiry = exp.Time
	}
	return claims, ra, nil
}

// maxRelayHops is internalAuth.maxRelayHops, defaulted.
func maxRelayHops() int {
	if internalAuthConfig != nil && internalAuthConfig.MaxRelayHops > 0 {
		return internalAuthConfig.MaxRelayHops
	}
	return auth.DefaultMaxRelayHops
}

// relayTargets is the list of targets internalAuth.relay allows server to
// relay to (empty if it may not relay at all).
func relayTargets(server string) []string {
	if internalAuthConfig == nil {
		return nil
	}
	return internalAuthConfig.Relay[server]
}

// checkRelayTarget enforces a relayed request's restrictions: it may only
// reach a tool, prompt or resource its relaying server's internalAuth.relay
// entry lists, and never an _admin.* tool. These apply on top of (never
// instead of) the caller's own role rules. List requests and the protocol's
// own methods are allowed - a relaying backend may need to see what it can
// call - and still get role-filtered results. A request that isn't part of
// a relay (ra.relayFrom empty) isn't restricted here.
func checkRelayTarget(ra requestAuth, req mcp.Request) error {
	if ra.relayFrom == "" {
		return nil
	}
	var kind, target string
	switch r := req.(type) {
	case *mcp.CallToolRequest:
		kind, target = "tool", r.Params.Name
		if strings.HasPrefix(target, dynamic_proxy.ReservedServerName+".") {
			return fmt.Errorf("relay from %q may not call %s: %s.* tools are never reachable through a relay", ra.relayFrom, target, dynamic_proxy.ReservedServerName)
		}
	case *mcp.GetPromptRequest:
		kind, target = "prompt", r.Params.Name
	case *mcp.ReadResourceRequest:
		kind, target = "resource", r.Params.URI
	default:
		return nil
	}
	for _, pattern := range relayTargets(ra.relayFrom) {
		if access_control.GlobMatch(pattern, target) {
			return nil
		}
	}
	return fmt.Errorf("relay from %q may not reach %s %s (not in its internalAuth.relay targets)", ra.relayFrom, kind, target)
}

// callerIdentityContextKey is the context key for the caller's
// auth.Identity - see callerIdentityFromContext.
type callerIdentityContextKey struct{}

// callerIdentityFromContext returns the identity createMiddleware put on
// ctx for minting tokens downstream (its Hop and Expiry already set for
// the next hop), or false if ctx carries none.
func callerIdentityFromContext(ctx context.Context) (auth.Identity, bool) {
	id, ok := ctx.Value(callerIdentityContextKey{}).(auth.Identity)
	return id, ok
}

// minterFor returns the relayheaders.MintFunc for one request: for each
// outbound request to a backend server, an identity assertion minted for
// that server and, only if internalAuth.relay lets that server relay (and
// selfUrl is configured), a relay token plus the proxy's callback URL.
// Without internalAuth, only the tool-call ID is sent.
func minterFor(id auth.Identity) relayheaders.MintFunc {
	return func(server string) (relayheaders.Set, error) {
		set := relayheaders.Set{ToolCallID: id.ToolCallID}
		if internalSigner == nil {
			return set, nil
		}
		assertion, err := internalSigner.SignAssertion(id, server)
		if err != nil {
			return relayheaders.Set{}, fmt.Errorf("minting identity assertion: %w", err)
		}
		set.Assertion = "Bearer " + assertion
		if selfURL != "" && len(relayTargets(server)) > 0 {
			relay, err := internalSigner.SignRelay(id, server)
			if err != nil {
				return relayheaders.Set{}, fmt.Errorf("minting relay token: %w", err)
			}
			set.RelayToken = "Bearer " + relay
			set.CallbackURL = selfURL
		}
		return set, nil
	}
}

func createMiddleware(verifier *auth.Verifier, gateway *dynamic_proxy.Gateway) func(next mcp.MethodHandler) mcp.MethodHandler {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(
			ctx context.Context,
			method string,
			req mcp.Request,
		) (result mcp.Result, err error) {
			// The SDK runs handlers on its own goroutines and never recovers,
			// so a panic anywhere below (a handler, a backend result it didn't
			// expect) would otherwise kill the whole process, and every
			// user's calls with it. Turn it into this request's error.
			defer func() {
				if r := recover(); r != nil {
					logging.Errorf("MCP method %s panicked: %v\n%s", method, r, debug.Stack())
					result, err = nil, fmt.Errorf("internal error handling %s", method)
				}
			}()

			// subscriptions/listen is the one method that keeps the SDK's
			// context values: its handler needs the request ID the SDK puts
			// there to register the listen stream, and it never calls a
			// backend, so there's no protocol version to leak. Stripping them
			// made every new-protocol client's notification stream fail
			// ("subscriptions/listen requires a request ID"), leaving the
			// gateway's advertised listChanged undeliverable.
			if method != methodSubscriptionsListen {
				original := ctx
				ctx = detachContextValues(ctx)
				// Saved only for sending notifications (e.g. relayed
				// progress) back to this client on this request's stream -
				// never used for a backend call. See
				// dynamic_proxy/progress_relay.go.
				ctx = dynamic_proxy.WithClientRequestContext(ctx, original)
			}

			// A relayed call's claims go through exactly the same
			// authorization checks below as a direct call from that same
			// client would — relaying grants no additional authority, and
			// checkRelayTarget narrows it further.
			extractedClaims, ra, err := authenticateRequest(verifier, req.GetExtra().Header)
			if err != nil {
				logging.Errorf("MCP method rejected: method=%s err=%v", method, err)
				return nil, err
			}
			if err := checkRelayTarget(ra, req); err != nil {
				logging.Errorf("MCP method rejected: method=%s err=%v", method, err)
				return nil, err
			}

			// Stashed so a native tool handler running downstream of next(...)
			// (a "utility" tool, e.g. proxy_utility.find_relevant_tools — see
			// main.go's registerUtilityTools) can read back the caller's
			// already-verified claims without re-deriving them.
			ctx = context.WithValue(ctx, callerClaimsContextKey{}, extractedClaims)

			// The identity tokens minted for this request carry: the next
			// hop's number, an expiry never later than the credential this
			// request arrived with (so relaying can't extend a user's
			// identity), and the relay origin, if any.
			subStr, _ := extractedClaims.GetSubject()
			identity := auth.Identity{
				Subject:    subStr,
				Roles:      access_control.RolesFromClaims(extractedClaims),
				Expiry:     ra.expiry,
				Hop:        ra.hop + 1,
				ToolCallID: ra.toolCallID,
				RelayFrom:  ra.relayFrom,
			}

			// Assign a tool call's correlation UUID: a relayed or loopback
			// call keeps the one its verified token carries, so the same
			// ID threads through the whole causal chain of one
			// client-initiated tool call; anything else gets a fresh one.
			// A client-supplied ToolCallIDHeader is ignored. See
			// docs/architecture/auth.md's "Tool-call correlation ID".
			toolCallID := ""
			if _, ok := req.(*mcp.CallToolRequest); ok {
				toolCallID = ra.toolCallID
				if toolCallID == "" {
					toolCallID = uuid.NewString()
				}
				identity.ToolCallID = toolCallID
			}

			ctx = context.WithValue(ctx, callerIdentityContextKey{}, identity)
			ctx = relayheaders.WithMinter(ctx, minterFor(identity))

			logging.Debugf("MCP method started: method=%s session_id=%s has_params=%v",
				method, req.GetSession().ID(), req.GetParams() != nil)

			if ctr, ok := req.(*mcp.CallToolRequest); ok {

				logging.Debugf("Calling tool: name=%s args=%s tool_call_id=%s",
					ctr.Params.Name, logging.Size(ctr.Params.Arguments), toolCallID)
				if !isToolRoleAuthorized(extractedClaims, ctr.Params.Name) {
					subStr, err := extractedClaims.GetSubject()
					if err != nil {
						subStr = "unknown"
					}
					e := fmt.Errorf("[%v] doesn't have a role that allows it to call tool [%v]", subStr, ctr.Params.Name)
					logging.Error(e.Error())
					return nil, e
				}

				if err := checkRateLimit(ctx, gateway, ctr.Params.Name, access_control.RolesFromClaims(extractedClaims)); err != nil {
					logging.Errorf("rate limit check failed: tool=%s error=%v", ctr.Params.Name, err)
					return nil, err
				}

				// If an orchestration guards this tool, route the whole
				// call through it instead of falling through to the plain
				// next(...) call below: triggeredRuntime.Guard runs the
				// phase-based call-authorization -> real call ->
				// result-authorization -> async audit flow itself,
				// invoking the real backend call via doCall.
				//
				// proxy_utility.call_tool's own invocation is explicitly
				// excluded here, regardless of whether a deployment's
				// toolMappings would otherwise match it (e.g. a catch-all
				// "*" pattern) — orchestration still applies to whatever
				// TARGET tool call_tool relays to, via that tool's own
				// ordinary tools/call loopback (utility_tools.go), so
				// nothing is silently unguarded; only call_tool's own
				// wrapper invocation is exempt.
				if orchName, orch, matched := triggeredRuntime.MatchOrchestration(ctr.Params.Name); matched && ctr.Params.Name != callToolToolName {
					// A tool whose results are checked mustn't leak through
					// its progress messages - free text from the backend that
					// no result check sees. Progress itself still flows.
					if len(orch.ResultAuthorization) > 0 {
						ctx = dynamic_proxy.WithoutProgressMessages(ctx)
					}
					subStr, err := extractedClaims.GetSubject()
					if err != nil {
						subStr = "unknown"
					}
					caller := triggered_tools_runtime.CallerIdentity{
						User:  subStr,
						Roles: access_control.RolesFromClaims(extractedClaims),
					}
					result, err := triggeredRuntime.Guard(ctx, orchName, caller, toolCallID, ctr.Params, func(ctx context.Context) (*mcp.CallToolResult, error) {
						res, err := next(ctx, method, req)
						if err != nil {
							return nil, err
						}
						ctRes, ok := res.(*mcp.CallToolResult)
						if !ok {
							return nil, fmt.Errorf("unexpected result type %T from backend tool call", res)
						}
						return ctRes, nil
					})
					if err != nil {
						logging.Errorf("tool call rejected by orchestration: method=%s err=%v", method, err)
						return nil, err
					}
					return result, nil
				}
			}

			if gpr, ok := req.(*mcp.GetPromptRequest); ok {
				logging.Debugf("Getting prompt: name=%s args=%s", gpr.Params.Name, logging.Size(gpr.Params.Arguments))
				if !isPromptRoleAuthorized(extractedClaims, gpr.Params.Name) {
					subStr, err := extractedClaims.GetSubject()
					if err != nil {
						subStr = "unknown"
					}
					e := fmt.Errorf("[%v] doesn't have a role that allows it to fetch prompt [%v]", subStr, gpr.Params.Name)
					logging.Error(e.Error())
					return nil, e
				}
			}

			if rrr, ok := req.(*mcp.ReadResourceRequest); ok {
				logging.Debugf("Reading resource: uri=%s", rrr.Params.URI)
				if !isResourceRoleAuthorized(extractedClaims, rrr.Params.URI) {
					subStr, err := extractedClaims.GetSubject()
					if err != nil {
						subStr = "unknown"
					}
					e := fmt.Errorf("[%v] doesn't have a role that allows it to read resource [%v]", subStr, rrr.Params.URI)
					logging.Error(e.Error())
					return nil, e
				}
			}

			result, err = next(ctx, method, req)

			if err != nil {
				logging.Errorf("MCP method failed: method=%s session_id=%s err=%v",
					method, req.GetSession().ID(), err)
			} else {
				logging.Debugf("MCP method completed: method=%s session_id=%s has_result=%v",
					method, req.GetSession().ID(), result != nil)
				// Log more for tool results.
				if ctr, ok := result.(*mcp.CallToolResult); ok {
					logging.Debugf("tool result: isError=%v structuredContent=%s",
						ctr.IsError, logging.Size(ctr.StructuredContent))
				}
			}

			if err == nil {
				if toolsResult, ok := result.(*mcp.ListToolsResult); ok {
					return filterTools(toolsResult, extractedClaims), nil
				}
				if promptsResult, ok := result.(*mcp.ListPromptsResult); ok {
					return filterPrompts(promptsResult, extractedClaims), nil
				}
				if resourcesResult, ok := result.(*mcp.ListResourcesResult); ok {
					return filterResources(resourcesResult, extractedClaims), nil
				}
				if resourceTemplatesResult, ok := result.(*mcp.ListResourceTemplatesResult); ok {
					return filterResourceTemplates(resourceTemplatesResult, extractedClaims), nil
				}
			}

			return result, err
		}
	}

}

// filterList keeps only the items whose name (per nameOf) authorized
// approves for claims, preserving relative order. Shared by filterTools and
// filterPrompts so the "loop items, check role authorization" logic isn't
// duplicated per feature kind.
func filterList[T any](items []T, claims jwt.MapClaims, nameOf func(T) string, authorized func(jwt.MapClaims, string) bool) []T {
	var allowed []T
	for _, item := range items {
		if authorized(claims, nameOf(item)) {
			allowed = append(allowed, item)
		}
	}
	return allowed
}

// filterTools applies two independent gates, in order: isToolRoleAuthorized
// (the caller's role must be able to call the tool at all - unchanged,
// shared with the real CallTool check) and then discoveryWatcher.Current().
// ListedTools (a global, not per-role, glob allowlist gating what may ever
// appear in a tools/list result - see MCPDiscoveryConfig.ListedTools's own
// doc comment). A tool an unauthorized caller can't call is never listed
// regardless of ListedTools; a tool ListedTools doesn't match is never
// listed even for a caller who could otherwise call it directly or via
// proxy_utility.call_tool.
func filterTools(original *mcp.ListToolsResult, claims jwt.MapClaims) *mcp.ListToolsResult {
	if original == nil {
		return &mcp.ListToolsResult{}
	}
	listed := listableTools(original.Tools, claims)

	// Shallow-copy the whole original result (not a hand-built literal
	// naming only Tools/NextCursor) so every other field the SDK attaches
	// to a list response - Meta, and Cacheable's TTLMs/CacheScope among
	// them - survives filtering unchanged. A hand-built literal silently
	// drops any field not named here, including ones the SDK adds later;
	// that's exactly how this dropped CacheScope (added after this
	// function was first written) down to its Go zero value "" - a value
	// outside the protocol's own ["public","private"] enum, which made a
	// strict client reject the whole tools/list response outright.
	filtered := *original
	filtered.Tools = listed
	return &filtered
}

// listableTools returns the tools a caller may see in a tools/list: those
// its roles can call, and that ListedTools (the global listing allowlist)
// matches. Shared by tools/list and proxy_utility.find_relevant_tools, so
// asking for relevant tools never reveals one tools/list would hide.
func listableTools(tools []*mcp.Tool, claims jwt.MapClaims) []*mcp.Tool {
	authorized := filterList(tools, claims, func(t *mcp.Tool) string { return t.Name }, isToolRoleAuthorized)

	listedTools := discoveryWatcher.Current().ListedTools
	listed := make([]*mcp.Tool, 0, len(authorized))
	for _, t := range authorized {
		for _, pattern := range listedTools {
			if access_control.GlobMatch(pattern, t.Name) {
				listed = append(listed, t)
				break
			}
		}
	}
	return listed
}

func filterPrompts(original *mcp.ListPromptsResult, claims jwt.MapClaims) *mcp.ListPromptsResult {
	if original == nil {
		return &mcp.ListPromptsResult{}
	}
	filtered := *original
	filtered.Prompts = filterList(original.Prompts, claims, func(p *mcp.Prompt) string { return p.Name }, isPromptRoleAuthorized)
	return &filtered
}

func filterResources(original *mcp.ListResourcesResult, claims jwt.MapClaims) *mcp.ListResourcesResult {
	if original == nil {
		return &mcp.ListResourcesResult{}
	}
	filtered := *original
	filtered.Resources = filterList(original.Resources, claims, func(r *mcp.Resource) string { return r.URI }, isResourceRoleAuthorized)
	return &filtered
}

func filterResourceTemplates(original *mcp.ListResourceTemplatesResult, claims jwt.MapClaims) *mcp.ListResourceTemplatesResult {
	if original == nil {
		return &mcp.ListResourceTemplatesResult{}
	}
	filtered := *original
	filtered.ResourceTemplates = filterList(original.ResourceTemplates, claims, func(t *mcp.ResourceTemplate) string { return t.URITemplate }, isResourceTemplateRoleAuthorized)
	return &filtered
}

func isToolRoleAuthorized(claims jwt.MapClaims, toolName string) bool {
	return accessPolicyEngine.IsToolAuthorized(claims, toolName)
}

func isPromptRoleAuthorized(claims jwt.MapClaims, promptName string) bool {
	return accessPolicyEngine.IsPromptAuthorized(claims, promptName)
}

func isResourceRoleAuthorized(claims jwt.MapClaims, uri string) bool {
	return accessPolicyEngine.IsResourceAuthorized(claims, uri)
}

func isResourceTemplateRoleAuthorized(claims jwt.MapClaims, uriTemplate string) bool {
	return accessPolicyEngine.IsResourceTemplateAuthorized(claims, uriTemplate)
}
