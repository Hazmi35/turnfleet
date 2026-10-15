# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Controller image. Multi-stage, reproducible, no shell in the final image.
ARG GO_IMAGE
ARG RUNTIME_IMAGE

FROM --platform=${BUILDPLATFORM} ${GO_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
ARG LDFLAGS

WORKDIR /workspace
COPY go.mod go.sum ./
# The module cache and the build cache are BuildKit cache mounts, not layers:
# they are shared between the two images built in one `make docker-build` and
# between the two `make docker-build` runs a CI job makes (one for the suite,
# one for the chart), and they never end up in the image. Without them a
# change to LDFLAGS — which the Makefile stamps with the build date — misses
# the layer cache and recompiles the whole dependency graph.
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# -trimpath keeps the build reproducible: no local paths end up in the binary.
# -a is deliberately absent: it forces every package to be rebuilt, which
# defeats the build cache above while producing the same binary. A build with
# no cache to reuse compiles everything anyway, so -a only ever cost time.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/turnfleet-controller ./cmd/turnfleet-controller

FROM ${RUNTIME_IMAGE}
# RUNTIME_IMAGE is declared before the first FROM, which puts it out of scope
# here: without this line the label below expands to an empty string.
ARG RUNTIME_IMAGE
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.title="turnfleet-controller" \
      org.opencontainers.image.description="Turnfleet TURNPool operator" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/Hazmi35/turnfleet" \
      org.opencontainers.image.url="https://github.com/Hazmi35/turnfleet" \
      org.opencontainers.image.documentation="https://github.com/Hazmi35/turnfleet/blob/main/docs/architecture.md" \
      org.opencontainers.image.vendor="Turnfleet Authors" \
      org.opencontainers.image.base.name="${RUNTIME_IMAGE}"
COPY --from=builder /out/turnfleet-controller /turnfleet-controller
USER 65532:65532
ENTRYPOINT ["/turnfleet-controller"]
