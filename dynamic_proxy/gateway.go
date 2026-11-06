// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package dynamic_proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
)

// SessionRetriever returns the backend session for server on behalf of
// client session sessionId. ctx is the calling request's: it bounds how
// long the call waits for a session, and must not be used for the
// session's own connection, which outlives the request.
type SessionRetriever func(ctx context.Context, sessionId string, server *discover_mcp_servers.ServerAttributes) (*mcp.ClientSession, error)

// ToolVisibilityFilter reports whether a discovered tool should be hidden
// from the client-facing gateway entirely (never registered on Server(),
// so it never appears in tools/list and can never be invoked via
// tools/call by an ordinary client). This exists so go-proxy's own
// "triggered tool" concept (see triggered-tools-runtime, whose tools are
// invoked only by the proxy's own orchestration engine) can be
// implemented without dynamic_proxy importing that package — which would
// create an import cycle, since triggered-tools-runtime already imports
// dynamic_proxy for ReservedServerName. A nil filter hides nothing.
type ToolVisibilityFilter func(tool *mcp.Tool) bool

// ReservedServerName is refused as a backend server name by OnAdded: it's
// reserved for the gateway's own native "_admin.*" tools (see main.go's
// registerAdminTools). Without this guard, a backend discovered under this
// exact name would namespace its own tools to "_admin.<name>" — colliding
// byte-for-byte with the gateway's native tool names. The MCP SDK's tool
// registry silently overwrites on a duplicate AddTool call for the same
// name (last registration wins, no error, and this recurs on every poll
// cycle for the life of the process, not just once at startup), so such a
// backend could hijack calls to security-critical tools like
// _admin.update_role_permissions even though
// access_control.AccessPolicyEngine's admin-only check for that name would
// still (correctly, but insufficiently) require the caller to be admin —
// that check only inspects the tool *name* string and has no way to know
// which handler is actually bound to it.
const ReservedServerName = "_admin"

// UtilityServerName is reserved the same way ReservedServerName is (see its
// doc comment) for the gateway's own native "proxy_utility.*" tools (see
// main.go's registerUtilityTools) — proxy-native tools that, unlike
// "_admin.*", are gated by the regular configured access rules rather than
// a hardcoded admin-only check, but still need the same collision
// protection: a discovered backend registered under this exact name could
// otherwise hijack calls to a utility tool.
const UtilityServerName = "proxy_utility"

// reservedServerNames is every name IsReservedServerName refuses.
var reservedServerNames = []string{ReservedServerName, UtilityServerName}

// IsReservedServerName reports whether name is reserved for one of the
// gateway's own native tool namespaces. Exported so other packages
// (triggered_tools_runtime) that independently track discovered servers can
// apply the same refusal.
func IsReservedServerName(name string) bool {
	return slices.Contains(reservedServerNames, name)
}

type Gateway struct {
	mu                       sync.RWMutex
	gatewayServer            *mcp.Server
	serverInfo               map[string]*discover_mcp_servers.ServerAttributes // serverName -> attributes
	getSession               SessionRetriever
	isHiddenTool             ToolVisibilityFilter
	toolRegistry             *genericRegistry[*mcp.Tool]
	promptRegistry           *genericRegistry[*mcp.Prompt]
	resourceRegistry         *genericRegistry[*mcp.Resource]
	resourceTemplateRegistry *genericRegistry[*mcp.ResourceTemplate]
	registries               []featureRegistry // toolRegistry, promptRegistry, resourceRegistry, resourceTemplateRegistry, and any future kinds; driven uniformly by OnAdded/OnRemoved
	progress                 *progressRelay    // in-flight calls whose progress is relayed to the client; see progress_relay.go
}

// gatewayCapabilities is what the gateway advertises to its clients,
// declared explicitly rather than left to the SDK's default, which derives
// capabilities from whatever is registered at the moment a client connects.
// That default made them depend on timing: a client connecting before
// discovery had registered any prompt (or resource) was told the gateway has
// none, for its whole session. Tools, prompts and resources are therefore
// always advertised - an empty list is a valid answer until backends are
// discovered. listChanged is true because registry changes are announced to
// clients that subscribe via subscriptions/listen (protocol 2026-07-28+; see
// createMiddleware). Logging, the SDK's other default, is left out: the
// gateway never sends log messages to clients. Nothing it can't relay
// (sampling, elicitation, completions, resource subscriptions) is advertised.
func gatewayCapabilities() *mcp.ServerCapabilities {
	return &mcp.ServerCapabilities{
		Tools:     &mcp.ToolCapabilities{ListChanged: true},
		Prompts:   &mcp.PromptCapabilities{ListChanged: true},
		Resources: &mcp.ResourceCapabilities{ListChanged: true},
	}
}

