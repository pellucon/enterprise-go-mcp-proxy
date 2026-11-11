# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/k8s.sh — generic Kubernetes helpers for scripts/deploy.sh:
# namespaces, go-proxy's Ingress-exposure overlay and its TLS helper,
# LoadBalancer polling.
# Sourced by deploy.sh, never executed. Reads deploy.sh's globals
# (FROM_GIT_PACKAGE, INGRESS_CLASS, TLS_SECRET, CLUSTER_ISSUER, ...).

# -----------------------------------------------------------------------
# ensure_namespace: idempotent get-or-create for a namespace this script
# doesn't otherwise assume exists — used only for $SERVER_NAMESPACE
# (--deploy-ai/--deploy-server), since the three fixed namespaces each have
# their own committed Namespace manifest applied explicitly by their deploy functions.
# -----------------------------------------------------------------------
ensure_namespace() {
  local namespace="$1"
  kubectl create namespace "$namespace" --dry-run=client -o yaml | kubectl apply -f -
}

# -----------------------------------------------------------------------
# apply_ingress_overlay NAMESPACE NAME OVERLAY_DIR HOST TLS_SECRET MANIFEST...:
# the Ingress-exposure counterpart to the two ways a Deployment/Service is
# otherwise applied (plain "kubectl apply" locally, apply_ghcr_release under
# --release), used for go-proxy (--ingress-host, k8s/overlays/ingress/).
# Generic in NAMESPACE/NAME/OVERLAY_DIR so another workload could reuse it
# with its own overlay directory. Same render-then-pipe
# Kustomize mechanism as apply_ghcr_release (and for the same
# --load-restrictor reason; MANIFEST paths must be absolute), with
# OVERLAY_DIR's service-clusterip-patch.yaml and ingress.yaml layered on,
# plus apply_ghcr_release's own image/pull-secret overrides when --release
# is in effect. NAME is the Deployment, Service, Ingress and image name.
# A non-empty TLS_SECRET adds a spec.tls block for HOST; --cluster-issuer
# adds cert-manager's annotation, so cert-manager fills that Secret, plus
# k8s/overlays/networkpolicy-acme-http01-solver.yaml so its HTTP-01 solver
# is reachable in this default-deny namespace. Never
# called unless --ingress-host was given, so the default paths
# stay byte-for-byte as they were. Prints kubectl apply's output, so a
# caller's "deployment.apps/NAME created" detection keeps working.
# -----------------------------------------------------------------------
apply_ingress_overlay() {
  local namespace="$1" name="$2" overlay_dir="$3" host="$4" tls_secret="$5"; shift 5
  local manifest_files=("$@")
  local tmpdir f
  tmpdir="$(mktemp -d)"

  {
    echo "apiVersion: kustomize.config.k8s.io/v1beta1"
    echo "kind: Kustomization"
    echo "resources:"
    for f in "${manifest_files[@]}"; do
      echo "  - $f"
    done
    echo "  - $overlay_dir/ingress.yaml"
    if [[ -n "$CLUSTER_ISSUER" ]]; then
      echo "  - $REPO_ROOT/k8s/overlays/networkpolicy-acme-http01-solver.yaml"
    fi
    if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
      echo "images:"
      echo "  - name: $name"
      echo "    newName: ghcr.io/$GITHUB_OWNER/$name"
      echo "    newTag: $RELEASE_VERSION"
    fi
    echo "patches:"
    echo "  - path: $overlay_dir/service-clusterip-patch.yaml"
    if [[ "$FROM_GIT_PACKAGE" == "true" ]] && ghcr_uses_pull_secret; then
      echo "  - target:"
      echo "      kind: Deployment"
      echo "      name: $name"
      echo "    patch: |-"
      echo "      - op: add"
      echo "        path: /spec/template/spec/imagePullSecrets"
      echo "        value:"
      echo "          - name: $GHCR_PULL_SECRET_NAME"
    fi
    if [[ -n "$tls_secret" ]]; then
      echo "  - target:"
      echo "      kind: Ingress"
      echo "      name: $name"
      echo "    patch: |-"
      echo "      - op: add"
      echo "        path: /spec/tls"
      echo "        value:"
      echo "          - hosts:"
      echo "              - __INGRESS_HOST__"
      echo "            secretName: $tls_secret"
    fi
    if [[ -n "$CLUSTER_ISSUER" ]]; then
      echo "  - target:"
      echo "      kind: Ingress"
      echo "      name: $name"
      echo "    patch: |-"
      echo "      apiVersion: networking.k8s.io/v1"
      echo "      kind: Ingress"
      echo "      metadata:"
      echo "        name: $name"
      echo "        annotations:"
      echo "          cert-manager.io/cluster-issuer: $CLUSTER_ISSUER"
    fi
  } > "$tmpdir/kustomization.yaml"

  kubectl kustomize --load-restrictor LoadRestrictionsNone "$tmpdir" \
    | sed -e "s#__INGRESS_HOST__#$host#g" -e "s#__INGRESS_CLASS__#$INGRESS_CLASS#g" \
    | kubectl apply -n "$namespace" -f -
  rm -rf "$tmpdir"
}

