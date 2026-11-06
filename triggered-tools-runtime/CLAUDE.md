# triggered-tools-runtime

Read `docs/architecture/orchestration.md` (repo root of go-proxy) before changing this package. Key points: a tool is "triggered" only via its `_meta` key `go-proxy.triggered-tool`; phase order in `Runtime.Guard` is fixed; expected input schemas are derived from `contract.go` structs via `jsonschema.For` — never hand-maintained; native steps (`proxy_utility.*`) bypass MCP entirely. This package must not be imported by `dynamic_proxy` (import cycle) — use the injected `ToolVisibilityFilter`.
