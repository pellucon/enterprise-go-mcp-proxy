# Rate limiting

> Cross-references to sections elsewhere name their file; see `README.md` for the index.

## Rate limiting

`platform-mcp-servers/rate-limiter` enforces per-tool, per-role call-rate limits on go-proxy's
behalf. It's deployed exactly like any other `platform-mcp-servers/*` backend — the generic
`--deploy-platform-servers`/`--deploy-all` loop, no dedicated deploy step — and exposes two tools:
`check_permission` (a `_meta`-marked triggered tool, never itself directly callable) and `get_stats`
(an ordinary, `accessRules`-gated tool — admin-only in every committed cfg, via the normal rule
mechanism, not a hardcoded check). The admission algorithm itself lives in an importable
`ratelimit` subpackage (`platform-mcp-servers/rate-limiter/ratelimit`), independent of the MCP
server wrapper around it. Like `event-aggregator`, it must run as a single pod (`replicas: 1`) —
both its usage-decay state and its stats recorder are in-process memory with no cross-replica
coordination, so a second replica would enforce/report independently-wrong numbers.

- **Not wired through orchestration** — unlike every other triggered tool in this repo,
  `check_permission` is never named in a `callAuthorization`/`resultAuthorization` step list.
  Instead `rate_limit_gate.go`'s `checkRateLimit` calls it directly from `middleware.go`, as an
  independent gate that runs after `isToolRoleAuthorized` and before `triggeredRuntime.Guard`'s
  orchestration flow, keyed purely off the top-level `rateLimiting` combined-config section's own
  `tools` map (by namespaced tool name) — not `toolMappings`. A tool absent from that map is
  skipped entirely, with no request sent to the rate-limiter at all; a tool present but itself a
  triggered tool is also skipped (with a logged warning), since a triggered tool is never
  registered on the gateway and so has nothing to rate-limit as a direct call. `checkRateLimit`
  fails closed: any error — rate-limiter unreachable, a timeout, a decode failure — denies the
  call, the same fail-closed convention every other guardrail-reachability failure in this codebase
  follows.
- **`TimeoutSeconds` travels as an explicit request argument, not an inferred context deadline** —
  `checkRateLimit` passes the config's own `timeoutSeconds` both as its own local
  `context.WithTimeout` bound *and* as the `check_permission` call's own argument, because a
  caller's `context.WithTimeout` deadline is a local, in-process value that never travels over the
  wire — `check_permission`'s handler has verified this directly (its own inbound `ctx` carries no
  deadline at all) rather than assumed it, so its internal poll loop bounds itself off the argument
  plus its own `time.Now()`, honoring `ctx.Done()` only as a secondary, best-effort signal.
- **Admission is a two-pool percentage split once a tool's optional global limit is reached**
  (`ratelimit.Limiter.Check`): below the limit, every call is admitted unconditionally regardless of
  role. At/above it, a role with an explicit `RolePercent` entry competes only within the
  "configured" pool (its own share, with an idle configured role's unused share borrowable by other
  active configured roles); a role with no entry competes only within the leftover "unconfigured"
  pool, split equally among whichever such roles are currently active. The two pools never cross —
  an operator who wants every role to keep *some* floor has to keep the configured percentages
  summing under 100 themselves; nothing enforces that automatically. Usage is tracked as a
  continuously-decaying estimate (`capacity/60` units/second, the same math `golang.org/x/time/rate`
  uses internally), not a fixed sliding window, and a config reload (`Limiter.Update`) preserves
  every tool's existing usage history rather than resetting it.
- **`Limiter.Wait` (what `check_permission` actually calls) adds strict per-`(tool, role)` FIFO
  ordering on top of `Check`'s own cross-role math** — callers queue via a `ticketQueue`, and only
  the front-of-queue caller polls `Check` (default every 100ms) until admitted or its own
  `ctx`/timeout gives up; leaving the queue (always deferred) promotes the new front, so an
  abandoned wait can never permanently stall the line. A `Decision.Reason` of `ReasonQueueTimeout`
  means the caller gave up before ever reaching the front (never got to attempt `Check` even once)
  — distinct from a `*Exhausted` reason, which means it *did* reach the front and was denied there.
- **`get_stats`** reports only *blocked* calls (never admitted ones) over a caller-chosen window
  (default 60, clamped to the server's own 3-hour retention), broken down by tool and by
  role-group, plus grand totals — backed by an in-memory, mutex-guarded ring that prunes from the
  front (records always arrive in non-decreasing time order, so this is O(evicted) per write, not
  O(n)).
- **Config**: an optional `rateLimiting` combined-config section (`ratelimit.Config{TimeoutSeconds,
  Tools map[string]ToolConfig{GlobalPerMinute, RolePercent}}`), keyed by namespaced tool name — a
  tool absent from `Tools`, or the whole section absent, is entirely unlimited. `Validate`
  (`ratelimit/config.go`) fails closed at parse time on a `RolePercent` set without a
  `GlobalPerMinute` (percentages of an undefined budget are meaningless), a non-positive
  `GlobalPerMinute`, a non-positive individual percent, or percentages summing over 100 — joining
  every violation found (`errors.Join`) rather than stopping at the first. Live-updatable the same
  way as every other `configsync`-managed section (see "Configuration sync (configsync)"
  (`configsync.md`)) — go-proxy both writes it (`_admin.update_rate_limiting`/
  `export_rate_limiting`) and reads it back itself (`rateLimitingWatcher.Current()`, read directly
  by `checkRateLimit` — the Watcher itself *is* the engine here, exactly like `discovery`, since
  actual enforcement runs inside the separate rate-limiter server, not go-proxy).
- **Test**: `platform_servers_test.go`'s `TestPlatformServersOrchestration` Scenarios 13–16 exercise
  a real in-process rate-limiter stand-in built on the actual `ratelimit.Limiter` (not
  reimplemented), with a frozen clock and a fast poll interval for determinism: a limited tool's
  first call admitted and second call denied only after actually waiting out the timeout; an
  unconfigured tool never triggers a single `check_permission` call across repeated calls;
  `get_stats` reachable by `admin` and denied for a fresh `guest` session; and a tool that's both
  triggered and (mistakenly) rate-limited is skipped with a logged warning and zero
  `check_permission` calls recorded.


