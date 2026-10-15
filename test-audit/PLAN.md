# Plan, ranked by (time saved or risk removed) / (effort + risk)

Savings marked **[measured]** come from a run recorded in `baseline/` or in the
commit that made the change. Savings marked **[derived]** are the same phases
measured in CI run 38719831132 and scaled by the measured ratio; they are the
ones to confirm on the next real CI run.

| # | Change | Saves | Effort | Risk | Status |
|---|---|---|---|---|---|
| 1 | Reuse the Go build cache between image builds (BuildKit cache mounts, drop `-a`) | CI **[derived]** ~45s in the suite step (the probe image stops recompiling the 281 packages the controller image just compiled); **[measured]** 399.4s → 237.7s for a cold `make docker-build`, → 27.1s for a warm one | 10 lines in 2 Dockerfiles | Low: the cache mounts are not layers, the artefact is unchanged, BuildKit is the default builder since Docker 23 | **done** `673adeb` |
| 2 | Chart step reuses the images the suite step built and loaded (`REUSE_IMAGES=1`) | CI **[derived]** 144s of the 174s step (81s + 56s build, 7s load) | 20 lines Makefile + 1 workflow line | Low: opt-in, default 0, so a standalone run still builds | **done** `2125eb4`, `059971d` |
| 3 | Build only the probe host binary for the e2e suite | CI **[derived]** ~14s (the controller host binary was 61.8s of the 62s `make build`; the probe alone still compiles 281 client-go/controller-runtime packages) | 8 lines Makefile | Low: nothing in the suite reads the controller host binary | **done** `9cb5ac4` |
| 4 | Dump cluster state when the controller rollout stalls | none; removes the undiagnosability of 8 of the last 50 E2E runs | 16 lines Makefile | None: same gate, same timeout | **done** `7939fd4` |
| 5 | envtest harness: measured timeouts, counter namespaces, one List per `markPodsReady` | no measurable timing win (`internal/controller` 20.04s → 19.78s over 5 runs each **[measured]**, within spread); removes two Gomega 1s defaults and a clock-derived namespace | 30 lines | Low | **done** `b244053` |
| 6 | Fix the tests that could not fail (2 vacuous planner tests, 1 vacuous version test, 3 weak webhook assertions) | none; removes false confidence | 60 lines | Low, each proven by break/restore | **done** `689ab72`, `46b62a6`, `73968fc` |
| 7 | Skip the e2e job for documentation-only pull requests | the whole job on those PRs: **10.0-13.5 min** → 0 | 7 lines workflow | Low: only `**.md`, `docs/**`, `LICENSE`, the PR template | **done** `d4822e6` |
| 8 | Parallelise the eight e2e specs | CI **[derived]** up to ~40s of the 67.7s spec time | days: unique namespaces, per-spec port ranges, serial labels for the two allocation-sensitive specs, then 5 clean reruns to prove it | Medium-high: they share one namespace, three relay nodes, the host network, and two of them assert on allocation counts and drain timing | not done, ranked |
| 9 | Run the image's probe binary instead of building one | CI **[derived]** ~48s of the 62s host build (the probe compile); locally **[measured]** 151.4s cold | 10 lines Makefile (`docker create` + `docker cp`) | Medium: the binary comes from the image rather than `make build`; identical tree, flags and platform, but surprising | not done, ranked |
| 10 | Persist the BuildKit cache across CI runs (`buildx` + `type=gha`) | CI **[derived]** most of the first image build, 101s per run | 15 lines Makefile + workflow, a `docker-container` builder | Medium: changes the builder for local users too, and can only be verified in CI | not done, ranked |
| 11 | Share one envtest control plane between the two envtest packages | **[measured]** 4.4s (`BeforeSuite` 3.4s + `AfterSuite` 1.1s of the webhook suite) of a 28s step | a `test/envtest` package importing both, with the helpers moved | Medium: two packages cannot share a process under `go test`; the helpers are package-private today | not done, ranked |
| 12 | Preload the third-party images instead of `kind load` per run | CI **[derived]** ~7s (coturn) of the 29s load step | a preload script + workflow cache | Low | not done, ranked |
| 13 | Shard the e2e job | **negative** after 1-3: each shard pays the 46s cluster create and its own image build, so two shards cost more wall time than one | — | — | **rejected** |

## Why sharding is rejected (13)

Before this branch the e2e job spent 46s creating the cluster, 154s building
images and 29s loading them, against 75s of specs: two shards would have paid
~230s of setup twice to halve 75s. After this branch the setup is ~110s and the
specs are still ~75s, so sharding remains a loss. Shard only if the specs grow
past the setup cost (roughly 8x more specs), or if the setup moves out of the
job (a prebuilt image, a cluster created by another job).

## What is deliberately not touched

* `-race` in `make test`: the envtest layer is 28s in CI, and dropping `-race`
  would trade real bug detection for a fraction of that.
* The two `docker build` invocations in `make docker-build`: they produce two
  different images and both are needed.
* `kind load` for the controller and probe images: a local registry was not
  measured, and the load is 22s of the job.
* The e2e suite's 4-minute `waitForRelays` timeout: convergence measured at
  8-21s per spec locally, so the timeout is generous but it is the failure path
  of a suite whose specs are not the bottleneck; shortening it would only make a
  real failure report sooner, which is not worth a change here.
