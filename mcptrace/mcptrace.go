// Copyright (c) 2026 Enterprise Go MCP Proxy
// SPDX-License-Identifier: MIT

// Package mcptrace provides a generic MCP request/response tracing
// middleware, built on the shared logging package, for any backend
// MCP server in this module that wants the same "log every call and its
// result" visibility go-proxy's own middleware.go already provides for the
// gateway itself. A backend server has no auth/role/orchestration concerns
// of its own (those are already enforced by go-proxy before a call ever
// reaches it), so this is deliberately just the trace: method/tool
// name/arguments in, error-or-result out.
package mcptrace

import (
	"context"

	"github.com/pellucon/enterprise-go-mcp-proxy/logging"
	"github.com/pellucon/enterprise-go-mcp-proxy/relayheaders"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Middleware returns an mcp.Middleware that logs, at Debug level, every
// received MCP method call — method name, session ID, and (for tools/call
// specifically) the tool name/arguments/tool-call ID, the last of these the
// same correlation ID go-proxy's own trace log already carries for the same
// call, so the two log lines can be matched up — followed by its outcome:
// the error at Error level, or isError/structuredContent (for a tool
// result) at Debug level. Install via
// server.AddReceivingMiddleware(mcptrace.Middleware()) right after
// mcp.NewServer, the same way go-proxy's own gateway installs its
// createMiddleware in main.go.
func Middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			sessionID := req.GetSession().ID()
			if ctr, ok := req.(*mcp.CallToolRequest); ok {
				// GetExtra() is nil for a request with no transport-level
				// extras — e.g. mcp.NewInMemoryTransports(), used by this
				// module's own in-process tests, never populates it.
				var toolCallID string
				if extra := ctr.GetExtra(); extra != nil {
					toolCallID = relayheaders.Extract(extra.Header).ToolCallID
				}
				logging.Debugf("Calling tool: name=%s args=%s session_id=%s tool_call_id=%s",
					ctr.Params.Name, logging.Size(ctr.Params.Arguments), sessionID, toolCallID)
			} else {
				logging.Debugf("MCP method started: method=%s session_id=%s", method, sessionID)
			}

			result, err := next(ctx, method, req)

			if err != nil {
				logging.Errorf("MCP method failed: method=%s session_id=%s err=%v", method, sessionID, err)
				return result, err
			}
			if ctr, ok := result.(*mcp.CallToolResult); ok {
				logging.Debugf("tool result: isError=%v structuredContent=%s", ctr.IsError, logging.Size(ctr.StructuredContent))
			} else {
				logging.Debugf("MCP method completed: method=%s session_id=%s", method, sessionID)
			}
			return result, nil
		}
	}
}
