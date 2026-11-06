#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Build step of the two-step build/deploy workflow (see scripts/deploy.sh's
# own header for the deploy half, and CLAUDE.md's "Build and deploy" section
# for the full rationale). Builds every docker image (tag ":local", same
# scheme as before this split) and this repo's two CLI binaries (bin/cli,
# go-gardening-demo/bin/cli) via the root Makefile's per-target incremental
# staleness tracking — a transitive Go-source dependency check (go list
# -deps), not a naive per-directory one, so editing a shared package like
# relayheaders correctly rebuilds every image that imports it, while editing
# one server only rebuilds that server's own image.
#
# scripts/deploy.sh no longer builds anything itself: run this first for
# whatever you're about to deploy. deploy.sh fails fast, pointing back here,
# if an image/binary it needs isn't already present.
#
# No target-selection flags on purpose: since staleness is tracked
# correctly, "build everything" is a cheap, side-effect-free, idempotent
# default (unlike deploy.sh's own flags, which gate real cluster mutations
# like pod restarts and so are worth keeping deliberate/explicit). Use
# --build-server for an ad hoc folder not in the static set, or run `make
# build-proxy`/`build-platform-servers`/`build-ai`/`build-gardening-demo`/
# `build-auth-server` directly if you only want one category.
#
# --release VERSION additionally publishes every image to a private GHCR
# package (ghcr.io/<owner>/<name>:<version>) — see CLAUDE.md's "Build and
# deploy" section, and scripts/deploy.sh's --release flag for the
# consuming half. Every image tag remains local-daemon-only (":local") too;
# publishing is additive, not a replacement.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Shared helpers, here github_owner_from_url/github_repo_from_url for
# --release's owner/repo inference — see scripts/lib/common.sh.
# shellcheck source=lib/common.sh
source "$REPO_ROOT/scripts/lib/common.sh"

SERVER_DIR=""
DO_CLEAN=false
DO_ALL=true
RELEASE_VERSION=""
GITHUB_OWNER=""
GITHUB_REPO=""
GITHUB_TOKEN_FILE=""

# One GOOS/GOARCH pair per target *deployer-machine* platform a
# --release deploy-bundle tarball is cross-compiled/published for — NOT the
# k8s cluster's own node architecture, which is a separate, unrelated axis
# (the published images are arm64-only, deliberately, regardless of this
# list): darwin/arm64 for an Apple Silicon Mac deployer, linux/arm64 for an
# arm64 Linux deployer (e.g. running deploy.sh/cli from inside a Linux VM or
# container rather than directly on macOS) — macOS can't execute a Linux ELF
# binary at all (confirmed the hard way), so this list must track the
# deployer's real OS, never assume one. Adding a further platform later is
# purely additive.
RELEASE_PLATFORMS=("darwin/arm64" "linux/arm64")

