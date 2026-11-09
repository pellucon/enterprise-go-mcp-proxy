# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Generic multi-stage build for any of this module's binaries: the proxy
# itself (BUILD_PATH=./cmd/go-proxy) or a backend server (e.g.
# BUILD_PATH=./platform-mcp-servers/rate-limiter). The Makefile always passes
# BUILD_PATH explicitly; the default below is the proxy.
# See deploy.sh for how each image is built from this one Dockerfile.
FROM golang:1.26-alpine AS build
ARG BUILD_PATH=./cmd/go-proxy
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app ${BUILD_PATH}

FROM alpine:3.20
# Links the image's GHCR package to this repository: GitHub lists the
# package on the repository's page, and the package can take its access
# from the repository's.
LABEL org.opencontainers.image.source="https://github.com/pellucon/enterprise-go-mcp-proxy" \
      org.opencontainers.image.licenses="MIT"
# declarative-guardrail's guardrailcfg config (declarativeGuardrails'
# per-declaration "timezone", e.g. "EST"/"America/New_York") is parsed via
# time.LoadLocation, which resolves named zones from the OS's IANA tzdata —
# not bundled by default in alpine, so without this package every named
# timezone (including otherwise-valid ones like "EST") fails to load and
# any binary that parses such a config (go-proxy itself, at startup, since
# it type-checks every combined-config section including
# declarativeGuardrails; declarative-guardrail's own Watcher) panics/errors.
RUN apk add --no-cache tzdata
RUN adduser -D -H app
COPY --from=build /out/app /app
# Relative config paths (e.g. auth.publicKeyPath: "./public.pem") resolve
# against this — it's where jwt-public-key's Secret volume gets mounted.
WORKDIR /app-run
USER app
ENTRYPOINT ["/app"]
