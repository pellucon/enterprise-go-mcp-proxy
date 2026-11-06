# stress-test

A stress test for the local Colima/k3s deployment of go-proxy + the
go-gardening-demo backends: it (re)deploys the stack, reseeds gardening
data, then runs `--client-count` concurrent clients issuing real MCP tool
calls through the real `cli` binary at `--calls-per-second` (per client)
for `--duration` minutes, and finally checks pod health and scans pod logs
for anomalies.

This is entirely new, additive code and data — nothing under this
directory modifies any other file in the repo. See "Flagged code gaps"
below for the one place this tool's design intentionally works around a
real gap in the existing code, without fixing it.

## Usage

```
./stress-test/run.sh --client-count 1 --calls-per-second 1 --duration 2
```

| Flag | Range | Default | Meaning |
|---|---|---|---|
| `--client-count` | 1-5 | 1 | Number of concurrent clients. Client *i* always replays `data/cli-calls<i>.txt` — deterministic per client. |
| `--calls-per-second` | 1-10 | 1 | **Per-client** pacing rate. Total system load = client-count × calls-per-second (up to 50 calls/sec at max). |
| `--duration` | 1-60 (minutes) | 2 | Wall-clock run length. There's no fixed call-count target — the number of calls a client makes is duration × rate, which is why the call files are ~5000 lines with rollover: a long/fast run can exceed one pass through a file several times over. |

Invalid flag values are rejected up front, before any build/deploy side
effect runs.

Run the smallest invocation first and inspect its output before trusting a
long, high-concurrency run — see "Verifying a run" below.

## What a run does

