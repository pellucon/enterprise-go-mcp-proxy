# dynamic_proxy

Read `docs/architecture/gateway.md` before changing this package. Adding a feature kind = one more `genericRegistry[T]` in `Gateway`. Removing a server must also be paired with `sessionMap.closeServerSessions` in the root package. Tests here use `httptest.Server` on random ports.
