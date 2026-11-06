// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerAdminTools adds the proxy's own native, in-process tools (never
// forwarded to any backend) directly to the gateway's *mcp.Server. All
// fifteen tools are authorized solely via access_control.AccessPolicyEngine's
// hardcoded "_admin." admin-only check, enforced by createMiddleware
// before any handler below ever runs — none of them re-derive or re-check
// the caller's role itself.
//
// One update/export pair per configsync-managed section, all built on the
// single generic registerSectionAdminTools below — following this repo's
// existing dynamic_proxy.Gateway.genericRegistry[T] idiom (CLAUDE.md: "one
// genericRegistry[T] instance per kind" over a runtime loop of interfaces)
// rather than 14 hand-copied blocks — except discovery, which needs its own
// dedicated registerDiscoveryAdminTools instead of the generic helper (see
// its own doc comment) — plus one more, export_full_config, which isn't a
// per-section pair at all (see registerExportFullConfigTool).
func registerAdminTools(server *mcp.Server) {
	// Reset, not append-only: runProxyServerOn (and so registerAdminTools/
	// registerUtilityTools) runs once per proxy instance, including once
	// per test in this repo's own test binary (doRunOn is called
	// repeatedly in the same process) — without this, nativeTools would
	// keep accumulating duplicate entries from every previous instance
	// across the life of the process.
	nativeTools = nil
	registerSectionAdminTools(server, "role_permissions", "role-based access control rule set", accessRulesWriter, validateWith(access_control.ParseConfig), accessPolicyEngine.Update, nil)
	registerSectionAdminTools(server, "orchestration", "tool-call orchestration configuration", orchestrationWriter, validateOrchestrationUpdate, triggeredRuntime.Update, nil)
	registerDiscoveryAdminTools(server, discoveryWriter, discoveryWatcher.SetLocal)

	// eventsClient.SetDictionary only matters for the replica that served
	// the _admin.update_event_dictionary call; every other replica's own
	// Watcher (see doRunOn) still picks it up over its usual NATS round
	// trip.
	registerSectionAdminTools(server, "event_dictionary", "event dictionary", eventDictionaryWriter, validateWith(events.ParseDictionary), eventsClient.SetDictionary, nil)

	// confirmDeclarativeGuardrailsLoaded: the update only returns once the
	// declarative-guardrail server reports the new version, so a following
	// orchestration update can safely reference newly added declarations.
	registerSectionAdminTools(server, "declarative_guardrails", "declarative-guardrails configuration", declarativeGuardrailsWriter, validateWith(guardrailcfg.ParseConfig), noApply[guardrailcfg.RawConfig], confirmDeclarativeGuardrailsLoaded)

	// event-aggregator, not the proxy itself, consumes this section (via
	// its own configsync.Watcher) — noApply here for the same reason as
	// declarative_guardrails above.
	registerSectionAdminTools(server, "event_aggregation", "event-aggregation configuration", eventAggregationWriter, validateWith(events.ParseAggregationConfig), noApply[events.AggregationConfig], nil)

	// rateLimitingWatcher.SetLocal, exactly like discovery above: no
	// separate downstream engine on the proxy side, just a config snapshot
	// rate_limit_gate.go's checkRateLimit reads directly.
	registerSectionAdminTools(server, "rate_limiting", "rate limiting configuration", rateLimitingWriter, validateWith(ratelimit.ParseConfig), rateLimitingWatcher.SetLocal, nil)

	// logging.ApplyConfig IS the downstream engine here (the package-level
	// slog.LevelVar) — same "apply directly, no separate engine object"
	// shape as eventsClient.SetDictionary above.
	registerSectionAdminTools(server, "log_level", "runtime log level (error/info/debug)", logLevelWriter, validateWith(logging.ParseConfig), logging.ApplyConfig, nil)

	registerExportFullConfigTool(server)
}

