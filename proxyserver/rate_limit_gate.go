// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/dynamic_proxy"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	triggered_tools_runtime "github.com/pellucon/enterprise-go-mcp-proxy/triggered-tools-runtime"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rateLimiterServerName/checkPermissionToolName name the platform server and
// triggered tool checkRateLimit calls out to (platform-mcp-servers/
// rate-limiter). Not configurable: exactly one such server is meaningful per
// deployment (see its own doc comment on the single-pod requirement).
const (
	rateLimiterServerName   = "rate-limiter"
	checkPermissionToolName = "check_permission"
)

// checkPermissionArgs mirrors rate-limiter's own CheckPermissionArgs —
// duplicated rather than imported, the same "package main can't import
// another package main" constraint as everywhere else in this repo.
//
// TimeoutSeconds is passed explicitly, not left for the server to infer from
// its own inbound ctx: a context.WithTimeout deadline is a local, in-process
// value that does not travel over the wire — verified directly, not
// assumed, by TestPlatformServersOrchestration's rate-limiting scenarios,
// which found the server's own ctx carries no deadline at all. The server
// must be told explicitly how long the caller is willing to wait so it can
// bound its own poll loop independently.
type checkPermissionArgs struct {
	Tool           string   `json:"tool"`
	Roles          []string `json:"roles"`
	TimeoutSeconds *int     `json:"timeoutSeconds,omitempty"`
}

// checkPermissionResult mirrors rate-limiter's ratelimit.Decision — only the
// one field go-proxy actually needs is decoded.
type checkPermissionResult struct {
	Allowed bool `json:"allowed"`
}

// checkRateLimit requests permission to call toolName on behalf of a caller
// holding roles, for every tool present in the "rateLimiting" config
// section — tools absent from that section never generate a request at
// all. Fails closed: any error (the rate-limiter server unreachable, the
// permission check itself denied, or a wait that hit its configured
// timeout) denies the underlying call, consistent with every other
// guardrail-reachability failure mode in this codebase.
func checkRateLimit(ctx context.Context, gateway *dynamic_proxy.Gateway, toolName string, roles []string) error {
	cfg := rateLimitingWatcher.Current()
	if cfg == nil {
		return nil // rateLimiting section entirely absent -> total no-op
	}
	if _, ok := cfg.Tools[toolName]; !ok {
		return nil // not configured for rate limiting -> no request sent at all
	}

	if tool, found := gateway.Tool(toolName); found && triggered_tools_runtime.IsTriggeredTool(tool) {
		logging.Errorf("rate limiting: tool %q is a triggered tool and cannot be rate limited; ignoring its rateLimiting config entry", toolName)
		return nil
	}

	sess, err := triggeredRuntime.SessionFor(ctx, rateLimiterServerName)
	if err != nil {
		return fmt.Errorf("rate limiter unavailable: %w", err)
	}

	callCtx := ctx
	if cfg.TimeoutSeconds != nil {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, time.Duration(*cfg.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	res, err := sess.CallTool(callCtx, &mcp.CallToolParams{
		Name:      checkPermissionToolName,
		Arguments: checkPermissionArgs{Tool: toolName, Roles: roles, TimeoutSeconds: cfg.TimeoutSeconds},
	})
	if err != nil {
		return fmt.Errorf("rate limit check failed: %w", err) // includes deadline-exceeded -> fail closed
	}
	if res.IsError {
		return fmt.Errorf("rate limit check rejected the call: %s", toolErrorText(res.Content))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return fmt.Errorf("failed to re-marshal check_permission's StructuredContent: %w", err)
	}
	var decision checkPermissionResult
	if err := json.Unmarshal(raw, &decision); err != nil {
		return fmt.Errorf("failed to decode check_permission output: %w", err)
	}
	if !decision.Allowed {
		return fmt.Errorf("rate limited: %s", toolName)
	}
	return nil
}
