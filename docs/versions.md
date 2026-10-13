# Pinned versions

Every version this repository builds with, tests against, or deploys is pinned
here together with the source it was read from. Nothing is fetched at an
unpinned version: Go tools are `tool` directives, container images are pinned by
tag **and** digest, and GitHub Actions are pinned by commit SHA.

Bumping a version means updating this table in the same pull request
(`.github/pull_request_template.md` asks for it, and Renovate groups the bumps
that must move together).

## Build and test toolchain

| Component | Pin | Where it is pinned | Source |
| --- | --- | --- | --- |
| Go | `1.27.2` | `go.mod` `go`/`toolchain`, `Dockerfile` `GO_IMAGE`, CI `GO_VERSION` | <https://go.dev/dl/?mode=json> (latest stable, `time` 2026-10-02) |
| kubebuilder | `v4.16.0` | scaffolding only; recorded in `PROJECT` | <https://github.com/kubernetes-sigs/kubebuilder/releases/tag/v4.16.0> (published 2026-09-10) |
| controller-runtime | `v0.25.2` | `go.mod` | <https://github.com/kubernetes-sigs/controller-runtime/releases/tag/v0.25.2> (published 2026-10-01) |
| controller-tools (`controller-gen`) | `v0.22.0` | `go.mod` `tool` | <https://github.com/kubernetes-sigs/controller-tools/releases/tag/v0.22.0> (published 2026-09-02) |
| setup-envtest | `v0.25.2` | `go.mod` `tool` | <https://github.com/kubernetes-sigs/controller-runtime/releases/tag/v0.25.2> |
| envtest Kubernetes | `1.37.0` | `Makefile` `ENVTEST_K8S_VERSION` | <https://github.com/kubernetes-sigs/controller-tools/releases/tag/envtest-v1.37.0> (binary asset `envtest-v1.37.0-linux-amd64.tar.gz`) |
| k8s.io/api, apimachinery, client-go, apiextensions-apiserver | `v0.37.0` | `go.mod` (required by controller-runtime v0.25.2) | `controller-runtime@v0.25.2/go.mod` <https://raw.githubusercontent.com/kubernetes-sigs/controller-runtime/v0.25.2/go.mod> |
| Kubernetes (cluster) | `1.37.x` | `Makefile` `KIND_IMAGE` | <https://github.com/kubernetes/kubernetes/releases> (v1.37.1 is the current stable patch) |
| golangci-lint | `v2.14.0` | `tools/go.mod` `tool`, `.custom-gcl.yml` `version` | <https://github.com/golangci/golangci-lint/releases/tag/v2.14.0> (published 2026-09-24) |
| kube-api-linter | `v0.0.0-20261012124120-79b557aa8c8d` | `.custom-gcl.yml` plugin | <https://pkg.go.dev/sigs.k8s.io/kube-api-linter?tab=versions> (no tagged releases; pseudo-version of `79b557aa8c8d`) |
| logtools (`logcheck` plugin) | `v0.10.1` | `.custom-gcl.yml` plugin | <https://proxy.golang.org/sigs.k8s.io/logtools/@latest> |
| govulncheck | `v1.9.0` (`golang.org/x/vuln`) | `go.mod` `tool` | <https://github.com/golang/vuln/releases/tag/v1.9.0> |
| crd-ref-docs | `v0.3.0` | `go.mod` `tool` | <https://github.com/elastic/crd-ref-docs/releases/tag/v0.3.0> |
| google/licensecheck | `v0.3.1` | `go.mod` (used by `hack/licenses`) | <https://pkg.go.dev/github.com/google/licensecheck> |
| kustomize | `v5.8.2` | `go.mod` `tool` | <https://github.com/kubernetes-sigs/kustomize/releases> (tag `kustomize/v5.8.2`) |
| kind | `v0.33.0` | `Makefile` / CI, developer install | <https://github.com/kubernetes-sigs/kind/releases/tag/v0.33.0> (published 2026-08-26) |
| pion/turn | `v5.1.2` | `go.mod` | <https://github.com/pion/turn/releases/tag/v5.1.2> (published 2026-09-15) |
| ginkgo / gomega | `v2.32.1` / `v1.43.0` | `go.mod` | resolved by `go get`; see <https://github.com/onsi/ginkgo/releases> |

### Notes on the Go toolchain

* The repository requires Go 1.27.2. The root `go.mod` declares both
  `go 1.27.2` and `toolchain go1.27.0`-style pins so a build cannot silently use
  an older toolchain: controller-runtime v0.25.2 itself requires Go 1.26.0, so
  1.27.2 is deliberately newer.
* `go`/`tool` directive support landed in Go 1.24, so the pinned toolchain
  supports `tool` directives. The lint toolchain alone lives in a separate
  `tools/` module because golangci-lint's dependency graph (several hundred
  modules) must not enter the module graph of the shipped binaries.
