# event-aggregator

Read `docs/architecture/events.md` (repo root) before changing this server. Must run as a single replica (ring buffer + disk tier + archiver are per-process; disk tier is on a PVC). New archiver transports register themselves from `init()` in `eventlog/`.
