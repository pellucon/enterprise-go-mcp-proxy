#!/usr/bin/env bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Deploy step of the two-step build/deploy workflow — single entry point for
# deploying any part of this system to the local k3s cluster: the proxy
# itself, and/or any of the three backend-server categories
# (platform-mcp-servers/, ai-mcp-servers/, go-gardening-demo/servers/),
# and/or one arbitrary server folder. Replaces the former
# deploy-proxy-server.sh / deploy-go-mcp-server.sh / deploy-all.sh — see
# CLAUDE.md's "Commands" section.
#
# This script no longer builds anything itself: by default it assumes every
# docker image/CLI binary it needs was already built by ./scripts/build.sh
# (into the same docker daemon this cluster's containerd reads from — true
# by default for Colima's `colima start --kubernetes`) and fails fast,
# pointing back at build.sh, if an expected image isn't present — see
# require_image (scripts/lib/images.sh) and CLAUDE.md's "Build and deploy" section.
#
# Passing --release VERSION switches to a different source entirely: images
# published by ./scripts/build.sh --release VERSION to GHCR (public
# packages pull anonymously; --github-token-file for private ones), pulled into whichever cluster kubectl is currently configured
# against — not necessarily this repo's own local Colima/k3s. --release is
# mandatory for this path (no auto-detection from a git tag/VERSION file —
# removed on purpose, so a version is never guessed, only ever stated). See
# require_ghcr_image/ensure_ghcr_pull_secret (scripts/lib/images.sh), and CLAUDE.md's "Build
# and deploy" section for the full deployer workflow and generic-cluster
# compatibility notes.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# Set when running from a real git checkout — false when running from an
# extracted deploy-bundle tarball (./scripts/build.sh --release), which has
# no .git at all. Used below to infer --github-owner from "git remote
# get-url origin" when possible, and to require it explicitly otherwise.
HAS_GIT_REPO=false
if [[ -d "$REPO_ROOT/.git" ]]; then
  HAS_GIT_REPO=true
fi

# Shared constants (namespaces, ports, claude-bridge paths) and every
# function this script calls live in sourced libraries under scripts/lib/
# (see docs/architecture/build-deploy.md, "Script layout"). They only
# define constants and functions; nothing runs until the main flow below.
for lib in common images k8s deploy-servers deploy-proxy deploy-auth deploy-gardening-demo; do
  # shellcheck source=/dev/null
  source "$REPO_ROOT/scripts/lib/$lib.sh"
done

# Namespace for the two flexible targets that make no fixed-namespace
# assumption at all: ai-mcp-servers/ai-powered-tools (--deploy-ai) and an
# arbitrary folder (--deploy-server). Defaults to $PROXY_NAMESPACE purely
# for convenience (matches this repo's historical single-namespace
# behavior) but is fully overridable via --namespace — no NetworkPolicy or
# RBAC in this repo is tailored to wherever this namespace actually is.
SERVER_NAMESPACE="mcp-proxy"

PROXY_REPLICAS=1
REPLICAS=1
CONFIG_FILE="proxy-server.cfg"
AI_API_KEY_FILE=""
AI_MODEL=""
AI_BASE_URL=""
USE_HEADLESS_CLAUDE=false
AI_POWERED_TOOLS_DIR="ai-mcp-servers/ai-powered-tools"

# go-gardening-demo/servers/garden-customer-admin and garden-staff-admin
# each sync a user's roles to an "auth server" (see their own -auth-server-*
# flags — go-gardening-demo's own code never hardcodes which one, or calls
# it "demo"). Default targets demo-authorization-server's own in-cluster
# Service DNS name (it's a k8s Deployment now, not a host process), in its
# own fixed $AUTH_NAMESPACE — this is a separate, overridable value (never
# baked into either server's checked-in k8s/deployment.yaml, which instead
# declares an AUTH_SERVER_URL env var with this same default, referenced
# from `args` via k8s's own $(AUTH_SERVER_URL) substitution — see
# deploy_category's gardening-demo block below) so a deployment can point
# them at a different, real auth server entirely. Unlike before, this no
# longer needs deferred resolution after option parsing: $AUTH_NAMESPACE is
# a fixed constant, not something --namespace can override.
AUTH_SERVER_URL="http://demo-authorization-server.$AUTH_NAMESPACE.svc.cluster.local:$DEMO_AUTH_PORT"

DO_PROXY=false
DO_PLATFORM=false
DO_AI=false
DO_GARDENING=false
DO_AUTH_SERVER=false
# Set only by --deploy-all/-all, never by an explicit standalone --deploy-ai
# — distinguishes "give me everything, best effort" from "I specifically
# asked for AI tools" below, since only the former should tolerate missing
# AI credentials by skipping ai-powered-tools rather than failing.
DEPLOY_ALL=false
SERVER_DIR=""

FROM_GIT_PACKAGE=false
RELEASE_VERSION=""
GITHUB_OWNER=""
GITHUB_TOKEN_FILE=""

