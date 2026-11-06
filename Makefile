# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Incremental build targets for go-proxy's docker images and CLI binaries.
# See scripts/build.sh for the user-facing entry point (a thin wrapper around
# `make`), scripts/deploy.sh for the deploy-only counterpart that consumes
# what this Makefile produces, and CLAUDE.md's "Build and deploy" section for
# the full rationale.
#
# Staleness is tracked per target via `go list -deps`, not a naive
# per-directory mtime check: every buildable target here lives in the same
# single Go module and several import shared root packages (access_control,
# auth, relayheaders, dynamic_proxy, triggered-tools-runtime, ...), so a
# directory-only check would silently miss rebuilds when a shared package
# changes. `go list -deps -e -f '{{if not .Standard}}{{.Dir}}{{end}}' <path>`
# enumerates every non-stdlib package directory a target transitively
# imports (verified directly against this repo, e.g.
# go-gardening-demo/servers/garden-business-ops — which looks self-contained
# by directory — actually pulls in access_control/auth/dynamic_proxy/
# relayheaders/triggered-tools-runtime); those directories' *.go files
# become this target's Make prerequisites via $(wildcard).
#
# All DOCKER_TARGETS/CLI rules below are generated from one `define...endef`
# macro per shape, instantiated once per target via $(foreach)+$(eval) — a
# plain `%` pattern rule can't carry a different, dynamically-computed
# prerequisite list per target, which is exactly what's needed here (each
# target's dependency set differs and isn't derivable by string
# substitution alone).
#
# Trade-offs, accepted deliberately:
#  - Every DEPS_* variable uses `:=` (immediate expansion), so each `make`
#    invocation pays one `go list -deps` call per target (13 total) at
#    parse time, regardless of which target was actually requested — a few
#    seconds of fixed overhead, acceptable for a local dev tool.
#  - `-e` keeps a compile error in one target's source from aborting
#    Makefile parsing for unrelated targets; a real error still surfaces
#    later at `docker build`'s own `go build` step inside the container.
#  - $(wildcard dir/*.go) includes _test.go files, which can trigger an
#    unnecessary rebuild on a test-only edit (over-invalidation) but never
#    misses a real one (under-invalidation) — safety over precision.
#  - This Makefile only decides *whether* to invoke `docker build` — it
#    doesn't change how fast that build itself runs. Docker's own layer
#    cache is still invalidated by any repo file change (Dockerfile's
#    `COPY . .` happens before `go build`), which is why Docker's native
#    caching alone was never a reliable incrementality mechanism here; not
#    in scope to change (Dockerfile itself is untouched).
#
# Publishing (scripts/build.sh --release VERSION): every image tag below
# remains local-daemon-only (":local") — publishing is a separate,
# additional step, not a replacement. GHCR_OWNER/RELEASE_VERSION (below),
# when both set, enable each target's publish-$(1) goal, which tags+pushes
# that target's current :local image to ghcr.io/$(GHCR_OWNER)/$(1):
# $(RELEASE_VERSION) — deliberately NOT gated on whether $(1) was actually
# rebuilt this run (see scripts/build.sh's own header comment): a version
# tag must have all DOCKER_TARGETS under it, not just whichever happened to
# change, or a deployer picking that version would find some images
# missing. publish-$(1) depends on image-$(1) (so the local image is at
# least present/current) but is itself .PHONY, so the tag+push always runs
# when requested regardless of image-$(1)'s own staleness outcome.

REPO_ROOT := $(patsubst %/,%,$(dir $(abspath $(lastword $(MAKEFILE_LIST)))))
STAMP_DIR := .build
BIN_DIR := bin
GARDENING_BIN_DIR := go-gardening-demo/bin
GO := go
DOCKER := docker

# Both empty by default — zero effect on plain `make`/`make all`. Set together
# by scripts/build.sh's --release VERSION (GHCR_OWNER inferred from the git
# remote unless overridden) to enable publish-$(1)/publish-all below.
GHCR_OWNER ?=
RELEASE_VERSION ?=

