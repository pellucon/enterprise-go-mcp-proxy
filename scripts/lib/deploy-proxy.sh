# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/deploy-proxy.sh — deploying go-proxy itself: deploy_proxy
# (namespace, RBAC, NetworkPolicies, config, NATS) and deploy_proxy_workload
# (its Deployment/Service, or the Ingress overlay). Sourced by
# scripts/deploy.sh, never executed.

# -----------------------------------------------------------------------
# deploy_proxy: everything go-proxy's own workload needs to already exist —
# namespace, shared ConfigMaps/Secrets, RBAC/NetworkPolicies/NATS — but
# deliberately NOT go-proxy's own Deployment/Service (see
# deploy_proxy_workload below for why that's split out, and for the part
# of this same overall job it covers instead). Everything here lands in
# the fixed $PROXY_NAMESPACE namespace.
# -----------------------------------------------------------------------
deploy_proxy() {
  if [[ ! -f "$CONFIG_FILE" ]]; then
    echo "Config file not found at $CONFIG_FILE (pass --config to override)" >&2
    exit 1
  fi

  # auth.publicKeyPath is optional (same nil/empty-if-absent convention as
  # internalAuth below): a config using jwksUrl-based auth instead (see
  # CLAUDE.md's auth section) leaves this empty, since there's no
  # local key file for it to name — the jwt-public-key Secret is then
  # simply skipped, below.
  local jwt_public_key
  jwt_public_key="$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('auth', {}).get('publicKeyPath', ''))" "$CONFIG_FILE")"
  if [[ -n "$jwt_public_key" && ! -f "$jwt_public_key" ]]; then
    echo "JWT public key not found at $jwt_public_key (from $CONFIG_FILE's auth.publicKeyPath)" >&2
    exit 1
  fi

  if [[ -n "$AI_API_KEY_FILE" && ! -f "$AI_API_KEY_FILE" ]]; then
    echo "AI API key file not found at $AI_API_KEY_FILE (from --ai-api-key-file)" >&2
    exit 1
  fi

  if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    require_ghcr_image "go-proxy"
  else
    require_image "go-proxy:local"
  fi

  echo "==> Applying namespace"
  kubectl apply -f k8s/namespace.yaml

  if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    ensure_ghcr_pull_secret "$PROXY_NAMESPACE"
  fi

  # All ConfigMaps/Secrets are created *before* deploy_proxy_workload
  # applies go-proxy's own Deployment (and therefore starts its pods) —
  # this ordering matters regardless of the gap in between (deploy_auth_server
  # may now run there too): an optional Secret volume does NOT make kubelet
  # wait/retry the way it does for a required one — if the Deployment
  # existed first and a pod started before this Secret did, kubelet
  # resolves that volume as permanently empty for that pod's whole
  # lifetime, and creating the Secret moments later doesn't retroactively
  # fix it.
  echo "==> Creating/updating proxy-server-config configmap from $CONFIG_FILE"
  kubectl create configmap proxy-server-config \
    --namespace "$PROXY_NAMESPACE" \
    --from-file=proxy-server.cfg="$CONFIG_FILE" \
    --dry-run=client -o yaml | kubectl apply -f -

  # No separate seeding step for the 7 configsync-managed
  # "<section>-override" ConfigMaps (access-rules, orchestration, discovery,
  # event-dictionary, declarative-guardrails, event-aggregation,
  # rate-limiting) here anymore: go-proxy seeds each of them itself, from
  # its own parsed --config, the moment it starts — before its HTTP
  # listener (and therefore /healthz) ever comes up — via
  # configsync.Writer.EnsureSeeded (see main.go's doRunOn and CLAUDE.md's
  # "Unify configuration into a single config-map mechanism"). The
  # "kubectl rollout status deployment/go-proxy" wait in
  # deploy_proxy_workload (below, run later in the main flow) already
  # blocks deploy_category "platform-mcp-servers" until go-proxy reports
  # ready, so every section is guaranteed seeded before declarative-guardrail/
  # event-aggregator/rate-limiter (the backend-only sections' consumers)
  # ever start — no new wait needed here for that ordering.

  if [[ -n "$jwt_public_key" ]]; then
    echo "==> Creating/updating jwt-public-key secret from $jwt_public_key"
    kubectl create secret generic jwt-public-key \
      --namespace "$PROXY_NAMESPACE" \
      --from-file=public.pem="$jwt_public_key" \
      --dry-run=client -o yaml | kubectl apply -f -
  else
    echo "==> Skipping jwt-public-key secret ($CONFIG_FILE has no auth.publicKeyPath — using jwksUrl instead)"
  fi

  # No go-proxy-internal-signing-key/internal-auth-public-key Secret
  # creation here anymore: go-proxy bootstraps its own internal-signing
  # keypair at runtime (internal_signing_key.go's
  # bootstrapInternalSigningKey) and serves the public half itself at
  # /.well-known/jwks-internal.json — a backend that wants to verify go-proxy's
  # internal identity assertion points its own -internal-jwks-url flag at
  # that endpoint directly (no currently-deployed backend does; see
  # CLAUDE.md's "Internal identity assertions" section).

  # Every k8s/*.yaml file EXCEPT go-proxy's own Deployment/Service —
  # RBAC, every NetworkPolicy, and NATS. Enumerated dynamically (not a
  # hardcoded file list) so a future new file in k8s/ is picked up here
  # automatically, the same "don't hand-maintain a list that can drift"
  # rationale as deploy_category's own directory scan. go-proxy's own
  # Deployment/Service are deliberately held back — see
  # deploy_proxy_workload below.
  local shared_manifests=() f
  for f in k8s/*.yaml; do
    case "$(basename "$f")" in
      go-proxy-deployment.yaml|go-proxy-service.yaml) continue ;;
    esac
    shared_manifests+=(-f "$f")
  done

  ensure_nats_secrets

  echo "==> Applying RBAC, network policy, and NATS"
  kubectl apply -n "$PROXY_NAMESPACE" "${shared_manifests[@]}"

  # A pod-template annotation carrying the config's hash restarts NATS
  # exactly when its config (users, passwords, TLS) changed - a Secret
  # volume update alone doesn't make NATS reload it.
  kubectl -n "$PROXY_NAMESPACE" patch deployment nats --type merge \
    -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"mcp.io/nats-config-hash\":\"$NATS_CONFIG_HASH\"}}}}}" >/dev/null

  apply_discovery_rbac
  apply_credentials_rbac
  apply_egress_policy

  # NATS-dependent servers (event-aggregator under platform-mcp-servers/)
  # fail fast at startup if they can't reach NATS, so this function must not
  # return — letting the caller proceed to deploy them — before the nats
  # Deployment applied just above has actually finished rolling out. Without
  # this wait, whichever of those backends happens to be deployed first
  # races NATS's own startup/image pull, crashes, and gets restarted by k8s
  # — not a hang, but it looks like one, and it's pure luck it self-heals.
  echo "==> Waiting for the nats deployment to roll out"
  kubectl -n "$PROXY_NAMESPACE" rollout status deployment/nats --timeout=120s
}

# -----------------------------------------------------------------------
# deploy_proxy_workload: builds+applies go-proxy's own Deployment/Service
# specifically — split out from deploy_proxy so scripts/deploy.sh's main flow can
# run deploy_auth_server (when requested) in between the two: everything
# deploy_proxy sets up (namespace, RBAC, NetworkPolicies, NATS, ConfigMaps/
# Secrets) has no dependency on demo-authorization-server, but go-proxy's
# own pod does, indirectly, via its synchronous JWKS fetch against
# auth.jwksUrl at startup (see auth/auth_service.go's NewVerifier — that
# fetch doesn't self-heal later if it fails). Deferring go-proxy's own
# Deployment/Service apply until after demo-authorization-server already
# exists means go-proxy only ever needs to start once in a combined
# --deploy-proxy --deploy-auth-server (or --deploy-all) run, instead of
# starting, failing its first JWKS fetch, and being explicitly restarted
# by deploy_auth_server afterward. Requires deploy_proxy to have already
# run (this or an earlier invocation) — same shared-setup assumption
# every other deploy target already has.
# -----------------------------------------------------------------------
deploy_proxy_workload() {
  if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    if [[ -n "$INGRESS_HOST" ]]; then
      echo "==> Applying go-proxy Deployment/Service/Ingress via Kustomize overlay (host $INGRESS_HOST)"
      apply_ingress_overlay "$PROXY_NAMESPACE" go-proxy "$REPO_ROOT/k8s/overlays/ingress" "$INGRESS_HOST" "$(proxy_ingress_tls_secret)" \
        "$REPO_ROOT/k8s/go-proxy-deployment.yaml" "$REPO_ROOT/k8s/go-proxy-service.yaml"
    else
      echo "==> Applying go-proxy Deployment/Service via Kustomize overlay"
      apply_ghcr_release "$PROXY_NAMESPACE" go-proxy "Deployment" \
        "$REPO_ROOT/k8s/go-proxy-deployment.yaml" "$REPO_ROOT/k8s/go-proxy-service.yaml"
    fi

    echo "==> Scaling to requested replica count"
    kubectl -n "$PROXY_NAMESPACE" scale deployment/go-proxy --replicas="$PROXY_REPLICAS"

    echo "==> Waiting for rollout"
    kubectl -n "$PROXY_NAMESPACE" rollout status deployment/go-proxy

    # Not necessarily k3s here — generic-cluster equivalent of the wait
    # below: poll the Service's own status instead of a k3s-only pod label
    # that may not exist at all on this cluster (which would otherwise hang
    # until "kubectl wait" gives up and hard-fail the whole script under
    # set -euo pipefail). No LoadBalancer at all on the --ingress-host
    # path — the ingress controller's own address is what clients use —
    # nor on a remote cluster without --public-load-balancer.
    if [[ -z "$INGRESS_HOST" ]] && expose_on_local_cluster "$PROXY_NAMESPACE" go-proxy "$PUBLIC_LOAD_BALANCER"; then
      poll_service_loadbalancer_address "$PROXY_NAMESPACE" go-proxy
    fi
    return
  fi

  local apply_output
  if [[ -n "$INGRESS_HOST" ]]; then
    echo "==> Applying go-proxy Deployment/Service/Ingress via Kustomize overlay (host $INGRESS_HOST)"
    apply_output="$(apply_ingress_overlay "$PROXY_NAMESPACE" go-proxy "$REPO_ROOT/k8s/overlays/ingress" "$INGRESS_HOST" "$(proxy_ingress_tls_secret)" \
      "$REPO_ROOT/k8s/go-proxy-deployment.yaml" "$REPO_ROOT/k8s/go-proxy-service.yaml")"
  else
    echo "==> Applying go-proxy Deployment/Service"
    apply_output="$(kubectl apply -n "$PROXY_NAMESPACE" -f k8s/go-proxy-deployment.yaml -f k8s/go-proxy-service.yaml)"
  fi
  echo "$apply_output"
  if [[ -z "$INGRESS_HOST" ]]; then
    expose_on_local_cluster "$PROXY_NAMESPACE" go-proxy
  fi

  # Whether the go-proxy Deployment object itself was just created by the
  # apply above (as opposed to already existing) determines whether
  # "rollout restart" is needed below.
  local go_proxy_just_created=false
  if echo "$apply_output" | grep -q '^deployment\.apps/go-proxy created$'; then
    go_proxy_just_created=true
  fi

  echo "==> Scaling to requested replica count"
  kubectl -n "$PROXY_NAMESPACE" scale deployment/go-proxy --replicas="$PROXY_REPLICAS"

  # "kubectl scale" above is a no-op when the replica count hasn't changed,
  # so it alone won't recreate already-running pods after a local rebuild —
  # the image tag (go-proxy:local) never changes, so its content changing
  # underneath isn't a pod-template diff Kubernetes would otherwise notice.
  # "rollout restart" forces every pod to be recreated regardless of
  # whether the image tag or replica count changed. Skipped when the
  # Deployment was *just* created by the apply above: its first-generation
  # pods already used the image built moments ago (and, if deploy_auth_server
  # already ran this invocation, already see a working demo-authorization-
  # server too), so restarting immediately would just be churn — and the
  # source of a confusing "1 old replicas are pending termination" even
  # right after a from-scratch deploy. When the Deployment already existed
  # (a redeploy), this restart also doubles as picking up
  # demo-authorization-server's current signing key if that was
  # redeployed earlier in this same invocation — deploy_auth_server
  # deliberately skips its own such restart whenever $DO_PROXY is true for
  # exactly this reason, so this is the only restart that happens, not a
  # second redundant one.
  if [[ "$go_proxy_just_created" == "true" ]]; then
    echo "==> go-proxy Deployment was just created — already running the freshly built image, no restart needed"
  else
    echo "==> Restarting deployment to pick up the freshly built image"
    kubectl -n "$PROXY_NAMESPACE" rollout restart deployment/go-proxy
  fi

  echo "==> Waiting for rollout"
  # Bounded, and on failure show why: a new pod that won't start (e.g. the
  # cfg was changed so that it differs from the live config, which go-proxy
  # refuses - see docs/architecture/configsync.md) would otherwise leave
  # this waiting for the Deployment's 10-minute progress deadline. The old
  # pod keeps serving meanwhile.
  if ! kubectl -n "$PROXY_NAMESPACE" rollout status deployment/go-proxy --timeout=180s; then
    echo "==> go-proxy's new pod didn't become ready. Its log:" >&2
    newest_pod=$(kubectl -n "$PROXY_NAMESPACE" get pods -l app=go-proxy --sort-by=.metadata.creationTimestamp -o name | tail -1)
    kubectl -n "$PROXY_NAMESPACE" logs "$newest_pod" --tail=20 >&2 || true
    exit 1
  fi

  # No svclb-* pod exists on the --ingress-host path: the Service is
  # ClusterIP there, so klipper never creates one.
  if [[ -n "$INGRESS_HOST" ]]; then
    return
  fi

  echo "==> Waiting for the LoadBalancer's port-80 proxy pod"
  # k3s's built-in ServiceLB ("klipper") creates this pod in kube-system
  # (not $PROXY_NAMESPACE), named svclb-go-proxy-*, to bind the Service's
  # port directly on the node.
  kubectl -n kube-system wait --for=condition=Ready pod -l svccontroller.k3s.cattle.io/svcname=go-proxy,svccontroller.k3s.cattle.io/svcnamespace="$PROXY_NAMESPACE" --timeout=60s
}

# -----------------------------------------------------------------------
# warn_if_unprotected: warns when a discovery namespace has no
# NetworkPolicy at all. Backends trust they're reachable only through
# go-proxy, so there any pod could call them directly, skipping its
# authentication, access rules and guardrails. deploy.sh protects the
# backends it deploys itself (isolate_backend in scripts/lib/deploy-servers.sh);
# a namespace someone else runs is theirs to protect, so this only warns.
# -----------------------------------------------------------------------
warn_if_unprotected() {
  local ns="$1"
  case "$ns" in
    "$PROXY_NAMESPACE"|"$GARDENING_NAMESPACE") return ;;
  esac
  if [[ -z "$(kubectl -n "$ns" get networkpolicy -o name 2>/dev/null)" ]]; then
    cat >&2 <<EOF2
WARNING: discovery namespace $ns has no NetworkPolicy, so any pod in the
cluster can call its backends directly, bypassing go-proxy's authentication,
access rules, guardrails and audit. Let only go-proxy reach them, as
examples/discovery/k8s/networkpolicy.yaml does (deploy.sh adds such a policy
itself for backends it deploys with --deploy-server/--deploy-ai --namespace).
EOF2
  fi
}

# -----------------------------------------------------------------------
# apply_egress_policy: with --egress-policies, applies the go-proxy-egress
# NetworkPolicy, which lets go-proxy's pods connect only to what it needs:
#   - DNS (kube-dns in kube-system);
#   - the Kubernetes API server (its EndpointSlice addresses, looked up now:
#     a NetworkPolicy sees the address behind the kubernetes Service);
#   - its own namespace: NATS, the platform servers, and itself (the
#     proxy_utility loopback to selfUrl);
#   - every discovery namespace, and the namespaces in static servers'
#     in-cluster URLs;
#   - the namespace of an in-cluster auth.jwksUrl (e.g. demo-auth);
#   - each --egress-allow CIDR[:PORT].
# Without --egress-policies, an existing policy is removed. It's computed
# from the effective config at deploy time, so it needs re-running after
# the discovery namespaces change.
# -----------------------------------------------------------------------
apply_egress_policy() {
  if [[ "$EGRESS_POLICIES" != "true" ]]; then
    if kubectl -n "$PROXY_NAMESPACE" get networkpolicy go-proxy-egress >/dev/null 2>&1; then
      echo "==> Removing go-proxy's egress policy (deployed without --egress-policies)"
      kubectl -n "$PROXY_NAMESPACE" delete networkpolicy go-proxy-egress >/dev/null
    fi
    return
  fi
  local discovery api_endpoints policy
  discovery="$(effective_discovery_config)" || { echo "Failed to read go-proxy's discovery config" >&2; exit 1; }
  api_endpoints="$(kubectl get endpointslices -n default -l kubernetes.io/service-name=kubernetes -o json)"
  if ! policy="$(python3 - "$PROXY_NAMESPACE" "$discovery" "$api_endpoints" "$CONFIG_FILE" "${EGRESS_ALLOW[@]+"${EGRESS_ALLOW[@]}"}" <<'PY'
import ipaddress, json, re, sys
from urllib.parse import urlparse

proxy_ns, discovery, endpoints, cfg_path = sys.argv[1], json.loads(sys.argv[2]), json.loads(sys.argv[3]), sys.argv[4]
extra = sys.argv[5:]
notes = []

def in_cluster_namespace(url):
    """The namespace of an in-cluster Service URL (<svc>.<ns>.svc[.cluster.local]), else None."""
    host = urlparse(url).hostname or ""
    m = re.fullmatch(r"[a-z0-9-]+\.([a-z0-9-]+)\.svc(\.cluster\.local)?", host)
    return m.group(1) if m else None

namespaces = set(discovery.get("k8sNamespaces") or []) if discovery.get("discoveryMode") == "k8s" else set()
for server in discovery.get("servers") or []:
    ns = in_cluster_namespace(server.get("serverUrl", ""))
    if ns:
        namespaces.add(ns)
    elif server.get("serverUrl"):
        notes.append(f"static server {server.get('serverName')!r} at {server['serverUrl']} is off-cluster: allow it with --egress-allow")
jwks = ((json.load(open(cfg_path)).get("auth") or {}).get("jwksUrl")) or ""
if jwks:
    ns = in_cluster_namespace(jwks)
    if ns:
        namespaces.add(ns)
    else:
        notes.append(f"auth.jwksUrl {jwks} is off-cluster: allow its host with --egress-allow (e.g. 0.0.0.0/0:443)")
namespaces.discard(proxy_ns)

rules = [
    {"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "kube-system"}},
             "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}}],
     "ports": [{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}]},
    {"to": [{"podSelector": {}}]},
]
for slice_ in endpoints.get("items") or []:
    ips = [ip for e in slice_.get("endpoints") or [] for ip in e.get("addresses") or []]
    ports = [{"protocol": "TCP", "port": p["port"]} for p in slice_.get("ports") or []]
    if ips:
        rules.append({"to": [{"ipBlock": {"cidr": f"{ip}/{32 if ':' not in ip else 128}"}} for ip in ips], "ports": ports})
if len(rules) == 2:
    sys.exit("couldn't find the Kubernetes API server's address (the kubernetes Service's EndpointSlices in default)")
if namespaces:
    rules.append({"to": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": ns}}} for ns in sorted(namespaces)]})
for entry in extra:
    cidr, _, port = entry.partition(":") if entry.count(":") <= 1 else (entry, "", "")
    try:
        ipaddress.ip_network(cidr, strict=False)
    except ValueError:
        sys.exit(f"--egress-allow {entry!r}: not CIDR[:PORT]")
    rule = {"to": [{"ipBlock": {"cidr": cidr}}]}
    if port:
        if not port.isdigit() or not 0 < int(port) < 65536:
            sys.exit(f"--egress-allow {entry!r}: bad port")
        rule["ports"] = [{"protocol": "TCP", "port": int(port)}]
    rules.append(rule)

for note in notes:
    print("NOTE: " + note, file=sys.stderr)
print(json.dumps({
    "apiVersion": "networking.k8s.io/v1",
    "kind": "NetworkPolicy",
    "metadata": {"name": "go-proxy-egress"},
    "spec": {"podSelector": {"matchLabels": {"app": "go-proxy"}}, "policyTypes": ["Egress"], "egress": rules},
}))
PY
  )"; then
    exit 1
  fi
  echo "==> Restricting go-proxy's outbound connections (--egress-policies)"
  echo "$policy" | kubectl apply -n "$PROXY_NAMESPACE" -f -
}

# -----------------------------------------------------------------------
# credentials_secrets: prints the backend credentials Secrets the effective
# discovery config names (k8sCredentials' values, static servers'
# credentialsSecret), one per line, sorted.
# -----------------------------------------------------------------------
credentials_secrets() {
  local cfg
  cfg="$(effective_discovery_config)" || return 1
  python3 - "$cfg" <<'PY'
import json, re, sys
cfg = json.loads(sys.argv[1])
names = set((cfg.get("k8sCredentials") or {}).values())
names |= {s["credentialsSecret"] for s in cfg.get("servers") or [] if s.get("credentialsSecret")}
names.discard("")
# A Kubernetes Secret name (DNS subdomain); anything else can't name a real
# Secret, and mustn't reach the Role's YAML.
bad = sorted(n for n in names if not re.fullmatch(r"[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?", n))
if bad:
    sys.exit(f"invalid credentials Secret name(s) in the discovery config: {bad}")
print("\n".join(sorted(names)))
PY
}

# -----------------------------------------------------------------------
# apply_credentials_rbac: grants go-proxy get on exactly the backend
# credentials Secrets the effective discovery config names, as the
# go-proxy-credentials Role, and nothing else - not the NATS passwords, the
# pull secret or any other Secret in its namespace. A credentials Secret
# added later with _admin.update_discovery needs this re-run
# (deploy.sh --deploy-proxy); until then go-proxy can't read it and calls
# to that server fail closed. With no credentials Secrets the Role is
# deleted rather than rendered: a rule with an empty resourceNames would
# cover every Secret.
# -----------------------------------------------------------------------
apply_credentials_rbac() {
  local secrets
  if ! secrets="$(credentials_secrets)"; then
    echo "Failed to read go-proxy's discovery config" >&2
    exit 1
  fi
  if [[ -z "$secrets" ]]; then
    echo "==> No backend credentials Secrets configured; go-proxy gets no Secret access beyond its signing key"
    kubectl -n "$PROXY_NAMESPACE" delete role,rolebinding go-proxy-credentials --ignore-not-found >/dev/null
    return
  fi
  echo "==> Granting go-proxy read access to backend credentials Secrets: $(echo $secrets)"
  {
    cat <<EOF2
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: go-proxy-credentials
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get"]
    resourceNames:
EOF2
    local name
    while IFS= read -r name; do
      printf '      - "%s"\n' "$name"
    done <<< "$secrets"
    cat <<EOF2
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: go-proxy-credentials
subjects:
  - kind: ServiceAccount
    name: go-proxy
    namespace: $PROXY_NAMESPACE
roleRef:
  kind: Role
  name: go-proxy-credentials
  apiGroup: rbac.authorization.k8s.io
EOF2
  } | kubectl apply -n "$PROXY_NAMESPACE" -f -
}

# -----------------------------------------------------------------------
# effective_discovery_config: prints, as JSON, the discovery config go-proxy
# will actually run with. That's the live discovery-override ConfigMap when
# one exists (go-proxy's configsync never overwrites it from the cfg on
# restart), otherwise $CONFIG_FILE's discovery section.
# -----------------------------------------------------------------------
effective_discovery_config() {
  local override
  if override="$(kubectl get configmap discovery-override -n "$PROXY_NAMESPACE" \
      -o jsonpath='{.data.config\.json}' 2>/dev/null)" && [[ -n "$override" ]]; then
    python3 -c "import json,sys; print(json.dumps(json.loads(sys.argv[1]).get('config', {})))" "$override"
  else
    python3 -c "import json,sys; print(json.dumps(json.load(open(sys.argv[1])).get('discovery', {})))" "$CONFIG_FILE"
  fi
}

# -----------------------------------------------------------------------
# discovery_namespaces: prints the effective discovery.k8sNamespaces, one
# per line - nothing when discovery isn't in k8s mode. Exits with an error
# (rather than letting go-proxy crash-loop on startup) when the effective
# config is in k8s mode but has no k8sNamespaces, which go-proxy rejects.
# -----------------------------------------------------------------------
discovery_namespaces() {
  local cfg
  cfg="$(effective_discovery_config)" || return 1
  python3 - "$cfg" <<'PY'
import json, sys
cfg = json.loads(sys.argv[1])
if cfg.get("discoveryMode") != "k8s":
    sys.exit(0)
if not cfg.get("k8sNamespaces"):
    sys.exit(3)
print("\n".join(cfg["k8sNamespaces"]))
PY
}

# -----------------------------------------------------------------------
# apply_discovery_rbac: grants go-proxy list/watch on Services in exactly
# the namespaces its discovery config trusts (k8s/discovery/).
# -----------------------------------------------------------------------
apply_discovery_rbac() {
  local namespaces rc=0
  namespaces="$(discovery_namespaces)" || rc=$?
  if [[ $rc -eq 3 ]]; then
    cat >&2 <<EOF2
go-proxy's discovery config is in k8s mode but has no discovery.k8sNamespaces.
go-proxy would refuse to start with it.

If $CONFIG_FILE is the problem, add the list of namespaces to discover
backends in, e.g. "k8sNamespaces": ["mcp-proxy", "gardening-demo"].

If $CONFIG_FILE is fine, the live discovery-override ConfigMap (which
always wins over the cfg) lacks it. Either add k8sNamespaces to it with
_admin.update_discovery, or delete it so it's re-seeded from $CONFIG_FILE
(this drops any discovery changes made with the admin tools):

  kubectl -n $PROXY_NAMESPACE delete configmap discovery-override
EOF2
    exit 1
  elif [[ $rc -ne 0 ]]; then
    echo "Failed to read go-proxy's discovery config" >&2
    exit 1
  fi

  if [[ -z "$namespaces" ]]; then
    echo "==> Discovery isn't in k8s mode; no discovery RBAC needed"
    return
  fi
  local ns
  while IFS= read -r ns; do
    echo "==> Granting go-proxy Service discovery in namespace $ns"
    ensure_namespace "$ns"
    kubectl apply -n "$ns" -f k8s/discovery/go-proxy-discovery-rbac.yaml
    warn_if_unprotected "$ns"
  done <<< "$namespaces"
}

# -----------------------------------------------------------------------
# warn_if_not_discovered NAMESPACE: --deploy-ai/--deploy-server can put a
# backend in any namespace, but go-proxy only discovers backends in the
# namespaces its discovery config lists. Warns (doesn't fail) when
# NAMESPACE isn't one of them, since the backend would deploy fine and
# then silently never show up.
# -----------------------------------------------------------------------
warn_if_not_discovered() {
  local namespace="$1" namespaces
  namespaces="$(discovery_namespaces 2>/dev/null)" || return 0
  [[ -z "$namespaces" ]] && return 0
  if ! grep -qxF "$namespace" <<< "$namespaces"; then
    echo "WARNING: namespace $namespace isn't in go-proxy's discovery.k8sNamespaces;" >&2
    echo "         go-proxy won't discover backends deployed there until it's added." >&2
  fi
}

# -----------------------------------------------------------------------
# NATS_COMPONENTS: every component that connects to NATS, each with its own
# NATS user (k8s/nats/nats.conf.tmpl) and credentials Secret
# (nats-client-<component>, mounted by that component's Deployment).
# -----------------------------------------------------------------------
NATS_COMPONENTS=(go-proxy event-aggregator declarative-guardrail rate-limiter ai-powered-tools)

# -----------------------------------------------------------------------
# ensure_nats_secrets: creates what NATS needs for authentication and TLS,
# in $PROXY_NAMESPACE, keeping whatever already exists so redeploys don't
# change any password or certificate:
#   - nats-tls: a private CA and NATS's server certificate (created once;
#     the CA key is discarded after signing, never stored in the cluster).
#     Delete the Secret and redeploy to replace them.
#   - nats-client-<component>: that component's user, a random password
#     (created once), and the CA certificate.
#   - nats-server-config: nats.conf rendered from k8s/nats/nats.conf.tmpl
#     with every component's password.
# Sets NATS_CONFIG_HASH (the config's hash) for deploy_proxy. To rotate the
# passwords, delete the nats-client-* Secrets and redeploy everything.
# -----------------------------------------------------------------------
ensure_nats_secrets() {
  local tmp ns="$PROXY_NAMESPACE" component password passwords_json
  tmp="$(mktemp -d)"
  # The directory holds the CA and server keys and every NATS password:
  # removed however this ends, including when a step fails (set -e).
  trap "rm -rf '$tmp'" EXIT

  if ! kubectl -n "$ns" get secret nats-tls >/dev/null 2>&1; then
    echo "==> Creating the NATS TLS CA and server certificate"
    openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj "/CN=go-proxy NATS CA" \
      -keyout "$tmp/ca.key" -out "$tmp/ca.crt" >/dev/null 2>&1
    openssl req -newkey rsa:2048 -nodes -subj "/CN=nats" \
      -keyout "$tmp/tls.key" -out "$tmp/tls.csr" >/dev/null 2>&1
    printf 'subjectAltName=DNS:nats,DNS:nats.%s,DNS:nats.%s.svc,DNS:nats.%s.svc.cluster.local\nextendedKeyUsage=serverAuth\n' \
      "$ns" "$ns" "$ns" > "$tmp/san.ext"
    openssl x509 -req -days 3650 -in "$tmp/tls.csr" -CA "$tmp/ca.crt" -CAkey "$tmp/ca.key" \
      -CAcreateserial -extfile "$tmp/san.ext" -out "$tmp/tls.crt" >/dev/null 2>&1
    kubectl create secret generic nats-tls --namespace "$ns" \
      --from-file=ca.crt="$tmp/ca.crt" --from-file=tls.crt="$tmp/tls.crt" --from-file=tls.key="$tmp/tls.key" >/dev/null
  fi
  kubectl -n "$ns" get secret nats-tls -o jsonpath='{.data.ca\.crt}' | base64 --decode > "$tmp/ca.crt"

  passwords_json="{"
  for component in "${NATS_COMPONENTS[@]}"; do
    password="$(kubectl -n "$ns" get secret "nats-client-$component" -o jsonpath='{.data.password}' 2>/dev/null | base64 --decode || true)"
    if [[ -z "$password" ]]; then
      echo "==> Creating NATS credentials for $component"
      password="$(openssl rand -hex 32)"
    fi
    printf '%s' "$component" > "$tmp/user"
    printf '%s' "$password" > "$tmp/password"
    kubectl create secret generic "nats-client-$component" --namespace "$ns" \
      --from-file=user="$tmp/user" --from-file=password="$tmp/password" --from-file=ca.crt="$tmp/ca.crt" \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    passwords_json+="\"$component\":\"$password\","
  done
  passwords_json="${passwords_json%,}}"

  # The passwords go in on stdin, not the command line.
  NATS_CONFIG_HASH="$(python3 -c '
import hashlib, json, re, sys
passwords = json.load(sys.stdin)
conf = open(sys.argv[1]).read()
def fill(m):
    user = m.group(1)
    if user not in passwords:
        sys.exit("nats.conf.tmpl names a user with no credentials: " + user)
    return passwords[user]
conf = re.sub(r"@@password:([a-z-]+)@@", fill, conf)
open(sys.argv[2], "w").write(conf)
print(hashlib.sha256(conf.encode()).hexdigest()[:16])
' "k8s/nats/nats.conf.tmpl" "$tmp/nats.conf" <<< "$passwords_json")"
  kubectl create secret generic nats-server-config --namespace "$ns" \
    --from-file=nats.conf="$tmp/nats.conf" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  rm -rf "$tmp"
  trap - EXIT
}