# proxy_ingress_tls_secret: the TLS Secret go-proxy's Ingress references —
# --tls-secret, else go-proxy-tls for cert-manager to fill (--cluster-issuer),
# else "" (no tls block: plain HTTP at the ingress, or TLS terminated in
# front of it).
proxy_ingress_tls_secret() {
  if [[ -n "$TLS_SECRET" ]]; then
    echo "$TLS_SECRET"
  elif [[ -n "$CLUSTER_ISSUER" ]]; then
    echo "go-proxy-tls"
  fi
}

# -----------------------------------------------------------------------
# poll_service_loadbalancer_address: --release's generic-cluster
# counterpart to waiting on k3s's own svclb-* pod (which only exists under
# k3s's own klipper ServiceLB, and would otherwise hang until "kubectl
# wait" times out and hard-fails the whole script under set -euo pipefail
# on any other cluster). Polls the Service's own status directly instead —
# works the same regardless of which LoadBalancer implementation (or none)
# the target cluster has. Non-fatal: prints the resolved address if one
# appears within ~60s, or a warning pointing at the cluster-compatibility
# notes otherwise, rather than aborting the deploy.
# -----------------------------------------------------------------------
poll_service_loadbalancer_address() {
  local namespace="$1" svc="$2" waited=0 address=""
  echo "==> Waiting for $svc's LoadBalancer address (namespace $namespace)"
  while [[ "$waited" -lt 60 ]]; do
    address="$(kubectl -n "$namespace" get svc "$svc" \
      -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
    if [[ -n "$address" ]]; then
      echo "==> $svc is reachable at LoadBalancer address $address"
      return
    fi
    sleep 2
    waited=$((waited + 2))
  done
  echo "WARNING: $svc's LoadBalancer address is still pending after 60s — this cluster may have no LoadBalancer implementation. See CLAUDE.md's \"Build and deploy\" section (cluster compatibility) for how to switch this Service to NodePort/Ingress instead." >&2
}

# -----------------------------------------------------------------------
# cluster_is_local: whether kubectl talks to a cluster on this machine -
# its API server is on a loopback address, as with Colima, kind or k3d, or
# is Docker Desktop's kubernetes.docker.internal (which resolves to
# 127.0.0.1; Docker Desktop serves LoadBalancer Services on localhost).
# minikube's VM address counts as remote here. On such a cluster a LoadBalancer Service is reachable only
# from this machine (localhost); on any other, a cloud provider gives it a
# public address.
# -----------------------------------------------------------------------
cluster_is_local() {
  local server host
  server="$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}' 2>/dev/null || true)"
  host="${server#*://}"
  host="${host%%/*}"
  if [[ "$host" == \[* ]]; then
    host="${host#[}"
    host="${host%%]*}"
  else
    host="${host%%:*}"
  fi
  case "$host" in
    127.*|localhost|::1|*.localhost|kubernetes.docker.internal) return 0 ;;
  esac
  return 1
}

# -----------------------------------------------------------------------
# expose_on_local_cluster NAMESPACE SERVICE [allow_public]: the Services'
# manifests are ClusterIP (reachable only inside the cluster); this makes
# SERVICE a LoadBalancer when the cluster is local ($LOCAL_CLUSTER), so it
# answers on localhost - or, when allow_public is "true", on any cluster,
# which gives it a public address on a cloud one. Otherwise it makes sure
# SERVICE is ClusterIP: kubectl apply doesn't undo a type an earlier deploy
# patched in (e.g. one run with --public-load-balancer). Returns 0 if
# SERVICE is now a LoadBalancer, 1 if it's ClusterIP.
# -----------------------------------------------------------------------
expose_on_local_cluster() {
  local namespace="$1" svc="$2" allow_public="${3:-false}" current
  if [[ "$LOCAL_CLUSTER" == "true" || "$allow_public" == "true" ]]; then
    kubectl -n "$namespace" patch service "$svc" --type merge -p '{"spec":{"type":"LoadBalancer"}}' >/dev/null
    return 0
  fi
  current="$(kubectl -n "$namespace" get service "$svc" -o jsonpath='{.spec.type}')"
  if [[ "$current" != "ClusterIP" ]]; then
    echo "==> Making $svc ClusterIP again (this cluster isn't local)"
    kubectl -n "$namespace" patch service "$svc" --type merge -p '{"spec":{"type":"ClusterIP"}}' >/dev/null
  fi
  return 1
}
