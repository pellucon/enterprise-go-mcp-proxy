#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Calls the hello-world example's say_hello tool through go-proxy.
#
# Usage:
#   ./call-tool.sh [name]   (default name: World)
#
# Environment:
#   CLI_FLAGS   login/endpoint flags for scripts/cli.sh
#               (default: -username admin -password admin)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI="$SCRIPT_DIR/../../scripts/cli.sh"
read -r -a cli_flags <<< "${CLI_FLAGS:--username admin -password admin}"

name="${1:-World}"
args="$(python3 -c 'import json, sys; print(json.dumps({"name": sys.argv[1]}))' "$name")"

"$CLI" "${cli_flags[@]}" call-tool -name hello-world.say_hello -args "$args"
