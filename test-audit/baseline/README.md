# Baseline measurements (before any change)

All numbers are measured, not estimated. Two sources:

* **CI** — GitHub Actions step timings and job logs for real runs of this
  workflow (`gh api .../actions/runs/<id>/jobs`, `gh api .../jobs/<id>/logs`).
  The E2E job runs on `ubuntu-latest` (4 vCPU); this is the number the plan
  cares about.
* **Local** — this workstation: 2 vCPU, 15 GiB, docker 26.1.5, kind v0.33.0,
  the pre-existing `turnfleet` kind cluster (1 control plane + 3 relay workers).
  Absolute local numbers are ~3-10x the CI numbers; only the split between
  phases and the before/after ratio are meaningful locally.

## CI: the two E2E steps, phase by phase

Run `38719831132` (PR `feat/relay-config-secret`, 2026-10-15, success).
Job `e2e`: 2026-10-15T10:22:44Z -> 10:32:51Z = **607 s**.

### Step "Run the end-to-end suite" (`make test-e2e`): 386 s

| Phase | Makefile target | Seconds |
|---|---|---|
| kind create cluster (pull node image, prepare nodes, CNI, join 3 workers) | `kind-up` | 46 |
| docker build controller image | `docker-build` | 101 |
| docker build probe image | `docker-build` | 53 |
| host binaries (`make build`: controller-gen + 2 `go build`) | `build` | 62 |
| CRDs applied | `install` | 6 |
| kind load coturn + controller + probe into 4 nodes | `load-images` | 29 |
| kustomize apply + rollout restart + rollout status | `deploy` | 12 |
| `go test -tags=e2e` | — | 81 |
| — of which the package reported | — | 67.7 |

**Setup 305 s (79 %), spec execution 67.7 s (18 %).**

### Step "Install the controller from the chart and probe a relay" (`make test-e2e-chart`): 174 s

| Phase | Makefile target | Seconds |
|---|---|---|
| docker build controller image (again) | `docker-build` | 81 |
| docker build probe image (again) | `docker-build` | 56 |
| host binaries (cached) | `build` | 3 |
| kind load coturn + controller + probe (again) | `load-images` | 7 |
| undeploy kustomize, uninstall CRDs | `undeploy`, `uninstall` | 6 |
| helm install + rollout status | `helm-install` | 12 |
| `go test -tags=e2e -run TestProbeCLIFromTheHost` | — | 9 |
| — of which the package reported | — | 7.5 |

**Setup 165 s (95 %), spec execution 7.5 s (4 %).**

### Why the images rebuild

The controller image built in the first step and the one built in the second
step have different IDs (`sha256:c27e170c6…` vs `sha256:dfa174b7c…`) although
nothing in the tree changed between them. `DATE` in the Makefile is
`$(shell date -u +%Y-%m-%dT%H:%M:%SZ)`, it is part of `LDFLAGS`, `LDFLAGS` is a
`docker build --build-arg`, and the `RUN go build … -a` layer therefore misses
the layer cache on every invocation. `-a` then recompiles the whole dependency
graph (client-go, controller-runtime, prometheus, pion).

### E2E job history (last 50 runs)

| Outcome | Runs |
|---|---|
| success | 36 |
| failure | 10 |
| cancelled (superseded by `concurrency`) | 4 |

Wall time of successful runs: 10.0-13.5 min, median ~12.2 min.

The 10 failures are **not** spec failures:

| Step that failed | Runs | Message |
|---|---|---|
| `deploy` (`Makefile:252/253`) | 8 | `error: timed out waiting for the condition` (rollout status, 180 s) |
| `docker-build` (`Makefile:175`) | 2 | renovate branches, dependency build break |

All 8 deploy failures are on 2026-10-13 between 06:48Z and 09:17Z; the same
step takes 11-12 s on the successful runs, so the 180 s timeout is not short —
something made the old pod unable to terminate within 3 minutes.

## CI: the `ci` job

Runs `make verify`, `helm-verify`, `lint`, `test`, `vuln`, `licenses`, DCO.
Job wall time 2.2-2.5 min (occasionally 3.2-3.5 min).

| Step | Seconds (run 38719831215 / 38721140331 / 38712171076) |
|---|---|
| Set up Go | 45 / 27 / 44 |
| Verify generated files | 2 / 4 / 4 |
| Lint the chart | 6 / 5 / 6 |
| Lint (golangci-lint) | 33 / 39 / 32 |
| Test (`make test`) | 28 / 29 / 27 |
| Scan for vulnerabilities | 15 / 17 / 15 |
| Check dependency licenses | 2 / 1 / 1 |

`make test` is 27-29 s in CI and 33.7 s locally — the smallest layer by far.

## envtest layer: local baseline

`go test -race ./...` minus `test/e2e`, `KUBEBUILDER_ASSETS=bin/k8s/1.37.0-linux-amd64`,
wall time **33.7 s** (two packages run concurrently, `-p` defaults to `GOMAXPROCS=2` here):

| Package | Elapsed | Tests |
|---|---|---|
| `internal/controller` | 21.86 s | 56 |
| `internal/webhook/v1alpha1` | 10.25 s | 69 |
| `cmd/turnfleet-probe` | 1.10 s | 11 |
| `internal/probe` | 1.08 s | 27 |
| `internal/relay` | 1.04 s | 88 |
| `internal/vantage` | 1.02 s | 7 |
| `internal/version` | 1.01 s | 2 |

Slowest individual tests: `TestAPIs` (webhook suite) 9.08 s,
`TestCrossPoolAddressConflict` 2.79 s, `TestDrainTimeoutBoundsTheWait` 1.63 s.

Ginkgo webhook suite: 22 specs in 7.41 s of which `BeforeSuite` is 3.43 s and
`AfterSuite` 1.12 s — **61 % of the suite is the envtest control plane, not
the specs.** The slowest real spec is 2.05 s ("stores the defaults the webhook
filled in").

Per-spec report: `baseline/ginkgo-webhook-before.json`.

## E2E layer: local baseline

`make test-e2e` against the pre-existing kind cluster:

| Phase | Seconds |
|---|---|
| setup (`kind-up docker-build build install load-images deploy`) | 484.1 |
| spec execution (`go test -tags=e2e -v`) | see `e2e-specs-before.txt` |

On this 2-vCPU box the two `docker build` invocations alone account for most of
the 484 s; on CI they are 154 s of the 386 s.
