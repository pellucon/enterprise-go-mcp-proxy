# Copyright (c) 2026 Enterprise Go MCP Proxy
# SPDX-License-Identifier: MIT

# Dedicated Dockerfile for the gardening-demo-seed image — the one-shot
# in-cluster seeding Job for go-gardening-demo (see ../k8s/job.yaml and
# scripts/deploy.sh's seed_gardening_demo_if_needed). Deliberately NOT
# built from the shared root Dockerfile: that one only ever copies a
# single compiled binary into its final image, with no mechanism for also
# bundling static data alongside it. This is the one target that needs
# exactly that — go-gardening-demo/initial-data/ is 1.6MB (sample-events
# .json alone is 1.35MB), well over a ConfigMap's 1MiB limit, so it has to
# be baked into the image rather than mounted. No other image target needs
# anything like this.
FROM golang:1.26-alpine AS build
# Declared but unused: the Makefile's shared DOCKER_IMAGE_RULE recipe
# always passes --build-arg BUILD_PATH=... for every target, including
# this one (whose actual go build path is hardcoded below, since this
# image only ever builds one specific binary). Declaring the ARG avoids
# Docker's "build-args were not consumed" warning.
ARG BUILD_PATH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/cli ./go-gardening-demo/cli

FROM alpine:3.20
# Links the image's GHCR package to this repository: GitHub lists the
# package on the repository's page, and the package can take its access
# from the repository's.
LABEL org.opencontainers.image.source="https://github.com/pellucon/enterprise-go-mcp-proxy" \
      org.opencontainers.image.licenses="MIT"
RUN adduser -D -H app
COPY --from=build /out/cli /app/cli
COPY go-gardening-demo/initial-data /app/initial-data
WORKDIR /app
USER app
ENTRYPOINT ["/app/cli"]
