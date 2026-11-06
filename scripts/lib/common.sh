# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/common.sh — constants shared by every script under scripts/
# (deploy.sh, build.sh, cleanup-k8s-cluster.sh, observe-k8s.sh) and small
# pure helpers, so values like the namespace names are defined exactly once.
# Sourced, never executed; defines constants and functions only, no other
# top-level side effects, and inherits the caller's shell options.

# Three fixed namespaces, not overridable by --namespace: go-proxy itself
# and every platform-mcp-servers/* backend need to share a namespace with
# go-proxy regardless (their ConfigMap mounts, seeded by go-proxy's own
# configsync, only work same-namespace — see CLAUDE.md's "Unify
# configuration into a single config-map mechanism"), and
# go-gardening-demo/servers/*, demo-authorization-server are each their own
# fixed, self-contained deployable unit. See k8s/*/namespace.yaml for the
# Namespace objects themselves.
PROXY_NAMESPACE="mcp-proxy"
GARDENING_NAMESPACE="gardening-demo"
AUTH_NAMESPACE="demo-auth"

# claude-bridge (see CLAUDE.md's "AI-powered tool discovery" section) runs
# directly on this host, never in k8s. Its files live outside the repo (so
# nothing about it needs a .gitignore entry), in a directory only this user
# can enter (ensure_claude_bridge_dir): at a fixed shared path like /tmp,
# another local user could read the token, plant a PID file to make the
# scripts kill a process of their choosing, or swap the binary.
CLAUDE_BRIDGE_PORT=8099
CLAUDE_BRIDGE_DIR="${XDG_RUNTIME_DIR:-$HOME/.cache}/go-proxy-claude-bridge"
CLAUDE_BRIDGE_BIN="$CLAUDE_BRIDGE_DIR/claude-bridge"
CLAUDE_BRIDGE_PID_FILE="$CLAUDE_BRIDGE_DIR/claude-bridge.pid"
CLAUDE_BRIDGE_LOG_FILE="$CLAUDE_BRIDGE_DIR/claude-bridge.log"
CLAUDE_BRIDGE_TOKEN_FILE="$CLAUDE_BRIDGE_DIR/token.txt"

# ensure_claude_bridge_dir creates CLAUDE_BRIDGE_DIR mode 0700, refusing one
# that's a symlink or isn't this user's.
ensure_claude_bridge_dir() {
  mkdir -p "$CLAUDE_BRIDGE_DIR"
  if [[ -L "$CLAUDE_BRIDGE_DIR" || ! -O "$CLAUDE_BRIDGE_DIR" ]]; then
    echo "$CLAUDE_BRIDGE_DIR is a symlink or isn't yours; remove it and retry" >&2
    exit 1
  fi
  chmod 700 "$CLAUDE_BRIDGE_DIR"
}

# claude_bridge_pid prints the PID in CLAUDE_BRIDGE_PID_FILE if that process
# is alive and is this user's claude-bridge - never some other process that
# later reused the PID.
claude_bridge_pid() {
  local pid
  [[ -f "$CLAUDE_BRIDGE_PID_FILE" ]] || return 1
  pid="$(cat "$CLAUDE_BRIDGE_PID_FILE")"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  [[ "$(ps -p "$pid" -o user= 2>/dev/null | tr -d ' ')" == "$(id -un)" ]] || return 1
  [[ "$(ps -p "$pid" -o command= 2>/dev/null)" == "$CLAUDE_BRIDGE_BIN "* ]] || return 1
  echo "$pid"
}

# demo-authorization-server (see demo-authorization-server/main.go) is
# deployed into k8s like any other server, via scripts/deploy.sh's --deploy-auth-server.
# This constant is still needed here: for its Service's fixed LoadBalancer
# port, and for the localhost URL a human running go-gardening-demo/cli.sh
# by hand logs into (go-gardening-demo's own automatic seeding no longer
# uses this at all — it runs in-cluster now, via demo-authorization-server's
# in-cluster Service DNS name instead).
DEMO_AUTH_PORT=8102

# All three fixed namespaces, for the scripts that act on every namespace
# this repo manages (cleanup-k8s-cluster.sh, observe-k8s.sh).
MANAGED_NAMESPACES=("$PROXY_NAMESPACE" "$GARDENING_NAMESPACE" "$AUTH_NAMESPACE")

# github_owner_from_url / github_repo_from_url URL: the owner / repository
# name of a GitHub remote URL (git@github.com:OWNER/REPO.git,
# https://github.com/OWNER/REPO(.git), ssh://git@github.com/OWNER/REPO.git),
# as used to infer --github-owner/--github-repo from "git remote get-url
# origin" (build.sh, deploy.sh). Print nothing useful for a non-GitHub URL;
# callers treat an empty result as "couldn't infer".
github_owner_from_url() {
  echo "$1" | sed -E 's#^(git@github\.com:|https://github\.com/|ssh://git@github\.com/)##; s#/.*$##'
}

github_repo_from_url() {
  echo "$1" | sed -E 's#^(git@github\.com:|https://github\.com/|ssh://git@github\.com/)##; s#^[^/]+/##; s#\.git$##'
}
