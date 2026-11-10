# Development and Deployment Guide

This guide covers everything needed to build, run, test, release and deploy Enterprise MCP Proxy. The README's [Development and Deployment](../README.md#development-and-deployment) section is the short version; this is the full reference.

## Contents

1. [Prerequisites](#1-prerequisites)
2. [Repository layout](#2-repository-layout)
3. [Supported MCP servers and clients](#3-supported-mcp-servers-and-clients)
4. [Configuration](#4-configuration)
5. [Build](#5-build)
6. [Deploy to a local cluster](#6-deploy-to-a-local-cluster)
7. [The development loop](#7-the-development-loop)
8. [Testing](#8-testing)
9. [Release](#9-release)
10. [Deploy a release](#10-deploy-a-release)
11. [CPU architectures](#11-cpu-architectures)
12. [Enterprise deployment: nginx ingress, HTTPS and Okta](#12-enterprise-deployment-nginx-ingress-https-and-okta)
13. [Deploying to other clusters](#13-deploying-to-other-clusters)
14. [Troubleshooting](#14-troubleshooting)

---

## 1. Prerequisites

### Tools

| Tool | Needed for | Notes |
|---|---|---|
| **Go** (the version in `go.mod`, currently 1.26.2) | Building, tests | Not needed to deploy a release |
| **Docker** | Building images | Colima provides the Docker runtime on macOS |
| **A local Kubernetes cluster** | Running the system | Reference setup: [Colima](https://github.com/abiosoft/colima) with k3s on macOS |
| **kubectl** | Deploying | Must point at the target cluster |
| **python3** | Deploy and release scripts | Used for small JSON tasks |
| **make**, **bash**, **curl** | Build and release scripts | Preinstalled on macOS and most Linux systems |

The reference development machine is an Apple Silicon Mac. Linux works the same way with any local cluster that provides a LoadBalancer (see [section 13](#13-deploying-to-other-clusters)).

### Starting Colima

```sh
colima start --cpu 2 --memory 4 --disk 40 --kubernetes --k3s-arg=--disable=traefik
```

`--disable=traefik` turns off k3s's bundled ingress controller, which the local setup doesn't use. k3s's built-in LoadBalancer ("ServiceLB") must stay enabled: it's what makes the proxy reachable at `http://localhost/mcp`.

### CPU, memory and disk

| Resource | Minimum | Recommended | Notes |
|---|---|---|---|
| CPUs (cluster VM) | 2 | 2–4 | |
| Memory (cluster VM) | 2 GiB | 4 GiB | The full stack (proxy, platform servers, NATS, demo auth server, 5 gardening servers) uses about 1.2 GiB of a 2 GiB VM. Use 4 GiB if you deploy the AI-powered tools, run more replicas or run the stress test. |
| Disk (cluster VM) | 20 GiB | 40 GiB | The 12 images total about 0.8 GB; the rest is for image layers, logs and the event store. |
| Disk (host) | 5 GB free | 10 GB free | Mostly the Go module and build caches |

To resize an existing Colima VM: `colima stop && colima start --memory 4 ...` (CPU and memory can change; disk can only grow).

### Accounts and secrets (only for some tasks)

| File | Needed for | Scopes / content |
|---|---|---|
| `github-token.txt` | Publishing or deploying a release | GitHub personal access token: `write:packages` + `read:packages` (+ `repo` to publish release bundles). Deploying needs only `read:packages`. |
| `ai-api-key.txt` | Deploying the AI-powered tools | An API key for the LLM provider |

Both file names are git-ignored. Never commit them.

## 2. Repository layout

| Path | Contents |
|---|---|
| `cmd/go-proxy/`, `proxyserver/` | The proxy: entry point, middleware, configuration, per-client backend sessions |
| `dynamic_proxy/`, `discover_mcp_servers/` | Aggregation of backend tools/prompts/resources; backend discovery |
| `auth/`, `access_control/`, `relayheaders/` | Token verification, role rules, identity relay |
| `triggered-tools-runtime/` | Triggered tools and orchestration (guardrails run through it) |
| `platform-mcp-servers/` | Declarative guardrails, event aggregator, rate limiter |
| `ai-mcp-servers/` | AI-powered tools |
| `configsync/`, `events/`, `logging/` | Live configuration, NATS events, runtime log level |
| `demo-authorization-server/` | A minimal OAuth server for local development and the demo |
| `go-gardening-demo/` | The Gardening Online Store demo: 5 backend servers, CLI, seed data |
| `cli/` | The proxy CLI (`scripts/cli.sh`) |
| `examples/` | Small runnable examples (e.g. [discovery](../examples/discovery/README.md)) |
| `stress-test/` | Load test driver |
| `scripts/`, `Makefile`, `Dockerfile` | Build, deploy and release |
| `k8s/`, `*/k8s/` | Kubernetes manifests |
| `docs/architecture/` | Design notes per subsystem |

## 3. Supported MCP servers and clients

### Servers

The proxy can front any MCP server that:

- **Speaks the Streamable HTTP transport.** The proxy connects to backends over HTTP. A server that only supports stdio needs an HTTP bridge in front of it.
- **Supports MCP spec version 2024-11-05 or later.** The proxy is built on the official MCP Go SDK (currently v1.8.0) and negotiates any version from 2024-11-05 up to 2026-07-28.
- **Is reachable from the proxy inside the cluster.** In Kubernetes discovery mode, label the server's Service `mcp.io/role=backend-mcp-server`. If the server isn't served at `/`, add an `mcp.io/path` annotation. See [Dynamic Discovery](../README.md#dynamic-discovery) and the [discovery example](../examples/discovery/README.md).

What the proxy relays to backends:
- **Tools, prompts, resources and resource templates** are aggregated and namespaced: `<server>.<tool>` and `<server>:<uri>`.
- **Elicitation works with protocol 2026-07-28.** The tool returns an "input required" result, which the proxy passes to the client, and the client's answers go back on the follow-up call. A Go MCP SDK server needs `StreamableHTTPOptions{Stateless: true}` to use that protocol. The client must use 2026-07-28 as well.
- **Older-style server-to-client requests are not relayed.** These are sampling, and elicitation before 2026-07-28. Backends that need them won't work through the proxy.
- **Results are limited to 16 MiB.** A single backend result is capped at the SDK's default 16 MiB event size.
- **Backend sessions are shared by all users.** Each proxy replica uses one MCP session per backend for everyone, so a server must keep per-user state keyed on the caller's verified identity, not on its MCP session. See [Keeping per-user state](../README.md#keeping-per-user-state).

Backends don't need to implement authentication. They receive a short-lived identity assertion signed by the proxy, never the client's own token, and network policies restrict them to traffic from the proxy. A backend that does require credentials (an API key, a token, or OAuth client credentials) can get the proxy's own, from a Secret assigned to it in the discovery config's `k8sCredentials`; see [Backend credentials](../README.md#backend-credentials). See [`docs/architecture/auth.md`](architecture/auth.md).

Outside Kubernetes discovery, servers can be listed explicitly in the `discovery` section (`"discoveryMode": "static"`). See [`docs/architecture/discovery.md`](architecture/discovery.md).

### Clients

The proxy serves one MCP endpoint, `/mcp`: `http://localhost/mcp` locally, or `https://<host>/mcp` in an enterprise deployment. A client can connect if it supports:

- **Streamable HTTP.** The proxy doesn't offer the older HTTP+SSE transport or stdio. A stdio-only client needs a local bridge that forwards to an HTTP endpoint.
- **MCP spec version 2024-11-05 or later**, up to 2026-07-28.
- **The MCP authorization flow.** The steps:
  1. An unauthenticated request gets `401` with a `WWW-Authenticate` header.
  2. That header points to `/.well-known/oauth-protected-resource`.
  3. That metadata names the identity provider.
  4. The client logs the user in there (OAuth 2.1 authorization code with PKCE) and sends the access token as a bearer token.

**Client registration:**
- The demo authorization server supports automatic (dynamic) client registration, so clients that register themselves work out of the box locally.
- Okta doesn't offer anonymous dynamic registration, so each client needs a pre-registered application and its client ID. See the [enterprise deployment guide](enterprise-deployment/enterprise-deployment.md#35-register-client-applications).

**Clients:**
- **The included CLI** (`scripts/cli.sh`) is the reference client and supports both registration modes (`-client-id`, `-scope`).
- **Claude Code and other MCP-capable agents and IDEs** that implement Streamable HTTP and MCP authorization can connect to the same endpoint.
- **Clients that connect from their vendor's cloud**, such as Claude Desktop and claude.ai custom connectors, ChatGPT connectors, Microsoft Copilot Studio, and the remote MCP features of the Claude and OpenAI APIs, need the proxy at a public HTTPS URL. Clients that run on your machine (the CLI, Claude Code, Cursor, VS Code with GitHub Copilot, the MCP Inspector) can use `http://localhost/mcp`. See [Giving the gateway a public URL with Cloudflare Tunnel](public-url-cloudflare-tunnel.md).

**Not supported yet:**
- Requests a backend server sends back to the client: sampling, and elicitation before protocol 2026-07-28. Clients on 2026-07-28 can answer elicitation ("input required" tool results).
- Resource subscriptions. The proxy doesn't advertise these capabilities, so clients won't try to use them.
- List-change notifications for clients using protocols older than 2026-07-28. The proxy serves clients statelessly, so it has no connection on which to send them; those clients see tool and prompt changes when they reconnect. Clients on 2026-07-28 subscribe with `subscriptions/listen`, and are notified.

## 4. Configuration

The proxy reads one JSON configuration file: `proxy-server.cfg` by default, or whatever `deploy.sh --config` names.

| Section | Purpose | Live-updatable |
|---|---|---|
| `auth` | How client tokens are verified: JWKS URL, issuer, audience, role claim, public URL | No |
| `internalAuth` | The identity assertion the proxy signs for backends, and which backends may relay calls back (`relay`, `maxRelayHops`) | No |
| `roleMapping` | Which role is the gateway administrator (required) | No |
| `selfUrl`, `natsUrl` | The proxy's in-cluster URL; the NATS event bus | No |
| `discovery` | Discovery mode (`k8s`, `static`, `scan`), label selector, polling intervals | Yes |
| `accessRules` | Which roles may see and call which tools, prompts and resources | Yes |
| `governanceOrchestration` | Which checks (triggered tools) run before calls and results | Yes |
| `declarativeGuardrails` | Guardrail declarations and constraints | Yes |
| `eventDictionary` | The catalog of event types | Yes |
| `eventAggregation` | Event store filter and retention | Yes |
| `rateLimiting` | Per-tool, per-role rate limits | Yes |
| `logLevel` | Runtime log level | Yes |

**Live-updatable sections are seeded once.** On its first start, the proxy copies each live-updatable section into its own Kubernetes ConfigMap (`<section>-override`). From then on the ConfigMap is the source of truth: changes made with the CLI's `update-*` commands (or the `_admin.update_<section>` MCP tools) apply immediately and survive restarts and redeploys. One exception to "immediately": declarative guardrails are read by a separate server through a mounted ConfigMap, which Kubernetes refreshes within a minute or two. An `update-declarative-guardrails` call therefore waits until that server confirms the new version, and says so in its reply. When you add a declaration together with an orchestration step that uses it, update the guardrails first.

The flip side: **editing `proxy-server.cfg` and redeploying never changes a section that has already been seeded,** so the proxy refuses to start with such a file rather than silently ignoring the edit. Each ConfigMap records which version of the file it last agreed with:
- **The file is unchanged and the live config moved on** through `update-*` commands: the proxy starts, and logs that the file is behind.
- **The file changed and matches the live config** (you regenerated it): the proxy starts, and the file becomes the new baseline.
- **The file changed and differs from the live config:** the proxy refuses to start and names the sections. During `deploy.sh` the old pod keeps serving, and the script prints the new pod's log.

**Changing configuration**, step by step:
1. Export the section: `./scripts/cli.sh -username admin -password admin export-role-permissions > rules.json`.
2. Edit the file, keeping its `version`, and apply it: `./scripts/cli.sh -username admin -password admin update-role-permissions -file rules.json`. It takes effect at once on every replica. A stale `version` is rejected: export again and redo the edit.
3. Regenerate the cfg from the live config, so a future deploy matches it: `./scripts/cli.sh -username admin -password admin export-full-config > proxy-server.cfg`, and commit it.

Every live section has the same `export-…`/`update-…` pair: `role-permissions`, `orchestration`, `discovery`, `event-dictionary`, `declarative-guardrails`, `event-aggregation`, `rate-limiting`, `log-level`.

**To make the file win instead** (discarding the live changes in a section), delete that section's ConfigMap (e.g. `kubectl delete configmap access-rules-override -n mcp-proxy`) and restart the proxy, so it seeds again from the file. For a clean slate, run `./scripts/cleanup-k8s-cluster.sh` and deploy again.

Details: [`docs/architecture/configsync.md`](architecture/configsync.md).

## 5. Build

```sh
./scripts/build.sh
```

This builds all 12 Docker images (the proxy, platform servers, AI tools, demo auth server, gardening servers and seed job) with the tag `:local`, plus two CLI binaries: `bin/cli` and `go-gardening-demo/bin/cli`. The images go straight into the Docker daemon that k3s uses, so no registry is involved.

- **Builds are incremental.** Each target is rebuilt only when a Go package it depends on changed, including shared packages, so running `build.sh` repeatedly is cheap.
- **Force a full rebuild:** `./scripts/build.sh --clean`
- **Build one extra server folder** (must contain `main.go` and a `k8s/` directory): `./scripts/build.sh --build-server path/to/server`
- **Build just the proxy binary:** `go build ./cmd/go-proxy`. The proxy reads its Kubernetes and NATS settings from inside the cluster, so in practice it runs in Kubernetes; the binary is mostly useful for checking compilation.

Images are built for the build machine's own CPU architecture; see [section 11](#11-cpu-architectures).

## 6. Deploy to a local cluster

`deploy.sh` never builds. Run `build.sh` first; deploy fails fast and tells you so if an image is missing.

```sh
./scripts/deploy.sh --deploy-all --config go-gardening-demo/gardening-demo.cfg
```

`--config go-gardening-demo/gardening-demo.cfg` gives the proxy the demo's configuration from the start (its access rules, guardrails and orchestration). Without it, the default `proxy-server.cfg` is used and the demo's seeding job adds those sections as live updates, so go-proxy logs on each start that the live configuration has moved on from the file. Keep passing the same `--config` on later deploys; see [section 4](#4-configuration).

`--deploy-all` deploys everything into three namespaces:

| Target flag | What it deploys | Namespace |
|---|---|---|
| `--deploy-proxy` (`-dpr`) | The proxy, NATS, RBAC, network policies, config | `mcp-proxy` |
| `--deploy-platform-servers` (`-dpl`) | Declarative guardrails, event aggregator, rate limiter | `mcp-proxy` |
| `--deploy-ai` (`-dai`) | AI-powered tools | `mcp-proxy` (or `--namespace`) |
| `--deploy-auth-server` (`-das`) | Demo authorization server (`http://localhost:8102`) | `demo-auth` |
| `--deploy-gardening-demo` (`-dgd`) | The 5 gardening servers, and a one-time job that seeds their data | `gardening-demo` |
| `--deploy-server PATH` | Any server folder with a `k8s/` directory | `--namespace` (default `mcp-proxy`) |

Deploy the proxy first; the other targets rely on what it sets up. Targets can be combined in any order in one command.

**AI-powered tools** need an LLM: pass `--ai-api-key-file ./ai-api-key.txt --ai-model <model>`. They call Google Gemini's OpenAI-compatible endpoint by default. Add `--ai-base-url <url>` to use any other OpenAI-compatible endpoint, e.g. `https://api.openai.com/v1`. For development only, `--use-headless-claude` uses your own logged-in Claude Code session instead of an API key. Without either, `--deploy-all` skips the AI tools with a warning.

**Other useful options:**

| Option | Purpose |
|---|---|
| `--config PATH` | Use a different proxy config (default `proxy-server.cfg`) |
| `--proxy-replicas N` | Run N proxy replicas |
| `--replicas N` | Replicas for servers deployed by a category target. Keep the event aggregator and rate limiter at 1, because they hold their state in memory. |

After deploying:

```sh
./scripts/cli.sh -username admin -password admin list-tools   # the proxy is at http://localhost/mcp
./scripts/observe-k8s.sh --watch                              # pods in all three namespaces
./scripts/cleanup-k8s-cluster.sh                              # remove everything
```

The demo auth server's users all have their username as their password. `admin`/`admin` is the gateway administrator.

## 7. The development loop

1. Edit the code.
2. `go test ./<package>` for the package you changed (see [section 8](#8-testing)).
3. `./scripts/build.sh`, which rebuilds only the affected images.
4. Redeploy only what changed. For example, `./scripts/deploy.sh --deploy-proxy` after a proxy change, or `./scripts/deploy.sh --deploy-platform-servers` after a platform-server change. Deploy restarts the affected pods.
5. Check the result with the CLI (`scripts/cli.sh`), and with `kubectl logs -n mcp-proxy deploy/go-proxy`.

To change the log level (`error`, `info` or `debug`) at run time without redeploying:
1. Call `_admin.export_log_level`, for example `./scripts/cli.sh -username admin -password admin call-tool -name _admin.export_log_level`.
2. Edit the level in the result.
3. Pass the edited result to `_admin.update_log_level`.

The change applies to the proxy and every server immediately.

To try a new backend server without touching the proxy, see the [discovery example](../examples/discovery/README.md).

## 8. Testing

### Unit and integration tests

```sh
go test ./...                                   # everything (about a minute)
go test -race ./...                             # with the race detector; safe module-wide
go test ./proxyserver -run TestName -v          # one proxy test
go test ./dynamic_proxy -run TestName -v        # one test in another package
```

The tests need no cluster and no external services: NATS is embedded, and backends run in-process.

**Ports.** The proxy's integration tests (`proxyserver/`) start real proxies and backends on **fixed local ports** (8081–8096, 8113–8115, 8120). Nothing else on the machine may be using those ports while they run, including a locally started `go-proxy`. `dynamic_proxy` tests use random ports instead.

**Writing new proxy tests.** Start servers only through the `startTestProxy`/`startTestBackend` helpers (`proxyserver/mcp_server_test.go`). They wait for full shutdown before the next test runs, which matters because the tests share package-level state. Pick ports no other test uses; see [`docs/architecture/testing.md`](architecture/testing.md) for the current assignments.

### Stress testing

`stress-test/run.sh` runs a load test against the local deployment: it rebuilds and redeploys the stack, then drives concurrent CLI clients issuing real tool calls against the gardening demo. Afterwards it checks pod health and scans the logs for errors.

```sh
./stress-test/run.sh --client-count 1 --calls-per-second 1 --duration 2
```

| Flag | Range | Default |
|---|---|---|
| `--client-count` | 1–5 | 1 |
| `--calls-per-second` (per client) | 1–10 | 1 |
| `--duration` (minutes) | 1–60 | 2 |

Start with the smallest run and check its report before trusting a long one. Use a 4 GiB cluster VM for larger runs. Details: [`stress-test/README.md`](../stress-test/README.md).

## 9. Release

A release publishes everything a deployer needs, so they can deploy without the source code, Go or a local build:

```sh
./scripts/build.sh --release v1.2.0 --github-token-file ./github-token.txt
```

This:
1. Requires a clean working tree. It creates and pushes the git tag `v1.2.0` at the current commit, and refuses if that tag already points to a different commit.
2. Builds as usual, then pushes **all 12 images** to GitHub Container Registry as `ghcr.io/<owner>/<image>:v1.2.0`, whether or not each one changed. That way every published version is complete.
3. Creates a GitHub Release on the tag, with a **deploy bundle** per platform: `deploy-bundle-v1.2.0-<os>-<arch>.tar.gz`. Each bundle contains:
   - The deploy, CLI and cluster-helper scripts.
   - Every Kubernetes manifest.
   - `proxy-server.cfg` and the enterprise deployment guide.
   - The demo seed data and the discovery example.
   - Prebuilt CLI binaries for that platform.
   - A `VERSION` file.

   There's no Go source.
4. Prints the Release page URL.

The token needs `write:packages` and `read:packages` to push images, plus `repo` to create the Release. The owner defaults to the `origin` remote's owner; override it with `--github-owner`.

To publish one extra server image under the same version, add `--build-server PATH`.

## 10. Deploy a release

On the deploying machine you need `kubectl` (pointing at the target cluster), Docker, `python3` and a GitHub token with `read:packages`. You don't need Go or the source code.

1. Download the bundle for your machine from the Release page, then extract it:
   ```sh
   tar xzf deploy-bundle-v1.2.0-darwin-arm64.tar.gz
   cd deploy-bundle-v1.2.0-darwin-arm64
   ```
2. Deploy:
   ```sh
   ./scripts/deploy.sh --deploy-all --release v1.2.0 \
     --github-owner <owner> --github-token-file /path/to/github-token.txt \
     --config go-gardening-demo/gardening-demo.cfg
   ```
3. Use it:
   ```sh
   ./scripts/cli.sh -username admin -password admin list-tools
   ```

What's different from a local deploy:
- **Images and the pull secret:** images are pulled from GHCR, and deploy creates a pull secret in each namespace.
- **The version is always explicit:** pass it with `--release`.
- **The owner:** pass `--github-owner` when deploying from a bundle, since there's no git remote to infer it from.
- **Any cluster:** this path works against any cluster `kubectl` points at, not just the local one.

All the deploy targets and options from [section 6](#6-deploy-to-a-local-cluster) work the same way. The exception is `--use-headless-claude`, which is for development only.

## 11. CPU architectures

Two separate architectures matter:

| What | Determined by | Supported |
|---|---|---|
| **Container images** (what the cluster runs) | The machine that ran `build.sh --release`. Images are built natively, not cross-built. | `arm64`, or `amd64` |
| **Deploy bundle** (where you run `deploy.sh` and the CLI) | The bundle you download | macOS and Linux, matching the release's architecture |

- A release built on an **arm64** machine (e.g. an Apple Silicon Mac) is published as `VERSION`, with `darwin-arm64` and `linux-arm64` bundles. Its images run on arm64 clusters.
- A release built on an **amd64** machine is published as `VERSION.amd64`: separate images, tag and Release, with `darwin-amd64` and `linux-amd64` bundles. Its images run on amd64 clusters.
- Deploy the variant that matches **your cluster's nodes**: `--release v1.2.0` for arm64 nodes, `--release v1.2.0.amd64` for amd64 nodes. Publishing both requires running the release on one machine of each kind.
- CLI binaries in the bundles are cross-compiled, so the bundle's OS and architecture only need to match the machine you deploy *from*.

## 12. Enterprise deployment: nginx ingress, HTTPS and Okta

For a production-style deployment:
- Clients reach the proxy at a port-less HTTPS URL such as `https://mcp.acme.com/mcp`.
- An **nginx ingress controller** terminates TLS.
- **Okta** is the identity provider.

The local workflow is untouched; all of this is opt-in.

The outline:
1. **Configure Okta.** Use a custom authorization server, add a scope, put the user's groups in the access token, create an access policy, and register client applications.
2. **Write a proxy config** from the template [`docs/enterprise-deployment/proxy-server.okta.cfg`](enterprise-deployment/proxy-server.okta.cfg):
   - Set Okta's `jwksUrl` and `issuer`.
   - Set `roleClaim: groups`.
   - Set `resourceUrl` to `https://<host>/mcp`.
   - Map your administrator group in `roleMapping`.
3. **Prepare the cluster:** an nginx ingress controller, a DNS record for the host, and a TLS certificate, either your own Secret or one issued by cert-manager.
4. **Deploy with the ingress options:**
   ```sh
   ./scripts/deploy.sh --deploy-proxy --deploy-platform-servers --release v1.2.0 \
     --github-token-file ./github-token.txt --config ./proxy-server.okta.cfg \
     --ingress-host mcp.acme.com --tls-secret mcp-acme-com-tls
   ```
   With `--cluster-issuer letsencrypt-prod` instead of `--tls-secret`, cert-manager issues and renews the certificate.

   `--ingress-host` switches the proxy's Service from a LoadBalancer to an Ingress. Only `/mcp` and `/.well-known/oauth-protected-resource` are routed. Deploy checks that the config's `resourceUrl` matches the published URL.
5. **Connect clients and verify the login.** Okta doesn't support automatic client registration, so the CLI needs `-client-id` and `-scope`.

Step-by-step instructions, verification and troubleshooting: [`docs/enterprise-deployment/enterprise-deployment.md`](enterprise-deployment/enterprise-deployment.md).

## 13. Deploying to other clusters

The Quick Start is tested on Colima with k3s. This section covers everything else.

### Docker Desktop

The Quick Start (a `--release` deploy) is supported on [Docker Desktop](https://www.docker.com/products/docker-desktop/) with Kubernetes enabled, but not yet tested.
- **Enable Kubernetes:** **Settings → Kubernetes → Enable Kubernetes**, cluster type **kubeadm**.
- **Resources:** give Docker Desktop at least 2 CPUs, 4 GiB of memory (its Kubernetes takes more than Colima's k3s) and 10 GiB of disk.
- **How it's reached:** Docker Desktop serves the proxy (`http://localhost/mcp`) and the demo login (`http://localhost:8102`) on `localhost` itself.
- **No isolation:** its Kubernetes doesn't enforce NetworkPolicies, so the backends aren't isolated from other pods. That's fine for the demo.
- **Building from source** needs Colima: that path waits for k3s's ServiceLB pods (see the table below).

### Other local clusters

They aren't supported as is. `deploy.sh` publishes the proxy (`http://localhost/mcp`) and the demo login (`http://localhost:8102`) through LoadBalancer Services on `localhost`, and only when the cluster's API server is on this machine:
- **minikube:** its API server is on a VM address, which doesn't count as local, so both stay internal.
- **kind:** it has no LoadBalancer implementation, so they never get an address.
- **k3d:** ports 80 and 8102 must be mapped when the cluster is created.
- **Rancher Desktop:** it keeps Traefik on port 80 by default.

On any of them, `kubectl port-forward` reaches both: `kubectl -n mcp-proxy port-forward svc/go-proxy 8080:80` and `kubectl -n demo-auth port-forward svc/demo-authorization-server 8102:8102`, then use the CLI with `-endpoint http://localhost:8080/mcp`.

### What any cluster needs

The manifests are namespace-agnostic and avoid local-only assumptions. On a cluster other than Colima/k3s, check these:

| Requirement | Used by | If missing |
|---|---|---|
| **LoadBalancer Services** (local clusters; elsewhere only with `--public-load-balancer`) | The proxy (`k8s/go-proxy-service.yaml`) and, on a local cluster, the demo auth server | Both Services are `ClusterIP` by default. `deploy.sh` makes them LoadBalancers only when the cluster is local, meaning its API server address is a loopback address or `localhost` (Colima, kind, k3d) or Docker Desktop's `kubernetes.docker.internal`; minikube's VM address doesn't count. There a LoadBalancer means `localhost`. On any other cluster, a LoadBalancer gets a public address, so the proxy stays `ClusterIP` unless you pass `--public-load-balancer`; use `--ingress-host` for HTTPS, or `kubectl port-forward`. The demo auth server is never made public there; port-forward to it to log in from your machine. |
| **k3s's built-in load balancer (ServiceLB)** | Builds deployed from source (without `--release`): `deploy.sh` waits for ServiceLB's pods in `kube-system` | Docker Desktop, kind and minikube have none, so that wait fails and the deploy stops: build from source on Colima. Release deploys don't depend on it: they wait for the Service's LoadBalancer address, and only warn if none arrives. Docker Desktop gives LoadBalancer Services `localhost`, so a release deploy works there |
| **A default StorageClass** | The event aggregator's volume | Install a storage provisioner, or set `storageClassName` in `platform-mcp-servers/event-aggregator/k8s/pvc.yaml` |
| **A CNI that enforces NetworkPolicies** (Calico, Cilium, most managed clusters) | Restricting backends to proxy traffic | Everything still works, but without that isolation |
| **The standard `cluster.local` DNS domain** | The proxy's `selfUrl` | Edit `selfUrl` in the config |

## 14. Troubleshooting

| Symptom | Likely cause and fix |
|---|---|
| `deploy.sh` says an image is missing | Run `./scripts/build.sh` first. |
| `http://localhost/mcp` doesn't answer | Something else on the Mac is using port 80, or the proxy isn't ready yet. Check with `./scripts/observe-k8s.sh`. |
| CLI login fails: "authorization server metadata … names issuer …" | `-auth-server` isn't the server's issuer URL (e.g. `127.0.0.1` instead of `localhost`, or an in-cluster address). Use the issuer's URL, or pass it as `-auth-issuer`. |
| The proxy won't start: "the deployed cfg changes … and now differs from" the live config | `proxy-server.cfg` was edited after its sections were seeded. Apply the change with `update-*` and regenerate the file with `export-full-config`, or delete the named `<section>-override` ConfigMaps; see [section 4](#4-configuration). |
| Pods are evicted or restart with `OOMKilled` | The cluster VM is too small; give Colima 4 GiB. |
| Tests fail with "address already in use" | Another process is using one of the test ports; see [section 8](#8-testing). |
| A new backend server isn't discovered | Check its Service label `mcp.io/role=backend-mcp-server`, and add `mcp.io/path` if it isn't served at `/`. The [discovery example](../examples/discovery/README.md) shows a working setup. |
| `deploy.sh --release` can't pull images | The token lacks `read:packages`, the version doesn't exist, or it was built for the other architecture (see [section 11](#11-cpu-architectures)). |
