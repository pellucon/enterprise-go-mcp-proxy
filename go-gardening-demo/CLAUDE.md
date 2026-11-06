# go-gardening-demo

Relevant design notes (repo root): `docs/architecture/guardrails.md` (business-logic validation, `configmerge`/`patchcfg`) and `docs/architecture/build-deploy.md` (in-cluster seeding Job). After editing `initial-data/client_access_rules.json`, `orchestration.json`, or `declarative_guardrails.json`, run `patchcfg` so committed `*.cfg` files stay in sync.
