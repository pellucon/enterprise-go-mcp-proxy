# shellcheck shell=bash

# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# scripts/lib/deploy-gardening-demo.sh — go-gardening-demo helpers: the
# one-shot in-cluster seeding Job. Sourced by scripts/deploy.sh, never
# executed.

# -----------------------------------------------------------------------
# render_gardening_demo_seed_job: go-gardening-demo/seed/k8s/job.yaml with
# its __AUTH_SERVER_URL__ placeholder substituted for this invocation's
# resolved $AUTH_SERVER_URL. A plain text substitution, not a Kustomize
# patch, because it must also work standalone for the local (no --release)
# path, which otherwise never touches Kustomize at all. With --release, the
# caller writes this function's output to a scratch file first and passes
# THAT as apply_ghcr_release's manifest_files entry, so the GHCR
# image-rewrite overlay renders on top of already-substituted content.
# -----------------------------------------------------------------------
render_gardening_demo_seed_job() {
  sed "s#__AUTH_SERVER_URL__#$AUTH_SERVER_URL#" go-gardening-demo/seed/k8s/job.yaml
}

# -----------------------------------------------------------------------
# seed_gardening_demo_if_needed: creates the gardening-demo-seed Job the
# first time --deploy-gardening-demo is ever run against a given cluster,
# then leaves it alone forever after — same "a Get that finds the object
# already exists is a no-op, never overwritten" idiom this repo already
# uses for ConfigMaps/Secrets (see CLAUDE.md's "Unify configuration..."
# section, internal_signing_key.go's bootstrapInternalSigningKey). This
# isn't just a style match: a Job's .spec.template is immutable once
# created, so a plain repeated "kubectl apply" of a Job whose image tag
# changed (true on every new --release version) would fail outright —
# checking existence first, and never re-applying an existing Job, avoids
# that entirely. Re-running seeding is technically data-safe (every
# load_* handler upserts by ID) but not desirable: it would silently
# overwrite any live changes made to demo data since the first deploy.
#
# To force reseeding on a future deploy: "kubectl delete job
# gardening-demo-seed -n gardening-demo". To seed/reseed immediately
# without waiting for a future deploy, run go-gardening-demo/cli.sh
# directly — unchanged, still works exactly as before.
# -----------------------------------------------------------------------
seed_gardening_demo_if_needed() {
  if kubectl -n "$GARDENING_NAMESPACE" get job gardening-demo-seed >/dev/null 2>&1; then
    echo "==> gardening-demo-seed Job already exists — go-gardening-demo was already seeded by an earlier deploy, skipping."
    echo "    (delete it — kubectl delete job gardening-demo-seed -n $GARDENING_NAMESPACE — to have the next deploy reseed automatically;"
    echo "    or reseed right now without deleting anything: go-gardening-demo/cli.sh -username admin -password admin init)"
    return
  fi

  echo "==> Seeding go-gardening-demo data via a one-shot in-cluster Job (gardening-demo-seed)"
  if [[ "$FROM_GIT_PACKAGE" == "true" ]]; then
    require_ghcr_image "gardening-demo-seed"
    ensure_ghcr_pull_secret "$GARDENING_NAMESPACE"

    local job_scratch
    job_scratch="$(mktemp -d)"
    render_gardening_demo_seed_job > "$job_scratch/job.yaml"
    apply_ghcr_release "$GARDENING_NAMESPACE" gardening-demo-seed Job "$job_scratch/job.yaml"
    rm -rf "$job_scratch"
  else
    require_image "gardening-demo-seed:local"
    render_gardening_demo_seed_job | kubectl apply -n "$GARDENING_NAMESPACE" -f -
  fi

  # "kubectl logs -f job/<name>" waits for the Job's pod to actually start
  # before streaming — no separate pod-ready wait needed — giving the same
  # live-progress-output experience the old host-side "cli init" gave.
  echo "==> Streaming gardening-demo-seed's output:"
  kubectl -n "$GARDENING_NAMESPACE" logs -f job/gardening-demo-seed || true

  # By the time "logs -f" returns, the pod has already terminated, so its
  # Complete/Failed condition should already be settled (or settles within
  # a few seconds) — this wait is just to get a clean pass/fail signal for
  # the WARNING below, not to actually wait out the seeding itself.
  if kubectl -n "$GARDENING_NAMESPACE" wait --for=condition=complete job/gardening-demo-seed --timeout=30s >/dev/null 2>&1; then
    echo "==> gardening-demo-seed completed."
  else
    echo "WARNING: gardening-demo-seed did not complete successfully (see its output above) — the go-gardening-demo servers are deployed but may not be fully seeded." >&2
    echo "         Common cause: demo-authorization-server isn't reachable yet (pass --deploy-auth-server, or deploy it separately)." >&2
    echo "         Delete the Job and redeploy to retry automatically (kubectl delete job gardening-demo-seed -n $GARDENING_NAMESPACE)," >&2
    echo "         or seed manually right now: go-gardening-demo/cli.sh -username admin -password admin init" >&2
  fi
}
