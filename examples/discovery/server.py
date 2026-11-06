# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# A minimal MCP server with one tool, for the discovery example (see
# README.md in this directory). It runs on the stock python:3.12-slim image
# with the official Python MCP SDK installed at pod startup — no compile,
# no docker build.

from mcp.server.mcpserver import MCPServer

server = MCPServer("hello-world")


# structured_output=False: return plain text only, not also a {"result": ...}
# structured copy.
@server.tool(structured_output=False)
def say_hello(name: str = "World") -> str:
    """Return a friendly greeting."""
    return f"Hello, {name}!"


if __name__ == "__main__":
    # Bind every interface so the Service can reach the pod. Streamable HTTP
    # is served at /mcp, which the Service's mcp.io/path annotation points
    # go-proxy at.
    server.run("streamable-http", host="0.0.0.0", port=8080, stateless_http=True)
