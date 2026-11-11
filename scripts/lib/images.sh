# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/images.sh — container images for scripts/deploy.sh: checking
# that a local (:local) or GHCR (--release) image exists, the GHCR pull
# Secret, and apply_ghcr_release's image-rewriting Kustomize overlay.
# Sourced by deploy.sh, never executed. Reads deploy.sh's globals
# (FROM_GIT_PACKAGE, GITHUB_OWNER, RELEASE_VERSION, GITHUB_TOKEN_FILE).

# Name of the docker-registry Secret --release creates in every namespace
# it deploys into (ensure_ghcr_pull_secret) and patches into Deployments,
# when --github-token-file is given.
GHCR_PULL_SECRET_NAME="ghcr-pull-secret"

# -----------------------------------------------------------------------
# ghcr_uses_pull_secret: whether --release pulls with a token. The
# project's packages are public, so by default images are pulled
# anonymously: no ghcr.io login, no pull Secret, no imagePullSecrets.
# --github-token-file is for private packages (a fork's, for example):
# then every --release Deployment/Job pulls with it.
# -----------------------------------------------------------------------
ghcr_uses_pull_secret() {
  [[ -n "$GITHUB_TOKEN_FILE" ]]
}

# -----------------------------------------------------------------------
# require_image: fails fast, with a pointer to ./scripts/build.sh, if the
# given :local image isn't already present in the docker daemon this
# cluster's containerd reads from. deploy.sh no longer builds anything
# itself — every docker build/prune call that used to live here moved to
# scripts/build.sh / the root Makefile.
# -----------------------------------------------------------------------
require_image() {
  local image_tag="$1" build_hint="${2:-}"
  if ! docker image inspect "$image_tag" >/dev/null 2>&1; then
    echo "Image $image_tag not found. Build it first: ./scripts/build.sh${build_hint:+ $build_hint}" >&2
    exit 1
  fi
}

# -----------------------------------------------------------------------
# ghcr_image_ref: fully-qualified ghcr.io reference for a given image
# basename, using this invocation's --github-owner/--release.
# -----------------------------------------------------------------------
ghcr_image_ref() {
  local name="$1"
  echo "ghcr.io/$GITHUB_OWNER/$name:$RELEASE_VERSION"
}

# -----------------------------------------------------------------------
# require_ghcr_image: --release's counterpart to require_image —
# fails fast, pointing at ./scripts/build.sh --release, if the requested
# version isn't actually published for this image. Uses the ghcr.io login
# scripts/deploy.sh's validation block performed, if any (anonymous for
# public packages). Every ./scripts/
# build.sh --release run publishes ALL images under the same version (see
# the Makefile's publish-all), so this should never fail for a genuinely
# published version — a failure here means the wrong version was named, or
# it was never published at all.
# -----------------------------------------------------------------------
require_ghcr_image() {
  local name="$1" ref
  ref="$(ghcr_image_ref "$name")"
  if ! docker manifest inspect "$ref" >/dev/null 2>&1; then
    echo "Image $ref not found on ghcr.io. Check the version and --github-owner; publish it with ./scripts/build.sh --release $RELEASE_VERSION --github-token-file <PATH>." >&2
    if ! ghcr_uses_pull_secret; then
      echo "If the package is private, pass --github-token-file PATH (a GitHub token with read:packages)." >&2
    fi
    exit 1
  fi
}

