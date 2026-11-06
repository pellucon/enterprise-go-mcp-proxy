#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Shows the pods running in the three fixed namespaces scripts/deploy.sh
# manages (mcp-proxy, gardening-demo, demo-auth) — see
# scripts/cleanup-k8s-cluster.sh for the same namespace list — plus any
# Jobs in gardening-demo (today, just gardening-demo-seed — see
# seed_gardening_demo_if_needed in scripts/deploy.sh). Deliberately does
# NOT cover any "other" namespace ai-mcp-servers/ai-powered-tools or a
# --deploy-server target may have landed in (see scripts/deploy.sh's
# --namespace flag) — this script makes no assumption about where those
# live; check them directly with `kubectl get pods -n <namespace>`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Shared constants (namespace names, claude-bridge paths, GitHub-remote
# parsing) — see scripts/lib/common.sh.
# shellcheck source=lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

NAMESPACES=("${MANAGED_NAMESPACES[@]}")
JOBS_NAMESPACE="$GARDENING_NAMESPACE"
WATCH=false
WATCH_INTERVAL_SECONDS=2

usage() {
  cat <<EOF
Usage: $0 [options]

Prints "kubectl get pods -o wide" for each of: ${NAMESPACES[*]}
Then prints "kubectl get jobs -o wide" for $JOBS_NAMESPACE (today, just
gardening-demo-seed — see seed_gardening_demo_if_needed in scripts/deploy.sh).

  --watch                Refresh every ${WATCH_INTERVAL_SECONDS}s until interrupted (Ctrl-C) —
                         kubectl itself can't watch more than one -n
                         namespace in a single call, so this just loops.
  -h, --help             Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --watch) WATCH=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

show_pods() {
  local ns
  for ns in "${NAMESPACES[@]}"; do
    echo "==> $ns"
    if kubectl get namespace "$ns" >/dev/null 2>&1; then
      kubectl get pods -n "$ns" -o wide
    else
      echo "(namespace does not exist yet)"
    fi
    echo
  done
}

show_jobs() {
  echo "==> $JOBS_NAMESPACE Jobs"
  if kubectl get namespace "$JOBS_NAMESPACE" >/dev/null 2>&1; then
    kubectl get jobs -n "$JOBS_NAMESPACE" -o wide
  else
    echo "(namespace does not exist yet)"
  fi
  echo
}

if [[ "$WATCH" == "true" ]]; then
  while true; do
    clear
    echo "Pods across ${NAMESPACES[*]} — refreshing every ${WATCH_INTERVAL_SECONDS}s, Ctrl-C to stop"
    echo
    show_pods
    show_jobs
    sleep "$WATCH_INTERVAL_SECONDS"
  done
else
  show_pods
  show_jobs
fi
