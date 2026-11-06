# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/deploy-servers.sh — deploying generic backend server folders
# (deploy_folder/deploy_category: platform servers, gardening servers,
# --deploy-server) and the AI-powered tools (incl. the host-side
# claude-bridge). Sourced by scripts/deploy.sh, never executed.

# -----------------------------------------------------------------------
# isolate_backend: lets only go-proxy reach a backend Deployment's pods, in
# a namespace deploy.sh doesn't otherwise manage. Backends trust they're
# reachable only through go-proxy (they don't check callers themselves),
# so a pod that could call one directly would skip go-proxy's
# authentication, access rules, guardrails, rate limits and audit.
# mcp-proxy, gardening-demo and demo-auth already have namespace-wide
# default-deny and allow-from-go-proxy policies; any other namespace gets
# this policy, scoped to the Deployment's own pods (its selector), not to
# the namespace, so other workloads sharing the namespace keep their
# traffic. Selecting the pods is enough to isolate them: a pod any
# NetworkPolicy selects accepts only what some policy allows.
# -----------------------------------------------------------------------
isolate_backend() {
  local namespace="$1" deployment="$2"
  case "$namespace" in
    "$PROXY_NAMESPACE"|"$GARDENING_NAMESPACE"|"$AUTH_NAMESPACE") return ;;
  esac
  local selector
  selector="$(kubectl -n "$namespace" get deployment "$deployment" -o jsonpath='{.spec.selector.matchLabels}')"
  echo "==> Letting only go-proxy reach deployment/$deployment's pods in namespace $namespace"
  python3 - "$deployment" "$selector" "$PROXY_NAMESPACE" <<'PY' | kubectl apply -n "$namespace" -f -
import json, sys
deployment, selector, proxy_ns = sys.argv[1], json.loads(sys.argv[2]), sys.argv[3]
print(json.dumps({
    "apiVersion": "networking.k8s.io/v1",
    "kind": "NetworkPolicy",
    "metadata": {"name": "allow-only-go-proxy-to-" + deployment},
    "spec": {
        "podSelector": {"matchLabels": selector},
        "policyTypes": ["Ingress"],
        "ingress": [{"from": [{
            "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": proxy_ns}},
            "podSelector": {"matchLabels": {"app": "go-proxy"}},
        }]}],
    },
}))
PY
}

