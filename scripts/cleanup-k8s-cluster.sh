#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Removes everything scripts/deploy.sh creates in the cluster's three fixed
# namespaces (Deployments, Pods, Services, ConfigMaps, the imperatively-
# created jwt-public-key Secret, namespaced RBAC, NetworkPolicies) by
# deleting mcp-proxy, gardening-demo, and demo-auth wholesale, then waits
# for each to fully terminate before exiting. That wait matters: `kubectl
# delete namespace` returns as soon as deletion is *requested*, not once
# it's finished — if a redeploy's `kubectl apply -f .../namespace.yaml` runs
# while the old namespace is still "Terminating", the apply either queues
# against a disappearing object or races it, so callers who chain
# cleanup.sh followed immediately by a deploy script need this script to
# only return once every namespace it touched is actually gone.
#
# go-proxy creates no cluster-scoped objects (its discovery RBAC is a
# namespaced Role per discovery.k8sNamespaces entry, k8s/discovery/, which
# goes away with its namespace), so deleting the namespaces is enough.
#
# Deliberately does NOT touch any "other" namespace ai-mcp-servers/
# ai-powered-tools or a --deploy-server target may have landed in (see
# scripts/deploy.sh's --namespace flag) — this script makes no assumption
# about where those live, so cleaning them up is the caller's own
# responsibility (e.g. `kubectl delete namespace <name>` directly).
#
# Also stops claude-bridge (see CLAUDE.md's "AI-powered tool discovery"
# section) if scripts/deploy.sh started it — it's a host-level process, not
# a k8s resource, so it's untouched by any namespace deletion below and
# needs its own step, independent of which namespaces actually exist.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Shared constants (namespace names, claude-bridge paths, GitHub-remote
# parsing) — see scripts/lib/common.sh.
# shellcheck source=lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

NAMESPACES=("${MANAGED_NAMESPACES[@]}")
TIMEOUT_SECONDS=120


usage() {
  cat <<EOF
Usage: $0 [options]

Deletes the three fixed namespaces this repo's scripts/deploy.sh manages:
${NAMESPACES[*]}

  --timeout SECONDS     max time to wait for each namespace to fully terminate (default: $TIMEOUT_SECONDS)
  -h, --help            Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --timeout) TIMEOUT_SECONDS="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

# kubectl acts on whatever context is current: say which, before deleting.
echo "==> Cleaning up kubectl context \"$(kubectl config current-context 2>/dev/null || echo none)\""

# Independent of which namespaces actually exist below — claude-bridge is a
# plain host process, not a k8s resource.
if bridge_pid="$(claude_bridge_pid)"; then
  echo "==> Stopping claude-bridge (pid $bridge_pid)"
  kill "$bridge_pid" 2>/dev/null || true
fi
rm -f "$CLAUDE_BRIDGE_PID_FILE" "$CLAUDE_BRIDGE_LOG_FILE" "$CLAUDE_BRIDGE_TOKEN_FILE"

namespace_existed=()
for ns in "${NAMESPACES[@]}"; do
  if kubectl get namespace "$ns" >/dev/null 2>&1; then
    namespace_existed+=("$ns")
    echo "==> Deleting namespace $ns (this removes every Deployment, Pod,"
    echo "    Service, ConfigMap, Secret, ServiceAccount, Role/RoleBinding, and"
    echo "    NetworkPolicy inside it)"
    kubectl delete namespace "$ns" --ignore-not-found
  else
    echo "Namespace $ns does not exist; nothing to clean up there."
  fi
done

for ns in "${namespace_existed[@]+"${namespace_existed[@]}"}"; do
  echo "==> Waiting for namespace $ns to fully terminate (up to ${TIMEOUT_SECONDS}s)"
  elapsed=0
  while kubectl get namespace "$ns" >/dev/null 2>&1; do
    if (( elapsed >= TIMEOUT_SECONDS )); then
      echo "Timed out after ${TIMEOUT_SECONDS}s waiting for namespace $ns to terminate." >&2
      echo "It may be stuck on a finalizer; check: kubectl get namespace $ns -o yaml" >&2
      exit 1
    fi
    sleep 2
    elapsed=$((elapsed + 2))
  done
done

echo "==> Done. ${NAMESPACES[*]} are fully removed; ./scripts/deploy.sh can be run again."