# Enterprise exposure path (see docs/enterprise-deployment/enterprise-deployment.md). Empty
# INGRESS_HOST (the default) means the plain exposure model: a LoadBalancer
# on :80 on a local cluster, ClusterIP elsewhere (see PUBLIC_LOAD_BALANCER
# below). Setting it instead applies go-proxy's Deployment/Service through
# k8s/overlays/ingress/ (a ClusterIP Service plus an Ingress for this host)
# and skips every LoadBalancer wait.
INGRESS_HOST=""
INGRESS_CLASS="nginx"

# go-proxy's and demo-authorization-server's Services are ClusterIP unless
# the cluster is local (LOCAL_CLUSTER, set after argument parsing), where
# they're made LoadBalancers so they answer on localhost. On any other
# cluster a LoadBalancer gets a public address, so go-proxy gets one only
# with --public-load-balancer, and demo-authorization-server never does.
PUBLIC_LOAD_BALANCER=false
TLS_SECRET=""

# Opt-in egress lockdown for go-proxy (apply_egress_policy in
# scripts/lib/deploy-proxy.sh): restrict what go-proxy's pods may connect
# to. EGRESS_ALLOW holds the extra off-cluster destinations
# (--egress-allow CIDR[:PORT]).
EGRESS_POLICIES=false
EGRESS_ALLOW=()

# cert-manager ClusterIssuer for go-proxy's Ingress (--cluster-issuer, only
# with --ingress-host). Empty (default): TLS comes from --tls-secret, or is
# terminated in front of the ingress controller.
CLUSTER_ISSUER=""

