# rate-limiter

Read `docs/architecture/rate-limiting.md` (repo root) before changing this server. Must run as a single replica (in-memory state). The algorithm lives in the importable `ratelimit` subpackage; `timeoutSeconds` is passed as an explicit argument because context deadlines do not cross the wire.
