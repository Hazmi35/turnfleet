# Pinned versions

Every version this repository builds with, tests against, or deploys is pinned,
and nothing is fetched at an unpinned version. The pins themselves live next to
what they pin, so there is one copy of each number and no second table to drift:

* Go: the `go` directive and the `tool` directives in `go.mod` (repository
  tools) and `tools/go.mod` (the lint toolchain); the `GO_VERSION` env in the
  workflows tracks the `go` directive.
* Build, test and cluster tooling: `Makefile` (`CONTROLLER_TOOLS_VERSION`,
  `ENVTEST_K8S_VERSION`, `KUSTOMIZE_VERSION`, `KIND_IMAGE`, `HELM_VERSION`).
* golangci-lint: the `tool` directive in `tools/go.mod`, matched by
  `.custom-gcl.yml`; its plugin modules are pinned in `.custom-gcl.yml` alone.
* Container images: the `Makefile` (`GO_IMAGE`, `RUNTIME_IMAGE`, `KIND_IMAGE`,
  `COTURN_IMAGE` with `COTURN_IMAGE_DIGEST`), referenced as `tag@sha256:digest`.
* GitHub Actions: pinned by commit SHA in `.github/workflows/`, each with the
  tag the SHA came from as a comment.

Bumping a version is a pull request that moves every copy of it together:
`.github/pull_request_template.md` asks for it and Renovate groups the bumps
that must move as one unit (`renovate.json`). What follows is the part the
manifests cannot express — which pins are deliberately held, and why (ADR 11).

## Notes on the Go toolchain

* The repository requires Go 1.27.2 (`go.mod` `go`). controller-runtime itself
  requires Go 1.26.0, so the pin is deliberately newer than the floor its
  dependencies set, and a build cannot silently pick an older toolchain.
* `go`/`tool` directive support landed in Go 1.24, so the pinned toolchain can
  carry the `tool` directives that replace the old `tools.go` blank imports.
* The lint toolchain alone lives in a separate `tools/` module: golangci-lint's
  dependency graph (several hundred modules) must not enter the module graph of
  the shipped binaries.

## Held modules and vulnerability floors

Two transitive dependencies are held at or above the versions that fix known
vulnerabilities, because `make vuln` (govulncheck) fails on a reachable one:
`google.golang.org/grpc` at `v1.83.2` (GO-2026-6443, an xDS server panic on a
request carrying neither `:authority` nor `Host`, and with it GO-2026-6348, heap
exhaustion via HTTP/2 DATA frame fragmentation) and
`go.opentelemetry.io/otel/*` at `v1.45.0` (GO-2026-6505, fixed in the OTLP trace
exporter and SDK). Lowering either floor re-introduces a failing `make vuln`
run.

Neither grpc advisory is reachable from this code today: `make vuln` lists them
among the vulnerabilities in packages the build imports but does not call. The
import is controller-runtime's metrics filters into `k8s.io/apiserver`'s webhook
token authenticator and from there the egress selector, which is a gRPC
*client*; turnfleet serves no gRPC, so the xDS server panic has no path in. The
floor stays current because the module is pinned here anyway and the fix is an
upstream backport, not because the code is exploitable.

`k8s.io/kube-openapi` is held at `v0.0.0-20260721132016-d427ff9ee9ad`, the
version every `k8s.io/*` v0.37.0 library requires. kustomize `v5.8.3`
(`kyaml v0.21.3`) requires a newer kube-openapi whose schema conversion switched
to `sigs.k8s.io/structured-merge-diff/v7`, which does not type-check against
`k8s.io/apimachinery@v0.37.0` (it still uses v6). The module graph therefore
resolves to kustomize `v5.8.2`, which is what the `tool` directive pins. Moving
past that requires the whole `k8s.io/*` set to move, which is a deliberate
change, not a Renovate bump.

## Container images

Images are referenced as `tag@sha256:digest`, and the digest is the multi-arch
index digest unless stated otherwise, so a build cannot pick up a re-pushed tag.

`COTURN_IMAGE` also carries the tag alone, because the kind cluster is loaded
with `docker save`, which cannot take an index digest; `COTURN_IMAGE_DIGEST` is
what that tag must resolve to and `make load-coturn` verifies the two before
loading. Digest pinning of the relay image itself is enforced by the validating
webhook and covered by the webhook envtest suite.

A coturn bump is a Renovate pull request. `renovate.json` extracts the
`COTURN_IMAGE`/`COTURN_IMAGE_DIGEST` pair as a single dependency, so the two can
never move apart (`make load-coturn` rejects a tag that does not resolve to the
recorded digest), and groups the example pool in `config/samples/` with it, so
one pull request moves both. Two copies still move by hand: the webhook test
fixtures, which need a syntactically valid digest rather than this one, and
`test/e2e`'s `defaultCoturnImage`, which Renovate's default `ignorePaths` skips
(`test/**`) and which `make test-e2e` overrides with the Makefile value anyway.
A coturn bump also re-captures `internal/relay/testdata/coturn-<version>-help.txt`,
the flag list `TestRenderedFlagsExistInPinnedCoturn` holds the renderer to.

