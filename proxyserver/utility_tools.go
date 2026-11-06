// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callToolToolName is proxy_utility.call_tool's own namespaced name — a
// named constant, not composed inline at each use site, because
// middleware.go's createMiddleware also needs it (to skip orchestration
// matching for call_tool's own invocation — see its doc comment there).
const callToolToolName = dynamic_proxy.UtilityServerName + ".call_tool"

// registerUtilityTools adds the proxy's own native, in-process "proxy_utility"
// tools directly to the gateway's *mcp.Server, namespaced "proxy_utility.*". Like
// "_admin.*" tools (admin_tools.go), they're implemented by the proxy
// itself rather than forwarded to a backend — but unlike "_admin.*", their
// own invocation is authorized through the regular configured access rules
// (access_control.AccessPolicyEngine.IsToolAuthorized's ordinary matchAny
// path), not a hardcoded admin-only check: dynamic_proxy.UtilityServerName
// is reserved against name collisions the same way dynamic_proxy.
// ReservedServerName is, but access_control.isAdminTool only ever matches
// the "_admin." prefix, so "proxy_utility.*" tools are just ordinary
// accessRules-gated tools from the authorization engine's point of view —
// every deployment's own config must grant whichever roles should be able
// to reach them. call_tool, get_my_id_details, get_prompts, and
// render_prompt are all meant to be broadly usable, so every committed
// *.cfg file (proxy-server.cfg/integration_test.cfg/orchestration_test.cfg)
// grants them via one role-wildcard rule — {"role": "*", "allowed_tools":
// ["proxy_utility.call_tool", "proxy_utility.get_my_id_details",
// "proxy_utility.get_prompts", "proxy_utility.render_prompt"]} — rather
// than listing them under each individual role: access_control.matchAny
// matches a rule's role pattern with filepath.Match, so "*" matches every
// role string, including ones the rest of that same config never otherwise
// names. This is a config-level choice, not special-cased in code — the
// rule is exactly as ordinary as any other accessRules entry.
//
// What makes them "utility" tools is the reverse power: a handler here may
// call any other tool on the caller's behalf. Two different shapes of that
// power are demonstrated by the two tools below:
//
//   - find_relevant_tools calls _filter_tool directly via
//     gateway.SessionFor(...).CallTool(...) — a plain backend-session call
//     that never re-enters gateway.Server()/createMiddleware, so it's never
//     subject to access-rule or orchestration enforcement for that inner
//     call. This is exactly the same dispatch shape dynamic_proxy.Gateway.
//     registerTool's own handler and triggered_tools_runtime.Runtime.Guard's
//     triggered-tool invocation already use.
//   - call_tool instead loops back into the proxy's own HTTP endpoint (see
//     its own doc comment below) specifically so the target tool call DOES
//     go through the full createMiddleware pipeline again — access rules
//     and orchestration both apply to the TARGET tool exactly as if the
//     caller had called it directly, because, from the gateway's point of
//     view, that's exactly what happens: an ordinary tools/call, relayed
//     with the same identity assertion mechanism a backend uses to call
//     back into the proxy (see CLAUDE.md's "Internal identity assertions"
//     section). call_tool's OWN invocation, by contrast, is checked like
//     any ordinary tool (no hardcoded bypass) but is explicitly exempted
//     from orchestration matching — see middleware.go.
//   - render_prompt is call_tool's own loopback shape, applied to
//     prompts/get instead of tools/call — see its own doc comment below.
func registerUtilityTools(gateway *dynamic_proxy.Gateway) {
	findRelevantToolsTool := &mcp.Tool{
		Name:        dynamic_proxy.UtilityServerName + ".find_relevant_tools",
		Description: "Given a task, return only your own tools relevant to that task.",
	}
	nativeTools = append(nativeTools, findRelevantToolsTool)
	mcp.AddTool(gateway.Server(), findRelevantToolsTool, func(ctx context.Context, req *mcp.CallToolRequest, args findRelevantToolsArgs) (*mcp.CallToolResult, findRelevantToolsResult, error) {
		// The caller's own already-authorized candidate list, built
		// in-process — no tools/list round trip needed, unlike
		// ai-powered-tools._filter_tool (an ordinary backend with no
		// privileged access to gateway internals) when it used to fetch
		// this itself. gateway.Tools() covers discovered *backend* tools
		// (excluding hidden triggered ones); nativeTools (main.go) covers
		// everything registerAdminTools/registerUtilityTools added
		// directly (there's no exported way to enumerate those back out of
		// *mcp.Server itself — see nativeTools' own doc comment) — without
		// this union, "_admin."/"proxy_utility." tools could never be
		// offered as candidates, even to a caller who can see and call them
		// via an ordinary tools/list. filterList applies the same
		// isToolRoleAuthorized check ListTools results go through, so an
		// admin caller sees _admin.* here too, and nobody else does.
		claims := callerClaimsFromContext(ctx)
		allTools := append(gateway.Tools(), nativeTools...)
		candidates := listableTools(allTools, claims)

		sess, err := gateway.SessionFor(ctx, req.GetSession().ID(), "ai-powered-tools")
		if err != nil {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: %w", err)
		}

		// A direct backend-session call, not a tools/call re-entering the
		// gateway — see the package doc comment above for why this is what
		// "bypasses access rules" without any separate bypass code. ctx
		// still carries the internal identity assertion/callback URL/
		// tool-call-ID createMiddleware stashed for THIS call, so
		// _filter_tool receives a proper assertion for the original caller
		// and shares its tool-call-ID (CLAUDE.md's "Tool-call correlation
		// ID" section) — no extra wiring needed.
		res, err := sess.CallTool(ctx, &mcp.CallToolParams{
			Name:      "_filter_tool",
			Arguments: map[string]any{"task": args.Task, "candidates": candidates},
		})
		if err != nil {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: _filter_tool call failed: %w", err)
		}
		if res.IsError {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: _filter_tool rejected the call: %s", toolErrorText(res.Content))
		}

		if len(res.Content) == 0 {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: _filter_tool returned no content")
		}
		textContent, ok := res.Content[0].(*mcp.TextContent)
		if !ok {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: expected text content from _filter_tool, got %T", res.Content[0])
		}
		var filterResult struct {
			Tools []mcp.Tool `json:"tools"`
		}
		if err := json.Unmarshal([]byte(textContent.Text), &filterResult); err != nil {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: failed to parse _filter_tool's result: %w", err)
		}
		// Only tools that were candidates, as go-proxy itself described them:
		// whatever answers as ai-powered-tools can't add tools, or put its own
		// text in front of the model as a tool's description.
		byName := make(map[string]*mcp.Tool, len(candidates))
		for _, c := range candidates {
			byName[c.Name] = c
		}
		var chosen []mcp.Tool
		for _, t := range filterResult.Tools {
			if c, ok := byName[t.Name]; ok {
				chosen = append(chosen, *c)
				delete(byName, t.Name)
			}
		}
		resultMaps, err := mapsFromTools(chosen)
		if err != nil {
			return nil, findRelevantToolsResult{}, fmt.Errorf("proxy_utility.find_relevant_tools: failed to encode result: %w", err)
		}
		return nil, findRelevantToolsResult{Tools: resultMaps}, nil
	})

	// get_my_id_details reads directly from what createMiddleware already
	// verified for THIS call - the same callerClaimsFromContext accessor
	// find_relevant_tools uses above - never a caller-supplied argument, so
	// there's no way to ask about anyone but yourself. Meant to be broadly
	// usable (there's no privilege-escalation risk in a caller learning
	// what it already knows about itself), so - like call_tool - every
	// committed *.cfg file grants it via the same role-wildcard rule.
	getMyIDDetailsTool := &mcp.Tool{
		Name:        dynamic_proxy.UtilityServerName + ".get_my_id_details",
		Description: "Return the id and roles of the caller currently making this request.",
	}
	nativeTools = append(nativeTools, getMyIDDetailsTool)
	mcp.AddTool(gateway.Server(), getMyIDDetailsTool, func(ctx context.Context, req *mcp.CallToolRequest, args getMyIDDetailsArgs) (*mcp.CallToolResult, getMyIDDetailsResult, error) {
		claims := callerClaimsFromContext(ctx)
		sub, _ := claims["sub"].(string)
		return nil, getMyIDDetailsResult{ID: sub, Roles: access_control.RolesFromClaims(claims)}, nil
	})

	// call_tool's own invocation is authorized like any ordinary tool —
	// see the func doc comment above — so every role meant to use it needs
	// an explicit accessRules grant (every committed *.cfg file grants it
	// to all roles). Its own outer invocation is deliberately excluded
	// from orchestration matching (middleware.go), regardless of what a
	// deployment's toolMappings say — orchestration only ever applies to
	// the TARGET tool, via the loopback below.
	callToolTool := &mcp.Tool{
		Name:        callToolToolName,
		Description: "Call a tool on your own behalf, subject to the same access rules and orchestration a direct call to it would be.",
	}
	nativeTools = append(nativeTools, callToolTool)
	mcp.AddTool(gateway.Server(), callToolTool, func(ctx context.Context, req *mcp.CallToolRequest, args callToolArgs) (*mcp.CallToolResult, any, error) {
		// A loopback token for the caller createMiddleware verified for
		// THIS call (the outer call_tool invocation): presenting it back
		// to the proxy's own address is what makes the inner call arrive
		// authenticated as the original caller, not as call_tool itself -
		// see loopbackHeaders.
		incoming, err := loopbackHeaders(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("proxy_utility.call_tool: %w", err)
		}

		// The progress handler relays the inner call's progress (which the
		// proxy itself relays from the backend) on to call_tool's own
		// caller - see dynamic_proxy/progress_relay.go.
		// MultiRoundTrip disabled for the same reason as go-proxy's backend
		// connections (session_proxies.go): an "input required" result is
		// passed back to call_tool's own caller, whose follow-up call
		// carries the answers (forwarded below).
		client := mcp.NewClient(&mcp.Implementation{Name: "proxy_utility", Version: "1.0.0"}, &mcp.ClientOptions{
			ProgressNotificationHandler: gateway.HandleBackendProgress,
			MultiRoundTrip:              &mcp.MultiRoundTripOptions{Disabled: true},
		})
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   incoming.CallbackURL,
			HTTPClient: incoming.Client(nil),
		}, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("proxy_utility.call_tool: failed to connect back to proxy at %s: %w", incoming.CallbackURL, err)
		}
		defer session.Close()

		// The full round trip through / (HTTPMiddleware -> createMiddleware)
		// is what applies access-rule and orchestration enforcement to
		// args.Tool exactly as a direct call would get — there is no
		// bypass here to undo, unlike find_relevant_tools's call to
		// _filter_tool above. res is returned as-is (IsError, Content,
		// StructuredContent all preserved) rather than through a typed
		// second return value, mirroring dynamic_proxy.Gateway.
		// registerTool's own passthrough of a backend's raw result.
		params := &mcp.CallToolParams{
			Name:           args.Tool,
			Arguments:      args.Arguments,
			InputResponses: req.Params.InputResponses,
			RequestState:   req.Params.RequestState,
		}
		done := gateway.RelayProgress(ctx, req.Session, req.Params.GetProgressToken(), params)
		defer done()

		res, err := session.CallTool(ctx, params)
		if err != nil {
			return nil, nil, fmt.Errorf("proxy_utility.call_tool: %w", err)
		}
		return res, nil, nil
	})

	// render_prompt is call_tool's prompts/get analog — same loopback
	// mechanism (relayheaders.FromContext -> a fresh session dialed at the
	// callback URL, presenting the original caller's own identity
	// assertion), but there's no orchestration-exclusion concern to mirror:
	// orchestration only ever matches a *mcp.CallToolRequest (see
	// middleware.go), never a prompts/get, so render_prompt's own outer
	// invocation is the only tool-call orchestration ever sees here, and
	// the inner prompts/get it triggers was never a candidate for
	// orchestration matching in the first place. The full round trip
	// through / is what applies isPromptRoleAuthorized to args.Name exactly
	// as a direct prompts/get would - no separate authorization check
	// duplicated here.
	renderPromptTool := &mcp.Tool{
		Name:        dynamic_proxy.UtilityServerName + ".render_prompt",
		Description: "Render a prompt by name and arguments, subject to the same access rules a direct prompts/get call for it would be.",
	}
	nativeTools = append(nativeTools, renderPromptTool)
	mcp.AddTool(gateway.Server(), renderPromptTool, func(ctx context.Context, req *mcp.CallToolRequest, args renderPromptArgs) (*mcp.CallToolResult, renderPromptResult, error) {
		incoming, err := loopbackHeaders(ctx)
		if err != nil {
			return nil, renderPromptResult{}, fmt.Errorf("proxy_utility.render_prompt: %w", err)
		}

		client := mcp.NewClient(&mcp.Implementation{Name: "proxy_utility", Version: "1.0.0"}, nil)
		session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint:   incoming.CallbackURL,
			HTTPClient: incoming.Client(nil),
		}, nil)
		if err != nil {
			return nil, renderPromptResult{}, fmt.Errorf("proxy_utility.render_prompt: failed to connect back to proxy at %s: %w", incoming.CallbackURL, err)
		}
		defer session.Close()

		result, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: args.Name, Arguments: args.Arguments})
		if err != nil {
			return nil, renderPromptResult{}, fmt.Errorf("proxy_utility.render_prompt: %w", err)
		}
		// *mcp.GetPromptResult isn't returned directly: PromptMessage.Content
		// is the SDK's Content interface, which - like mcp.Tool's own any
		// InputSchema/OutputSchema fields (see mapsFromTools below) - infers
		// as an unconstrained schema rather than a proper JSON object,
		// rejected by at least one real MCP client. Round-tripping through
		// GetPromptResult's own MarshalJSON into a map[string]any sidesteps
		// this the same way mapsFromTools does for []mcp.Tool.
		promptMap, err := mapFromValue(result)
		if err != nil {
			return nil, renderPromptResult{}, fmt.Errorf("proxy_utility.render_prompt: failed to encode result: %w", err)
		}
		return nil, renderPromptResult{Prompt: promptMap}, nil
	})

	// list_triggered_tools is the only way to see a triggered tool's name
	// at all: by design (see triggered_tools_runtime.TriggeredToolMetaKey /
	// dynamic_proxy.Gateway.registerTool's isHiddenTool skip), a triggered
	// tool is never registered on the gateway server, so it never appears
	// in an ordinary tools/list. Two independent sources feed the listing:
	// gateway.HiddenTools() (backend-hosted tools marked hidden via _meta,
	// which already carry a real Description/InputSchema from their own
	// server) and triggeredRuntime.NativeTools() (in-process Go functions
	// registered via RegisterNativeCallAuthorization/
	// RegisterNativeResultAuthorization, e.g.
	// proxy_utility.request_constraints_validation — see
	// triggered-tools-runtime/native.go). A native one has no discovered
	// *mcp.Tool at all, so synthesizeNativeToolStub below builds one — not
	// just a bare name, but the real InputSchema/OutputSchema inferred from
	// the same contract.go structs validate.go itself checks discovered
	// tools against (via triggered_tools_runtime.InputSchemaFor/
	// AuthorizationOutputSchema), since which contract a native tool
	// implements is exactly known and fixed, unlike its Description (no
	// free-text description exists anywhere for a native tool to source
	// one from).
	// get_prompts mirrors find_relevant_tools' in-process, no-round-trip
	// shape (gateway.Prompts(), the promptRegistry counterpart to Tools())
	// but needs no inner backend call at all — matching is done here
	// directly via access_control.GlobMatch, the same wildcard semantics
	// every accessRules pattern already uses (e.g. "garden-*.rake_*"),
	// against each prompt's namespaced name. filterList applies the same
	// isPromptRoleAuthorized check prompts/list results go through, so a
	// caller never sees a prompt beyond what their own role already grants.
	getPromptsTool := &mcp.Tool{
		Name:        dynamic_proxy.UtilityServerName + ".get_prompts",
		Description: "Return your own prompts. By default returns all of them; pass namePattern (a glob, e.g. \"garden-online-store.*\" or \"*rake*\") to match against each prompt's namespaced name and return only the matches.",
	}
	nativeTools = append(nativeTools, getPromptsTool)
	mcp.AddTool(gateway.Server(), getPromptsTool, func(ctx context.Context, req *mcp.CallToolRequest, args getPromptsArgs) (*mcp.CallToolResult, getPromptsResult, error) {
		claims := callerClaimsFromContext(ctx)
		prompts := filterList(gateway.Prompts(), claims, func(p *mcp.Prompt) string { return p.Name }, isPromptRoleAuthorized)

		if args.NamePattern != "" {
			matched := make([]*mcp.Prompt, 0, len(prompts))
			for _, p := range prompts {
				if access_control.GlobMatch(args.NamePattern, p.Name) {
					matched = append(matched, p)
				}
			}
			prompts = matched
		}
		sort.Slice(prompts, func(i, j int) bool { return prompts[i].Name < prompts[j].Name })
		return nil, getPromptsResult{Prompts: prompts}, nil
	})

	listTriggeredToolsTool := &mcp.Tool{
		Name:        dynamic_proxy.UtilityServerName + ".list_triggered_tools",
		Description: "List every triggered tool - a tool reserved for the proxy's own orchestration engine and hidden from an ordinary tools/list. By default returns just each tool's namespaced name; pass verbose=true for the full Tool object. A native triggered tool (a plain Go function with no MCP-registered Tool of its own) gets a synthesized Tool object carrying its namespaced name and its real InputSchema/OutputSchema, inferred from its contract.",
	}
	nativeTools = append(nativeTools, listTriggeredToolsTool)
	mcp.AddTool(gateway.Server(), listTriggeredToolsTool, func(ctx context.Context, req *mcp.CallToolRequest, args listTriggeredToolsArgs) (*mcp.CallToolResult, any, error) {
		tools := append([]*mcp.Tool{}, gateway.HiddenTools()...)
		for _, nt := range triggeredRuntime.NativeTools() {
			tools = append(tools, synthesizeNativeToolStub(nt))
		}
		// Only those the caller's roles cover, like every other listing.
		tools = filterList(tools, callerClaimsFromContext(ctx), func(t *mcp.Tool) string { return t.Name }, isToolRoleAuthorized)
		sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

		if args.Verbose {
			return nil, tools, nil
		}
		names := make([]string, len(tools))
		for i, t := range tools {
			names[i] = t.Name
		}
		return nil, names, nil
	})
}

