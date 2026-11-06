// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/pellucon/enterprise-go-mcp-proxy/access_control"
	"github.com/pellucon/enterprise-go-mcp-proxy/auth"
	"github.com/pellucon/enterprise-go-mcp-proxy/discover_mcp_servers"
	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/events"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/declarative-guardrail/guardrailcfg"
	"github.com/pellucon/enterprise-go-mcp-proxy/platform-mcp-servers/rate-limiter/ratelimit"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"
)

// proxyConfigFile is the unexported wire envelope for the combined
// proxy-server.cfg file: discovery config, access rules, auth settings,
// orchestration config, event dictionary, and declarative-guardrails
// config, all in one JSON document. Each section is kept as
// json.RawMessage so it can be handed off to each package's own ParseConfig
// (or, for auth, unmarshaled directly) without this file needing to know
// their internal wire shapes.
type proxyConfigFile struct {
	Discovery    json.RawMessage `json:"discovery"`
	AccessRules  json.RawMessage `json:"accessRules"`
	Auth         json.RawMessage `json:"auth,omitempty"`
	InternalAuth json.RawMessage `json:"internalAuth,omitempty"`
	// The file's own JSON key is "governanceOrchestration", not
	// "orchestration" — a deliberate rename of just this top-level key;
	// the section's own internal shape (orchestrations/toolMappings, and
	// toolMappings' own per-entry "orchestration" field naming which
	// named orchestration a tool pattern maps to) is unchanged, and so is
	// every other place this feature is still called "orchestration"
	// internally (the configsync section name "orchestration" used for
	// the orchestration-override ConfigMap and the
	// _admin.update_orchestration/export_orchestration admin tools —
	// those exchange OrchestrationConfig directly, with no outer wrapper
	// key at all, so they're unaffected by this rename either way).
	Orchestration json.RawMessage `json:"governanceOrchestration,omitempty"`
	// DeclarativeGuardrails is never consumed by the proxy itself (see
	// CLAUDE.md's "Unify configuration into a single config-map
	// mechanism") - it exists here purely so proxy-server.cfg remains the
	// single source of truth deploy.sh seeds every "<section>-override"
	// ConfigMap from, and so _admin.export/update_declarative_guardrails
	// has something to serve on a freshly-started, never-yet-admin-updated
	// proxy. EventDictionary is the same kind of seed source, but the
	// proxy also reads it back itself (via eventsClient/its own Watcher in
	// main.go's doRunOn) to publish its own events. Both are always
	// present, like Discovery/AccessRules above - not pointer-nil-optional
	// like Auth/InternalAuth/Orchestration, since an absent section here
	// would silently leave 2 of the 5 config-map sections permanently
	// unseedable standalone.
	EventDictionary       json.RawMessage `json:"eventDictionary"`
	DeclarativeGuardrails json.RawMessage `json:"declarativeGuardrails"`

	// EventAggregation is event-aggregator's ingestion filter and tiered-
	// storage thresholds (see events.AggregationConfig). Optional, like
	// Orchestration above: nil means the section is absent, and
	// event-aggregator's own process falls back to its bundled local
	// default (event_aggregation_test.json) rather than anything from
	// this file — go-proxy is only ever this section's admin/write
	// surface, never a consumer itself, same as DeclarativeGuardrails.
	EventAggregation json.RawMessage `json:"eventAggregation,omitempty"`

	// RateLimiting configures per-tool, per-role call-rate limits, enforced
	// by the separate rate-limiter platform server; go-proxy itself only
	// reads which tool names are present (to decide whether to request
	// permission at all) and TimeoutSeconds (the deadline it applies to that
	// request) — see rate_limit_gate.go. Optional, like Orchestration above:
	// nil means the section is absent, and no tool is ever checked.
	RateLimiting json.RawMessage `json:"rateLimiting,omitempty"`

	// LogLevel configures the process-wide runtime log severity
	// (error/info/debug — see the logging package). Optional, like
	// Orchestration above: nil means the section is absent, and the
	// process simply keeps whatever level logging.Init was last given
	// (or the package's own Info-level zero-value default if never set).
	LogLevel json.RawMessage `json:"logLevel,omitempty"`

	// SelfURL is the proxy's own callable MCP endpoint address (e.g.
	// "http://go-proxy.mcp-proxy.svc.cluster.local/mcp" in k8s,
	// "http://localhost:8081/mcp" standalone), handed to every backend so
	// a tool can call back into the proxy, relaying a call as the original
	// client (see CLAUDE.md's "Internal identity assertions" section). A
	// plain string, not a sub-object, so unlike the other sections above
	// it's read directly rather than via RawMessage. Empty means "not
	// configured": the callback-URL header is simply never sent.
	SelfURL string `json:"selfUrl,omitempty"`

	// NatsURL is the NATS server URL configsync uses for change-
	// notification pings (see configsync.Connect) and go-proxy's own
	// eventsClient uses to publish events - a plain string, like SelfURL,
	// not a sub-object. Required: doRunOn fails startup if this is empty.
	// See CLAUDE.md's "NATS event system" section for why NATS is a hard
	// dependency and how readiness (not liveness) is gated on it via
	// /healthz.
	NatsURL string `json:"natsUrl,omitempty"`

	// RoleMapping maps the gateway's built-in logical roles (today only
	// "administrator", the role allowed to call _admin.* tools) to the
	// role names this deployment's IdP actually issues — see
	// access_control.RoleMapping. Startup-only, like auth/selfUrl/natsUrl:
	// never configsync-managed, so no admin tool can change it. Required:
	// loadProxyConfig fails if it's absent or names no administrator.
	RoleMapping json.RawMessage `json:"roleMapping,omitempty"`
}

