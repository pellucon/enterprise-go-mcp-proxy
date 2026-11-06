#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Deploys the hello-world example MCP server (see README.md) into its own
# namespace with kubectl. go-proxy discovers it by its Service label, once
# that namespace is in go-proxy's discovery.k8sNamespaces (see README.md's
# "Let go-proxy discover the namespace") — no restart is needed.
#
# Usage:
#   ./deploy.sh            deploy (or update) the example
#   ./deploy.sh --delete   remove it again (go-proxy drops its tools)
#
# Environment:
#   NAMESPACE   target namespace (default: mcp-examples)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAMESPACE="${NAMESPACE:-mcp-examples}"

case "${1:-}" in
  "") ;;
  --delete)
    echo "==> Deleting namespace $NAMESPACE"
    kubectl delete namespace "$NAMESPACE" --ignore-not-found --wait=true
    exit 0
    ;;
  -h|--help)
    sed -n '9,16p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
    exit 0
    ;;
  *)
    echo "Unknown argument: $1 (try --help)" >&2
    exit 1
    ;;
esac

echo "==> Ensuring namespace $NAMESPACE"
kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

echo "==> Publishing server.py as ConfigMap hello-world-server"
kubectl create configmap hello-world-server -n "$NAMESPACE" \
  --from-file=server.py="$SCRIPT_DIR/server.py" --dry-run=client -o yaml | kubectl apply -f -

existed=false
kubectl get deployment/hello-world -n "$NAMESPACE" >/dev/null 2>&1 && existed=true

echo "==> Applying manifests"
kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/k8s/"

# Lets go-proxy list Services here. On its own this trusts nothing: go-proxy
# only treats Services as backends in the namespaces its discovery config
# lists (checked below).
echo "==> Granting go-proxy Service discovery in $NAMESPACE"
kubectl apply -n "$NAMESPACE" -f "$SCRIPT_DIR/../../k8s/discovery/go-proxy-discovery-rbac.yaml"

# A ConfigMap change alone doesn't restart pods; on a redeploy, restart so an
# edited server.py takes effect.
if $existed; then
  kubectl rollout restart deployment/hello-world -n "$NAMESPACE" >/dev/null
fi

echo "==> Waiting for hello-world to become ready (first start installs the MCP SDK from PyPI)"
kubectl rollout status deployment/hello-world -n "$NAMESPACE" --timeout=300s

# go-proxy runs with its live discovery-override ConfigMap (seeded from its
# cfg on first start), so that's where k8sNamespaces is checked.
discovered=unknown
if override="$(kubectl get configmap discovery-override -n mcp-proxy -o jsonpath='{.data.config\.json}' 2>/dev/null)" && [[ -n "$override" ]]; then
  discovered="$(python3 -c 'import json,sys; print("yes" if sys.argv[2] in json.loads(sys.argv[1]).get("config", {}).get("k8sNamespaces", []) else "no")' "$override" "$NAMESPACE")"
fi
if [[ "$discovered" == "no" ]]; then
  cat <<EOF2

NOTE: go-proxy doesn't discover backends in namespace $NAMESPACE yet - it isn't
in go-proxy's discovery.k8sNamespaces. Add it (see README.md, "Let go-proxy
discover the namespace"), then run ./wait-for-discovery.sh.
EOF2
else
  echo "Deployed. Next: ./wait-for-discovery.sh"
fi