// synthesizeNativeToolStub builds a *mcp.Tool standing in for a native
// triggered tool (nt) — one implemented as a plain Go function
// (triggered-tools-runtime/native.go), never registered as a real
// mcp.Tool anywhere, on any server (see this file's registerUtilityTools
// doc comment). Its Name matches the namespaced (dynamic_proxy.
// UtilityServerName + ".<name>") form an orchestration step's own
// {"server": "proxy_utility", "tool": "<name>"} refers to. InputSchema/
// OutputSchema are the real, inferred contract schemas
// (triggered_tools_runtime.InputSchemaFor/AuthorizationOutputSchema) —
// which contract a native tool implements is exactly known (nt.StepType),
// so there's no need to leave these blank the way a bare name would.
// Description has no equivalent source (no free-text description exists
// anywhere for a native tool), so it states only what's structurally true
// of every native tool, not anything implementation-specific.
func synthesizeNativeToolStub(nt triggered_tools_runtime.NativeToolInfo) *mcp.Tool {
	name := dynamic_proxy.UtilityServerName + "." + nt.Name
	tool := &mcp.Tool{
		Name:        name,
		Description: fmt.Sprintf("Native triggered tool (%s step) - implemented as an in-process Go function, not a real MCP tool hosted by any server.", nt.StepType),
	}
	inputSchema, err := triggered_tools_runtime.InputSchemaFor(nt.StepType)
	if err != nil {
		logging.Errorf("[proxy_utility.list_triggered_tools] failed to infer input schema for native tool %q: %v", name, err)
		return tool
	}
	outputSchema, err := triggered_tools_runtime.AuthorizationOutputSchema()
	if err != nil {
		logging.Errorf("[proxy_utility.list_triggered_tools] failed to infer output schema for native tool %q: %v", name, err)
		return tool
	}
	tool.InputSchema = inputSchema
	tool.OutputSchema = outputSchema
	return tool
}