// ProxyConfig is the fully-parsed, in-memory combined configuration.
// Auth, InternalAuth, and Orchestration are pointers: nil means that
// section was absent/empty from the file, distinguishing "no section
// given" (callers should leave an existing default alone, or treat the
// feature as a total no-op) from "section present" (callers should use it,
// even if some of its fields are empty/zero).
type ProxyConfig struct {
	Discovery             *discover_mcp_servers.MCPDiscoveryConfig
	AccessRules           *access_control.AccessConfig
	Auth                  *auth.Config
	InternalAuth          *auth.SignerConfig
	Orchestration         *triggered_tools_runtime.OrchestrationConfig
	EventDictionary       *events.Dictionary
	DeclarativeGuardrails *guardrailcfg.RawConfig
	EventAggregation      *events.AggregationConfig
	RateLimiting          *ratelimit.Config
	LogLevel              *logging.LevelConfig
	SelfURL               string
	NatsURL               string
	RoleMapping           access_control.RoleMapping
}

// loadProxyConfig reads and parses the combined config file at path,
// delegating each section to its owning package's parser.
func loadProxyConfig(path string) (*ProxyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var raw proxyConfigFile
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("invalid json format: %w", err)
	}

	discoveryCfg, err := discover_mcp_servers.ParseConfig(raw.Discovery)
	if err != nil {
		return nil, fmt.Errorf("failed to parse \"discovery\" section: %w", err)
	}

	accessCfg, err := access_control.ParseConfig(raw.AccessRules)
	if err != nil {
		return nil, fmt.Errorf("failed to parse \"accessRules\" section: %w", err)
	}

	var authCfg *auth.Config
	if len(raw.Auth) > 0 {
		authCfg = &auth.Config{}
		if err := json.Unmarshal(raw.Auth, authCfg); err != nil {
			return nil, fmt.Errorf("failed to parse \"auth\" section: %w", err)
		}
	}

	var internalAuthCfg *auth.SignerConfig
	if len(raw.InternalAuth) > 0 {
		internalAuthCfg = &auth.SignerConfig{}
		if err := json.Unmarshal(raw.InternalAuth, internalAuthCfg); err != nil {
			return nil, fmt.Errorf("failed to parse \"internalAuth\" section: %w", err)
		}
		if err := validateInternalAuth(internalAuthCfg); err != nil {
			return nil, fmt.Errorf("invalid \"internalAuth\" section: %w", err)
		}
	}

	var orchestrationCfg *triggered_tools_runtime.OrchestrationConfig
	if len(raw.Orchestration) > 0 {
		orchestrationCfg, err = triggered_tools_runtime.ParseConfig(raw.Orchestration)
		if err != nil {
			return nil, fmt.Errorf("failed to parse \"governanceOrchestration\" section: %w", err)
		}
	}

	eventDictionaryCfg, err := events.ParseDictionary(raw.EventDictionary)
	if err != nil {
		return nil, fmt.Errorf("failed to parse \"eventDictionary\" section: %w", err)
	}

	// ParseRawConfig, not ParseConfig: go-proxy is only ever this section's
	// admin surface (see CLAUDE.md's "Unify configuration into a single
	// config-map mechanism") - it persists the ConfigMap via
	// configsync.Writer[guardrailcfg.RawConfig] but never evaluates a
	// guardrail itself, so it needs the plain JSON-roundtrippable shape,
	// not Guardrails' unexported, evaluation-ready compiled form (see
	// ParseRawConfig's doc comment for the export/update bug that shape
	// caused).
	declarativeGuardrailsCfg, err := guardrailcfg.ParseRawConfig(raw.DeclarativeGuardrails)
	if err != nil {
		return nil, fmt.Errorf("failed to parse \"declarativeGuardrails\" section: %w", err)
	}

	var eventAggregationCfg *events.AggregationConfig
	if len(raw.EventAggregation) > 0 {
		eventAggregationCfg, err = events.ParseAggregationConfig(raw.EventAggregation)
		if err != nil {
			return nil, fmt.Errorf("failed to parse \"eventAggregation\" section: %w", err)
		}
	}

	var rateLimitingCfg *ratelimit.Config
	if len(raw.RateLimiting) > 0 {
		rateLimitingCfg, err = ratelimit.ParseConfig(raw.RateLimiting)
		if err != nil {
			return nil, fmt.Errorf("failed to parse \"rateLimiting\" section: %w", err)
		}
	}

	var logLevelCfg *logging.LevelConfig
	if len(raw.LogLevel) > 0 {
		logLevelCfg, err = logging.ParseConfig(raw.LogLevel)
		if err != nil {
			return nil, fmt.Errorf("failed to parse \"logLevel\" section: %w", err)
		}
	}

	roleMappingCfg, err := access_control.ParseRoleMapping(raw.RoleMapping)
	if err != nil {
		return nil, fmt.Errorf("failed to parse \"roleMapping\" section: %w", err)
	}

	return &ProxyConfig{
		Discovery:             discoveryCfg,
		AccessRules:           accessCfg,
		Auth:                  authCfg,
		InternalAuth:          internalAuthCfg,
		Orchestration:         orchestrationCfg,
		EventDictionary:       eventDictionaryCfg,
		DeclarativeGuardrails: declarativeGuardrailsCfg,
		EventAggregation:      eventAggregationCfg,
		RateLimiting:          rateLimitingCfg,
		LogLevel:              logLevelCfg,
		SelfURL:               raw.SelfURL,
		NatsURL:               raw.NatsURL,
		RoleMapping:           roleMappingCfg,
	}, nil
}