usage() {
  cat <<EOF
Usage: $0 [targets] [options]

Targets (combinable — pass any subset; at least one is required):
  --deploy-proxy, -dpr              Deploy only the proxy itself (namespace,
                                     RBAC, NetworkPolicies, ConfigMaps/Secrets,
                                     go-proxy Deployment/Service, NATS) into
                                     the fixed $PROXY_NAMESPACE namespace
  --deploy-platform-servers, -dpl   Deploy every server under
                                     platform-mcp-servers/ (event-aggregator —
                                     NATS-dependent — declarative-guardrail
                                     and rate-limiter, which aren't) into the
                                     fixed $PROXY_NAMESPACE namespace
  --deploy-ai, -dai                 Deploy ai-mcp-servers/ai-powered-tools
                                     into --namespace (default: $SERVER_NAMESPACE)
                                     (requires --ai-api-key-file and
                                     --ai-model, or --use-headless-claude
                                     instead)
  --deploy-gardening-demo, -dgd     Deploy every server under
                                     go-gardening-demo/servers/ into the
                                     fixed $GARDENING_NAMESPACE namespace (one
                                     pod each, regardless of --replicas — see
                                     go-gardening-demo/README.md), then seed
                                     them with go-gardening-demo/initial-data
                                     via a one-shot in-cluster Job
                                     (gardening-demo-seed) — created once per
                                     cluster and left alone on every later
                                     deploy (delete the Job to have the next
                                     deploy reseed automatically; see
                                     CLAUDE.md). Works identically whether or
                                     not --release is given. garden-customer-admin/
                                     garden-staff-admin sync roles to the auth
                                     server at --auth-server-url — see
                                     --deploy-auth-server below
  --deploy-auth-server, -das        Deploy demo-authorization-server (see
                                     demo-authorization-server/main.go) into
                                     the fixed $AUTH_NAMESPACE namespace, at a
                                     fixed 1 replica, reachable at
                                     localhost:$DEMO_AUTH_PORT via its own
                                     LoadBalancer Service — needed before
                                     --deploy-gardening-demo's seeding Job
                                     can log in
  --deploy-all, -all                Shorthand for all five targets above.
                                     Unlike a standalone --deploy-ai, if
                                     neither --ai-api-key-file/--ai-model nor
                                     --use-headless-claude is given, this
                                     doesn't fail — it just skips
                                     ai-powered-tools with a warning and
                                     deploys everything else
  --deploy-server PATH               Deploy one arbitrary server folder (must
                                     contain main.go + a non-empty k8s/) into
                                     --namespace (default: $SERVER_NAMESPACE) —
                                     independent of the category targets
                                     above, can be combined with them

Options:
  --release VERSION      Deploy from a GHCR release (published via
                         ./scripts/build.sh --release VERSION) instead of
                         requiring images already in this machine's local
                         docker daemon — works against any cluster kubectl
                         is configured against, not just this repo's own
                         local Colima/k3s. Passing this flag at all is what
                         switches to that path (no separate on/off flag,
                         and no auto-detection from a git tag/VERSION file
                         — a version is only ever stated explicitly, never
                         guessed). Pulls anonymously unless
                         --github-token-file is given. Skips this repo's
                         k3s/Colima-only waits (see CLAUDE.md's "Build and
                         deploy" section)
                         — the gardening-demo seeding Job itself works
                         identically either way. Cannot be combined with
                         --use-headless-claude.
  --github-owner NAME    GHCR/GitHub owner to pull from (default: inferred
                         from "git remote get-url origin"). Only used
                         together with --release. Required explicitly
                         when running from an extracted deploy-bundle
                         tarball (no .git directory to infer it from).
  --github-token-file PATH
                         Only with --release, and only for private
                         packages (the project's are public): a local file
                         containing a GitHub PAT with read:packages scope.
                         Logs in to ghcr.io with it, and creates a
                         $GHCR_PULL_SECRET_NAME imagePullSecret in every
                         touched namespace.
  --namespace NAME       k8s namespace for --deploy-ai/--deploy-server only
                         (default: $SERVER_NAMESPACE) — created if it doesn't
                         already exist. Has NO effect on --deploy-proxy/
                         --deploy-platform-servers (always $PROXY_NAMESPACE),
                         --deploy-gardening-demo (always $GARDENING_NAMESPACE),
                         or --deploy-auth-server (always $AUTH_NAMESPACE) —
                         those three are fixed namespaces this repo manages
                         RBAC/NetworkPolicies for; --deploy-ai/--deploy-server
                         make no such namespace assumption, so nothing here
                         is tailored to wherever --namespace points.
                         go-proxy only discovers backends in the namespaces
                         its --config lists in discovery.k8sNamespaces (a
                         warning is printed if NAME isn't one of them).
  --proxy-replicas N     Replicas for go-proxy (default: $PROXY_REPLICAS)
  --replicas N           Replicas for every server deployed via a category
                         target or --deploy-server (default: $REPLICAS) — does not
                         affect ai-powered-tools, always deployed at 1
  --config PATH          Combined proxy config JSON file — see below
                         (default: $CONFIG_FILE). Its "declarativeGuardrails"/
                         "eventDictionary" sections seed declarative-guardrail's/
                         the 2 event-publishing-or-subscribing servers'
                         config too, via go-proxy's own self-seeding at
                         startup (see "Config map seeding" below) — there's
                         no separate flag for either anymore.
  --ai-api-key-file PATH Path to a local file containing the API key
                         ai-powered-tools uses to call its configured LLM.
                         Required, together with --ai-model, whenever
                         ai-mcp-servers is being deployed (--deploy-ai or
                         --deploy-all) — passing only one, or requesting an
                         AI deploy without either, is a script error.
  --ai-model NAME        Model name for ai-powered-tools to request (e.g.
                         "gemini-2.5-flash"). See --ai-api-key-file above.
  --ai-base-url URL      Optional: base URL of the OpenAI-compatible chat
                         completions endpoint ai-powered-tools calls (default:
                         Google Gemini's, built into ai-powered-tools).
                         Only valid together with --ai-api-key-file/--ai-model;
                         omitting it on a later AI deploy resets the default.
  --use-headless-claude  Development-time only: instead of a real API key,
                         ai-powered-tools gets completions via a local
                         claude-bridge process (started/reused by this
                         script) wrapping your own authenticated Claude
                         Code session. Cannot be combined with
                         --ai-api-key-file/--ai-model — pick one backend.
                         Only takes effect together with --deploy-ai/
                         --deploy-all; see CLAUDE.md's "AI-powered tool
                         discovery" section. Never for a real deployment.
  --auth-server-url URL  Base URL garden-customer-admin/garden-staff-admin
                         (--deploy-gardening-demo/--deploy-all) sync
                         customer/staffer roles to, passed via "kubectl set
                         env ... AUTH_SERVER_URL=..." on every gardening-demo
                         deploy (default: demo-authorization-server's own
                         in-cluster Service DNS name within $AUTH_NAMESPACE,
                         reaching it from inside the cluster) — point this
                         at a different auth server's admin API to use one
                         instead of demo-authorization-server.
  --public-load-balancer On a cluster that isn't local (its API server isn't
                         on this machine), make go-proxy's Service a
                         LoadBalancer anyway - on a cloud cluster that's a
                         public address, serving plain HTTP. Without it,
                         go-proxy is reachable only inside such a cluster
                         (use --ingress-host, or kubectl port-forward). On a
                         local cluster both go-proxy and
                         demo-authorization-server are LoadBalancers on
                         localhost regardless; demo-authorization-server is
                         never made public.
  --egress-policies      Opt in to restricting what go-proxy's pods may
                         connect to (applies with --deploy-proxy): DNS, the
                         Kubernetes API server, its own namespace (NATS,
                         platform servers, itself), the discovery
                         namespaces, and the namespace of an in-cluster
                         auth.jwksUrl. Everything off-cluster - an external
                         IdP such as Okta, off-cluster static backends,
                         backend credentials' OAuth token URLs - needs
                         --egress-allow. Pass it (and the same
                         --egress-allow) on every --deploy-proxy: a deploy
                         without it removes the policy. Re-run after
                         changing discovery namespaces. Needs a network
                         plugin that enforces egress policies (k3s's does).
  --egress-allow CIDR[:PORT]
                         With --egress-policies, also let go-proxy connect
                         to CIDR (on PORT, or any port), e.g.
                         --egress-allow 0.0.0.0/0:443 for an internet IdP.
                         Repeatable.
  --ingress-host HOST    Enterprise exposure path: publish go-proxy through
                         an Ingress for HOST (e.g. mcp.acme.com) instead of
                         k8s/go-proxy-service.yaml's LoadBalancer — the
                         Service is applied as ClusterIP, and the Ingress
                         from k8s/overlays/ingress/ is added (only /mcp and
                         /.well-known/oauth-protected-resource are routed).
                         Omit it (the default) and nothing about the
                         exposure changes. Requires an ingress controller
                         already installed in the cluster. Combine with
                         --config pointing at a cfg whose auth.resourceUrl
                         is https://HOST/mcp — see
                         docs/enterprise-deployment/enterprise-deployment.md.
  --ingress-class NAME   IngressClass for --ingress-host (default: $INGRESS_CLASS)
  --tls-secret NAME      kubernetes.io/tls Secret (in $PROXY_NAMESPACE) the
                         Ingress terminates TLS with. Only used together
                         with --ingress-host; omit it when TLS is
                         terminated in front of the ingress controller, or
                         when the controller has a default certificate.
  --cluster-issuer NAME  cert-manager ClusterIssuer (e.g. letsencrypt-prod)
                         that issues and renews the TLS certificate for
                         --ingress-host automatically, into Secret
                         --tls-secret if given, else go-proxy-tls. Only
                         used together with --ingress-host. Requires
                         cert-manager and that ClusterIssuer in the
                         cluster — see
                         docs/enterprise-deployment/enterprise-deployment.md.
  -h, --help             Show this help

--config's file determines which local PEM file backs the jwt-public-key
Secret (auth.publicKeyPath); its full content becomes the proxy-server-config
ConfigMap verbatim. Only consulted when --deploy-proxy/--deploy-all is given.
(The internalAuth section, if present, needs no local key file at all —
go-proxy generates its own internal-signing keypair at runtime and
coordinates it across replicas via a Secret it creates itself; see
CLAUDE.md's "Internal identity assertions".)

--deploy-platform-servers/--deploy-gardening-demo/--deploy-auth-server/
--deploy-server assume --deploy-proxy (or a previous run of it) has already
set up the mcp-proxy namespace/RBAC/NetworkPolicies/NATS — they don't create
that shared setup themselves.
This now includes the config-map seeding below: declarative-guardrail,
rate-limiter, and event-aggregator (the platform-mcp-servers/ backends) all
read their config from a ConfigMap go-proxy itself only ever seeds as part
of starting up — i.e. only once --deploy-proxy/-all has actually started (or
restarted) go-proxy against this --config.
--deploy-server ai-mcp-servers/ai-powered-tools bypasses all of --ai-api-key-file/
--ai-model/--use-headless-claude entirely, same as it already bypasses the
API-key Secret and --ai-model today.

Config map seeding: go-proxy itself seeds all 7 "<section>-override"
ConfigMaps (access-rules, orchestration, discovery, event-dictionary,
declarative-guardrails, event-aggregation, rate-limiting) from its own
parsed --config, the moment it starts — before its HTTP listener ever comes
up (configsync.Writer.EnsureSeeded, called from main.go's doRunOn) — each
as a {"version":1,"config":{...}} envelope, the same shape/data-key
convention go-proxy's own admin tools write when live-updated later (see
CLAUDE.md's "Unify configuration into a single config-map mechanism"). This
script has no seeding step of its own anymore; it only needs to get
go-proxy started (or restarted) against the right --config, which
--deploy-proxy/-all already does. This is a one-time bootstrap, not a sync:
a ConfigMap that already exists is left untouched, so a live
_admin.update_<section> call survives a later redeploy/restart instead of
being reset back to --config's content.

Examples:
  $0 --deploy-proxy
  $0 --deploy-platform-servers
  $0 --deploy-ai --ai-api-key-file ./ai-api-key.txt --ai-model gemini-2.5-flash
  $0 --deploy-ai --ai-api-key-file ./openai-key.txt --ai-model gpt-4o-mini --ai-base-url https://api.openai.com/v1
  $0 --deploy-ai --use-headless-claude
  $0 --deploy-server platform-mcp-servers/declarative-guardrail --replicas 3
  $0 --deploy-all --ai-api-key-file ./ai-api-key.txt --ai-model gemini-2.5-flash
  $0 --deploy-all                                   # skips ai-powered-tools with a warning
  $0 --deploy-auth-server
  $0 --deploy-proxy --deploy-auth-server --deploy-gardening-demo
  $0 --deploy-all --release v0.9.1 --github-owner pellucon \\
     --ai-api-key-file ./ai-api-key.txt --ai-model gemini-2.5-flash
  $0 --deploy-proxy --deploy-platform-servers --release v0.9.1 \\
     --github-owner pellucon --config ./proxy-server.okta.cfg \\
     --ingress-host mcp.acme.com --tls-secret mcp-acme-com-tls
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-proxy|-dpr) DO_PROXY=true; shift ;;
    --deploy-platform-servers|-dpl) DO_PLATFORM=true; shift ;;
    --deploy-ai|-dai) DO_AI=true; shift ;;
    --deploy-gardening-demo|-dgd) DO_GARDENING=true; shift ;;
    --deploy-auth-server|-das) DO_AUTH_SERVER=true; shift ;;
    --deploy-all|-all) DO_PROXY=true; DO_PLATFORM=true; DO_AI=true; DO_GARDENING=true; DO_AUTH_SERVER=true; DEPLOY_ALL=true; shift ;;
    --deploy-server) SERVER_DIR="$2"; shift 2 ;;
    --namespace) SERVER_NAMESPACE="$2"; shift 2 ;;
    --proxy-replicas) PROXY_REPLICAS="$2"; shift 2 ;;
    --replicas) REPLICAS="$2"; shift 2 ;;
    --config) CONFIG_FILE="$2"; shift 2 ;;
    --ai-api-key-file) AI_API_KEY_FILE="$2"; shift 2 ;;
    --ai-model) AI_MODEL="$2"; shift 2 ;;
    --ai-base-url) AI_BASE_URL="$2"; shift 2 ;;
    --use-headless-claude) USE_HEADLESS_CLAUDE=true; shift ;;
    --auth-server-url) AUTH_SERVER_URL="$2"; shift 2 ;;
    --release) RELEASE_VERSION="$2"; shift 2 ;;
    --github-owner) GITHUB_OWNER="$2"; shift 2 ;;
    --github-token-file) GITHUB_TOKEN_FILE="$2"; shift 2 ;;
    --ingress-host) INGRESS_HOST="$2"; shift 2 ;;
    --public-load-balancer) PUBLIC_LOAD_BALANCER=true; shift ;;
    --egress-policies) EGRESS_POLICIES=true; shift ;;
    --egress-allow) EGRESS_ALLOW+=("$2"); shift 2 ;;
    --ingress-class) INGRESS_CLASS="$2"; shift 2 ;;
    --tls-secret) TLS_SECRET="$2"; shift 2 ;;
    --cluster-issuer) CLUSTER_ISSUER="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "Unknown option: $1" >&2; usage; exit 1 ;;
  esac