// fullConfigExport is proxy-server.cfg's own shape (see proxyConfigFile),
// but populated from each section's live current value rather than parsed
// from a file — deliberately without any VersionedConfig[T] envelope, so
// the JSON this produces is byte-for-byte a valid combined config file, not
// something that needs unwrapping first. InternalAuth is a pointer, like
// ProxyConfig's own, so an unconfigured deployment round-trips back to
// "absent" rather than a zero-valued section.
type fullConfigExport struct {
	// Discovery is `map[string]any`, not discover_mcp_servers.
	// MCPDiscoveryConfig directly, for the same reason triggered-tools-
	// runtime/contract.go's CallRequest/CallResult fields aren't typed
	// directly either: MCPDiscoveryConfig's MarshalJSON produces the real
	// discoveryMode/servers/... wire shape (see its doc comment), but the
	// MCP SDK's output-schema inference (jsonschema.For) derives a tool's
	// output schema from pure struct-field reflection - it never invokes
	// MarshalJSON - so a directly-typed field here would get a schema
	// demanding MCPDiscoveryConfig's raw Go field names (Mode, ServerMap,
	// ...) and then reject its own correct output as unexpected
	// properties. A bare `any` would also infer schema-unconstrained, but
	// as the literal JSON Schema boolean-shorthand `true` at this
	// property's position (jsonschema-go's documented behavior for `any`)
	// - valid per the JSON Schema spec, but rejected by at least one real
	// MCP client (Claude Code CLI's own stricter schema validator, which
	// requires every schema position to be an object). `map[string]any`
	// instead infers as `{"type":"object","additionalProperties":true}` -
	// still fully unconstrained, but an actual object schema. The
	// registerExportFullConfigTool handler below round-trips
	// discovery.Config through json.Marshal/Unmarshal into this shape
	// (MarshalJSON still runs during that Marshal), rather than assigning
	// it directly, since a struct value isn't assignable to a
	// map[string]any-typed field.
	Discovery             map[string]any                              `json:"discovery"`
	AccessRules           access_control.AccessConfig                 `json:"accessRules"`
	Auth                  auth.Config                                 `json:"auth"`
	InternalAuth          *auth.SignerConfig                          `json:"internalAuth,omitempty"`
	Orchestration         triggered_tools_runtime.OrchestrationConfig `json:"governanceOrchestration"`
	EventDictionary       events.Dictionary                           `json:"eventDictionary"`
	DeclarativeGuardrails guardrailcfg.RawConfig                      `json:"declarativeGuardrails"`
	EventAggregation      events.AggregationConfig                    `json:"eventAggregation"`
	RateLimiting          ratelimit.Config                            `json:"rateLimiting"`
	LogLevel              logging.LevelConfig                         `json:"logLevel,omitempty"`
	SelfURL               string                                      `json:"selfUrl,omitempty"`
	NatsURL               string                                      `json:"natsUrl,omitempty"`
	RoleMapping           access_control.RoleMapping                  `json:"roleMapping"`
}

// toObjectMap round-trips v through JSON into a map[string]any - used to
// populate fullConfigExport.Discovery (see its own doc comment for why
// that field isn't typed directly as v's own type).
func toObjectMap(v any) (map[string]any, error) {
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

// registerDiscoveryAdminTools is a special-cased alternative to
// registerSectionAdminTools, needed only for "discovery":
// discover_mcp_servers.MCPDiscoveryConfig has no JSON tags on its own
// fields and a custom MarshalJSON that reroutes through a differently-named
// internal wire shape (discoveryMode/servers/listedTools/... — see its own
// doc comment). jsonschema.For's output-schema inference reflects on the
// struct's raw (untagged) field names, never invoking MarshalJSON, so a
// directly-typed configsync.VersionedConfig[MCPDiscoveryConfig] tool
// produces a schema that the type's own real, MarshalJSON-produced output
// never satisfies — the tool rejects its own correct result as "unexpected
// additional properties". (OrchestrationConfig also has a custom
// MarshalJSON, but its fields carry the SAME json tags that MarshalJSON
// marshals through, so schema inference and real output agree there — this
// problem is specific to MCPDiscoveryConfig's particular history, not a
// pattern shared by other sections.)
//
// The fix mirrors registerExportFullConfigTool's own Discovery field: round
// trip through map[string]any, which infers as an unconstrained object
// schema, on both sides — export via toObjectMap (same helper), update by
// re-marshaling the incoming map back to JSON bytes and handing it to
// discover_mcp_servers.ParseConfig, the same parser proxy-server.cfg's own
// "discovery" section goes through.
func registerDiscoveryAdminTools(server *mcp.Server, writer *configsync.Writer[discover_mcp_servers.MCPDiscoveryConfig], apply func(*discover_mcp_servers.MCPDiscoveryConfig)) {
	const name = "discovery"
	const description = "backend server discovery configuration"

	updateTool := &mcp.Tool{
		Name:        dynamic_proxy.ReservedServerName + ".update_" + name,
		Description: fmt.Sprintf("Replace the gateway's entire %s (admin only). Takes the exact envelope export_%s returns - a version mismatch against the config map's actual current version is rejected.", description, name),
	}
	nativeTools = append(nativeTools, updateTool)
	mcp.AddTool(server, updateTool, func(ctx context.Context, req *mcp.CallToolRequest, input configsync.VersionedConfig[map[string]any]) (*mcp.CallToolResult, any, error) {
		raw, err := json.Marshal(input.Config)
		if err != nil {
			return nil, nil, fmt.Errorf("re-encoding discovery config: %w", err)
		}
		parsed, err := discover_mcp_servers.ParseConfig(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid discovery config: %w", err)
		}
		// Dropping a pin go-proxy depends on would make discovery refuse
		// that server, so refuse the update instead (see pinning.go).
		if err := discover_mcp_servers.UnpinnedNames(parsed, currentMustPin()); err != nil {
			return nil, nil, fmt.Errorf("invalid discovery config: %w", err)
		}
		out, err := writer.Update(ctx, configsync.VersionedConfig[discover_mcp_servers.MCPDiscoveryConfig]{Version: input.Version, Config: *parsed})
		if err != nil {
			return nil, nil, err
		}
		apply(&out.Config)
		text := fmt.Sprintf("%s updated to version %d.", description, out.Version)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})

	exportTool := &mcp.Tool{
		Name:        dynamic_proxy.ReservedServerName + ".export_" + name,
		Description: fmt.Sprintf("Export the gateway's current %s, with its version, as JSON (admin only).", description),
	}
	nativeTools = append(nativeTools, exportTool)
	mcp.AddTool(server, exportTool, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, configsync.VersionedConfig[map[string]any], error) {
		out, err := writer.Export(ctx)
		if err != nil {
			return nil, configsync.VersionedConfig[map[string]any]{}, err
		}
		m, err := toObjectMap(out.Config)
		if err != nil {
			return nil, configsync.VersionedConfig[map[string]any]{}, fmt.Errorf("re-encoding discovery config: %w", err)
		}
		return nil, configsync.VersionedConfig[map[string]any]{Version: out.Version, Config: m}, nil
	})
}