func NewGateway(getSession SessionRetriever, isHiddenTool ToolVisibilityFilter, toolsPollFrequencySeconds int, promptsPollFrequencySeconds int, resourcesPollFrequencySeconds int, resourceTemplatesPollFrequencySeconds int) *Gateway {
	g := &Gateway{
		gatewayServer: mcp.NewServer(&mcp.Implementation{
			Name:    "dynamic-gateway",
			Version: "1.0.0",
		}, &mcp.ServerOptions{Capabilities: gatewayCapabilities()}),
		serverInfo:   make(map[string]*discover_mcp_servers.ServerAttributes),
		getSession:   getSession,
		isHiddenTool: isHiddenTool,
		progress:     newProgressRelay(),
	}

	g.toolRegistry = newGenericRegistry(
		"tool",
		toolsPollFrequencySeconds,
		fetchTools,
		toolName,
		g.registerTool,
		g.gatewayServer.RemoveTools,
		func(t *mcp.Tool, serverName string) string {
			t.Name = serverName + "." + t.Name
			return t.Name
		},
	)
	g.promptRegistry = newGenericRegistry(
		"prompt",
		promptsPollFrequencySeconds,
		fetchPrompts,
		promptName,
		g.registerPrompt,
		g.gatewayServer.RemovePrompts,
		func(p *mcp.Prompt, serverName string) string {
			p.Name = serverName + "." + p.Name
			return p.Name
		},
	)
	g.resourceRegistry = newGenericRegistry(
		"resource",
		resourcesPollFrequencySeconds,
		fetchResources,
		resourceURI,
		g.registerResource,
		g.gatewayServer.RemoveResources,
		func(r *mcp.Resource, serverName string) string {
			r.URI = serverName + ":" + r.URI
			return r.URI
		},
	)
	g.resourceTemplateRegistry = newGenericRegistry(
		"resourceTemplate",
		resourceTemplatesPollFrequencySeconds,
		fetchResourceTemplates,
		resourceTemplateURI,
		g.registerResourceTemplate,
		g.gatewayServer.RemoveResourceTemplates,
		func(t *mcp.ResourceTemplate, serverName string) string {
			t.URITemplate = serverName + ":" + t.URITemplate
			return t.URITemplate
		},
	)
	g.registries = []featureRegistry{g.toolRegistry, g.promptRegistry, g.resourceRegistry, g.resourceTemplateRegistry}

	return g
}

func (g *Gateway) Server() *mcp.Server {
	return g.gatewayServer
}

// Tool returns the discovered tool currently registered under namespacedName,
// if any — deliberately NOT filtered through isHiddenTool the way Tools()
// is, since callers of this (e.g. the rate-limiting gate, checking whether a
// target tool is a triggered tool) specifically need to see a hidden/
// triggered tool's own _meta, not just the client-visible subset.
func (g *Gateway) Tool(namespacedName string) (*mcp.Tool, bool) {
	return g.toolRegistry.Item(namespacedName)
}

// Tools returns a snapshot of every tool currently visible on the gateway
// server — used by go-proxy's own native "proxy_utility.*" tools (main.go's
// registerUtilityTools) to build an already-namespaced candidate list
// in-process, without a tools/list round trip the way an external backend
// has to. g.toolRegistry.Items() alone isn't enough: it's populated
// unconditionally by reconcile even for tools registerTool decides to hide
// (triggered tools, see registerTool below), so this re-applies the same
// isHiddenTool check to exactly mirror what's actually registered on
// gatewayServer.
func (g *Gateway) Tools() []*mcp.Tool {
	items := g.toolRegistry.Items()
	if g.isHiddenTool == nil {
		return items
	}
	visible := make([]*mcp.Tool, 0, len(items))
	for _, t := range items {
		if !g.isHiddenTool(t) {
			visible = append(visible, t)
		}
	}
	return visible
}

// Prompts returns a snapshot of every prompt currently visible on the
// gateway server — the promptRegistry counterpart to Tools() above, used
// by go-proxy's own native "proxy_utility.get_prompts" tool (main.go's
// registerUtilityTools) to build its result in-process, without a
// prompts/list round trip. Unlike Tools(), there's no hidden-prompt
// concept to filter out — every registered prompt is visible — so this is
// just promptRegistry.Items() directly.
func (g *Gateway) Prompts() []*mcp.Prompt {
	return g.promptRegistry.Items()
}