done

# Passing --release at all is what selects the GHCR deploy path — no
# separate --fromGitPackage/-fg flag anymore, and (see below) no
# auto-detection of a version either: deploying a published release always
# means stating its version explicitly.
if [[ -n "$RELEASE_VERSION" ]]; then
  FROM_GIT_PACKAGE=true
fi

if [[ -n "$TLS_SECRET" && -z "$INGRESS_HOST" ]]; then
  echo "--tls-secret only applies together with --ingress-host" >&2
  exit 1
fi

if [[ -n "$CLUSTER_ISSUER" && -z "$INGRESS_HOST" ]]; then
  echo "--cluster-issuer only applies together with --ingress-host" >&2
  exit 1
fi

# With --ingress-host, the --config cfg's auth.resourceUrl must name exactly
# the URL this run publishes go-proxy at — otherwise clients are pointed at
# the wrong place by the 401 discovery hint and protected-resource metadata,
# which only shows up much later as a confusing login failure. Checked up
# front, before anything is applied. Only when --deploy-proxy, the one
# target that consults --config.
if [[ "$DO_PROXY" == "true" && -n "$INGRESS_HOST" ]]; then
  if [[ ! -f "$CONFIG_FILE" ]]; then
    echo "Config file not found at $CONFIG_FILE (from --config)" >&2
    exit 1
  fi
  cfg_auth_field() {
    python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('auth', {}).get(sys.argv[2], ''))" "$CONFIG_FILE" "$1"
  }
  # Same scheme rule as proxy_ingress_tls_secret (scripts/lib/k8s.sh —
  # kept inline here so this check reads on its own): go-proxy's Ingress
  # has TLS with --tls-secret or --cluster-issuer.
  proxy_scheme="http"
  if [[ -n "$TLS_SECRET" || -n "$CLUSTER_ISSUER" ]]; then proxy_scheme="https"; fi
  if [[ "$proxy_scheme" == "http" ]]; then
    echo "WARNING: --ingress-host without --tls-secret or --cluster-issuer publishes go-proxy over plain HTTP: callers' tokens would cross the network unencrypted. Fine only if TLS is terminated in front of the ingress controller." >&2
  fi
  expected="$proxy_scheme://$INGRESS_HOST/mcp"
  actual="$(cfg_auth_field resourceUrl)"
  if [[ "$actual" != "$expected" ]]; then
    echo "$CONFIG_FILE's auth.resourceUrl is \"$actual\", but this run publishes go-proxy at \"$expected\" — make them match" >&2
    exit 1
  fi