// registerExportFullConfigTool adds _admin.export_full_config: unlike
// registerSectionAdminTools' pairs, this is read-only (no matching
// "update_full_config" — round-tripping every section's version-checked
// update atomically together isn't something this tool attempts) and
// spans all 5 configsync-managed sections plus the 4 that are never
// admin-tool-managed (auth/internalAuth/selfUrl/natsUrl, loaded once at
// startup and never changed at runtime — see CLAUDE.md's "Unify
// configuration into a single config-map mechanism" for why those stay
// out of configsync). The result is meant to be saved as a fresh
// proxy-server.cfg: deploying with it reproduces the exact configuration a
// running gateway currently has, including every admin-tool change made
// since its own last deploy — just with each section's version reset to 1
// on next seed, since versions aren't part of the exported document.
func registerExportFullConfigTool(server *mcp.Server) {
	tool := &mcp.Tool{
		Name:        dynamic_proxy.ReservedServerName + ".export_full_config",
		Description: "Export the gateway's entire current effective configuration (every section, without per-section version numbers) as a single proxy-server.cfg-shaped JSON document (admin only). Deploying with this file reproduces the same configuration.",
	}
	nativeTools = append(nativeTools, tool)
	mcp.AddTool(server, tool, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, fullConfigExport, error) {
		discovery, err := discoveryWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting discovery: %w", err)
		}
		accessRules, err := accessRulesWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting role permissions: %w", err)
		}
		orchestration, err := orchestrationWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting orchestration: %w", err)
		}
		eventDictionary, err := eventDictionaryWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting event dictionary: %w", err)
		}
		declarativeGuardrails, err := declarativeGuardrailsWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting declarative guardrails: %w", err)
		}
		eventAggregation, err := eventAggregationWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting event aggregation: %w", err)
		}
		rateLimiting, err := rateLimitingWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting rate limiting: %w", err)
		}
		logLevel, err := logLevelWriter.Export(ctx)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("exporting log level: %w", err)
		}
		discoveryMap, err := toObjectMap(discovery.Config)
		if err != nil {
			return nil, fullConfigExport{}, fmt.Errorf("re-encoding discovery: %w", err)
		}

		return nil, fullConfigExport{
			Discovery:             discoveryMap,
			AccessRules:           accessRules.Config,
			Auth:                  authConfig,
			InternalAuth:          internalAuthConfig,
			Orchestration:         orchestration.Config,
			EventDictionary:       eventDictionary.Config,
			DeclarativeGuardrails: declarativeGuardrails.Config,
			EventAggregation:      eventAggregation.Config,
			RateLimiting:          rateLimiting.Config,
			LogLevel:              logLevel.Config,
			SelfURL:               selfURL,
			NatsURL:               natsURLConfig,
			RoleMapping:           roleMappingConfig,
		}, nil
	})
}