// HiddenTools returns a snapshot of every discovered tool currently hidden
// from the gateway server as a triggered tool — the isHiddenTool(t) == true
// counterpart to Tools() above. Used by go-proxy's own native
// "proxy_utility.list_triggered_tools" tool (main.go's registerUtilityTools)
// to list triggered tools, which by design never appear in an ordinary
// tools/list/Tools() result.
func (g *Gateway) HiddenTools() []*mcp.Tool {
	if g.isHiddenTool == nil {
		return nil
	}
	items := g.toolRegistry.Items()
	hidden := make([]*mcp.Tool, 0, len(items))
	for _, t := range items {
		if g.isHiddenTool(t) {
			hidden = append(hidden, t)
		}
	}
	return hidden
}

func (g *Gateway) OnAdded(ctx context.Context, servers discover_mcp_servers.ServerMap) {
	for name, attr := range servers {
		if IsReservedServerName(name) {
			logging.Errorf("[Gateway] Refusing to add discovered server named %q: this name is reserved for the gateway's own native tools", name)
			continue
		}

		logging.Debugf("[Gateway] Server added: %s (%s)", name, attr.ServerUrl)

		g.mu.Lock()
		g.serverInfo[name] = attr
		g.mu.Unlock()

		// Start polling this server for every feature kind. Each registry's
		// own immediate baseline tick performs the initial discovery
		// asynchronously and reconciles the gateway's registrations when it
		// completes.
		for _, reg := range g.registries {
			reg.Start(ctx, attr)
		}
	}
}

// InitialPollsPending returns how many (server, feature kind) first polls
// are still outstanding for the servers the gateway knows about. Zero means
// every known server's tools, prompts, resources and resource templates
// have been fetched (or attempted) at least once.
func (g *Gateway) InitialPollsPending() int {
	pending := 0
	for _, reg := range g.registries {
		pending += reg.PendingFirstPolls()
	}
	return pending
}

func (g *Gateway) OnRemoved(servers discover_mcp_servers.ServerMap) {
	for name := range servers {
		logging.Debugf("[Gateway] Server removed: %s", name)

		for _, reg := range g.registries {
			reg.RemoveServer(name)
		}

		g.mu.Lock()
		delete(g.serverInfo, name)
		g.mu.Unlock()
	}

	for name := range servers {
		for _, reg := range g.registries {
			reg.Stop(name)
		}
	}
}

// registerTool is the tool-kind registerFunc[*mcp.Tool] passed to
// newGenericRegistry: it wires a single namespaced tool's AddTool handler,
// which forwards the call to the correct backend session via getSession.
// By the time this runs, tool.Name has already been mutated to its
// namespaced form by the registry's namespaceFunc, so the original,
// unnamespaced name is recovered from namespacedName instead.
func (g *Gateway) registerTool(namespacedName string, tool *mcp.Tool) {
	// Defense-in-depth, redundant with OnAdded's IsReservedServerName check:
	// OnAdded should already have refused any reserved server name before
	// this could ever be reached for one, but this is the actual
	// registration point, so it's guarded too in case that ever changes.
	for _, reserved := range reservedServerNames {
		if strings.HasPrefix(namespacedName, reserved+".") {
			logging.Errorf("[Gateway] Refusing to register tool %q: it collides with the %q reserved namespace", namespacedName, reserved)
			return
		}
	}

	if g.isHiddenTool != nil && g.isHiddenTool(tool) {
		// RemoveTools is a defensive no-op in the common case (the tool was
		// never registered), but covers the case where a backend's tool was
		// visible on a previous poll tick and has since been (re)marked
		// hidden — reconcile's own diffing wouldn't otherwise catch this,
		// since it still considers the tool "current", not "stale".
		logging.Debugf("[Gateway] Hiding triggered tool %q from clients (invoked only by go-proxy's own orchestration)", namespacedName)
		g.gatewayServer.RemoveTools(namespacedName)
		return
	}

	serverName, originalName := removeFirstComponent(namespacedName)
	mcp.AddTool(g.gatewayServer, tool, func(ctx context.Context, req *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, any, error) {
		// Forwarded byte for byte, not decoded and re-encoded: decoding into
		// map[string]any rounds numbers to float64 (1e-400 becomes 0), so the
		// backend would get different values from those a guardrail
		// constraint checked. Still checked to be a JSON object.
		var args any
		if len(req.Params.Arguments) > 0 {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(req.Params.Arguments, &object); err != nil {
				return nil, nil, fmt.Errorf("failed to parse tool arguments: %w", err)
			}
			args = req.Params.Arguments
		}

		sess, err := g.SessionFor(ctx, req.GetSession().ID(), serverName)
		if err != nil {
			return nil, nil, err
		}

		// InputResponses/RequestState carry a client's answers when it calls
		// again after an "input required" result (protocol 2026-07-28+):
		// the backend's result is passed back to the client unchanged (the
		// backend connection doesn't answer it itself), and the client's
		// follow-up call is forwarded here like any other.
		params := &mcp.CallToolParams{
			Name:           originalName,
			Arguments:      args,
			InputResponses: req.Params.InputResponses,
			RequestState:   req.Params.RequestState,
		}
		done := g.RelayProgress(ctx, req.Session, req.Params.GetProgressToken(), params)
		defer done()

		res, err := sess.CallTool(ctx, params)
		if err != nil {
			return nil, nil, fmt.Errorf("backend tool call failed: %w", err)
		}
		return res, nil, nil
	})
}