usage() {
  cat <<EOF
Usage: $0 [options]

With no options, builds every docker image and both CLI binaries (bin/cli,
go-gardening-demo/bin/cli) that need building — safe and cheap to run
repeatedly; already-up-to-date targets are skipped (see the Makefile).

Options:
  --build-server PATH   Also build one arbitrary server folder (must contain
                         main.go + a non-empty k8s/, same validation
                         scripts/deploy.sh's --deploy-server uses) into
                         "\$(basename PATH):local" — independent of the
                         normal target set, can be combined with it.
  --clean                Remove build artifacts (.build/ stamps, bin/,
                         go-gardening-demo/bin/) before building. Can be
                         passed alone to just clean and exit.
  --release VERSION      Publish this build as a private release: requires a
                         clean working tree, creates+pushes a git tag named
                         VERSION at HEAD (unless it already points there),
                         tags+pushes EVERY image (regardless of whether it
                         needed a rebuild this run) to
                         ghcr.io/<owner>/<name>:VERSION — a given version is
                         always a complete, deployable set — and publishes a
                         "deploy-bundle-VERSION-<goos>-<arch>.tar.gz" as a
                         GitHub Release asset on that same tag for each
                         platform in RELEASE_PLATFORMS (today darwin/arm64
                         and linux/arm64): scripts/deploy.sh + every k8s
                         manifest/config file it needs + freshly
                         cross-compiled CLI binaries, no Go source at all —
                         see CLAUDE.md's "Build and deploy" section. Prints
                         the Release page URL to download it from at the
                         end. Requires --github-token-file. Combine with
                         --build-server to also publish that one ad hoc
                         image under the same version.

                         Docker images are built natively (plain "docker
                         build", no buildx/QEMU) — they only ever come out
                         in the host machine's own architecture. So this
                         script detects it ("go env GOARCH") and, when
                         running on an amd64 host, automatically republishes
                         as "VERSION.amd64" instead of VERSION — a distinct
                         tag/git-tag/GitHub-Release from an arm64 build of
                         the same VERSION, with darwin/amd64+linux/amd64
                         tarballs instead of the arm64 pair — so an amd64
                         build can never silently overwrite an arm64 one (or
                         vice versa) under the same version string. Deploy
                         with "--release VERSION" on an arm64 cluster/CLI
                         machine, "--release VERSION.amd64" on an amd64 one.
                         A host that's neither arm64 nor amd64 is a hard
                         error for --release (plain, no-release builds are
                         unaffected either way).
  --github-owner NAME    GHCR/GitHub owner to publish under (default:
                         inferred from "git remote get-url origin").
                         Only used together with --release.
  --github-repo NAME     GitHub repository name to create the Release in
                         (default: inferred from "git remote get-url
                         origin", e.g. "enterprise-go-mcp-proxy"). Only
                         used together with --release.
  --github-token-file PATH
                         Local file containing a GitHub PAT with
                         write:packages + read:packages + repo scope (repo
                         is needed to create the Release and upload its
                         deploy-bundle tarball assets). Required together
                         with --release.
  -h, --help             Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --build-server) SERVER_DIR="$2"; shift 2 ;;
    --clean) DO_CLEAN=true; shift ;;
    --release) RELEASE_VERSION="$2"; shift 2 ;;
    --github-owner) GITHUB_OWNER="$2"; shift 2 ;;
    --github-repo) GITHUB_REPO="$2"; shift 2 ;;
    --github-token-file) GITHUB_TOKEN_FILE="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

