# Utility tools and AI-powered tool discovery

> Cross-references to sections elsewhere name their file; see `README.md` for the index.

## Utility tools

A **utility tool** (`proxy_utility.<name>`) is, like an `_admin.*` tool, implemented natively by the
proxy itself — registered directly on `gateway.Server()` via `mcp.AddTool`, never forwarded to a
backend. It differs from `_admin.*` in exactly one respect: its own invocation is authorized
through the *regular configured access rules* (`access_control.AccessPolicyEngine.
IsToolAuthorized`'s ordinary `matchAny` path — `access_control.isAdminTool` only ever matches the
`"_admin."` prefix, never `"proxy_utility."`), not the hardcoded admin-only check, and with no
special-case bypass of its own either — a utility tool is just an ordinary `accessRules`-gated
tool name from the authorization engine's point of view. Every committed `*.cfg` file
(`proxy-server.cfg`/`proxyserver/testdata/integration_test.cfg`/`proxyserver/testdata/orchestration_test.cfg`) grants `proxy_utility.call_tool`
via a **role-wildcard** `clientAccessRules` entry — `{"role": "*", "allowed_tools":
["proxy_utility.call_tool"]}` — rather than listing it under each individual role, since it's meant
to be broadly usable: `access_control.matchAny` (`access_policy_engine.go`) matches a rule's `role`
field with `filepath.Match` the same way it matches `"development.*"` against `"development.manager"`
(see below), so `"*"` matches every role string a caller might present, including ones that same
config never otherwise names. The committed `*.cfg` files grant
`proxy_utility.call_tool`, `proxy_utility.get_my_id_details`, `proxy_utility.get_prompts`,
`proxy_utility.render_prompt` and `proxy_utility.find_relevant_tools` through that one
`{"role": "*", "allowed_tools": [...]}` rule (`go-gardening-demo/gardening_demo_test.cfg`'s `*`
rule grants just `find_relevant_tools`). Every one of them only ever acts within the caller's own
authority: `find_relevant_tools` only proposes tools the caller may already call (see "AI-powered
tool discovery"), though each call costs an LLM request - a deployment can grant it to fewer roles. `proxy_utility.list_triggered_tools`
is deliberately **not** in that wildcard rule in any committed `*.cfg` — its own orchestration-internal
visibility (below) makes it a narrower, operator-facing tool; `utility_tools_test.go`'s own test config
grants it to `admin` specifically to exercise it.
`dynamic_proxy.
UtilityServerName` (`"proxy_utility"`) is reserved against discovered-server-name collisions the exact
same way `dynamic_proxy.ReservedServerName` (`"_admin"`) is — both are checked by the same
`IsReservedServerName` (a small-slice membership check) in `Gateway.OnAdded` and
`registerTool`'s defense-in-depth guard.

What makes a utility tool "utility" is the opposite power to `_admin.*`'s restriction: a utility
tool's handler may call other tools/prompts on the caller's behalf, or expose gateway-internal state
a caller has no other way to reach. `utility_tools.go`'s six tools span both:

- **`find_relevant_tools`** calls `ai-powered-tools._filter_tool` *directly* — via
  `gateway.SessionFor(ctx, sessionID, serverName)` (the same live, `Gateway.serverInfo`-backed session
  resolution every discovered tool call already uses, exported specifically for this) followed by
  a plain `sess.CallTool(...)` — without that inner call ever being subject to access-rule or
  orchestration enforcement. This isn't a separate bypass mechanism: access-rule/orchestration
  enforcement only happens inside `createMiddleware`, which wraps `gateway.Server()` itself, and a
  direct backend-session call never re-enters it — exactly the same dispatch shape
  `dynamic_proxy.Gateway.registerTool`'s own handler and `triggered_tools_runtime.Runtime.Guard`'s
  triggered-tool invocation already use.
- **`call_tool`** takes a tool name and arguments as input and calls that tool *as if the caller
  had called it directly* — access rules and orchestration both apply to the *target* tool exactly
  as they would a direct call, which is the opposite of `find_relevant_tools`'s inner call. It
  achieves this by looping back into the proxy's own HTTP endpoint (`selfUrl`) as an ordinary
  `tools/call`, presenting a loopback token (`loopbackHeaders`) minted for the caller
  `createMiddleware` verified for the outer `call_tool` call. Unlike a backend's relay token, it's
  only ever sent to `selfUrl`, so it keeps the caller's full authority (an administrator can reach
  `_admin.*` through `call_tool`) — unless the outer call is itself part of a backend's relay, whose
  restrictions it carries along. Each nested `call_tool` is one hop, so nesting stops at
  `internalAuth.maxRelayHops`. See "Internal identity assertions" (`auth.md`). Since that inner request is genuinely a fresh HTTP round
  trip through `/`, it goes through `createMiddleware` from the top: `isToolRoleAuthorized` runs
  against the *original caller's* role for the *target* tool, and `triggeredRuntime.
  MatchOrchestration`/`Guard` apply exactly as they would for a direct call — there is no bypass to
  reason about here, unlike `find_relevant_tools`'s inner call. `call_tool`'s *own* invocation, by
  contrast, is checked exactly like any other tool (no special case at all in `IsToolAuthorized`)
  but is explicitly excluded from orchestration matching in `createMiddleware` — `ctr.Params.Name
  != callToolToolName` alongside the `triggeredRuntime.MatchOrchestration` check — regardless of
  whether a deployment's `toolMappings` would otherwise match it (e.g. a catch-all `"*"` pattern):
  orchestration only ever runs for the *target* tool's own loopback `tools/call`, never for the
  `call_tool` wrapper call itself. Requires `internalAuth`/`selfUrl` configured for the same reason
  `find_relevant_tools` does (below) — the loopback has no token or callback URL to present
  without them, and fails cleanly.
- **`get_my_id_details`** takes no input and returns the caller's own `sub`/roles, read directly
  from what `createMiddleware` already verified for *this* call — the same `callerClaimsFromContext`
  accessor `find_relevant_tools` uses. No inner call, no loopback, no session dispatch at all, so
  there's no way to ask about anyone but yourself — which is exactly why it carries no
  privilege-escalation risk and can be granted as broadly as `call_tool`.
- **`render_prompt`** is `call_tool`'s `prompts/get` analog: the identical loopback-through-`/`
  mechanism (same loopback token, same fresh HTTP round trip through `createMiddleware`),
  just calling `session.GetPrompt` instead of `session.CallTool`, so `isPromptRoleAuthorized` runs
  against the *original caller's* role for the target prompt exactly as a direct `prompts/get`
  would. There's no orchestration-exclusion concern to mirror `call_tool`'s own — orchestration only
  ever matches a `CallToolRequest`, never a `prompts/get`. Its result is round-tripped through
  `map[string]any` rather than typed as `*mcp.GetPromptResult` directly, since the SDK's
  `PromptMessage.Content` interface type infers as an unconstrained schema that at least one real
  MCP client rejects outright.
- **`get_prompts`** mirrors `find_relevant_tools`'s in-process, no-round-trip shape, but needs no
  inner backend call at all: it glob-matches (`access_control.GlobMatch`, the same wildcard
  semantics every `accessRules` pattern already uses) an optional `namePattern` against
  `gateway.Prompts()` — the `promptRegistry` counterpart to `Tools()` — already filtered through the
  caller's own `isPromptRoleAuthorized`, so a caller never sees a prompt beyond what their role
  already grants.
- **`list_triggered_tools`** is the *only* way to see a triggered tool's name at all — by design, a
  triggered tool is never registered on the gateway server (see "Triggered tools / orchestration" (`orchestration.md`)
  above), so it never appears in an ordinary `tools/list`. It unions two sources: `gateway.
  HiddenTools()` (backend-hosted triggered tools, which already carry a real description/schema
  from their own server) and `triggeredRuntime.NativeTools()` (in-process Go functions like
  `proxy_utility.request_constraints_validation`, which have no discovered `*mcp.Tool` at all —
  `synthesizeNativeToolStub` builds one from the fixed, exactly-known contract type each native tool
  implements, since that's knowable even though a free-text description isn't). An optional
  `verbose` argument switches between just names and full `*mcp.Tool` objects.

Every one of the six still acts as the *original* caller on whatever inner call it makes, since
each reuses the same `ctx` `createMiddleware` already populated for the outer utility tool call: see
`callerClaimsContextKey`/`callerClaimsFromContext` in `middleware.go` for `find_relevant_tools`'s/
`get_my_id_details`'s/`get_prompts`'s direct, in-process paths (a backend session call there gets
tokens minted for that backend by the ctx's minter, like any backend call), and
`callerIdentityFromContext` + `loopbackHeaders` for `call_tool`'s/`render_prompt`'s loopback paths,
in `utility_tools.go`.

**Test**: `utility_tools_test.go` (`proxyserver` package) is a dedicated, self-contained test proxy/backend pair
(ports `:8113`–`:8115`) covering three of the six tools end to end — a per-caller-filtered
`find_relevant_tools` candidate list that excludes both an unauthorized ordinary tool and a hidden
triggered tool, `call_tool`'s loopback authorization (granted vs. denied target, and its own
invocation excluded from orchestration matching), and `list_triggered_tools` (admin-only in this
test's own config) returning both a backend-hosted and a native triggered tool while excluding an
ordinary one, in both terse and `verbose` form. **`get_my_id_details`, `render_prompt`, and
`get_prompts` have no dedicated test coverage anywhere in the repo** — `integration_test.go` grants
and counts them (see its `27 tools` assertion) but never actually calls any of the three; a real
call path for `render_prompt`/`get_prompts` does exist in production, though, since
`go-gardening-demo/gardening-demo.cfg` grants `render_prompt` against the real prompts each
`go-gardening-demo/servers/*` backend registers via `promptstore` (see below) — a known gap, not a
design decision, mirroring the "implemented and configured but untested" pattern noted elsewhere in
this doc (e.g. `result_ai_constraints_validation` under "Declarative constraint validation" (`guardrails.md`)).

`dynamic_proxy.Gateway.Tools()` (`gateway.go`) gives `find_relevant_tools` in-process access to
every *discovered backend* tool currently visible on the gateway — privileged gateway access
that only this tool has (see "AI-powered tool discovery"). It's `toolRegistry.Items()` (a
live snapshot the registry already maintained) re-filtered through `isHiddenTool`, since `Items()`'s
backing map is populated even for triggered tools `registerTool` decided to hide — `Tools()` exists
specifically so its candidate list can't accidentally leak a hidden tool's name.

`Tools()` alone isn't the *whole* candidate set, though: it only ever covers backends discovered
through `toolRegistry`, never `_admin.*`/`proxy_utility.*` tools themselves, since those are
registered directly on `gateway.Server()` via `mcp.AddTool` (`admin_tools.go`/`utility_tools.go`)
and never touch `toolRegistry` at all — and the go-sdk's `*mcp.Server` exposes no exported way to
enumerate its own registered tools back out afterward (`Server.listTools` is unexported). A plain
`tools/list` call *does* include them (the SDK's own internal registry has everything, regardless
of which mechanism added it), so leaving them out of `find_relevant_tools`'s candidates would make
its results a strict subset of what `list-tools` shows the same caller — concretely, an admin
asking about "configurations update and export" would get nothing back, despite `_admin.
export_role_permissions` being right there in their own `tools/list`. `main.go`'s `nativeTools`
package var closes this gap: `registerAdminTools`/`registerSectionAdminTools`/
`registerExportFullConfigTool` and `registerUtilityTools` each append the `*mcp.Tool` they build to
it at registration time (the one point where its identity is known, since nothing can ask the SDK
for it later), and `find_relevant_tools`'s handler unions it into `Tools()`'s result before
`filterList` applies the caller's `isToolRoleAuthorized` check — so an admin caller's candidates
include `_admin.*` (and `proxy_utility.*` itself) exactly when their own `tools/list` would.
`registerAdminTools` resets `nativeTools = nil` on entry, since it (and so `registerUtilityTools`,
called right after it) runs once per proxy instance — without the reset, a process that starts
more than one instance in its lifetime (every test in this repo's own test binary does, via
repeated `doRunOn` calls) would keep accumulating duplicate entries from every earlier instance.
`call_tool` doesn't consume `Tools()`/`nativeTools` at all — its loopback re-derives authorization
from scratch for whatever single tool name it's given.

`utility_tools.go`'s `registerUtilityTools(gateway *dynamic_proxy.Gateway)` is called once from
`runProxyServerOn`, next to `registerAdminTools`, and registers all six tools.
`proxy_utility.find_relevant_tools` is the client-facing entry point described in "AI-powered tool
discovery"; `proxy_utility.call_tool` is a general-purpose "call any tool on my behalf"
primitive with no dependency on `ai-powered-tools` or any other specific backend; `get_my_id_details`,
`render_prompt`, `get_prompts`, and `list_triggered_tools` round out the namespace with identity
introspection, the prompts-side equivalent of `call_tool`, prompt discovery, and orchestration-internal
tool visibility, respectively.

A third, distinct pattern under the same `proxy_utility.*` namespace:
`proxy_utility.request_constraints_validation`/`proxy_utility.result_constraints_validation`
(`constraints_validation.go`, see "Native (in-process) triggered tools" (`orchestration.md`) under "Triggered tools /
orchestration" and "Declarative constraint validation" (`guardrails.md`)) are namespaced `proxy_utility.*`
purely as a config-referencing convention — unlike `find_relevant_tools`/`call_tool`, they are
**never** registered on `gateway.Server()` via `mcp.AddTool`, never appear in `tools/list`, and are
**not** gated by `accessRules` at all. They're triggered tools first, proxy-native second: reachable
only as `callAuthorization`/`resultAuthorization` orchestration steps, exactly like a discovered
backend's triggered tools, just invoked as a direct in-process Go function call instead of an MCP
`tools/call`.

## AI-powered tool discovery

`ai-mcp-servers/` (import path e.g. `github.com/pellucon/enterprise-go-mcp-proxy/ai-mcp-servers/ai-powered-tools`) is a top-level directory, sibling to `platform-mcp-servers/` and `go-gardening-demo/servers/`, for backends that need a library this repo doesn't want every other server depending on — here, `llmclient` (an OpenAI-compatible chat-completions client). Nothing about discovery, routing, or access control treats `ai-mcp-servers/` specially: its one server, `ai-powered-tools`, is an ordinary backend, built the same way (shared root `Dockerfile`, `--build-arg BUILD_PATH=./ai-mcp-servers/ai-powered-tools`) as any other server directory — via `./scripts/build.sh` (see "Build and deploy" (`build-deploy.md`)). Unlike `platform-mcp-servers/*`, though, it's never deployed via `scripts/deploy.sh`'s generic `deploy_category` loop (`--deploy-platform-servers`) — it's deployed separately by `deploy_ai_tools` (triggered by `--deploy-ai`/`--deploy-all`), because it needs two extra steps that loop doesn't perform: creating its API-key Secret and setting its model via `kubectl set env` (see below).

- **A two-hop design, split across a utility tool and a backend tool**: the client-facing entry point is go-proxy's own native `proxy_utility.find_relevant_tools` (see "Utility tools") — not a discovered backend tool. It builds the caller's own already-authorized candidate list in-process (`dynamic_proxy.Gateway.Tools()` unioned with `nativeTools`, filtered through `isToolRoleAuthorized` — see "Utility tools" for why the union is needed, not `Tools()` alone), then calls `ai-powered-tools._filter_tool` directly — a plain backend-session call that bypasses access-rule/orchestration enforcement for that inner call (see "Utility tools" for why). `_filter_tool` itself is an ordinary discovered backend tool with **no** privileged access to gateway internals and no relay step of its own: its input is `{task, candidates}`, both supplied directly by `proxy_utility.find_relevant_tools` — it never relays a `tools/list` back into the proxy itself. The whitelist defense in `ai-mcp-servers/ai-powered-tools/runtime.go`'s `FindRelevantTools` (LLM output is treated as proposed tool *names* only, filtered against the given candidates, always returning the caller's own untampered copy — see its doc comment for the prompt-injection reasoning) still guarantees the result is a subset of what was passed in. `_filter_tool` is deliberately left unreachable by ordinary clients — no `accessRules` entry grants any non-admin role access to it — so `proxy_utility.find_relevant_tools` is the only realistic path to it. This still requires `internalAuth`/`selfUrl` to be configured on the proxy for the *inner* call to carry a proper identity assertion (see "Internal identity assertions" (`auth.md`)) — without them, `ctx` simply carries no assertion to attach, same no-op-when-absent convention as everywhere else.
- **Configuration is CLI flags** (plus one env var, `LLM_BASE_URL`, an override for `-llm-base-url` — see the deployment bullet below) (`-port`/`-llm-base-url`/`-model`/`-api-key-path`/`-request-timeout-seconds`/`-prompt-prefix`, plus `-use-headless-claude`/`-claude-bridge-url`/`-claude-bridge-token-path` — see below), matching every other backend server's convention — there's no `ai-powered-tools`-specific JSON config file. `-api-key-path` empty (the default) means `_filter_tool` is unconfigured: the tool stays registered and reachable, but every call fails cleanly at call time ("unconfigured" vs "misconfigured") (a *set but unreadable/empty* key file still fails fast at startup, in `main()`, before the server ever starts listening). `main.go`'s `readOptionalSecretFile` helper implements this "missing file ⇒ unconfigured, any other read error or empty file ⇒ fatal" convention once, shared by both `-api-key-path` and `-claude-bridge-token-path`.
- **`runtime.go`'s `Runtime` has exactly two completion backends**, selected once at startup and never mixed per-call: `NewRuntime` (the API-key path above, via `llmclient.ChatCompletion`) and `NewHeadlessBridgeRuntime` (below). Both just build a `completionFunc` closure (`func(ctx, prompt) (string, error)`) — `FindRelevantTools`'s retry loop, `stripMarkdownFence`, and the name-whitelist prompt-injection defense live downstream of that seam, once, regardless of which backend produced the text.
- **A second tool, `ai_result_validator`, shares this same `Runtime`/`completionFunc`** — `Runtime.ValidateResult(ctx, result, constraint)` backs the `*_ai_constraints_validation` steps (see "Declarative constraint validation" (`guardrails.md`)); no separate flags, no separate backend selection. `completionFunc` takes a system message alongside the prompt (`llmclient.Config.System` for an API key; for the headless-Claude bridge, the request's `system` field, passed to `claude -p --append-system-prompt` via `llmclient.ClaudeCodePromptWithSystem`). `ValidateResult` puts the task and constraint there, and the data in the prompt inside `<data-XXXX>…</data-XXXX>`, `XXXX` random per call, which the instructions name as data only. It refuses data larger than `defaultMaxValidateInputBytes` (64 KiB) before any LLM call, and accepts only `{"answer": "ok"}`/`{"answer": "violation"}` (case-insensitive, a markdown fence tolerated) — anything else is retried up to `maxJSONRetries` times, then returned as an error, which go-proxy treats as "could not be checked". The answer is a tiny JSON object, not a bare word, because `NewRuntime`'s `completionFunc` sets `JSONMode: true` (OpenAI `response_format: json_object`).
- **Development-time headless-Claude backend**: `llmclient.ClaudeCodePrompt` (`llmclient/claudeClient.go`) shells out to a local, already-authenticated `claude` CLI in headless mode, so a developer can exercise `_filter_tool` (and, through it, `proxy_utility.find_relevant_tools`) using their own Claude subscription instead of a real LLM API key — but only from a process running directly on the host machine, since a k8s pod is a Linux container with no `claude` binary, no OS/arch match, and no authenticated session of its own. `claude-bridge/main.go` (a separate top-level binary, never containerized, never deployed) is a tiny HTTP server run directly on the host that wraps `ClaudeCodePrompt`: `ai-powered-tools`, still running in its pod as always, calls the bridge over HTTP (`NewHeadlessBridgeRuntime`) via `-use-headless-claude=true`, reaching it at `http://host.docker.internal:8099` — empirically confirmed reachable from a pod in this repo's local Colima/k3s setup. The same test also showed binding `0.0.0.0` (required for `host.docker.internal` to work at all) makes the port reachable from the whole LAN, not just the cluster's own VM, so every `/prompt` request must present a shared-secret `X-Claude-Bridge-Token` header (checked via `crypto/subtle.ConstantTimeCompare`) — this stops arbitrary LAN hosts from calling the bridge, but isn't a real security boundary (no TLS, so the token itself is visible to a LAN-local eavesdropper) — an accepted limitation for a local-development-only tool. `claude-bridge` also exposes an unauthenticated `GET /healthz`, used only by `scripts/deploy.sh`'s readiness poll after starting it (never the real `/prompt` path, which would otherwise conflate "process is listening" with "auth works"). This whole mechanism — like `ClaudeCodePrompt` itself — is for local development only and must never be reached from a real deployment.
  - **The `claude` session has no tools.** The prompt carries text other people control (task, tool arguments and results, backend tool descriptions), so a prompt injection must find nothing to run on the developer's machine: `ClaudeCodePromptWithSystem` passes `--tools ""` (no built-in tools), `--strict-mcp-config` with no `--mcp-config` (no MCP servers), `--setting-sources project` (none of the developer's user settings, pre-approved tools or hooks; the throwaway working directory has no project settings) and `--permission-mode dontAsk`. The bridge caps a request body at 4 MiB and runs at most 4 `claude` processes at once; further callers wait for a slot or until they give up.
- **The bridge's shared-secret token is ephemeral, scoped 1:1 to the bridge process's own lifetime**: `scripts/deploy.sh`'s `start_claude_bridge` generates a fresh one (`openssl rand -hex 32`) every time it actually *starts* a bridge process — never reused across restarts, never written into the repo. It lives, with the bridge's binary, PID file and log, in `CLAUDE_BRIDGE_DIR` (`$XDG_RUNTIME_DIR` or `~/.cache`, `/go-proxy-claude-bridge`), a directory `ensure_claude_bridge_dir` keeps at mode 0700 and owned by the user (refusing a symlink): at a shared path like `/tmp` another local user could read the token, plant a PID file, or swap the binary. The token is written with `umask 077`, and `claude_bridge_pid` treats a PID as the bridge's only if that process is this user's and runs the bridge binary, so a reused PID is never killed. If a bridge from an earlier run is already alive (checked via a pidfile + `GET /healthz`), the token is *not* regenerated — the existing file is read back to (re)create the matching k8s Secret. This means there's no "stale token" scenario to guard against (the token can only change when the process itself restarts): `scripts/cleanup-k8s-cluster.sh` stops the bridge (if running) and removes its pidfile/log/token files, bracketing the token's entire lifetime exactly between "bridge started" and "cleanup ran" — independent of `--namespace`/whether the k8s namespace exists at all, since the bridge is a host-level process, not a k8s resource.
- **LLM response flakiness**: observed in practice against Gemini — an occasional (roughly 1-in-10) response comes back with stray corrupted content spliced into otherwise well-formed JSON (unrelated to the prompt, doesn't reproduce on a fresh request), breaking `json.Unmarshal`. `runtime.go`'s `FindRelevantTools` retries the whole chat-completion call up to `maxJSONRetries` (2) additional times when the response fails to parse as JSON, and always sets `llmclient.Config.JSONMode` (→ standard OpenAI `response_format: {"type": "json_object"}`) as a cheap additional guard — not every OpenAI-compatible provider honors that field, so it reduces but doesn't replace the retry. A request-level failure (network error, non-200, etc.) from `llmclient.ChatCompletion` itself is *not* retried — only a response that came back successfully but didn't parse warrants trying again. The existing fail-closed behavior (give up and return an error after exhausting retries) is unchanged.
- **Deployment is via `scripts/deploy.sh --deploy-ai` (or `--deploy-all`), not a separate script**, and picks between the two backends above: either `--ai-api-key-file PATH` + `--ai-model NAME` together (never one alone — a script error, almost certainly a typo'd flag name rather than a deliberate partial config), or `--use-headless-claude` alone — combining `--use-headless-claude` with either AI flag is also a script error (two contradictory backends configured at once). Requesting an AI deploy with a *partial* backend selection (`--ai-api-key-file`/`--ai-model` given alone, likely a typo'd flag name) is always a hard error, regardless of which target requested it. An explicit standalone `--deploy-ai` with *no* backend selection at all is also a hard error — a focused request specifically about AI tools shouldn't silently do nothing. `--deploy-all` is the one exception: given with no AI backend selected at all, it prints a warning and skips `ai-powered-tools` rather than failing the whole deploy, since "give me everything" shouldn't be blocked by one category's missing, unrelated setup. `--use-headless-claude` given without `--deploy-ai`/`--deploy-all` prints a warning and does nothing, since it would otherwise silently have no effect.
  - **API-key branch**: `deploy_ai_tools` creates an `ai-powered-tools-api-key` Secret from the key file, deploys via the same generic `deploy_folder` every server uses, then runs one `kubectl set env deployment/ai-powered-tools AI_MODEL=...` call (one rollout) to inject the chosen model, plus the LLM endpoint: `LLM_BASE_URL=<url>` when the optional `--ai-base-url` flag is given (only valid with `--ai-api-key-file`/`--ai-model`, must be `http(s)://`), else `LLM_BASE_URL-` to remove it, so a previous deploy's URL never sticks. The endpoint's default exists only in the server (`main.go`'s `defaultLLMBaseURL`, Gemini's OpenAI-compatible API): `resolveLLMBaseURL` picks an explicit `-llm-base-url` flag, else the `LLM_BASE_URL` env var, else that default, and the manifest sets neither — so a plain `kubectl apply`/GitOps deploy gets the default, and an overlay can override it with the same env var — the checked-in `ai-mcp-servers/ai-powered-tools/k8s/deployment.yaml` declares `AI_MODEL` as a container env var with a default and references it from `args` via k8s's own `$(AI_MODEL)` substitution, since (unlike `proxy-server.cfg`, which the deploy script can freely rewrite into a temp copy before it becomes a ConfigMap) there's no equivalent per-invocation templating mechanism for a checked-in k8s manifest's `args`.
  - **Headless-Claude branch**: `deploy_ai_tools` calls `start_claude_bridge` (see above), then creates an `ai-powered-tools-claude-bridge-token` Secret from whichever token file is now current, deploys the same way, and always sets `USE_HEADLESS_CLAUDE` (unconditionally on *every* `deploy_ai_tools` run, both `"true"` and `"false"` — otherwise switching back out of headless mode later would leave a previous deploy's `"true"` stuck on the live Deployment via `kubectl set env`'s persistence) via the same `$(USE_HEADLESS_CLAUDE)` env-substitution pattern as `AI_MODEL`.
  - Both branches: `kubectl set env` triggers the pod restart needed to pick up the new value, so no separate `rollout restart` call is needed. The k8s manifest mounts each Secret (`ai-api-key`, `claude-bridge-token`) as its own optional volume at its own directory (`/app-run`, `/claude-bridge`) — deliberately two directories, not `subPath`-mounting both files into one: `subPath` + an absent optional Secret is a real kubelet gotcha that can fail container startup outright instead of resolving to "nothing there," unlike a whole-directory optional mount. Deploying `ai-mcp-servers/ai-powered-tools` directly via `deploy.sh --deploy-server` bypasses all of this (no Secret, no model/headless-mode override) — the pod still starts fine (both `-api-key-path` and `-claude-bridge-token-path` pointing at a nonexistent optional-Secret mount are treated as "unconfigured," see `main.go`), just with `find_relevant_tools` unconfigured and the model at its baked-in default.