// validateWith adapts a section's parser to registerSectionAdminTools'
// validate: only whether it parses matters, not the parsed value.
func validateWith[T any](parse func([]byte) (T, error)) func([]byte) error {
	return func(raw []byte) error {
		_, err := parse(raw)
		return err
	}
}

// noApply is the "no downstream engine to notify" apply for
// declarative_guardrails — the proxy never consumes declarativeGuardrails
// itself, it's purely the write/admin side for its backend consumer. A
// generic function value (rather than passing a bare nil) so Go can infer
// registerSectionAdminTools' T from it the same way it infers T from every
// other call site's real apply function.
func noApply[T any](*T) {}

// registerSectionAdminTools registers the update_<name>/export_<name> tool
// pair for one configsync-managed section. Both tools exchange a
// configsync.VersionedConfig[T] envelope (version + config together), not a
// bare T — export_<name> returns it, and update_<name> takes it back
// unmodified except for edits to Config, so a version mismatch (another
// replica or another admin call updated the section since this caller last
// exported it) is rejected by writer.Update with a clear error, not
// silently overwritten. See configsync.Writer.Update's doc comment for the
// optimistic-concurrency mechanics.
//
// apply is invoked with the newly-written config immediately after a
// successful update, so the replica that served the call reflects it right
// away rather than waiting for its own NATS round trip — this is
// access_control.AccessPolicyEngine.Update / triggered_tools_runtime.Runtime.
// Update / configsync.Watcher.SetLocal depending on the section (see
// registerAdminTools above), or noApply for event_dictionary/
// declarative_guardrails, which the proxy never consumes itself.
//
// validate runs before anything is written, on the update's config
// re-encoded as JSON: the same parser the section's Watcher (or, for a
// section only a backend consumes, that backend) loads it with. An update
// that wouldn't load is refused - otherwise it could weaken the replica
// serving the call (apply gets whatever was written) and leave every
// replica unable to start, since an override ConfigMap always wins over
// the cfg.
//
// confirm (may be nil) runs after apply, for a section a separate backend
// consumes: it may block until that backend reports the new version, and
// returns a sentence appended to the result text (see
// confirmDeclarativeGuardrailsLoaded). It can't fail the call - the write
// has already happened.
func registerSectionAdminTools[T any](server *mcp.Server, name, description string, writer *configsync.Writer[T], validate func([]byte) error, apply func(*T), confirm func(ctx context.Context, version int64) string) {
	updateTool := &mcp.Tool{
		Name:        dynamic_proxy.ReservedServerName + ".update_" + name,
		Description: fmt.Sprintf("Replace the gateway's entire %s (admin only). Takes the exact envelope export_%s returns - a version mismatch against the config map's actual current version is rejected.", description, name),
	}
	nativeTools = append(nativeTools, updateTool)
	mcp.AddTool(server, updateTool, func(ctx context.Context, req *mcp.CallToolRequest, input configsync.VersionedConfig[T]) (*mcp.CallToolResult, any, error) {
		raw, err := json.Marshal(input.Config)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid %s: %w", description, err)
		}
		if err := validate(raw); err != nil {
			return nil, nil, fmt.Errorf("invalid %s, not applied: %w", description, err)
		}
		out, err := writer.Update(ctx, input)
		if err != nil {
			return nil, nil, err
		}
		apply(&out.Config)
		text := fmt.Sprintf("%s updated to version %d.", description, out.Version)
		if confirm != nil {
			text += confirm(ctx, out.Version)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: text}},
		}, nil, nil
	})

	exportTool := &mcp.Tool{
		Name:        dynamic_proxy.ReservedServerName + ".export_" + name,
		Description: fmt.Sprintf("Export the gateway's current %s, with its version, as JSON (admin only).", description),
	}
	nativeTools = append(nativeTools, exportTool)
	mcp.AddTool(server, exportTool, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, configsync.VersionedConfig[T], error) {
		out, err := writer.Export(ctx)
		if err != nil {
			return nil, configsync.VersionedConfig[T]{}, err
		}
		return nil, out, nil
	})
}