# name -> BUILD_PATH (relative, no leading ./). Basenames match the image
# tag scheme scripts/deploy.sh's deploy_folder()/deploy_proxy() already use
# (<basename>:local), so build.sh/Makefile/deploy.sh all agree on names.
BUILD_PATH_go-proxy                  := cmd/go-proxy
BUILD_PATH_declarative-guardrail     := platform-mcp-servers/declarative-guardrail
BUILD_PATH_event-aggregator          := platform-mcp-servers/event-aggregator
BUILD_PATH_rate-limiter              := platform-mcp-servers/rate-limiter
BUILD_PATH_ai-powered-tools          := ai-mcp-servers/ai-powered-tools
BUILD_PATH_garden-business-ops       := go-gardening-demo/servers/garden-business-ops
BUILD_PATH_garden-customer-admin     := go-gardening-demo/servers/garden-customer-admin
BUILD_PATH_garden-customer-service   := go-gardening-demo/servers/garden-customer-service
BUILD_PATH_garden-online-store       := go-gardening-demo/servers/garden-online-store
BUILD_PATH_garden-staff-admin        := go-gardening-demo/servers/garden-staff-admin
BUILD_PATH_demo-authorization-server := demo-authorization-server

# gardening-demo-seed doesn't build a server at all — BUILD_PATH here only
# drives this target's staleness computation (go list -deps against the
# CLI package it actually builds), and its Dockerfile is its own dedicated
# one (go-gardening-demo/seed/Dockerfile), not the shared root Dockerfile,
# since it also needs to bundle go-gardening-demo/initial-data/ into its
# image — see that Dockerfile's own header comment for why. EXTRA_DEPS
# additionally invalidates this one image whenever a seed JSON file
# changes, since none of that content is Go source and DEPS_$(1)_FILES
# alone would never notice it changed.
BUILD_PATH_gardening-demo-seed       := go-gardening-demo/cli
DOCKERFILE_gardening-demo-seed       := go-gardening-demo/seed/Dockerfile
EXTRA_DEPS_gardening-demo-seed       := $(wildcard go-gardening-demo/initial-data/*.json)

DOCKER_TARGETS := go-proxy declarative-guardrail event-aggregator rate-limiter \
  ai-powered-tools garden-business-ops garden-customer-admin \
  garden-customer-service garden-online-store garden-staff-admin \
  gardening-demo-seed demo-authorization-server

$(STAMP_DIR) $(BIN_DIR) $(GARDENING_BIN_DIR):
	@mkdir -p $@

# $(1) = target name. Uses BUILD_PATH_$(1), which must already be set
# (either statically above, or dynamically for the ad hoc target below).
define DOCKER_IMAGE_RULE
DOCKERFILE_$(1) ?= Dockerfile
EXTRA_DEPS_$(1) ?=
DEPS_$(1) := $$(shell $(GO) list -deps -e -f '{{if not .Standard}}{{.Dir}}{{end}}' ./$(BUILD_PATH_$(1)) 2>/dev/null | grep '^$(REPO_ROOT)')
DEPS_$(1)_FILES := $$(foreach d,$$(DEPS_$(1)),$$(wildcard $$(d)/*.go))

$(STAMP_DIR)/$(1).image.stamp: $$(DEPS_$(1)_FILES) $$(EXTRA_DEPS_$(1)) go.mod go.sum $$(DOCKERFILE_$(1)) | $(STAMP_DIR)
	@echo "==> Building docker image $(1):local (BUILD_PATH=./$(BUILD_PATH_$(1)), Dockerfile=$$(DOCKERFILE_$(1)))"
	$(DOCKER) build -f $$(DOCKERFILE_$(1)) --build-arg BUILD_PATH=./$(BUILD_PATH_$(1)) -t $(1):local .
	@touch $$@

.PHONY: image-$(1) publish-$(1)
image-$(1): $(STAMP_DIR)/$(1).image.stamp

# Always tags+pushes when invoked (not stamp-gated) — see the "Publishing"
# header comment above for why this must not skip an unchanged image.
publish-$(1): image-$(1)
	@if [ -z "$(GHCR_OWNER)" ] || [ -z "$(RELEASE_VERSION)" ]; then \
		echo "GHCR_OWNER and RELEASE_VERSION must both be set to publish $(1)" >&2; exit 1; \
	fi
	@echo "==> Tagging/pushing ghcr.io/$(GHCR_OWNER)/$(1):$(RELEASE_VERSION)"
	$(DOCKER) tag $(1):local ghcr.io/$(GHCR_OWNER)/$(1):$(RELEASE_VERSION)
	$(DOCKER) push ghcr.io/$(GHCR_OWNER)/$(1):$(RELEASE_VERSION)
endef
$(foreach t,$(DOCKER_TARGETS),$(eval $(call DOCKER_IMAGE_RULE,$(t))))

# $(1) = a unique key for the DEPS_ variable name, $(2) = output binary
# path, $(3) = package path to build (e.g. "cli" or "go-gardening-demo/cli").
# The binary file itself is the Make target — no synthetic stamp needed.
define GO_BINARY_RULE
DEPS_$(1) := $$(shell $(GO) list -deps -e -f '{{if not .Standard}}{{.Dir}}{{end}}' ./$(3) 2>/dev/null | grep '^$(REPO_ROOT)')
DEPS_$(1)_FILES := $$(foreach d,$$(DEPS_$(1)),$$(wildcard $$(d)/*.go))

$(2): $$(DEPS_$(1)_FILES) go.mod go.sum | $$(dir $(2))
	@echo "==> Building $(2)"
	$(GO) build -o $(2) ./$(3)
endef
$(eval $(call GO_BINARY_RULE,cli,$(BIN_DIR)/cli,cli))
$(eval $(call GO_BINARY_RULE,gardening-cli,$(GARDENING_BIN_DIR)/cli,go-gardening-demo/cli))

# -----------------------------------------------------------------------
# Ad hoc target — mirrors scripts/deploy.sh's --deploy-server PATH.
# scripts/build.sh --build-server PATH invokes:
#   make ADHOC_NAME=<basename> ADHOC_BUILD_PATH=<rel-path> adhoc-image
# which reuses the exact same DOCKER_IMAGE_RULE macro (same correctness
# guarantees, same .build/<name>.image.stamp naming) as every static
# target, so deploy.sh's `docker image inspect <name>:local` preflight
# works identically regardless of how the image was built.
# -----------------------------------------------------------------------
ifdef ADHOC_NAME
  ifndef ADHOC_BUILD_PATH
    $(error ADHOC_BUILD_PATH must be set together with ADHOC_NAME)
  endif
  BUILD_PATH_$(ADHOC_NAME) := $(ADHOC_BUILD_PATH)
  $(eval $(call DOCKER_IMAGE_RULE,$(ADHOC_NAME)))
endif

.PHONY: adhoc-image
adhoc-image:
ifndef ADHOC_NAME
	$(error ADHOC_NAME must be set — this target is only meant to be invoked via scripts/build.sh --build-server)
endif
	@$(MAKE) $(STAMP_DIR)/$(ADHOC_NAME).image.stamp

# -----------------------------------------------------------------------
# Convenience groupings — mainly for a developer running `make` directly;
# scripts/build.sh itself only ever calls `make all` (see its own header).
# -----------------------------------------------------------------------
.PHONY: build-proxy build-platform-servers build-ai build-gardening-demo build-auth-server all publish-all prune clean

build-proxy:            image-go-proxy $(BIN_DIR)/cli
build-platform-servers: image-declarative-guardrail image-event-aggregator image-rate-limiter
build-ai:                image-ai-powered-tools
build-gardening-demo:   image-garden-business-ops image-garden-customer-admin \
                        image-garden-customer-service image-garden-online-store \
                        image-garden-staff-admin image-gardening-demo-seed $(GARDENING_BIN_DIR)/cli
build-auth-server:      image-demo-authorization-server

all: build-proxy build-platform-servers build-ai build-gardening-demo build-auth-server

# Publishes every DOCKER_TARGETS image under GHCR_OWNER/RELEASE_VERSION —
# see scripts/build.sh --release, and the "Publishing" comment above.
publish-all: $(foreach t,$(DOCKER_TARGETS),publish-$(t))

prune:
	@echo "==> Pruning dangling Docker images"
	$(DOCKER) image prune -f

clean:
	rm -rf $(STAMP_DIR) $(BIN_DIR) $(GARDENING_BIN_DIR)
