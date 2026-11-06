#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Waits until go-proxy has discovered the hello-world example server, i.e.
# until hello-world.say_hello shows up in the CLI's list-tools output.
#
# Environment:
#   TIMEOUT     seconds to wait before giving up (default: 180)
#   NAMESPACE   namespace the example was deployed to (default: mcp-examples)
#   CLI_FLAGS   login/endpoint flags for scripts/cli.sh
#               (default: -username admin -password admin)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLI="$SCRIPT_DIR/../../scripts/cli.sh"
TOOL="hello-world.say_hello"
TIMEOUT="${TIMEOUT:-180}"
read -r -a cli_flags <<< "${CLI_FLAGS:--username admin -password admin}"

NAMESPACE="${NAMESPACE:-mcp-examples}"
deadline=$((SECONDS + TIMEOUT))

# go-proxy discovers the Service, which exists before its pod is ready, so a
# listed tool isn't necessarily callable yet. Wait for the pod first.
echo "==> Waiting for the hello-world pod to be ready"
kubectl rollout status deployment/hello-world -n "$NAMESPACE" --timeout="${TIMEOUT}s"

echo "==> Waiting for go-proxy to discover $TOOL"
waited=false
while true; do
  # list-tools prints one "<name><TAB><description>" line per tool.
  if "$CLI" "${cli_flags[@]}" list-tools 2>/dev/null | cut -f1 | grep -qx "$TOOL"; then
    $waited && echo
    echo "Discovered $TOOL. Next: ./call-tool.sh [name]"
    exit 0
  fi
  if (( SECONDS >= deadline )); then
    $waited && echo
    echo "Timed out: $TOOL not discovered after ${TIMEOUT}s." >&2
    echo "Check: kubectl get pods,svc -n $NAMESPACE" >&2
    exit 1
  fi
  printf '.'
  waited=true
  sleep 3
done