// registerPrompt is the prompt-kind registerFunc[*mcp.Prompt] passed to
// newGenericRegistry, mirroring registerTool for prompts/get.
func (g *Gateway) registerPrompt(namespacedName string, prompt *mcp.Prompt) {
	serverName, originalName := removeFirstComponent(namespacedName)
	g.gatewayServer.AddPrompt(prompt, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		sess, err := g.SessionFor(ctx, req.GetSession().ID(), serverName)
		if err != nil {
			return nil, err
		}

		res, err := sess.GetPrompt(ctx, &mcp.GetPromptParams{Name: originalName, Arguments: req.Params.Arguments})
		if err != nil {
			return nil, fmt.Errorf("backend prompt call failed: %w", err)
		}
		return res, nil
	})
}

// registerResource is the resource-kind registerFunc[*mcp.Resource] passed to
// newGenericRegistry, mirroring registerPrompt for resources/read. Resources
// are namespaced by URI (not Name, which need not be unique), so their
// namespaced form is split back apart via splitNamespacedResourceURI rather
// than removeFirstComponent.
func (g *Gateway) registerResource(namespacedURI string, resource *mcp.Resource) {
	serverName, originalURI := splitNamespacedResourceURI(namespacedURI)
	g.gatewayServer.AddResource(resource, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sess, err := g.SessionFor(ctx, req.GetSession().ID(), serverName)
		if err != nil {
			return nil, err
		}

		res, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		if err != nil {
			return nil, fmt.Errorf("backend resource read failed: %w", err)
		}
		return res, nil
	})
}

// registerResourceTemplate is the resource-template-kind
// registerFunc[*mcp.ResourceTemplate] passed to newGenericRegistry. Unlike
// registerResource, it cannot capture a single static "original URI" at
// registration time: a resource is one URI, but one template matches many
// (the SDK dispatches a resources/read whose URI matches this template's
// pattern to this same handler, per-request, only after finding no exact
// Resource match). So only serverName is derived once, from the template's
// own namespaced name; the concrete URI is re-derived from req.Params.URI on
// every call.
func (g *Gateway) registerResourceTemplate(namespacedTemplate string, template *mcp.ResourceTemplate) {
	serverName, _ := splitNamespacedResourceURI(namespacedTemplate)
	g.gatewayServer.AddResourceTemplate(template, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		_, originalURI := splitNamespacedResourceURI(req.Params.URI)
		sess, err := g.SessionFor(ctx, req.GetSession().ID(), serverName)
		if err != nil {
			return nil, err
		}

		res, err := sess.ReadResource(ctx, &mcp.ReadResourceParams{URI: originalURI})
		if err != nil {
			return nil, fmt.Errorf("backend resource read failed: %w", err)
		}
		return res, nil
	})
}

// SessionFor looks up the backend session for serverName on behalf of
// sessionId, shared by registerTool, registerPrompt, registerResource, and
// registerResourceTemplate. Exported so go-proxy's own native "proxy_utility.*"
// tools (main.go's registerUtilityTools) can reach a backend session
// directly, the same live, already-correct way (backed by g.serverInfo,
// kept current by OnAdded/OnRemoved) any discovered tool call does.
func (g *Gateway) SessionFor(ctx context.Context, sessionId string, serverName string) (*mcp.ClientSession, error) {
	g.mu.RLock()
	info, ok := g.serverInfo[serverName]
	g.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no server information found for server: %s", serverName)
	}

	sess, err := g.getSession(ctx, sessionId, info)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve session for client %s, server %s: %w", sessionId, serverName, err)
	}
	return sess, nil
}

func removeFirstComponent(name string) (string, string) {
	prefix, after, found := strings.Cut(name, ".")
	if !found {
		return name, ""
	}
	return prefix, after
}

// splitNamespacedResourceURI splits a namespaced resource URI
// (serverName + ":" + originalURI) back into its parts. Resources use ":"
// rather than removeFirstComponent's "." because resource URIs routinely
// contain dots (e.g. "file:///a.txt"); only the first colon is ever the
// injected separator since server names never contain one.
func splitNamespacedResourceURI(namespacedURI string) (serverName, originalURI string) {
	prefix, after, found := strings.Cut(namespacedURI, ":")
	if !found {
		return namespacedURI, ""
	}
	return prefix, after
}