# -----------------------------------------------------------------------
# deploy_folder: apply+scale+restart-if-needed for one arbitrary server
# folder, into the given namespace. Nothing here hardcodes a specific
# server name or namespace — works for any folder containing a buildable
# main.go (package main) directly in it and a non-empty k8s/ subdirectory
# with that server's Deployment+Service, deployed into whichever namespace
# the caller passes (every manifest in this repo's k8s/ directories is
# namespace-agnostic — see k8s/*/namespace.yaml for the Namespace objects
# themselves). Assumes the image was already built by ./scripts/build.sh —
# see require_image (scripts/lib/images.sh).
# -----------------------------------------------------------------------
deploy_folder() {
  local server_dir="$1" replicas="$2" namespace="$3"

  if [[ ! -d "$server_dir" ]]; then
    echo "Not a directory: $server_dir" >&2
    exit 1
  fi

  local abs_dir basename
  abs_dir="$(cd "$server_dir" && pwd)"
  basename="$(basename "$abs_dir")"

  # main.go is only needed for the local ":local" build path (Docker build
  # context for ./scripts/build.sh --build-server) — --release pulls
  # an already-published image and builds nothing from this folder's
  # source, so a deploy-bundle tarball (no Go source at all) can omit it.
  if [[ "$FROM_GIT_PACKAGE" == "false" && ! -f "$abs_dir/main.go" ]]; then
    echo "$server_dir does not contain a main.go" >&2
    exit 1
  fi
  if [[ ! -d "$abs_dir/k8s" ]] || [[ -z "$(ls -A "$abs_dir/k8s" 2>/dev/null)" ]]; then
    echo "$server_dir does not contain a non-empty k8s/ subdirectory" >&2
    exit 1
  fi

  # Still required even though this script no longer builds anything
  # itself: ./scripts/build.sh --build-server needs the same folder inside
  # the repo (its module code must be in the Docker build context), and the
  # image name/hint this function reports below assumes that build path.
  case "$abs_dir" in
    "$REPO_ROOT"/*) ;;
    *)
      echo "$server_dir must be inside the repo ($REPO_ROOT)" >&2
      exit 1
      ;;
  esac

  if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    require_ghcr_image "$basename"
    ensure_ghcr_pull_secret "$namespace"

    local manifest_files=() f
    for f in "$abs_dir"/k8s/*.yaml; do
      manifest_files+=("$f")
    done
    apply_ghcr_release "$namespace" "$basename" "Deployment" "${manifest_files[@]}"
    isolate_backend "$namespace" "$basename"

    echo "==> Scaling deployment/$basename to $replicas replicas"
    kubectl -n "$namespace" scale "deployment/$basename" --replicas="$replicas"
    kubectl -n "$namespace" rollout status "deployment/$basename"

    echo
    echo "==> Deployed $basename (image $(ghcr_image_ref "$basename")) into namespace $namespace with $replicas replicas."
    return
  fi

  local image_tag="${basename}:local"
  require_image "$image_tag" "--build-server $server_dir"

  echo "==> Applying $abs_dir/k8s/ into namespace $namespace"
  # Deliberately not "-o name" here: the default verb-annotated output
  # ("deployment.apps/foo created" vs "... configured"/"... unchanged") is
  # needed below to tell a just-created Deployment (already running the
  # image just built, no restart needed) apart from one that already
  # existed (may still be running an older image under the same local tag).
  local apply_output
  apply_output="$(kubectl apply -n "$namespace" -f "$abs_dir/k8s/")"
  echo "$apply_output"

  local deployments
  deployments="$(echo "$apply_output" | awk '{print $1}' | grep '^deployment\.apps/' | cut -d/ -f2)"
  if [[ -z "$deployments" ]]; then
    echo "No Deployment found in $abs_dir/k8s/ — nothing to scale" >&2
    exit 1
  fi

  local name
  for name in $deployments; do
    isolate_backend "$namespace" "$name"
    echo "==> Scaling deployment/$name to $replicas replicas"
    kubectl -n "$namespace" scale "deployment/$name" --replicas="$replicas"

    # "scale" alone is a no-op when the replica count hasn't changed, so it
    # won't recreate already-running pods after a local rebuild under this
    # image's unchanged tag. "rollout restart" forces every pod to be
    # recreated so a redeploy always picks up the freshly built image — but
    # only when the Deployment already existed: a just-created one's
    # first-generation pods already used the image built moments ago, so
    # restarting immediately would just be churn.
    if echo "$apply_output" | grep -q "^deployment\.apps/$name created\$"; then
      echo "==> deployment/$name was just created — already running the freshly built image, no restart needed"
    else
      echo "==> Restarting deployment/$name to pick up the freshly built image"
      kubectl -n "$namespace" rollout restart "deployment/$name"
    fi
    kubectl -n "$namespace" rollout status "deployment/$name"
  done

  echo
  echo "==> Deployed $basename (image $image_tag) into namespace $namespace with $replicas replicas."
}

# -----------------------------------------------------------------------
# deploy_category: deploy every qualifying server folder directly under a
# given parent directory, into the given namespace.
# -----------------------------------------------------------------------
deploy_category() {
  local parent_dir="$1" replicas="$2" namespace="$3"
  shopt -s nullglob
  local dir
  for dir in "$parent_dir"/*/; do
    if [[ ! -d "${dir}k8s" ]] || [[ -z "$(ls -A "${dir}k8s" 2>/dev/null)" ]]; then
      echo "==> Skipping $dir (no k8s/ subdirectory found)" >&2
      continue
    fi
    echo "==> Deploying backend server: $dir"
    deploy_folder "$dir" "$replicas" "$namespace"
  done
  shopt -u nullglob
}

# -----------------------------------------------------------------------
# start_claude_bridge: ensures a claude-bridge process is running on this
# host, generating a fresh shared-secret token whenever it actually starts
# one — never reused across restarts, never written into the repo (see
# CLAUDE.md's "AI-powered tool discovery" section). Idempotent: if a
# claude-bridge process from an earlier run is still alive and its token
# file still exists, this does nothing but confirm that, so deploy_ai_tools
# can read back the same token to (re)create the k8s Secret from.
# -----------------------------------------------------------------------
start_claude_bridge() {
  ensure_claude_bridge_dir
  local already_running=false pid
  if pid="$(claude_bridge_pid)"; then
    if [[ -f "$CLAUDE_BRIDGE_TOKEN_FILE" ]]; then
      already_running=true
    else
      # Alive but the token file is missing — inconsistent state (e.g.
      # someone deleted the token file by hand while the process kept
      # running). Kill the stale process and start fresh rather than
      # trying to recover a token we no longer have.
      echo "==> claude-bridge is running but its token file is missing — restarting"
      kill "$pid" 2>/dev/null || true
      rm -f "$CLAUDE_BRIDGE_PID_FILE"
    fi
  fi

  if [[ "$already_running" == "true" ]]; then
    echo "==> claude-bridge already running (pid $pid)"
    return
  fi

  echo "==> Building claude-bridge"
  go build -o "$CLAUDE_BRIDGE_BIN" ./claude-bridge

  echo "==> Generating a fresh claude-bridge token"
  (umask 077 && openssl rand -hex 32 > "$CLAUDE_BRIDGE_TOKEN_FILE")

  echo "==> Starting claude-bridge on port $CLAUDE_BRIDGE_PORT"
  (umask 077 && exec nohup "$CLAUDE_BRIDGE_BIN" -port ":$CLAUDE_BRIDGE_PORT" -token-file "$CLAUDE_BRIDGE_TOKEN_FILE" \
    > "$CLAUDE_BRIDGE_LOG_FILE" 2>&1 < /dev/null) &
  echo $! > "$CLAUDE_BRIDGE_PID_FILE"

  echo "==> Waiting for claude-bridge to become ready"
  local waited=0
  until curl -s -o /dev/null -m 1 "http://127.0.0.1:$CLAUDE_BRIDGE_PORT/healthz"; do
    sleep 1
    waited=$((waited + 1))
    if [[ "$waited" -ge 10 ]]; then
      echo "claude-bridge did not become ready within 10s — check $CLAUDE_BRIDGE_LOG_FILE" >&2
      exit 1
    fi
  done
  echo "==> claude-bridge is ready (pid $(cat "$CLAUDE_BRIDGE_PID_FILE"))"
}

