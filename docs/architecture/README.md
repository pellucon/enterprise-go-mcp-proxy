# go-proxy architecture notes

Detailed design notes, split out of the root `CLAUDE.md`. Read the file covering whatever area you're changing.

Unqualified gateway file names in these notes (`main.go`, `middleware.go`, `config.go`, `session_proxies.go`, `admin_tools.go`, `utility_tools.go`, `constraints_validation.go`, `rate_limit_gate.go`, `internal_signing_key.go`, and their `*_test.go` files) refer to the `proxyserver/` package, which holds all of the gateway's code; the binary's entry point, `cmd/go-proxy/main.go`, only calls `proxyserver.Main`. Test fixtures live in `proxyserver/testdata/`.

| File | Covers |
|---|---|
| `gateway.md` | The two backend-connection trackers, `dynamic_proxy.Gateway` registration/namespacing, `ReadResource` template dispatch, backend protocol-version isolation |
| `discovery.md` | `discover_mcp_servers` modes (static/scan/k8s), `ServerMonitor`, namespace layout, NetworkPolicies |
| `auth.md` | JWT verification, role-claim remapping, `HTTPMiddleware`/401 + protected-resource metadata, access rules, internal identity assertions + JWKS + signing-key bootstrap, `relayheaders`, backend relay, tool-call correlation IDs |
| `orchestration.md` | Triggered tools (`_meta` hiding), native triggered tools, `Runtime.Guard` phases, step parameters, lazy signature validation, orchestration admin tools |
| `guardrails.md` | `declarative-guardrail`, `proxy_utility.*_constraints_validation` (JSON Schema / AI), business-logic validation (`validate_store_requests`), `configmerge`/`patchcfg` |
| `rate-limiting.md` | `rate-limiter` server, `checkRateLimit` gate, admission algorithm, FIFO wait, `get_stats` |
| `events.md` | `events` package, dictionary, NATS as hard dependency, `/healthz`, producers/consumers, `event-aggregator` tiered storage |
| `configsync.md` | `configsync` Writer/Watcher, ConfigMap seeding, admin tools, `export_full_config`, runtime log level |
| `utility-and-ai-tools.md` | `proxy_utility.*` tools (`call_tool`, `find_relevant_tools`, ...), `nativeTools`, `ai-powered-tools`, headless-Claude bridge |
| `testing.md` | `proxyserver` package test teardown rules, fixed test ports |
| `build-deploy.md` | Makefile staleness tracking, `build.sh`/`deploy.sh`, `--release`/GHCR, deploy-bundle tarballs, seeding Job, generic-cluster notes, `--ingress-host` overlay, k3s/Colima requirements, `cli` |

Operator-facing (not design notes): `../enterprise-deployment/enterprise-deployment.md` (with its config template `proxy-server.okta.cfg`) — step-by-step production deployment behind an nginx ingress with a port-less HTTPS URL and Okta.