// findRelevantToolsArgs is proxy_utility.find_relevant_tools' input: just the
// task, mirroring ai-powered-tools' old find_relevant_tools (now
// _filter_tool) input shape. The candidate tool list is never
// caller-supplied — it's built from the caller's own already-authorized
// view of the gateway, above.
type findRelevantToolsArgs struct {
	Task string `json:"task" jsonschema:"A natural-language description of the task you're trying to accomplish. It is used to filter your own visible tools down to just the ones relevant to that task — be specific enough that unrelated tools can be told apart from relevant ones."`
}

// findRelevantToolsResult wraps find_relevant_tools' list in a named
// field rather than returning a bare array: some MCP clients (e.g. Claude
// Desktop) require every tool's inferred outputSchema to be a JSON object
// at the top level, and a bare slice Out infers as an array instead.
// Tools is []map[string]any, not []mcp.Tool: mcp.Tool's own InputSchema/
// OutputSchema fields are themselves declared `any` in the go-sdk, and a
// bare `any` struct field infers as the JSON Schema boolean-shorthand
// `true` rather than an object once jsonschema-go recurses into it -
// valid per the JSON Schema spec, but rejected by at least one real MCP
// client (Claude Code CLI's own stricter schema validator). See
// ai-mcp-servers/ai-powered-tools/main.go's matching
// toolsFromMaps/mapsFromTools for the same fix, applied there for
// _filter_tool's identical shape.
type findRelevantToolsResult struct {
	Tools []map[string]any `json:"tools"`
}

