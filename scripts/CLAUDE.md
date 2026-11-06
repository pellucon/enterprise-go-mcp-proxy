# scripts

Read `docs/architecture/build-deploy.md` (repo root) before changing these scripts. `build.sh` builds, `deploy.sh` only deploys (fails fast via `require_image`). Anything specific to the local Colima/k3s cluster must stay out of the `--release` path.
