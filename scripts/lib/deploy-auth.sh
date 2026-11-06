# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/deploy-auth.sh — deploying demo-authorization-server (behind
# its LoadBalancer on :$DEMO_AUTH_PORT) and restarting go-proxy so it picks
# up the server's new signing key. Sourced by scripts/deploy.sh, never
# executed.

# -----------------------------------------------------------------------
# deploy_auth_server: thin wrapper around deploy_folder for
# demo-authorization-server, always at a fixed 1 replica (see its own
# k8s/deployment.yaml's comment on why — every bit of its state is
# in-process memory with no cross-replica coordination), into its own
# fixed $AUTH_NAMESPACE namespace. deploy_folder's own "kubectl rollout
# status" wait only proves the pod itself is Ready, not that klipper's
# LoadBalancer proxy pod has bound the Service's port yet — waited for here
# the same way deploy_proxy_workload waits for go-proxy's own svclb-* pod,
# since any external client (e.g. a human running go-gardening-demo/cli.sh
# by hand afterward) needs localhost:$DEMO_AUTH_PORT to actually be
# reachable. go-gardening-demo's own automatic seeding no longer depends
# on this at all — it runs in-cluster now (see
# seed_gardening_demo_if_needed), reaching demo-authorization-server via
# its in-cluster Service DNS name instead.
# -----------------------------------------------------------------------
deploy_auth_server() {
  echo "==> Applying demo-auth namespace"
  kubectl apply -f demo-authorization-server/k8s/namespace.yaml

  deploy_folder "demo-authorization-server" 1 "$AUTH_NAMESPACE"

  # Only ever a LoadBalancer on a local cluster: it hands an admin token to
  # anyone who logs in as admin/admin, so on any other cluster it stays
  # reachable only from inside it (port-forward to use it from outside).
  if ! expose_on_local_cluster "$AUTH_NAMESPACE" demo-authorization-server; then
    echo "==> demo-authorization-server stays internal on this (non-local) cluster; to log in from this machine:"
    echo "      kubectl -n $AUTH_NAMESPACE port-forward svc/demo-authorization-server $DEMO_AUTH_PORT:$DEMO_AUTH_PORT"
  elif [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    # Generic-cluster equivalent of the two local-only checks below: no
    # k3s-only pod label to hang on, and no assumption that "localhost" on
    # this machine reaches the cluster at all (true only via Colima's own
    # host-forwarding). See the "Cluster compatibility" notes in CLAUDE.md.
    poll_service_loadbalancer_address "$AUTH_NAMESPACE" demo-authorization-server
  else
    echo "==> Waiting for the LoadBalancer's port-$DEMO_AUTH_PORT demo-authorization-server pod"
    kubectl -n kube-system wait --for=condition=Ready pod -l svccontroller.k3s.cattle.io/svcname=demo-authorization-server,svccontroller.k3s.cattle.io/svcnamespace="$AUTH_NAMESPACE" --timeout=60s

    echo "==> Waiting for demo-authorization-server to answer on localhost:$DEMO_AUTH_PORT"
    local waited=0
    until curl -s -o /dev/null -m 1 "http://127.0.0.1:$DEMO_AUTH_PORT/healthz"; do
      sleep 1
      waited=$((waited + 1))
      if [[ "$waited" -ge 30 ]]; then
        echo "demo-authorization-server did not become reachable at localhost:$DEMO_AUTH_PORT within 30s" >&2
        exit 1
      fi
    done
    echo "==> demo-authorization-server is ready at http://localhost:$DEMO_AUTH_PORT"
  fi

  # go-proxy's auth.jwksUrl points at this Service's in-cluster DNS name,
  # but per auth/auth_service.go's NewVerifier, that JWKS keyset is only
  # fetched synchronously once, at go-proxy's own startup — it does not
  # self-heal via a later background refresh the way NATS's
  # RetryOnFailedConnect does (confirmed empirically: a go-proxy pod that
  # started before this Service existed keeps rejecting every token as
  # Unauthorized indefinitely, not just for a brief startup race). Every
  # demo-authorization-server restart also mints a brand-new signing key
  # (see its own main.go doc comment), which independently invalidates
  # whatever go-proxy had cached even if go-proxy itself didn't restart.
  #
  # When $DO_PROXY is true for this same invocation, the main flow below
  # always calls deploy_proxy_workload right after this function returns —
  # it either creates go-proxy fresh (already seeing this Service, no
  # restart needed) or, on a redeploy, unconditionally restarts it anyway
  # (to pick up its own freshly built image), which picks up the current
  # key as a side effect. Restarting go-proxy here too in that case would
  # just be a redundant second restart — see deploy_proxy_workload's own
  # comment. So this only fires for the other case: an already-running
  # go-proxy from an earlier invocation, when THIS invocation isn't also
  # (re)deploying it — e.g. a standalone --deploy-auth-server rerun after
  # demo-authorization-server was rescheduled independently of this script.
  if [[ "$DO_PROXY" == "false" ]] && kubectl -n "$PROXY_NAMESPACE" get deployment/go-proxy >/dev/null 2>&1; then
    echo "==> Restarting go-proxy so its JWKS cache picks up demo-authorization-server's current key"
    kubectl -n "$PROXY_NAMESPACE" rollout restart deployment/go-proxy
    kubectl -n "$PROXY_NAMESPACE" rollout status deployment/go-proxy
  fi
}
