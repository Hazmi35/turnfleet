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
RUN go mod download
COPY . .
# -trimpath keeps the build reproducible: no local paths end up in the binary.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -a -ldflags "${LDFLAGS}" -o /out/turnfleet-controller ./cmd/turnfleet-controller

FROM ${RUNTIME_IMAGE}
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.title="turnfleet-controller" \
      org.opencontainers.image.description="Turnfleet TURNPool operator" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.source="https://github.com/hzmi/turnfleet" \
      org.opencontainers.image.url="https://github.com/hzmi/turnfleet" \
      org.opencontainers.image.documentation="https://github.com/hzmi/turnfleet/blob/main/docs/architecture.md" \
      org.opencontainers.image.vendor="Turnfleet Authors" \
      org.opencontainers.image.base.name="${RUNTIME_IMAGE}"
COPY --from=builder /out/turnfleet-controller /turnfleet-controller
USER 65532:65532
ENTRYPOINT ["/turnfleet-controller"]
