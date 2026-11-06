# configsync

Read `docs/architecture/configsync.md` before changing this package. `Writer` is only for go-proxy admin-tool handlers (optimistic version check + NATS ping); `Watcher` is for every reader (re-reads its mounted file on ping and every `PollInterval` as a safety net, keeps the last-good value on failure). `EnsureSeeded` must stay idempotent.
