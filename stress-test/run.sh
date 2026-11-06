#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Thin launcher for stress-test/driver (see stress-test/README.md) — cd's
# to the repo root first so the driver's own relative paths (bin/cli,
# stress-test/data/cli-callsN.txt, ./scripts/build.sh, ./scripts/deploy.sh)
# resolve the same way regardless of the caller's own cwd, matching
# scripts/cli.sh's own rationale.
#
# Prefers a prebuilt stress-test/bin/driver (build it yourself via
# `go build -o stress-test/bin/driver ./stress-test/driver` — there's no
# Makefile target for this, unlike bin/cli, since the driver itself is
# only ever launched once per stress-test run, not thousands of times, so
# `go run`'s one-time compile overhead is a non-issue here) — falls back
# to `go run` otherwise.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

if [[ -x "stress-test/bin/driver" ]]; then
  exec stress-test/bin/driver "$@"
fi
exec go run ./stress-test/driver "$@"