1. **Setup**: `./scripts/build.sh` then
   `./scripts/deploy.sh --deploy-proxy --deploy-platform-servers --deploy-auth-server --deploy-gardening-demo --config go-gardening-demo/gardening-demo.cfg`
   — this single deploy command already force-restarts every pre-existing
   Deployment and reseeds gardening data via its own embedded `cli init`
   step (verified directly against `scripts/deploy.sh`'s `deploy_folder`).
   Because that reseed step is wrapped so its own failure only prints a
   `WARNING:` line rather than making `deploy.sh` itself exit nonzero, the
   driver additionally scans the captured deploy output for that string
   *and* makes one live call afterward to positively confirm gardening
   data landed, before proceeding. A baseline pod snapshot is taken right
   after setup (not all-zero restart counts — setup itself legitimately
   restarts every pod), and `event-aggregator.reset_events` is called once
   so the run starts with a clean event buffer.
2. **Client run**: each client is one long-lived `cli` REPL process
   (single login, reused for the whole run — not a fresh process per
   call, which would hammer demo-authorization-server and add per-call
   login latency that could prevent hitting the higher end of
   `--calls-per-second`). See "REPL framing" below for how the driver
   captures each call's result from a process it's only talking to over
   stdin/stdout/stderr pipes.
3. **Teardown**: re-snapshot pods and diff against the baseline (name-set
   diff, not just restart-count, since a crash-and-recreate can produce an
   entirely new pod name); collect pod logs scoped to the run's own time
   window and scan for `level=ERROR`/`level=WARN`; write `report.md`.

## Output format

```
stress-test/runs/<YYYYMMDD-HHMMSS>/
  config.json                     — resolved flags + timestamps
  client1.jsonl … client<N>.jsonl — one JSON line per call (see CallRecord
                                     in driver/classify.go for the exact
                                     shape: tool, args line, timestamps,
                                     latency, outcome, captured stdout/stderr)
  pods-baseline.json              — post-setup pod snapshot
  pods-final.json                 — teardown pod snapshot
  logs/                           — raw kubectl logs per component
  setup-build.log, setup-deploy.log
  report.md                       — the human-readable summary
```

The driver's own process exit code is nonzero if the teardown phase found
any pod-health finding or log anomaly — safe to use in a CI-style gate,
not just interactively.

## Call-file generation

`data/cli-calls1.txt` … `cli-calls5.txt` are static, checked-in data,
generated once by `generator/` — **not** regenerated on every run, so runs
stay comparable to each other. Regenerate them (e.g. after
go-gardening-demo's seed data changes) with:

```
go run ./stress-test/generator
```

Each file is ~5000+ lines: ~95% covering all 26 non-`load_*`,
non-hidden-triggered gardening tools (weighted toward reads/purchases,
every tool guaranteed a floor so none goes effectively untested), ~5%
covering `proxy_utility.get_my_id_details`, `proxy_utility.get_prompts`,
`declarative-guardrail.evaluate_declarations`,
`event-aggregator.get_event_stats`, `event-aggregator.reset_events`, and
`rate-limiter.get_stats`. File *N* is generated with `rand.NewSource(N)`
— reproducible byte-for-byte on a re-run, but distinct from every other
file.

A few tools need special handling to always have a valid target, since
the files are static and generated ahead of any real run:

- `purchase_fulfilled` targets are read from `online_customer_orders.json`
  at generation time (whichever orders currently have
  `fulfillmentActualDate: null`) — this pool is deliberately shared/reused
  across repeated calls, since `purchase_fulfilled` has no
  already-fulfilled guard (double-fulfilling is a harmless no-op).
- `add_supply_order` mints a synthetic id and is paired with a
  `mark_supply_order_fulfilled` call referencing that same id 50-500 lines
  later in the same file — `supply_orders.json`'s seed rows are all
  pre-fulfilled, so there's no valid pending target in seed data alone.
- `resolve_customer_service_call` targets are read from
  `customer_service_calls.json`'s unresolved rows — typically a very
  small pool (single digits) that exhausts almost immediately. Every
  subsequent call against it legitimately business-errors as "already
  resolved" — **this is an anticipated, spec-consistent outcome, not a
  generation bug**; the report annotates it as such.

Every generated `-args` value is **bare JSON with no wrapping quotes**
(`-args {"key":"value"}`, never `-args '{...}'` or `-args "{...}"`) — the
CLI's REPL tokenizer (`cli/repl.go`) doesn't understand JSON at all, it
just tracks its own literal quote-toggle state, which never activates for
a token starting with `{`. That also means **no string value used
anywhere in the generated args may contain a space** — a space anywhere
inside a JSON string value, not just around it, is treated as an ordinary
token boundary and silently splits one `-args` value into several tokens.
Every generated line is mechanically re-tokenized (via a private port of
`cli/repl.go`'s own `tokenize()`) before being written, specifically to
catch this class of regression at generation time rather than mid-run.

## REPL framing

The REPL's `"> "` prompt can't be used to frame one call's output:
`cli/repl.go` uses `github.com/chzyer/readline`, which doesn't write the
prompt at all when stdin isn't a real terminal (piping commands into a
real `bin/cli` REPL process with all three streams piped produces zero
prompt bytes on either stdout or stderr).

Instead, `driver/client.go` merges both the stdout and stderr readers onto
one channel and drains it in **two phases** per call (`awaitSettledOutput`):
phase 1 blocks for the *first* byte on either stream, bounded by the same
generous per-call timeout (30s) used as the overall safety net; phase 2
then declares the call complete once both streams have stayed quiet for
`idleWindow` (20ms). The two-phase split is load-bearing, not a
simplification: the underlying MCP round trip (especially through an
orchestration step, e.g. `purchase_products`) can easily exceed a short
idle window, so an idle countdown started before any byte has arrived
would fire with nothing captured, record the call as empty, and then
attribute its delayed output to the *next* command's `CallRecord`. See the
doc comment in `driver/client.go`.

Merging both streams onto one channel (rather than watching stdout alone)
is necessary too, not just simpler: a transport-level denial produces
**zero stdout bytes** (an unauthorized/unknown-tool call writes only to
stderr), so a stdout-only "wait for first byte"
strategy would hang forever on exactly that outcome.

`idleWindow` itself is deliberately short (20ms) because, by the time
phase 1 has already seen data start arriving, everything left is just the
CLI's own synchronous, in-memory `fmt.Println` calls (`printContent`/
`printStructuredContent` — no more network I/O to wait out), and because
it's paid on *every* call, eating directly into the `--calls-per-second`
budget — at the max flag value (10/sec, a 100ms/call window), a
too-generous idle window would meaningfully undercut the requested rate.
Re-verify this empirically (e.g. instrument a run and look at the
distribution of inter-chunk gaps once phase 1 has already started) before
trusting it at the very top end of `--calls-per-second` in a real
high-rate run — do NOT apply this same short duration to phase 1's "wait
for the first byte at all" step, which must bound the real round trip.

## Outcome classification

Every call's outcome is derived purely from its own captured
`(stdout, stderr)` pair — **not** from pod logs (see "Flagged code gaps"
below for why) — into five categories, not three:

- `success` — no `Error: ` line at all.
- `business-error` — the tool ran and returned `IsError:true` (stdout has
  content before the CLI's own wrapping `Error: ` line on stderr).
- `guardrail-rejected` — the orchestration engine blocked the call via a
  call-authorization/result-authorization step before it ever reached a
  backend (matched on `"rejected by call-authorization step"`/
  `"rejected by result-authorization step"` in stderr).
- `authz-denied` — the caller's role isn't granted the tool at all
  (matched on `"doesn't have a role that allows it"`).
- `transport-error` — everything else with empty stdout and a stderr
  error: a genuine network/connection failure, or anything else that
  fails the RPC before a tool runs.

The last three all share the same wire shape (empty stdout, an RPC-level
error) — they were originally one `transport-error` bucket, which was
wrong: a guardrail rejection (`"blocked by constraint(s): ..."`) is the
system working *correctly*, not a transport failure, and conflating it
with genuine connectivity problems made a healthy result look alarming.
`guardrail-rejected` is expected and common, reported the same way as
`business-error`; `authz-denied` and `transport-error` should both be
rare-to-nonexistent in a normal run (this test always authenticates as
`admin`, granted everything) and are each reported individually for
scrutiny.

## A deploy-scope gotcha this tool works around

This stress test deliberately never deploys `ai-mcp-servers/ai-powered-tools`
(it needs real API credentials or a local headless-Claude bridge this
driver has no way to provide, and none of the 32 tools in the call mix
are `ai-powered-tools` tools). But go-gardening-demo's own real
`orchestration.json` wires two guardrail steps —
`proxy_utility.request_ai_constraints_validation` (on
`garden-customer-service.add_customer_service_call`) and
`proxy_utility.result_ai_constraints_validation` (on
`garden-customer-admin.get_customer_details` **and**
`garden-customer-admin.load_customers`) — that call
`ai-powered-tools.ai_result_validator`, whose `block` declarations refuse
the calls they cover when it's unreachable (fail closed). Since
orchestration config lives in a k8s ConfigMap that persists across pod
restarts, this doesn't bite on a completely fresh cluster's first setup,
but it *does* break every second-and-later run, because
`go-gardening-demo/cli`'s own `init` sequence unconditionally re-applies
the real `orchestration.json` (with those two AI-dependent steps) as one
of its own steps — and its very own `load_customers` seeding step is one
of the tools they guard, so seeding itself starts failing from the second
run onward.

`driver/orchestration_fix.go`'s `neutralizeAIGuardedOrchestration` works
around this surgically (not by resetting orchestration wholesale) — it
strips just those two AI-dependent steps from whichever orchestrations
reference them, leaving every other guardrail (all declarative
JSON-schema and business-logic-validation based, none of which depend on
`ai-powered-tools`) fully intact. `setup.go` calls it twice: once before
deploying gardening-demo (so `cli init`'s own `load_customers` step can
succeed), and once after (since `cli init`'s own orchestration-update step
would otherwise silently reintroduce both AI steps, which would then
reject 100% of `get_customer_details`/`add_customer_service_call` calls
throughout the actual client-run phase too — misreporting a deploy-scope
artifact as if it were a real system problem).

## Flagged code gaps (not implemented, per instruction)

**garden-online-store's (and the other gardening servers') business-rule
rejection paths emit no `logging.*` call at all** — a rejection (e.g.
insufficient stock, unknown id) is only observable in the tool's own
`IsError:true` response, never in a pod log line. This is exactly why this
driver classifies business errors from the CLI's own output rather than
scanning pod logs for them, and why the log-scan phase expects zero
`level=ERROR`/`level=WARN` matches from ordinary rejections — any match
found there is presumptively a genuine anomaly (crash, panic, real
dispatch failure), not routine noise. **This gap is intentionally not
fixed** — the fix would be adding `logging.Errorf`/`logging.Infof` calls
to each rejection path in `go-gardening-demo/servers/*/main.go`, an
application-code change outside this tool's scope.

No other code-change gaps were identified while building this tool.

## Verifying a run

Before trusting a full `--client-count 5 --calls-per-second 10 --duration 60`
run:

1. Run the smallest invocation: `--client-count 1 --calls-per-second 1 --duration 1`.
2. Confirm `setup-deploy.log` has no `WARNING: 'cli init' failed` line, and
   `pods-baseline.json` lists every expected pod, all healthy.
3. Confirm `client1.jsonl` has roughly 60 records (validates 1/sec pacing
   over 1 minute); spot-check a few records' `stdout`/`stderr` against
   what you'd expect from that line's tool/args — no bleed between
   consecutive calls' captured output is the single riskiest thing to
   verify (see "REPL framing" above).
4. Confirm `pods-final.json` matches the baseline exactly and
   `report.md`'s pod-health/log-scan sections both say PASS.
5. Only then, try `--client-count 2 --calls-per-second 3 --duration 2` to
   validate multi-client concurrency before a full-scale run.