Renovate maintains the other image pins in the `Makefile` the same way:
`KIND_IMAGE`, `GO_IMAGE` and `RUNTIME_IMAGE` are each extracted as one
dependency holding its `tag@sha256:digest`, because the Dockerfiles name their
base images only as `ARG`s that the Makefile passes at build time — a default
manager sees `FROM ${GO_IMAGE}` and no version to resolve. `GO_IMAGE` carries
the Go toolchain and is grouped with the `go` directive and the workflows'
`GO_VERSION` (`go toolchain` in `renovate.json`); `KIND_IMAGE` is what the
`kind node image` rule groups; distroless `nonroot` is a tag rather than a
version, so that dependency is digest upkeep.

The coturn image is the upstream, unmodified BSD-3-Clause data plane. It is
never vendored into this repository (see `docs/adr/0002-coturn-data-plane.md`).

## Helm

Helm, which lints and renders `charts/turnfleet`, is the one pinned tool that is
not a Go module and not an image: `HELM_VERSION` in the Makefile is the pin, and
the `helm` target downloads the release archive from `get.helm.sh` into
`$(LOCALBIN)` unless that version is already there, checking it against the
`.sha256sum` the same release publishes next to it. The checksum is not a second
pin — it comes from the version being installed — so a Renovate bump moves the
version and the checksum together. `renovate.json` reads the line with a
`github-releases` custom manager on `helm/helm`, which is where the tag comes
from. (`kind` is pinned the same way in the e2e workflow: version plus the
release's checksum.)

The chart carries no version of its own for the images: `make helm-install`
passes the Makefile's `IMAGE_REGISTRY` and `IMAGE_TAG`, the same values
`make deploy` sets on the cluster. Installed without those flags the chart uses
its `appVersion`, which until the project tags a release is `dev` — the tag
`make docker-build` builds and `make docker-buildx` pushes. A release that
publishes images should bump the chart's `appVersion` with them, so an install
without flags gets the released image.

## GitHub Actions

Actions are pinned by commit SHA, and the comment after each pin is the tag the
SHA came from, so an upgrade is a deliberate edit and a re-pointed tag cannot
change what runs. The DCO check runs `hack/verify-dco.sh` from this repository
instead of a third-party action, which keeps the workflow free of extra pins and
tokens.

## Release tooling

`.github/workflows/release.yaml` installs the two tools that sign, scan and
attest a release, and pins both in its own `env`. syft (`SYFT_VERSION`) is
pinned the way `e2e.yaml` pins the kind binary: the release tarball and its
sha256, checked before the binary runs. Neither pin is tracked by Renovate: a
bot bump of the signing tool is a change to a trust anchor, and the checksum
that belongs to a download pin is not something a bot can compute.

cosign is **held on the v2 line** (`v2.6.5`, released 2026-08-06) although v3
exists. cosign v3 writes signatures and attestations as OCI 1.1 referrer
artifacts, and ghcr.io does not serve the Referrers API, so a v3-signed release
is not discoverable there; the v2 line writes the `sha256-<digest>.sig` and
`.att` tags ghcr.io has served since 2021, which both cosign v2 and v3 verify.
Moving to v3 starts with ghcr.io serving that API (ADR 16), not with a version
bump.

## Agent tooling

Claude Code needs 2.1.278 or newer to read `AGENTS.md` instead of its own
`CLAUDE.md` (see Provenance below).

The harness content itself carries no pin and names no vendor: the skills and
subagent definitions live under `.agents/`, the hook policy under `hack/agent/`,
and each harness is wired to them by symlinks and adapters (ADR 10). Only a
harness that reads the instruction file through its own rules needs a floor
version, which is why Claude Code is the only one named.

## Provenance

The pins were verified against the sources below, not from memory:

* `kubectl`/`kind`/`controller-runtime`/`controller-tools`/`envtest` versions
  were cross-checked so that the envtest control plane (1.37.0) matches the
  `k8s.io/*` libraries (v0.37.0) that controller-runtime requires. The kind node
  image must stay on that same Kubernetes minor version (`renovate.json` groups
  it).
* Claude Code 2.1.278 is the floor for `AGENTS.md` support:
  <https://github.com/anthropics/claude-code/blob/main/CHANGELOG.md> — "Added
  AGENTS.md support: in a project with no CLAUDE.md, Claude Code reads
  AGENTS.md instead". This repository relies on that behaviour.
* The `--license` flag of the pinned kubebuilder accepts only `apache2` and
  `none` (`kubebuilder init --help`), so the project is initialised with
  `--license none` and supplies its own AGPL-3.0-only header from
  `hack/boilerplate.go.txt`.
* Every coturn flag the renderer emits is checked against the pinned image with
  `turnserver --help`; the flags used are listed in
  `docs/adr/0008-coturn-config-rendering.md`.
