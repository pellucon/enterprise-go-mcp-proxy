// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

package triggered_tools_runtime

import "github.com/modelcontextprotocol/go-sdk/mcp"

// TriggeredToolMetaKey is the mcp.Tool._meta key a backend server sets
// (value: JSON `true`) on any tool it wants hidden from ordinary clients —
// never exposed via tools/list/tools/call, reachable only by an internal
// go-proxy caller that dials it directly by (server, tool) name. Originally
// this meant exactly one caller, the orchestration engine (Runtime.Guard);
// it no longer does — go-proxy's rate-limiting gate (rate_limit_gate.go's
// checkRateLimit, in the root package) is a second, independent caller with
// no relationship to orchestration at all, reusing this same annotation
// purely for the "hide it, it's for internal use only" property. A tool
// marked this way isn't implicitly owned by either caller; checkRateLimit
// specifically checks IsTriggeredTool so it can back off a tool that's
// already reserved by orchestration (or vice versa), rather than assuming
// it owns every triggered tool it sees. A single MCP server may freely mix
// platform and ordinary tools — this is a per-tool annotation, not a
// per-server classification. Namespaced under "go-proxy." since this is a
// private convention meaningful only to this project's own proxy code, not
// a generic MCP concept.
const TriggeredToolMetaKey = "go-proxy.triggered-tool"

// IsTriggeredTool reports whether tool is marked as a triggered tool via its
// _meta. Safe to call both on a tool constructed locally (e.g. by a sample
// platform server registering its own tools) and on one discovered over
// the wire — mcp.Tool.Meta is a plain map[string]any either way, so a
// discovered tool's "_meta" JSON object unmarshals directly into it with
// no special handling required.
func IsTriggeredTool(tool *mcp.Tool) bool {
	if tool == nil || tool.Meta == nil {
		return false
	}
	v, ok := tool.Meta[TriggeredToolMetaKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}