if [[ -n "$RELEASE_VERSION" ]]; then
  # Docker images are built with plain "docker build" (no buildx/QEMU) —
  # they always come out in whichever architecture this host natively is.
  # Detect it and, on amd64, republish under a distinct "VERSION.amd64"
  # version/tag (images, git tag, GitHub Release, tarballs all derive from
  # $RELEASE_VERSION below, so this one substitution covers everything) —
  # otherwise an amd64 build and an arm64 build of the same VERSION would
  # silently overwrite each other's images under the identical GHCR tag.
  host_arch="$(go env GOARCH)"
  case "$host_arch" in
    arm64) ;; # today's default target — no change
    amd64)
      RELEASE_VERSION="$RELEASE_VERSION.amd64"
      RELEASE_PLATFORMS=("darwin/amd64" "linux/amd64")
      echo "==> Building on an amd64 host — publishing as $RELEASE_VERSION (darwin/amd64 + linux/amd64 tarballs) instead of the arm64 default, so the two architectures never collide under one version"
      ;;
    *)
      echo "--release is only supported when building on an arm64 or amd64 host (detected: $host_arch)" >&2
      exit 1
      ;;
  esac

  if [[ -z "$GITHUB_TOKEN_FILE" ]]; then
    echo "--release requires --github-token-file PATH (a GitHub PAT with write:packages + read:packages + repo scope)" >&2
    exit 1
  fi
  if [[ ! -f "$GITHUB_TOKEN_FILE" ]]; then
    echo "GitHub token file not found at $GITHUB_TOKEN_FILE (from --github-token-file)" >&2
    exit 1
  fi

  if [[ -z "$GITHUB_OWNER" || -z "$GITHUB_REPO" ]]; then
    origin_url="$(git remote get-url origin 2>/dev/null || true)"
    if [[ -z "$GITHUB_OWNER" ]]; then
      GITHUB_OWNER="$(github_owner_from_url "$origin_url")"
      if [[ -z "$GITHUB_OWNER" ]]; then
        echo "Could not infer --github-owner from 'git remote get-url origin' ($origin_url) — pass --github-owner NAME explicitly" >&2
        exit 1
      fi
      echo "==> Inferred --github-owner=$GITHUB_OWNER from git remote 'origin'"
    fi
    if [[ -z "$GITHUB_REPO" ]]; then
      GITHUB_REPO="$(github_repo_from_url "$origin_url")"
      if [[ -z "$GITHUB_REPO" ]]; then
        echo "Could not infer --github-repo from 'git remote get-url origin' ($origin_url) — pass --github-repo NAME explicitly" >&2
        exit 1
      fi
      echo "==> Inferred --github-repo=$GITHUB_REPO from git remote 'origin'"
    fi
  fi

  if ! [[ "$RELEASE_VERSION" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
    echo "--release VERSION '$RELEASE_VERSION' is not a valid docker tag (must match ^[A-Za-z0-9_][A-Za-z0-9_.-]*\$, <=128 chars)" >&2
    exit 1
  fi

  if [[ -n "$(git status --porcelain)" ]]; then
    echo "--release requires a clean working tree (a release must represent one exact, reproducible commit) — commit or stash your changes first" >&2
    exit 1
  fi

  existing_tag_commit="$(git rev-parse -q --verify "refs/tags/$RELEASE_VERSION" 2>/dev/null || true)"
  head_commit="$(git rev-parse HEAD)"
  if [[ -z "$existing_tag_commit" ]]; then
    echo "==> Creating and pushing git tag $RELEASE_VERSION at $head_commit"
    git tag "$RELEASE_VERSION"
    git push origin "$RELEASE_VERSION"
  elif [[ "$existing_tag_commit" == "$head_commit" ]]; then
    echo "==> git tag $RELEASE_VERSION already points at HEAD ($head_commit) — re-publishing the same commit"
  else
    echo "git tag $RELEASE_VERSION already exists and points at a different commit ($existing_tag_commit, not HEAD's $head_commit) — choose a new --release version or move/delete the tag deliberately" >&2
    exit 1
  fi
fi

if [[ "$DO_CLEAN" == "true" ]]; then
  echo "==> Cleaning build artifacts (.build/ stamps, bin/, go-gardening-demo/bin/)"
  make clean
fi

if [[ -n "$SERVER_DIR" ]]; then
  DO_ALL=false
fi
# --clean alone (no --build-server) just cleans and exits; the normal build
# still runs by default whenever --build-server wasn't the only thing asked
# for, matching "no flags = build everything".
if [[ "$DO_CLEAN" == "true" && -z "$SERVER_DIR" ]]; then
  echo
  echo "==> Done."
  exit 0
fi

if [[ -n "$RELEASE_VERSION" ]]; then
  echo "==> Logging in to ghcr.io as $GITHUB_OWNER"
  docker login ghcr.io -u "$GITHUB_OWNER" --password-stdin < "$GITHUB_TOKEN_FILE"
fi

MAKE_PUBLISH_ARGS=()
if [[ -n "$RELEASE_VERSION" ]]; then
  MAKE_PUBLISH_ARGS=(GHCR_OWNER="$GITHUB_OWNER" RELEASE_VERSION="$RELEASE_VERSION")
fi

echo "==> Pruning dangling Docker images"
make prune

if [[ "$DO_ALL" == "true" ]]; then
  if [[ -n "$RELEASE_VERSION" ]]; then
    make "${MAKE_PUBLISH_ARGS[@]}" all publish-all
  else
    make all
  fi
fi

if [[ -n "$SERVER_DIR" ]]; then
  # Same validation scripts/deploy.sh's deploy_folder() applies today.
  if [[ ! -d "$SERVER_DIR" ]]; then
    echo "Not a directory: $SERVER_DIR" >&2
    exit 1
  fi
  abs_dir="$(cd "$SERVER_DIR" && pwd)"
  basename="$(basename "$abs_dir")"
  if [[ ! -f "$abs_dir/main.go" ]]; then
    echo "$SERVER_DIR does not contain a main.go" >&2
    exit 1
  fi
  case "$abs_dir" in
    "$REPO_ROOT"/*) rel_path="${abs_dir#"$REPO_ROOT"/}" ;;
    *)
      echo "$SERVER_DIR must be inside the repo ($REPO_ROOT) so its module code is in the Docker build context" >&2
      exit 1
      ;;
  esac
  if [[ -n "$RELEASE_VERSION" ]]; then
    make "${MAKE_PUBLISH_ARGS[@]}" ADHOC_NAME="$basename" ADHOC_BUILD_PATH="$rel_path" adhoc-image "publish-$basename"
  else
    make ADHOC_NAME="$basename" ADHOC_BUILD_PATH="$rel_path" adhoc-image
  fi
fi

if [[ -n "$RELEASE_VERSION" ]]; then
  # Deploy-bundle tarballs: scripts/deploy.sh + every k8s manifest/config
  # file it needs + freshly cross-compiled CLI binaries, no Go source at
  # all — published as GitHub Release assets on the same tag above, so a
  # deployer never needs git or a Go toolchain. A curated, hardcoded list,
  # not a directory scan: deciding which server directories belong in a
  # public deploy artifact is exactly the kind of one-time, reviewable
  # decision that should require a conscious addition when a new service is
  # added, not silent inclusion via glob.
  BUNDLE_PATHS=(
    scripts/deploy.sh scripts/cli.sh scripts/cleanup-k8s-cluster.sh scripts/observe-k8s.sh
    scripts/lib
    k8s
    platform-mcp-servers/declarative-guardrail/k8s
    platform-mcp-servers/event-aggregator/k8s
    platform-mcp-servers/rate-limiter/k8s
    ai-mcp-servers/ai-powered-tools/k8s
    demo-authorization-server/k8s
    go-gardening-demo/cli.sh
    go-gardening-demo/k8s
    go-gardening-demo/seed/k8s
    go-gardening-demo/servers/garden-business-ops/k8s
    go-gardening-demo/servers/garden-customer-admin/k8s
    go-gardening-demo/servers/garden-customer-service/k8s
    go-gardening-demo/servers/garden-online-store/k8s
    go-gardening-demo/servers/garden-staff-admin/k8s
    go-gardening-demo/initial-data
    go-gardening-demo/gardening-demo.cfg
    proxy-server.cfg
    docs/enterprise-deployment
    examples/discovery
  )

  release_scratch="$(mktemp -d)"
  # Removed however the release ends: it ends up holding the GitHub token's
  # header file.
  trap 'rm -rf "$release_scratch"' EXIT
  bundle_assets=()
  for platform in "${RELEASE_PLATFORMS[@]}"; do
    goos="${platform%/*}"; goarch="${platform#*/}"
    bundle_name="deploy-bundle-$RELEASE_VERSION-$goos-$goarch"
    bundle_dir="$release_scratch/$bundle_name"
    mkdir -p "$bundle_dir"

    # From the commit being released (git archive), not the working tree:
    # a file git ignores - a key, a local cfg - sitting in one of these
    # directories must never end up in a public release asset.
    git archive HEAD -- "${BUNDLE_PATHS[@]}" | tar -x -C "$bundle_dir"

    echo "==> Cross-compiling CLIs for $goos/$goarch"
    mkdir -p "$bundle_dir/bin" "$bundle_dir/go-gardening-demo/bin"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -o "$bundle_dir/bin/cli" ./cli
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -o "$bundle_dir/go-gardening-demo/bin/cli" ./go-gardening-demo/cli

    printf '%s\n' "$RELEASE_VERSION" > "$bundle_dir/VERSION"

    echo "==> Packaging $bundle_name.tar.gz"
    tar -C "$release_scratch" -czf "$release_scratch/$bundle_name.tar.gz" "$bundle_name"
    bundle_assets+=("$bundle_name.tar.gz")
  done

  echo "==> Ensuring GitHub Release $RELEASE_VERSION exists on $GITHUB_OWNER/$GITHUB_REPO"
  # The token goes to curl in a header file (-H @file), never on its command
  # line, where any local user could read it from the process list. The
  # file is in release_scratch (mktemp -d, mode 0700), removed below.
  auth_header_file="$release_scratch/auth-header"
  (umask 077 && printf 'Authorization: Bearer %s\n' "$(cat "$GITHUB_TOKEN_FILE")" > "$auth_header_file")
  release_api="https://api.github.com/repos/$GITHUB_OWNER/$GITHUB_REPO/releases"

  resp="$(curl -sS -w '\n%{http_code}' -H @"$auth_header_file" \
    -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" \
    "$release_api/tags/$RELEASE_VERSION")"
  http_code="$(tail -n1 <<<"$resp")"
  body="$(sed '$d' <<<"$resp")"

  if [[ "$http_code" == "200" ]]; then
    echo "==> GitHub Release $RELEASE_VERSION already exists — reusing it"
  elif [[ "$http_code" == "404" ]]; then
    echo "==> Creating GitHub Release $RELEASE_VERSION"
    # Built as its own variable, not nested inline as another command's
    # argument ("curl ... -d "$(python3 ...)"") — macOS's ancient system
    # bash (3.2.57, the only "bash" on a plain Mac; confirmed empirically,
    # see CLAUDE.md's "Build and deploy" section) mis-parses a python3 dict
    # literal (braces/colons/commas) nested that deeply alongside several
    # other double-quoted arguments on the same command, splitting it into
    # multiple broken python3 invocations. A plain variable reference has
    # no such issue.
    release_body="$(python3 -c "import json,sys; print(json.dumps({'tag_name': sys.argv[1], 'name': sys.argv[1], 'body': 'Published by ./scripts/build.sh --release.'}))" "$RELEASE_VERSION")"
    resp="$(curl -sS -w '\n%{http_code}' -X POST -H @"$auth_header_file" \
      -H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28" \
      "$release_api" \
      -d "$release_body")"
    http_code="$(tail -n1 <<<"$resp")"
    body="$(sed '$d' <<<"$resp")"
    if [[ "$http_code" != "201" ]]; then
      echo "Failed to create GitHub Release $RELEASE_VERSION (HTTP $http_code): $body" >&2
      exit 1
    fi
  else
    echo "Failed to look up GitHub Release $RELEASE_VERSION (HTTP $http_code): $body" >&2
    exit 1
  fi

  release_id="$(python3 -c "import json,sys; print(json.load(sys.stdin)['id'])" <<<"$body")"
  upload_url="$(python3 -c "import json,sys; print(json.load(sys.stdin)['upload_url'])" <<<"$body")"
  upload_url="${upload_url%%\{*}"

  for asset_name in "${bundle_assets[@]}"; do
    tarball_path="$release_scratch/$asset_name"

    existing_id="$(curl -sS -H @"$auth_header_file" -H "Accept: application/vnd.github+json" \
      "$release_api/$release_id/assets" | python3 -c "