// mapsFromTools converts real mcp.Tool values into their map[string]any
// wire representation - see findRelevantToolsResult's own doc comment
// for why this tool's Output is declared that way instead of []mcp.Tool
// directly.
func mapsFromTools(tools []mcp.Tool) ([]map[string]any, error) {
	maps := make([]map[string]any, len(tools))
	for i, t := range tools {
		raw, err := json.Marshal(t)
		if err != nil {
			return nil, fmt.Errorf("tool %d (%s): %w", i, t.Name, err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("tool %d (%s): %w", i, t.Name, err)
		}
		maps[i] = m
	}
	return maps, nil
}

// mapFromValue is mapsFromTools' single-value counterpart - round-trips v
// through its own JSON marshaling (respecting a custom MarshalJSON, e.g.
// *mcp.GetPromptResult's) into a map[string]any, for the same reason
// mapsFromTools does: a concrete SDK type with an interface-typed field
// (e.g. PromptMessage.Content) infers as an unconstrained schema instead
// of a proper JSON object.
func mapFromValue(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// getMyIDDetailsArgs is proxy_utility.get_my_id_details' input: none - it
// only ever reports the caller's own identity, never a caller-chosen one.
type getMyIDDetailsArgs struct{}

// getMyIDDetailsResult is proxy_utility.get_my_id_details' output.
type getMyIDDetailsResult struct {
	ID    string   `json:"id" jsonschema:"The caller's own subject (JWT \"sub\" claim)."`
	Roles []string `json:"roles" jsonschema:"The caller's own roles, normalized the same way access_control.RolesFromClaims normalizes them for every other authorization check (handles both a single-string and array-shaped role claim, already accounting for the configured roleClaim remapping)."`
}

// callToolArgs is proxy_utility.call_tool's input: the namespaced name of
// the tool to call and the arguments to call it with.
type callToolArgs struct {
	Tool      string         `json:"tool" jsonschema:"The namespaced name of the tool to call, e.g. \"customer-service.say_hello\"."`
	Arguments map[string]any `json:"arguments,omitempty" jsonschema:"Arguments to pass to the tool, in the shape that tool's own input schema expects."`
}

// renderPromptArgs is proxy_utility.render_prompt's input - the same
// {Name, Arguments} shape a real prompts/get call takes (mcp.
// GetPromptParams), Arguments being map[string]string per the MCP spec
// (prompt arguments are always strings, unlike a tool's arbitrary-shaped
// arguments).
type renderPromptArgs struct {
	Name      string            `json:"name" jsonschema:"The namespaced name of the prompt to render, e.g. \"garden-online-store.product_on_sale_for_me\"."`
	Arguments map[string]string `json:"arguments,omitempty" jsonschema:"String arguments to substitute into the prompt, in the shape that prompt's own arguments expect."`
}

// renderPromptResult is proxy_utility.render_prompt's output - see its
// registration site for why Prompt is map[string]any rather than
// *mcp.GetPromptResult directly.
type renderPromptResult struct {
	Prompt map[string]any `json:"prompt"`
}

// getPromptsArgs is proxy_utility.get_prompts' input.
type getPromptsArgs struct {
	NamePattern string `json:"namePattern,omitempty" jsonschema:"A glob pattern (access_control.GlobMatch semantics — \"*\" matches any characters) matched against each prompt's namespaced name, e.g. \"garden-online-store.*\" or \"*rake*\". Omit to return every prompt."`
}

// getPromptsResult is proxy_utility.get_prompts' output. Wrapped in a
// named field rather than returning a bare array — same reasoning as
// findRelevantToolsResult above.
type getPromptsResult struct {
	Prompts []*mcp.Prompt `json:"prompts"`
}

// listTriggeredToolsArgs is proxy_utility.list_triggered_tools' input.
type listTriggeredToolsArgs struct {
	Verbose bool `json:"verbose,omitempty" jsonschema:"When true, return the full Tool object for each triggered tool; otherwise (the default) return just its namespaced name."`
}

// loopbackHeaders returns the relay headers proxy_utility.call_tool/
// render_prompt present when calling back into the proxy as the caller of
// the current request: a loopback token (auth.LoopbackAudience) and the
// proxy's own address. The token is only ever sent to selfURL, never to a
// backend, which is why it keeps the caller's full authority (including
// _admin.* for an administrator) - unless the current request is itself
// part of a backend's relay, whose restrictions the token carries along
// (auth.ClaimRelayFrom). Its hop count and expiry come from the current
// request, so nesting call_tool is bounded by internalAuth.maxRelayHops.
func loopbackHeaders(ctx context.Context) (relayheaders.Set, error) {
	if internalSigner == nil || selfURL == "" {
		return relayheaders.Set{}, fmt.Errorf("calling back into the proxy needs internalAuth and selfUrl configured")
	}
	id, ok := callerIdentityFromContext(ctx)
	if !ok {
		return relayheaders.Set{}, fmt.Errorf("no verified caller identity on this request")
	}
	if id.Hop > maxRelayHops() {
		return relayheaders.Set{}, fmt.Errorf("too many nested calls (at most %d)", maxRelayHops())
	}
	token, err := internalSigner.SignLoopback(id)
	if err != nil {
		return relayheaders.Set{}, fmt.Errorf("minting loopback token: %w", err)
	}
	return relayheaders.Set{
		RelayToken:  "Bearer " + token,
		CallbackURL: selfURL,
		ToolCallID:  id.ToolCallID,
	}, nil
}
