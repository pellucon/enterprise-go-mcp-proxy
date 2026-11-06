# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository. It is deliberately short: detailed design notes for each subsystem live in `docs/architecture/` (index: `docs/architecture/README.md`). **Before changing a subsystem, read its file there.**

## What this is

`go-proxy` is an MCP (Model Context Protocol) gateway: it sits between MCP clients and one or more backend MCP servers, discovers backend servers dynamically, aggregates and namespaces their tools/prompts/resources/resource-templates into a single server, enforces JWT-based role/access control, and forwards `tools/call`/`prompts/get`/`resources/read` requests to the right backend on behalf of each connected client.

## Commands

- Build: `go build ./cmd/go-proxy` (the binary's entry point; all gateway code is in `proxyserver/`)
- Docker images + CLI binaries: `./scripts/build.sh` (`-h` for flags; incremental via the root `Makefile`, `--clean` to force, `--release VERSION` to publish to GHCR).
- Deploy to the local k3s cluster: `./scripts/deploy.sh` (`-h`). It never builds — run `build.sh` first. `--release VERSION` deploys from GHCR to whatever cluster `kubectl` points at.
- Run: `./go-proxy [-config proxy-server.cfg]` — listens on `:8081`. One combined JSON config (`config.go`'s `loadProxyConfig`/`ProxyConfig`); `natsUrl` is required. Test configs: `proxyserver/testdata/integration_test.cfg`, `proxyserver/testdata/orchestration_test.cfg`.
- Enterprise deploy (nginx ingress + HTTPS + Okta, opt-in via `deploy.sh --ingress-host` and the template `docs/enterprise-deployment/proxy-server.okta.cfg`; local defaults untouched): `docs/enterprise-deployment/enterprise-deployment.md`.
- CLI: `./scripts/cli.sh [flags] [<command> ...]` — default endpoint `http://localhost/mcp` (k8s); pass `-endpoint http://localhost:8081/mcp` for a standalone binary.
- Tests: `go test ./...`; single test `go test ./dynamic_proxy/ -run TestName -v` (gateway tests: `go test ./proxyserver -run TestName -v`). `-race` is safe module-wide.

## Code map

| Path | Role | Details |
|---|---|---|
| `proxyserver/main.go`, `proxyserver/middleware.go`, `proxyserver/config.go`, `proxyserver/session_proxies.go` (the binary's entry point `cmd/go-proxy/main.go` only calls `proxyserver.Main`) | Proxy entry point, auth/access/orchestration middleware, combined config, per-client backend sessions | `gateway.md`, `auth.md` |
| `sessioncache/` | The backend session cache both `session_proxies.go` and `triggered-tools-runtime` use | `gateway.md` |
| `dynamic_proxy/` | `Gateway`, generic per-kind `FeaturePoller`/`genericRegistry`, namespacing | `gateway.md` |
| `discover_mcp_servers/` | Backend discovery (static/scan/k8s), `ServerMonitor` | `discovery.md` |
| `auth/`, `access_control/`, `relayheaders/`, `forwarded/`, `backendauth/`, `proxyserver/internal_signing_key.go` | JWT verify/sign, role rules, `X-Go-Proxy-*` relay headers, `X-Forwarded-*`-aware public base URL, go-proxy's own per-backend credentials | `auth.md` |
| `triggered-tools-runtime/`, `proxyserver/constraints_validation.go` | Triggered tools + orchestration engine; native `proxy_utility.*` guardrail steps | `orchestration.md`, `guardrails.md` |
| `proxyserver/rate_limit_gate.go`, `platform-mcp-servers/rate-limiter/` | Rate limiting | `rate-limiting.md` |
| `events/`, `platform-mcp-servers/event-aggregator/` | NATS events, aggregation | `events.md` |
| `configsync/`, `proxyserver/admin_tools.go`, `logging/` | Live-updatable config sections, `_admin.*` tools, runtime log level | `configsync.md` |
| `proxyserver/utility_tools.go`, `ai-mcp-servers/`, `llmclient/`, `claude-bridge/` | `proxy_utility.*` tools, AI tool filtering | `utility-and-ai-tools.md` |
| `scripts/` (`scripts/lib/`: sourced helpers deploy.sh and the other scripts share), `Makefile`, `k8s/`, `*/k8s/` | Build, release, deploy | `build-deploy.md` |
| `go-gardening-demo/` | Demo backends, CLI, seed data | `guardrails.md`, `build-deploy.md` |

## Rules and gotchas that apply everywhere

**Backend connection state lives in two places — invalidate both.** The discovery pollers (`dynamic_proxy` registries, via `gateway.OnRemoved`) and the per-client tool-call sessions (`sessionMap`, via `sessions.closeServerSessions(name)`). `runProxyServer` must clean up both when `ServerMonitor` reports a removed server. `closeServerSessions` must delete its map entry even if `session.Close()` errors, or reconnection breaks permanently. The tool-call sessions and `triggered-tools-runtime`'s platform-server sessions are both a `sessioncache.Cache`, which also drops a session when its connection ends (`dropWhenClosed`, waiting on `ClientSession.Wait`), so a stateful backend's restart costs one call. It connects on its own bounded context, never the caller's (that would leak a caller's per-request identity into the shared session), and callers wait only as long as their own context. Any new cache of a backend connection should use it.

**`proxyserver` test lifecycle.** The gateway's tests (`proxyserver/`) start real proxies/backends on fixed ports and share package-level vars that `doRunOn` reassigns. Always go through `startTestProxy`/`startTestBackend` (`mcp_server_test.go`), which block cleanup until full teardown. Pick ports not used by another test (see `docs/architecture/testing.md` for current assignments). `dynamic_proxy` tests use `httptest.Server` on random ports instead.

**Security invariants.**
- Never forward a client's `Authorization` header/JWT to a backend. Backends get a short-lived proxy-signed identity assertion instead (`internalAuth`). A backend that requires credentials gets go-proxy's *own*, the same for every user (`backendauth`: a Secret in go-proxy's namespace, named only by go-proxy's own config — the discovery section's `k8sCredentials` (keyed by `<namespace>/<service>`) or a static entry's `credentialsSecret`, never by anything a backend controls such as a Service annotation), applied in every backend client — pollers, `session_proxies.go`, `triggered-tools-runtime` — via `backendauth.Client`. Any new backend connection must use it too.
- Relay values travel only on the per-request `ctx` (the caller's `auth.Identity` and a `relayheaders.WithMinter` minter that `relayheaders.ServerTransport` asks, per outbound request, for tokens minted for that backend), never on a session- or connection-scoped field — sessions are shared across concurrent callers. Any new backend connection uses a `ServerTransport` for its server.
- The identity assertion (`X-Go-Proxy-Assertion`) is never a credential. The proxy accepts back only a relay token (sent only to backends `internalAuth.relay` lists) or its own loopback token (`proxy_utility.call_tool`/`render_prompt`, sent only to `selfUrl`), on `X-Go-Proxy-Relay-Token`.
- If a client `Authorization` header is present, it is the only auth path, even if invalid; never fall back to a relay token.
- A relayed call never gets more authority than the original caller had, and less: only its backend's `internalAuth.relay` targets, never `_admin.*`, no later expiry than the original credential, at most `maxRelayHops` hops. `internalAuth` is startup-only, so admin tools can't widen it. Client-supplied `X-Go-Proxy-Tool-Call-Id` headers are ignored.
- A backend credential Secret is sent only to the hosts in its `mcp.io/allowed-hosts` annotation (`backendauth`); a Secret without it is refused.
- go-proxy's Kubernetes access is by name (`k8s/go-proxy-serviceaccount-rbac.yaml`): its override ConfigMaps, its signing-key Secret, and — through the `go-proxy-credentials` Role `deploy.sh` renders — only the credentials Secrets the discovery config names. Never grant it namespace-wide `secrets`/`configmaps` access, and never access to `proxy-server-config`. A new API call on another object needs its own by-name rule.
- Backends are trusted to be reachable only through the proxy; nothing in this repo enforces that except NetworkPolicies.
- NATS is authenticated, TLS-only, and permissioned per component (`k8s/nats/nats.conf.tmpl`): only go-proxy publishes events, only event-aggregator reads them. A new NATS client needs its own user there, a `NATS_COMPONENTS` entry in `scripts/lib/deploy-proxy.sh`, a `nats-client-<name>` Secret mount, and a NetworkPolicy entry. Audit payloads go through `auditPayload` (16 KiB cap; no `result` on `result-blocked`).
- Every discovered Service is trusted as a backend, so k8s discovery only looks in `discovery.k8sNamespaces` (required; no cluster-wide mode), and nothing a Service controls (labels, annotations) may pick its credentials, its URL's host, or a pinned name. Every name go-proxy trusts for a security decision (the platform servers, orchestration step servers, `internalAuth.relay` sources — `proxyserver/pinning.go`'s `trustedServerNames`) must be in `k8sPinnedServers`: discovery refuses it otherwise, and startup and the admin updates fail. A new by-name dependency on a server belongs in `trustedServerNames`. See "Discovery trust rules" in `discovery.md`.

**Access-rule quirks.**
- Name/URI patterns use `access_control.GlobMatch` (`*` crosses `/`), roles use `filepath.Match`.
- `resources/read` is always checked against `allowed_resources`, even for URIs that match a template — template reads need an `allowed_resources` pattern too.
- `_admin.*` tools are allowed only for the administrator role — the role name in the cfg's startup-only `roleMapping.administrator` (required — no default, startup fails without it; exact match), fixed in `AccessPolicyEngine` at construction and never touched by `Update`/admin tools; `proxy_utility.*` tools go through the normal rules.
- Resources/templates are namespaced `<server>:<uri>`; tools/prompts are `<server>.<name>`.

**MCP SDK pitfalls.**
- `jsonschema.For` infers `json.RawMessage` as an array and ignores `MarshalJSON` — use `any` for fields holding arbitrary JSON or custom-marshaled types.
- A discovered (over-the-wire) tool receives `CallRequest any` as `map[string]any`, not `json.RawMessage`; `json.Marshal`+`Unmarshal` it rather than type-asserting.
- `detachContextValues` must run first in `createMiddleware` so the client's negotiated protocol version doesn't leak to backends — except for `subscriptions/listen`, which never reaches a backend and needs the SDK's request ID from that context to open the list-changed notification stream.
- `createMiddleware` saves the client request's original (unstripped) context with `dynamic_proxy.WithClientRequestContext` before detaching it. It's used **only** to send notifications back to that client on its request's stream (the progress relay, `dynamic_proxy/progress_relay.go`) — never pass it to a backend call. Every backend `mcp.Client` that forwards tool calls needs `ProgressNotificationHandler: gateway.HandleBackendProgress`, and every forwarded `CallToolParams` goes through `Gateway.RelayProgress`.
- Elicitation is passed through, not answered: every backend `mcp.Client` that forwards tool calls needs `MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}`, and every forwarded `CallToolParams` must copy the client's `InputResponses` and `RequestState`. A `result.NeedsInput()` result must reach the client unchanged when allowed: `Guard` runs result checks on it (its questions and content reach the client too) and blocks it if they object, but defers the result audit to the follow-up call.
- The gateway's MCP server declares its capabilities explicitly (`dynamic_proxy.gatewayCapabilities`): tools, prompts and resources always, no logging. Don't fall back to the SDK default, which advertises only what's registered when a client connects.
- Don't register Sampling/Elicitation/Subscribe handlers without building the relay for them — the SDK advertises capabilities based on which handlers exist.
- Tool arguments are forwarded to backends byte for byte (`registerTool`): decoding them into `map[string]any` rounds numbers to float64, so a backend would get values other than those a guardrail checked.
- The SDK runs handlers on its own goroutines and never recovers from a panic, which would kill the process; `createMiddleware` recovers it into the request's error (see `gateway.md`). Still check backend results (e.g. `len(res.Content)`) rather than rely on that.
- Since SDK v1.8.0, `StreamableClientTransport` caps a single SSE event at 16 MiB (`MaxEventSize`, 0 = default): a backend result larger than that fails in go-proxy's backend sessions (and in the CLIs). Raise `MaxEventSize` where those transports are built if that's ever needed.

**Config conventions.**
- Optional config sections are nil-if-absent and make their feature a total no-op.
- Every section except `auth`/`internalAuth`/`roleMapping` (and the plain `selfUrl`/`natsUrl` strings) is managed by `configsync` (seeded into a `<section>-override` ConfigMap at startup, live-updated via `_admin.update_<section>`). At startup `Writer.Reconcile` compares the cfg with the live override against a recorded baseline: a cfg left behind by admin updates starts (warning), a cfg edited so it differs from the live config is refused. Adding a section means a `Writer` (and its `Reconcile` in `doRunOn`), maybe a `Watcher`, an admin-tool table entry, a `config_drift.go` `liveSections` entry, a CLI export/update pair, its `<section>-override` name in `k8s/go-proxy-serviceaccount-rbac.yaml`, and a `k8s/go-proxy-deployment.yaml` mount — see `configsync.md`.
- The event dictionary is duplicated in `proxyserver/testdata/event_dictionary_test.json`, `proxy-server.cfg`, and `proxyserver/testdata/orchestration_test.cfg`; keep them in sync (validation fails closed).
- Guardrail/rate-limit reachability failures fail closed. A guardrail declaration that can't be checked (validator error or unreachable, invalid constraint, data over `maxInputBytes`) counts as triggered, so its own actions (block/alert/log) apply; never skip it. `_admin.update_<section>` validates with the section's own parser before writing.

**Repo conventions.**
- `package main` can't import another `package main`; shared logic goes in an importable subpackage, and test stand-ins duplicate backend logic. The gateway itself is importable (`github.com/pellucon/enterprise-go-mcp-proxy/proxyserver`); the backends' `package main` servers are not.
- Every `k8s/` manifest is namespace-agnostic; `deploy.sh` picks the namespace with `-n`.