# -----------------------------------------------------------------------
# ensure_ghcr_pull_secret: idempotent get-or-create for the
# kubernetes.io/dockerconfigjson imagePullSecret every --release
# Deployment in a given namespace needs when the packages are private
# (--github-token-file given); a no-op otherwise, since public packages
# pull anonymously. Same idempotent-apply idiom as ensure_namespace (scripts/lib/k8s.sh). Callers
# always invoke this only after the target namespace is already known to
# exist. Safe to call more than once per namespace per invocation (e.g.
# once per deploy_category loop iteration) — cheap and idempotent.
# -----------------------------------------------------------------------
ensure_ghcr_pull_secret() {
  local namespace="$1" tmpdir
  ghcr_uses_pull_secret || return 0
  # The registry credential is built into a private file and handed to
  # kubectl with --from-file, never as --docker-password on its command
  # line, where any local user could read the token from the process list.
  tmpdir="$(mktemp -d)"
  python3 - "$GITHUB_OWNER" "$GITHUB_TOKEN_FILE" "$tmpdir/.dockerconfigjson" <<'PY'
import base64, json, os, sys
owner, token_file, out = sys.argv[1], sys.argv[2], sys.argv[3]
token = open(token_file).read().strip()
auth = base64.b64encode(f"{owner}:{token}".encode()).decode()
fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump({"auths": {"ghcr.io": {"username": owner, "password": token, "auth": auth}}}, f)
PY
  kubectl create secret generic "$GHCR_PULL_SECRET_NAME" \
    --namespace "$namespace" \
    --type=kubernetes.io/dockerconfigjson \
    --from-file=.dockerconfigjson="$tmpdir/.dockerconfigjson" \
    --dry-run=client -o yaml | kubectl apply -f -
  rm -rf "$tmpdir"
}

# -----------------------------------------------------------------------
# apply_ghcr_release: renders a throwaway Kustomize overlay over the given
# manifest files — rewriting the named $kind's image to
# ghcr.io/$GITHUB_OWNER/<name>:$RELEASE_VERSION and, with a token, adding
# $GHCR_PULL_SECRET_NAME as an imagePullSecret — then applies the fully
# substituted result in one shot. Kustomize computes the final manifest
# client-side, so kubectl only ever sees the already-correct object: no
# window where it briefly declares the wrong image, and exactly one
# rollout generation per deploy. See CLAUDE.md's "Build and deploy" section.
#
# $kind is almost always "Deployment"; "Job" also works unchanged (used by
# seed_gardening_demo_if_needed in scripts/lib/deploy-gardening-demo.sh) since a Job's pod-template nesting
# is structurally identical to a Deployment's — only the Kustomize patch
# target's "kind" needs to name the right one.
#
# Assumes exactly one $kind object named $name across the given files, with
# a container also named $name — the same convention every other
# --release code path already assumes (confirmed true for every
# manifest in this repo).
#
# kubectl apply -k doesn't accept --load-restrictor (only the separate
# "kubectl kustomize" render subcommand does), which is required here since
# the overlay's own kustomization.yaml lives in a scratch tmpdir outside the
# repo tree it references — hence render-then-pipe rather than "apply -k"
# directly.
#
# manifest_files MUST be absolute paths: Kustomize resolves "resources:"
# relative to the kustomization.yaml's own directory (the scratch tmpdir
# this function creates), not the caller's cwd — a relative path here
# resolves against the wrong base and fails with a confusing "no such file
# or directory" from Kustomize itself, not from this script.
# -----------------------------------------------------------------------
apply_ghcr_release() {
  local namespace="$1" name="$2" kind="$3"; shift 3
  local manifest_files=("$@")
  local ghcr_ref
  ghcr_ref="$(ghcr_image_ref "$name")"

  local tmpdir
  tmpdir="$(mktemp -d)"

  {
    echo "apiVersion: kustomize.config.k8s.io/v1beta1"
    echo "kind: Kustomization"
    echo "resources:"
    local f
    for f in "${manifest_files[@]}"; do
      echo "  - $f"
    done
    echo "images:"
    echo "  - name: $name"
    echo "    newName: ghcr.io/$GITHUB_OWNER/$name"
    echo "    newTag: $RELEASE_VERSION"
    if ghcr_uses_pull_secret; then
      echo "patches:"
      echo "  - target:"
      echo "      kind: $kind"
      echo "      name: $name"
      echo "    patch: |-"
      echo "      - op: add"
      echo "        path: /spec/template/spec/imagePullSecrets"
      echo "        value:"
      echo "          - name: $GHCR_PULL_SECRET_NAME"
    fi
  } > "$tmpdir/kustomization.yaml"

  echo "==> Applying $name via Kustomize overlay (image $ghcr_ref)"
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$tmpdir" | kubectl apply -n "$namespace" -f -
  rm -rf "$tmpdir"
}