import json, sys
name = sys.argv[1]
for a in json.load(sys.stdin):
    if a['name'] == name:
        print(a['id'])
        break
" "$asset_name")"
    if [[ -n "$existing_id" ]]; then
      echo "==> Replacing existing asset $asset_name (id $existing_id)"
      curl -sS -X DELETE -H @"$auth_header_file" -H "Accept: application/vnd.github+json" \
        "$release_api/assets/$existing_id" >/dev/null
    fi

    echo "==> Uploading $asset_name"
    resp="$(curl -sS -w '\n%{http_code}' -X POST -H @"$auth_header_file" \
      -H "Content-Type: application/gzip" --data-binary "@$tarball_path" \
      "$upload_url?name=$asset_name")"
    http_code="$(tail -n1 <<<"$resp")"
    if [[ "$http_code" != "201" ]]; then
      echo "Failed to upload asset $asset_name (HTTP $http_code)" >&2
      exit 1
    fi
  done

  rm -rf "$release_scratch"
fi

echo
if [[ -n "$RELEASE_VERSION" ]]; then
  echo "==> Done. Published release $RELEASE_VERSION to ghcr.io/$GITHUB_OWNER."
  echo "    Deploy-bundle tarball(s) for a deployer to download:"
  echo "      https://github.com/$GITHUB_OWNER/$GITHUB_REPO/releases/tag/$RELEASE_VERSION"
  echo "    (open in a browser you're already logged into as the repo owner — click the"
  echo "    deploy-bundle-$RELEASE_VERSION-<goos>-<arch>.tar.gz asset to download it directly,"
  echo "    no token/curl needed for a same-account download; see CLAUDE.md's \"Build and"
  echo "    deploy\" section for the scripted curl-based alternative for a non-owner deployer)."
else
  echo "==> Done. Built images/binaries are ready for ./scripts/deploy.sh."
fi
