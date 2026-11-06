// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package proxyserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pellucon/enterprise-go-mcp-proxy/configsync"
	"github.com/pellucon/enterprise-go-mcp-proxy/logging"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// guardrailConfirmTimeout bounds how long _admin.update_declarative_guardrails
// waits for the declarative-guardrail server to report the version it just
// wrote. The wait covers the kubelet's ConfigMap-to-volume sync (typically
// up to a minute or two) plus the server's own reload. Vars so tests can
// shorten them.
var (
	guardrailConfirmTimeout  = 3 * time.Minute
	guardrailConfirmInterval = 2 * time.Second
)

// confirmDeclarativeGuardrailsLoaded is update_declarative_guardrails'
// confirm step: it blocks until the declarative-guardrail server reports
// configsync version >= version (via its get_config_version tool), so the
// admin call returns only once the new declarations are actually in effect.
//
// Why: the proxy applies its own sections (e.g. orchestration) instantly,
// but declarative-guardrail reads declarations from a mounted ConfigMap
// that the kubelet refreshes up to a couple of minutes later. An
// orchestration update made right after adding a declaration (exactly what
// go-gardening-demo's `cli init` seeding does) would otherwise reference a
// declaration the guardrail server doesn't know yet, and every call through
// that step would fail closed until it caught up.
//
// It never fails the admin call - the ConfigMap is already written by then.
// It returns a sentence for the tool's result text instead: confirmed, not
// confirmed in time, or not checked (outside k8s, where Writer.Update never
// touches the file the guardrail server reads, so waiting could never
// succeed; or no reachable guardrail server / one without
// get_config_version). Limitation: with several declarative-guardrail
// replicas, only the replica this proxy's session reaches is checked.
func confirmDeclarativeGuardrailsLoaded(ctx context.Context, version int64) string {
	if triggeredRuntime == nil {
		return ""
	}
	if _, err := configsync.K8sClientset(); err != nil {
		return ""
	}
	fetch := func(ctx context.Context) (int64, error) {
		return declarativeGuardrailConfigVersion(ctx, defaultDeclarativeGuardrailServer)
	}
	confirmed, err := waitForConfigVersion(ctx, version, fetch, guardrailConfirmTimeout, guardrailConfirmInterval)
	switch {
	case err != nil:
		logging.Errorf("update_declarative_guardrails: could not confirm version %d with %s: %v", version, defaultDeclarativeGuardrailServer, err)
		return fmt.Sprintf(" Not confirmed with %s (%v).", defaultDeclarativeGuardrailServer, err)
	case !confirmed:
		logging.Errorf("update_declarative_guardrails: %s did not load version %d within %s", defaultDeclarativeGuardrailServer, version, guardrailConfirmTimeout)
		return fmt.Sprintf(" WARNING: %s had not loaded it after %s; orchestration steps that use newly added declarations may fail closed until it does.", defaultDeclarativeGuardrailServer, guardrailConfirmTimeout)
	default:
		return fmt.Sprintf(" Confirmed in effect on %s.", defaultDeclarativeGuardrailServer)
	}
}

// waitForConfigVersion polls fetch every interval until it reports a
// version >= target (true), timeout passes or ctx is done (false), or fetch
// fails (false + that error - no retry: a failure here means the server is
// unreachable or doesn't support version reporting, neither of which a
// retry fixes within the timeout).
func waitForConfigVersion(ctx context.Context, target int64, fetch func(context.Context) (int64, error), timeout, interval time.Duration) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		v, err := fetch(ctx)
		if err != nil {
			return false, err
		}
		if v >= target {
			return true, nil
		}
		if !time.Now().Add(interval).Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-time.After(interval):
		}
	}
}

// declarativeGuardrailConfigVersion calls serverName's get_config_version
// tool over the same session cache the orchestration engine uses.
func declarativeGuardrailConfigVersion(ctx context.Context, serverName string) (int64, error) {
	sess, err := triggeredRuntime.SessionFor(ctx, serverName)
	if err != nil {
		return 0, err
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "get_config_version"})
	if err != nil {
		return 0, err
	}
	if res.IsError {
		return 0, fmt.Errorf("get_config_version failed: %s", resultText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return 0, err
	}
	var out struct {
		Version int64 `json:"version"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("unexpected get_config_version output: %w", err)
	}
	return out.Version, nil
}

// resultText joins a tool result's text content, for error messages.
func resultText(res *mcp.CallToolResult) string {
	var out string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			if out != "" {
				out += " "
			}
			out += t.Text
		}
	}
	return out
}