* `k8s.io/kube-openapi` is held at `v0.0.0-20260721132016-d427ff9ee9ad`, the
  version every `k8s.io/*` v0.37.0 library requires. kustomize `v5.8.3`
  (`kyaml v0.21.3`) requires a newer kube-openapi whose schema conversion
  switched to `sigs.k8s.io/structured-merge-diff/v7`, which does not
  type-check against `k8s.io/apimachinery@v0.37.0` (it still uses v6). The
  module graph therefore resolves to kustomize `v5.8.2`, which is what the
  `tool` directive pins.

## Container images

Images are referenced as `tag@sha256:digest`. The digest is the multi-arch
index digest unless stated otherwise.

| Image | Pin | Used by | Source |
| --- | --- | --- | --- |
| `golang:1.27.2-bookworm` | `@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61` | `Dockerfile`, `Dockerfile.probe` builder stage | <https://hub.docker.com/v2/repositories/library/golang/tags/1.27.2-bookworm> |
| `gcr.io/distroless/static-debian12:nonroot` | `@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab` | both runtime stages | <https://github.com/GoogleContainerTools/distroless> (index digest read from the registry `Docker-Content-Digest` header) |
| `kindest/node:v1.37.0` | `@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5` | `make kind-up`, e2e | <https://hub.docker.com/v2/repositories/kindest/node/tags/v1.37.0> |
| `coturn/coturn:4.18.0-r0` | `@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3` | relay data plane | <https://github.com/coturn/coturn/releases/tag/docker%2F4.18.0-r0> (published 2026-09-08) |
| `ghcr.io/hzmi/turnfleet-controller` | built by CI | operator | `image.yaml` |
| `ghcr.io/hzmi/turnfleet-probe` | built by CI | probe CLI | `image.yaml` |

The coturn image is the upstream, unmodified BSD-3-Clause data plane. It is
never vendored into this repository (see `docs/adr/0002-coturn-data-plane.md`).

## GitHub Actions

Actions are pinned by commit SHA; the comment after each pin is the tag the SHA
came from. Sources are the `refs/tags/<tag>` entries of each repository.

| Action | Pin | Source |
| --- | --- | --- |
| `actions/checkout` | `3d3c42e5aac5ba805825da76410c181273ba90b1` (v7.0.1) | <https://github.com/actions/checkout/releases> |
| `actions/setup-go` | `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e` (v7.0.0) | <https://github.com/actions/setup-go/releases> |
| `actions/cache` | `55cc8345863c7cc4c66a329aec7e433d2d1c52a9` (v6.1.0) | <https://github.com/actions/cache/releases> |
| `actions/upload-artifact` | `cf430e030ddbb5b0abf93d22962f4752f3646cd9` (v7.0.2) | <https://github.com/actions/upload-artifact/releases> |
| `docker/setup-buildx-action` | `f87e5991a6d7451dcb8d9637bfbc97413f497069` (v4.4.1) | <https://github.com/docker/setup-buildx-action/releases> |
| `docker/setup-qemu-action` | `99012661954931238ded8c8b007157a8430204e1` (v4.4.0) | <https://github.com/docker/setup-qemu-action/releases> |
| `docker/login-action` | `dbcb813823bdd20940b903addbd779551569679f` (v4.6.0) | <https://github.com/docker/login-action/releases> |
| `docker/build-push-action` | `c3c9e263c25d99ce0380d002d59b67737d91b0dc` (v7.4.0) | <https://github.com/docker/build-push-action/releases> |
| `docker/metadata-action` | `dc802804100637a589fabce1cb79ff13a1411302` (v6.2.0) | <https://github.com/docker/metadata-action/releases> |

The DCO check runs `hack/verify-dco.sh` from this repository instead of a
third-party action, which keeps the workflow free of extra pins and tokens.

## Agent tooling

| Component | Pin | Source |
| --- | --- | --- |
| Claude Code (minimum) | `2.1.278` | <https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md>: "Added AGENTS.md support: in a project with no CLAUDE.md, Claude Code reads AGENTS.md instead". This repository relies on that behaviour, so 2.1.278 is the floor. |
| Claude Code (current at the time of writing) | `2.1.296` | same changelog |

Because a `CLAUDE.md` in the working directory or any parent shadows
`AGENTS.md`, `make verify` fails if one exists anywhere in the tree
(`hack/verify-no-claude-md.sh`).

## Verification log

The pins above were verified against the sources listed, not from memory:

* `kubectl`/`kind`/`controller-runtime`/`controller-tools`/`envtest` versions
  were cross-checked so that the envtest control plane (1.37.0) matches the
  `k8s.io/*` libraries (v0.37.0) that controller-runtime v0.25.2 requires.
* The `--license` flag of the pinned kubebuilder accepts only `apache2` and
  `none` (`kubebuilder init --help`), so the project is initialised with
  `--license none` and supplies its own AGPL-3.0-only header from
  `hack/boilerplate.go.txt`.
* Every coturn flag the renderer emits is checked against the pinned image with
  `turnserver --help`; the flags used are listed in
  `docs/adr/0006-coturn-config-rendering.md`.