fi

# Say which cluster this is about to change, before anything is applied:
# kubectl acts on whatever context is current.
echo "==> Deploying to kubectl context \"$(kubectl config current-context 2>/dev/null || echo none)\""

# A local build always targets the local cluster (its images live in this
# machine's docker); a release may target any cluster.
LOCAL_CLUSTER=true
if [[ "$FROM_GIT_PACKAGE" == "true" ]] && ! cluster_is_local; then
  LOCAL_CLUSTER=false
fi

# The demo's login (admin/admin, from demo-authorization-server, which
# proxy-server.cfg trusts) is fine for a demo, never for real backends.
if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
  if [[ "$DO_AUTH_SERVER" == "true" ]] || [[ "$DO_PROXY" == "true" && -f "$CONFIG_FILE" && "$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('auth', {}).get('jwksUrl', ''))" "$CONFIG_FILE" 2>/dev/null)" == *demo-authorization-server* ]]; then
    echo "WARNING: this deploys the demo login (demo-authorization-server, admin/admin). It's for demos only - don't put real backends behind it; see docs/enterprise-deployment/enterprise-deployment.md for a real identity provider." >&2
  fi
fi

if [[ "$DO_PROXY" == "false" && "$DO_PLATFORM" == "false" && "$DO_AI" == "false" && "$DO_GARDENING" == "false" && "$DO_AUTH_SERVER" == "false" && -z "$SERVER_DIR" ]]; then
  echo "No deploy target given — pass at least one of --deploy-proxy/--deploy-platform-servers/--deploy-ai/--deploy-gardening-demo/--deploy-auth-server/--deploy-all/--deploy-server" >&2
  usage
  exit 1
fi

# --use-headless-claude and --ai-api-key-file/--ai-model are two different,
# mutually exclusive LLM backends for ai-powered-tools — combining them is
# much more likely a mistake than an intentional "prefer one, ignore the
# other" (same rationale as the --ai-api-key-file/--ai-model "both or
# neither" check below).
if [[ "$USE_HEADLESS_CLAUDE" == "true" && ( -n "$AI_API_KEY_FILE" || -n "$AI_MODEL" ) ]]; then
  echo "--use-headless-claude cannot be combined with --ai-api-key-file/--ai-model — pick one LLM backend for ai-powered-tools" >&2
  exit 1
fi
if [[ "$USE_HEADLESS_CLAUDE" == "true" && "$DO_AI" == "false" ]]; then
  echo "WARNING: --use-headless-claude was given without --deploy-ai/--deploy-all — it has no effect on its own." >&2
fi

# --ai-api-key-file/--ai-model must be given together or not at all,
# regardless of which target requested AI — a typo'd flag name is much more
# likely than a deliberate partial config.
if [[ -n "$AI_API_KEY_FILE" && -z "$AI_MODEL" ]]; then
  echo "--ai-api-key-file was given without --ai-model — both are required together, or neither" >&2
  exit 1
fi
# --ai-base-url only configures the API-key backend, so it needs it.
if [[ -n "$AI_BASE_URL" && ( -z "$AI_API_KEY_FILE" || -z "$AI_MODEL" ) ]]; then
  echo "--ai-base-url requires --ai-api-key-file and --ai-model (it configures the API-key backend; not used with --use-headless-claude)" >&2
  exit 1
fi
if [[ -n "$AI_BASE_URL" && ! "$AI_BASE_URL" =~ ^https?:// ]]; then
  echo "--ai-base-url must be an http:// or https:// URL, got: $AI_BASE_URL" >&2
  exit 1
fi
if [[ -z "$AI_API_KEY_FILE" && -n "$AI_MODEL" ]]; then
  echo "--ai-model was given without --ai-api-key-file — both are required together, or neither" >&2
  exit 1
fi
# Whenever ai-mcp-servers is actually being deployed with the normal
# API-key backend (--deploy-ai/--deploy-all, and NOT --use-headless-claude)
# and neither credential was given at all, an explicit standalone
# --deploy-ai is outright required to have one — deploying that MCP server
# with a permanently unconfigured API key/model isn't a reasonable default
# outcome for a request specifically about AI tools. --deploy-all is
# different: "give me everything" shouldn't be blocked by one category's
# missing, unrelated setup, so it just skips ai-powered-tools with a
# warning instead (the "one given without the other" checks above already
# ruled out a partial/typo'd config by this point, regardless of which
# target requested AI — that always stays a hard error). Skipped entirely
# when --use-headless-claude is set instead, since that's a third,
# credential-free backend.
if [[ "$DO_AI" == "true" && "$USE_HEADLESS_CLAUDE" == "false" && -z "$AI_API_KEY_FILE" && -z "$AI_MODEL" ]]; then
  if [[ "$DEPLOY_ALL" == "true" ]]; then
    echo "WARNING: --deploy-all was given but neither --ai-api-key-file/--ai-model nor --use-headless-claude was — skipping ai-powered-tools (pass one of those to include it)." >&2
    DO_AI=false
  else
    echo "--deploy-ai requires both --ai-api-key-file and --ai-model, unless --use-headless-claude is given instead" >&2
    exit 1
  fi
fi

# --release is host-only-dev-tooling's opposite: it deploys a published
# image to a possibly-remote cluster, where --use-headless-claude's
# claude-bridge (wrapping this machine's own authenticated Claude Code
# session, reachable only via host.docker.internal on this machine's own
# Colima VM) is meaningless.
if [[ "$FROM_GIT_PACKAGE" == "true" && "$USE_HEADLESS_CLAUDE" == "true" ]]; then
  echo "--release cannot be combined with --use-headless-claude — headless-claude is host-only dev tooling, meaningless when deploying a published image" >&2
  exit 1
fi

if [[ -n "$GITHUB_TOKEN_FILE" && "$FROM_GIT_PACKAGE" != "true" ]]; then
  echo "--github-token-file is only used together with --release" >&2
  exit 1
fi
if [[ -n "$GITHUB_TOKEN_FILE" && ! -f "$GITHUB_TOKEN_FILE" ]]; then
  echo "GitHub token file not found at $GITHUB_TOKEN_FILE (from --github-token-file)" >&2
  exit 1
fi

if [[ "$FROM_GIT_PACKAGE" == "true" && -z "$GITHUB_OWNER" ]]; then
  if [[ "$HAS_GIT_REPO" == "true" ]]; then
    origin_url="$(git remote get-url origin 2>/dev/null || true)"
    GITHUB_OWNER="$(github_owner_from_url "$origin_url")"
    if [[ -z "$GITHUB_OWNER" ]]; then
      echo "Could not infer --github-owner from 'git remote get-url origin' ($origin_url) — pass --github-owner NAME explicitly" >&2
      exit 1
    fi
    echo "==> Inferred --github-owner=$GITHUB_OWNER from git remote 'origin'"
  else
    echo "--github-owner is required when running outside a git checkout (no $REPO_ROOT/.git — this looks like an extracted deploy-bundle tarball, which has no git remote to infer it from)" >&2
    exit 1
  fi
fi

if [[ "$FROM_GIT_PACKAGE" == "true" ]] && ghcr_uses_pull_secret; then
  echo "==> Logging in to ghcr.io as $GITHUB_OWNER"
  docker login ghcr.io -u "$GITHUB_OWNER" --password-stdin < "$GITHUB_TOKEN_FILE"
fi

# -----------------------------------------------------------------------
# Main flow
# -----------------------------------------------------------------------
if [[ "$DO_PROXY" == "true" ]]; then
  deploy_proxy
fi

if [[ "$DO_AUTH_SERVER" == "true" ]]; then
  # Run before deploy_proxy_workload below (when both are requested in the
  # same invocation) so go-proxy's own first-ever startup already sees
  # demo-authorization-server up — see deploy_proxy_workload's own comment
  # for why this ordering means go-proxy only ever starts once. Also run
  # before --deploy-gardening-demo further down, whose seeding Job needs
  # demo-authorization-server up and reachable in-cluster to log in.
  deploy_auth_server
fi

if [[ "$DO_PROXY" == "true" ]]; then
  deploy_proxy_workload
fi

if [[ "$DO_PLATFORM" == "true" ]]; then
  # platform-mcp-servers/* (event-aggregator) are NATS-dependent and fail
  # fast at startup if NATS isn't reachable yet —
  # wait for its rollout even if --deploy-proxy wasn't part of *this*
  # invocation (NATS may already be up from an earlier run; the wait is
  # then a cheap no-op).
  echo "==> Waiting for the nats deployment to roll out"
  kubectl -n "$PROXY_NAMESPACE" rollout status deployment/nats --timeout=120s
  deploy_category "platform-mcp-servers" "$REPLICAS" "$PROXY_NAMESPACE"
fi

if [[ "$DO_AI" == "true" ]]; then
  warn_if_not_discovered "$SERVER_NAMESPACE"
  deploy_ai_tools
fi

if [[ "$DO_GARDENING" == "true" ]]; then
  echo "==> Applying gardening-demo namespace and NetworkPolicies"
  kubectl apply -n "$GARDENING_NAMESPACE" -f go-gardening-demo/k8s/

  # None of go-gardening-demo/servers/* are NATS-dependent (unlike the
  # platform/sample branches above), so no rollout wait is needed here.
  # garden-customer-admin/garden-staff-admin do need an auth server
  # reachable at --auth-server-url (default: reaching
  # demo-authorization-server's in-cluster Service in $AUTH_NAMESPACE) —
  # deploy it first with --deploy-auth-server if it isn't already up.
  #
  # --replicas is deliberately ignored here, always deploying 1: each
  # server's state is a local in-memory map with no cross-pod sharing
  # mechanism (see go-gardening-demo/README.md) — a single pod per server
  # is an accepted assumption for this demo, not just a default.
  echo "==> go-gardening-demo servers: deploying a single pod per server (--replicas is ignored here — these servers hold local in-memory state with no cross-pod sharing)"
  deploy_category "go-gardening-demo/servers" "1" "$GARDENING_NAMESPACE"

  # AUTH_SERVER_URL is set unconditionally (every run, not just when
  # --auth-server-url deviates from its default) for the same reason
  # deploy_ai_tools sets USE_HEADLESS_CLAUDE unconditionally: otherwise a
  # later redeploy back to the default would leave an earlier run's
  # override stuck on the live Deployment via "kubectl set env"'s
  # persistence. Both Deployments declare an AUTH_SERVER_URL env var with
  # a default (see their own k8s/deployment.yaml), referenced from `args`
  # via k8s's own $(AUTH_SERVER_URL) substitution — this overrides it here,
  # the same pattern deploy_ai_tools uses for AI_MODEL.
  echo "==> Setting garden-customer-admin/garden-staff-admin's auth server URL to $AUTH_SERVER_URL"
  kubectl -n "$GARDENING_NAMESPACE" set env deployment/garden-customer-admin AUTH_SERVER_URL="$AUTH_SERVER_URL"
  kubectl -n "$GARDENING_NAMESPACE" set env deployment/garden-staff-admin AUTH_SERVER_URL="$AUTH_SERVER_URL"
  kubectl -n "$GARDENING_NAMESPACE" rollout status deployment/garden-customer-admin
  kubectl -n "$GARDENING_NAMESPACE" rollout status deployment/garden-staff-admin

  # deploy_folder's own "kubectl rollout status" (above) only proves each
  # pod is Ready, not that go-proxy has discovered it yet — k8s discovery
  # polls every discovery.frequencySeconds (default 2s) and then each
  # kind's own poller (toolsPollFrequencySeconds, default 5s) before a
  # newly-added server's tools are actually registered on the gateway.
  # Giving that a few seconds' head start avoids cli init's first tool
  # call racing the proxy's own discovery loop.
  echo "==> Waiting for go-proxy to discover the go-gardening-demo servers"
  sleep 10

  seed_gardening_demo_if_needed
fi

if [[ -n "$SERVER_DIR" ]]; then
  warn_if_not_discovered "$SERVER_NAMESPACE"
  ensure_namespace "$SERVER_NAMESPACE"
  deploy_folder "$SERVER_DIR" "$REPLICAS" "$SERVER_NAMESPACE"
fi

echo
echo "==> Done."
if [[ "$DO_PROXY" == "true" && -n "$INGRESS_HOST" ]]; then
  if [[ -n "$(proxy_ingress_tls_secret)" ]]; then
    echo "    go-proxy is published at https://$INGRESS_HOST/mcp via Ingress go-proxy"
  else
    echo "    go-proxy is published via Ingress go-proxy for host $INGRESS_HOST (/mcp) —"
    echo "    https:// if TLS is terminated in front of the ingress controller, http:// otherwise"
  fi
  echo "    Check the ingress controller's address with: kubectl get ingress go-proxy -n $PROXY_NAMESPACE"
elif [[ "$DO_PROXY" == "true" && "$(kubectl -n "$PROXY_NAMESPACE" get svc go-proxy -o jsonpath='{.spec.type}' 2>/dev/null)" != "LoadBalancer" ]]; then
  echo "    go-proxy is reachable only inside the cluster (this cluster isn't local). To reach it:"
  echo "      - from this machine: kubectl -n $PROXY_NAMESPACE port-forward svc/go-proxy 8080:80"
  echo "        then use http://localhost:8080/mcp"
  echo "      - from anywhere: redeploy with --ingress-host HOST (HTTPS via an Ingress), or"
  echo "        --public-load-balancer (a public plain-HTTP address)"
elif [[ "$DO_PROXY" == "true" && "$FROM_GIT_PACKAGE" == "true" ]]; then
  proxy_address="$(kubectl -n "$PROXY_NAMESPACE" get svc go-proxy \
    -o jsonpath='{.status.loadBalancer.ingress[0].ip}{.status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)"
  if [[ -n "$proxy_address" ]]; then
    echo "    go-proxy is reachable at http://$proxy_address/mcp"
  else
    echo "    go-proxy's LoadBalancer address is still pending — check with:"
    echo "      kubectl get svc go-proxy -n $PROXY_NAMESPACE"
    echo "    (see CLAUDE.md's \"Build and deploy\" section if this cluster has no LoadBalancer implementation)"
  fi
elif [[ "$DO_PROXY" == "true" ]]; then
  echo "    go-proxy is reachable at http://localhost/mcp"
  echo "    (:80 is the default HTTP port, so it's omitted from the URL — this is a fixed"
  echo "    port via k3s's ServiceLB, stable across redeploys, unlike a NodePort)"
fi