// validateInternalAuth checks the internalAuth section's relay settings.
// It's startup-only (not a configsync section), so the _admin.* tools can
// never widen who may relay; a mistake here fails startup rather than
// being silently ignored.
// maxInternalTTLSeconds bounds internalAuth.ttlSeconds.
const maxInternalTTLSeconds = 3600

func validateInternalAuth(cfg *auth.SignerConfig) error {
	switch cfg.Audience {
	case auth.RelayAudience, auth.LoopbackAudience:
		return fmt.Errorf("audience %q is reserved for go-proxy's relay/loopback tokens; the identity assertion needs a different one", cfg.Audience)
	}
	if cfg.MaxRelayHops < 0 {
		return fmt.Errorf("maxRelayHops must not be negative")
	}
	// Minted tokens are short-lived on purpose: a leaked assertion or relay
	// token is usable until it expires.
	if cfg.TTLSeconds < 0 || cfg.TTLSeconds > maxInternalTTLSeconds {
		return fmt.Errorf("ttlSeconds must be between 1 and %d (0 for the default)", maxInternalTTLSeconds)
	}
	for server, targets := range cfg.Relay {
		if err := discover_mcp_servers.ValidateServerName(server); err != nil {
			return fmt.Errorf("relay: %w", err)
		}
		if len(targets) == 0 {
			return fmt.Errorf("relay[%q]: list at least one target, or remove the entry", server)
		}
		for _, target := range targets {
			if target == "" {
				return fmt.Errorf("relay[%q]: empty target", server)
			}
			if strings.HasPrefix(target, dynamic_proxy.ReservedServerName+".") {
				return fmt.Errorf("relay[%q]: %q: relayed calls can never reach %s.* tools", server, target, dynamic_proxy.ReservedServerName)
			}
		}
	}
	return nil
}
