#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Thin launcher for cli/ (see CLAUDE.md) — cd's to the repo root
# first so relative paths typed as command arguments (e.g. load-events'
# required -file go-gardening-demo/initial-data/sample-events.json) resolve
# the same way regardless of the caller's own cwd, matching
# go-gardening-demo/cli.sh's own rationale.
#
# Prefers the binary ./scripts/build.sh builds (bin/cli) when present, for
# faster startup — falls back to `go run` (always fresh, no build step
# required) otherwise. If you're actively editing cli/, rerun
# ./scripts/build.sh to pick up your changes, or delete bin/cli to force
# `go run` until you do.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if [[ -x "bin/cli" ]]; then
  exec bin/cli "$@"
fi
exec go run ./cli "$@"