# -----------------------------------------------------------------------
# deploy_ai_tools: ai-powered-tools' special path — Secret(s) + AI_MODEL/
# LLM_BASE_URL/USE_HEADLESS_CLAUDE env, which the generic deploy_category loop doesn't
# perform, so ai-powered-tools is never deployed via that loop. Lands in
# $SERVER_NAMESPACE (--namespace, default $PROXY_NAMESPACE) — the "other
# backend, no fixed namespace assumed" bucket, ensured to exist here since
# nothing else guarantees it does.
# -----------------------------------------------------------------------
deploy_ai_tools() {
  ensure_namespace "$SERVER_NAMESPACE"

  if [[ "$USE_HEADLESS_CLAUDE" == "true" ]]; then
    start_claude_bridge
    echo "==> Creating/updating ai-powered-tools-claude-bridge-token secret"
    kubectl create secret generic ai-powered-tools-claude-bridge-token \
      --namespace "$SERVER_NAMESPACE" \
      --from-file=token.txt="$CLAUDE_BRIDGE_TOKEN_FILE" \
      --dry-run=client -o yaml | kubectl apply -f -
  else
    echo "==> Creating/updating ai-powered-tools-api-key secret from $AI_API_KEY_FILE"
    kubectl create secret generic ai-powered-tools-api-key \
      --namespace "$SERVER_NAMESPACE" \
      --from-file=ai-api-key.txt="$AI_API_KEY_FILE" \
      --dry-run=client -o yaml | kubectl apply -f -
  fi

  echo "==> Deploying ai-powered-tools"
  deploy_folder "$AI_POWERED_TOOLS_DIR" 1 "$SERVER_NAMESPACE"

  # USE_HEADLESS_CLAUDE is set unconditionally (both "true" and "false") on
  # every run — otherwise redeploying *out* of headless mode later would
  # leave the previous deploy's "true" stuck on the live Deployment via
  # "kubectl set env"'s persistence.
  echo "==> Setting ai-powered-tools' USE_HEADLESS_CLAUDE to $USE_HEADLESS_CLAUDE"
  kubectl -n "$SERVER_NAMESPACE" set env deployment/ai-powered-tools USE_HEADLESS_CLAUDE="$USE_HEADLESS_CLAUDE"

  if [[ "$USE_HEADLESS_CLAUDE" == "false" ]]; then
    # The model choice is a deploy-time value, not something baked into the
    # checked-in k8s/deployment.yaml (which has no per-invocation templating
    # mechanism the way $CONFIG_FILE's ConfigMap does) — instead the
    # Deployment declares an AI_MODEL env var with a default, referenced from
    # `args` via k8s's own $(AI_MODEL) substitution, and "kubectl set env"
    # overrides it here.
    #
    # The LLM endpoint's default lives only in the server itself (Google
    # Gemini); the manifest doesn't set it. --ai-base-url overrides it via an
    # LLM_BASE_URL env var, and without the flag that var is removed
    # ("LLM_BASE_URL-"), so a URL from an earlier deploy never sticks. One
    # set env call either way, so one rollout.
    if [[ -n "$AI_BASE_URL" ]]; then
      echo "==> Setting ai-powered-tools' model to $AI_MODEL and LLM endpoint to $AI_BASE_URL"
      kubectl -n "$SERVER_NAMESPACE" set env deployment/ai-powered-tools AI_MODEL="$AI_MODEL" LLM_BASE_URL="$AI_BASE_URL"
    else
      echo "==> Setting ai-powered-tools' model to $AI_MODEL (default LLM endpoint)"
      kubectl -n "$SERVER_NAMESPACE" set env deployment/ai-powered-tools AI_MODEL="$AI_MODEL" LLM_BASE_URL-
    fi
  fi

  kubectl -n "$SERVER_NAMESPACE" rollout status deployment/ai-powered-tools
}
