#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Thin wrapper around `go run ./go-gardening-demo/cli` so the CLI can be run
# from anywhere (e.g. `~/enterprise-go-mcp-proxy/go-gardening-demo/cli.sh ...`
# or adding this directory to $PATH and just running `cli.sh ...`) without the
# caller needing to `cd` into the repo root or spell out the path
# to go-gardening-demo/cli themselves. All arguments are forwarded as-is —
# see `cli.sh -h` for usage.
#
# Prefers the binary ./scripts/build.sh builds (go-gardening-demo/bin/cli)
# when present, for faster startup — falls back to `go run` (always fresh,
# no build step required) otherwise. If you're actively editing
# go-gardening-demo/cli/, rerun ./scripts/build.sh to pick up your changes,
# or delete go-gardening-demo/bin/cli to force `go run` until you do.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if [[ -x "go-gardening-demo/bin/cli" ]]; then
  exec go-gardening-demo/bin/cli "$@"
fi
exec go run ./go-gardening-demo/cli "$@"
