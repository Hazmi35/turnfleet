# Test suite audit — Turnfleet

Branch: `test-audit/2026-10-15` (9 commits, not pushed).

Everything below is measured on this workstation (2 vCPU, 15 GiB, docker 26.1.5,
buildx 0.13.1, kind v0.33.0) or read out of real CI runs of this repository
(GitHub Actions, `ubuntu-latest`, 4 vCPU). Each number says which. Local and CI
absolute times are not comparable — the local box is 2-3x slower — so local
numbers are used for *ratios* between phases, and CI numbers for the wall time
the plan is about. Raw data is in `baseline/`; the ranked plan is in `PLAN.md`;
the judgment calls are in `DECISIONS.md`.

---

## 1. Summary

**The E2E job is the pull request's critical path: 10.0-13.5 min, median
12.2 min, against 2.2 min for the ci job.** Almost none of it was the tests.
`make test-e2e` spent 305s of its 386s on setup and 67.7s running specs; the
chart step spent 165s of its 174s on setup and 7.5s on its one spec. Two thirds
of the whole job (353s of 560s) was `docker build`, and three of the four image
builds recompiled from source a tree that had not changed.

| Layer / job | Before | After | How |
|---|---|---|---|
| `ci` job (unit + envtest, lint, vuln) | 2.2-2.5 min | unchanged | nothing here was the bottleneck |
| `internal/controller` envtest | 20.04s local, 5 runs **[measured]** | 19.78s local, 5 runs **[measured]** | one `List` per `markPodsReady` instead of one per pod — within the run-to-run spread, so a correctness fix, not a timing win |
| `make test-e2e` setup | 484.1s local **[measured]** | 294.5s local **[measured]** (-39%) | cacheable images, probe-only host build |
| `make test-e2e` specs | 116.5s local | 90.8s local (variance, not a change: see §5) | — |
| `make test-e2e` total | 600.6s local **[measured]** | 385.3s local **[measured]** (-36%) | |
| `make test-e2e` (CI step) | 386s **[measured]** | ~320s **[derived]** | probe image build 53s → ~5s, host build 62s → ~48s |
| `make test-e2e-chart` (CI step) | 174s **[measured]** | ~35s **[derived]**; 48.1s local **[measured]** | `REUSE_IMAGES=1` removes 144s of build and load |
| `e2e` job total | 10.0-13.5 min, median 12.2 **[measured, 50 runs]** | ~6.7 min **[derived]** | |
| docs-only pull requests | full e2e job | skipped | `paths-ignore` |
| flake rate (specs) | 0 failures in 50 CI runs and 1 local run | 0 failures in 5 reruns (§5) | |

The five changes with the biggest effect:

1. **`673adeb` — reuse the Go build cache between image builds.** `docker build`
   cold: **399.4s → 256.7s [measured]**, warm: **27.1s [measured]**. The probe
   image now reuses the packages the controller image just compiled.
2. **`2125eb4` + `059971d` — the chart step reuses the images the suite step
   built and loaded.** Removes 137s of build and 7s of `kind load` from a step
   whose spec takes 7.5s. Chart step 174s → 48.1s local **[measured]**.
3. **`d4822e6` — skip the e2e job for documentation-only pull requests.** Saves
   the whole 10-13 min job on those PRs.
4. **`9cb5ac4` — build only the probe host binary for the suite.** The controller
   host binary was 61.8s of CI's 62s `make build` and nothing in the suite reads
   it. ~14s **[derived]**.
5. **`7939fd4` — dump cluster state when the controller rollout stalls.** No time
   saved; it is the difference between an 8-in-50 failure that costs a re-run to
   learn nothing and one that can be diagnosed from the log.

Two tests that could not fail at all were also fixed (`689ab72`, `46b62a6`), and
three weak assertions in the webhook suite (`73968fc`) — each proven by breaking
the implementation and showing the fixed test fails where the old one passed.

---

## 2. Changes made

One line per commit, with its measured effect.

| Commit | Change | Effect |
|---|---|---|
| `689ab72` | Pin the plan size the two stability tests compare | Old tests passed against a planner that placed **zero** relays (verified: `ComputePlan` returning `Plan{}` left both `ok`); now they fail. No spec lost. |
| `46b62a6` | Assert the version fallback's output, not that it is non-empty | Old test could not fail (verified: making the fallback accept `"(devel)"` passed); now it fails. |
| `73968fc` | Fix three webhook assertions (deny-list content, nil-Probe panic, declared `wantField` never read) and pin seven field paths instead of one | Each proven by break/restore; the nil-Probe case turned a whole-binary panic into a one-test failure. |
| `b244053` | envtest: explicit Gomega timeouts, counter-derived namespaces, one `List` per `markPodsReady` | two 1s library defaults and a clock-derived namespace removed; `internal/controller` 20.04s → 19.78s over 5 runs each **[measured]**, i.e. no measurable speedup. |
| `673adeb` | BuildKit cache mounts in both Dockerfiles, `-a` dropped | `make docker-build` **399.4s → 256.7s** cold, **→ 27.1s** warm **[measured]**; artefact identical apart from the stamp that is meant to vary. |
| `9cb5ac4` | `probe-bin` target; `make test-e2e` no longer builds the controller host binary | CI **[derived]** ~14s; local probe-only cold build 151.4s vs controller 194.8s **[measured]**. |
| `2125eb4` | `REUSE_IMAGES` + `images-for-e2e`; the chart target uses them | Chart step's build and load become optional (default 0 = unchanged). |
| `7939fd4` | `await-rollout`: same gate, same 180s, dumps deployment/pods/nodes/events on failure | 8-in-50 undiagnosable failures become diagnosable. |
| `059971d` | CI chart step passes `REUSE_IMAGES=1` | Removes 144s from the step's 174s **[derived]**; 48.1s local **[measured]**. |
| `d4822e6` | `paths-ignore` on the e2e workflow for `**.md`, `docs/**`, `LICENSE`, the PR template | Docs-only PRs save the whole 10-13 min job. |

Net: **+98 lines of tests and Makefile/workflow, −2 lines of Dockerfile.**

---

## 3. Changes tried and reverted

* **Removing the `date` stamp from `LDFLAGS`** so the image layer cache would
  hit. Rejected before implementing: it is documented behaviour
  (`.github/workflows/image.yaml` explains why a released binary reports its
  build date) and it changes the artefact. The cache mounts give the same or
  better speedup with no semantic change. See `DECISIONS.md` §1.
* **Replacing `kubectl rollout status` with `kubectl wait --for=condition=Available`**
  in `deploy`. It would make the 8-in-50 flake disappear because it does not
  wait for old pods to terminate — i.e. by hiding the symptom. Not attempted;
  diagnostics added instead. `DECISIONS.md` §2.
* **A `fake.NewClientBuilder()` in the webhook unit tests** to cover the PodIP
  branch. Rejected: the repository has no fake client in it, and the stronger
  API-server-level coverage already exists. `DECISIONS.md` §4.
* **Deleting `TestValidateDeleteIsUnconditional`.** Kept: it pins an interface
  contract and costs milliseconds. `DECISIONS.md` §5.
* **Parallelising the e2e specs.** Not attempted in this change: ~40s of a job
  whose setup this branch already cut by ~3.5 min, with several ways to be
  subtly wrong. Ranked in `PLAN.md` §8 with its measured prize.
* **Sharding the e2e job.** Rejected with numbers: each shard would pay the 46s
  cluster create and its own image build to halve ~75s of specs.

---

## 4. Bad tests found

Worst first. `wrong` = the suite could be green while the behaviour is broken;
`cosmetic` = weak but not false.

| # | Location | Category | Severity | Evidence | Fix |
|---|---|---|---|---|---|
| 1 | `internal/relay/plan_test.go:715` | false confidence | wrong | Compared `len(first.Relays)` with `len(second.Relays)` and never pinned the size. **Reproduced**: an empty `Plan` from `ComputePlan` left the test `ok`. | Pinned to 2 relays before the comparison — **fixed** `689ab72` |
| 2 | `internal/relay/podip_test.go:91` | false confidence | wrong | Same shape with `Replicas = 3`. **Reproduced** the same way. | Pinned to 3 relays — **fixed** `689ab72` |
| 3 | `internal/version/version_test.go:45` | false confidence | wrong | Set `version = "dev"` then asserted `v != ""`; `Info()` can only replace a non-empty version. **Reproduced**: accepting `"(devel)"` as usable passed. | Pins both observable outcomes — **fixed** `46b62a6` |
| 4 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:77` | false confidence | wrong | `len(deniedCIDRs) != len(want)`: a same-length wrong list passed. **Reproduced** with a 15-entry list (default is 12). | `slices.Equal` — **fixed** `73968fc` |
| 5 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:86` | false confidence | wrong | `t.Errorf` on the nil-`Probe` guard, then `pool.Spec.Probe.Timeout` two lines later. **Reproduced**: a defaulter that leaves `Probe` nil panicked the whole binary (`nil pointer dereference [recovered, repanicked]`), so no other test in the package ran. | `t.Fatalf` — **fixed** `73968fc` |
| 6 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:772` | false confidence | wrong | `TestValidatePortRangePartitioning` declares `wantField` for every failing case and never reads it. **Reproduced**: reporting `spec.relayPortRange.end` passed. | Asserts the declared field — **fixed** `73968fc` |
| 7 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:815` | false confidence | cosmetic | `TestValidateSpecMatchesPathRoot` pinned one of the validator's ten paths. **Reproduced**: pointing the image error at `spec.dataplane` passed. | Pins the exact 7-path set a one-violation-per-family spec produces — **fixed** `73968fc` |
| 8 | `internal/controller/suite_test.go:179` | missing coverage | wrong | `_, err := f.reconciler.Reconcile(...)`: the `ctrl.Result` is discarded in every controller test, so **no test asserts any of the three requeue contracts** — `secretRetryInterval` (`turnpool_controller.go:154`), `drainPollInterval` (`status.go:402`), `probeInterval` (`turnpool_controller.go:210`). A regression that made the controller never requeue, or spin with a zero delay, passes the whole suite. | **Not fixed** — needs a small change to `reconcile()` to return the result plus three assertions; left as the top remaining test-quality item (`PLAN.md` has it under §8's neighbour list, `REPORT.md` §10) |
| 9 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:557` | missing coverage | cosmetic | Every table case builds `&TURNPoolValidator{}` with a nil `Client`, and `validatePodIPDeployment` returns `nil` immediately when `c == nil` (`turnpool_webhook.go:401`), so the two "valid PodIP pool" cases assert nothing about attachment existence or address routability. | **Not fixed** — the coverage lives at the API-server level in `turnpool_webhook_envtest_test.go` ("rejects a declared attachment that does not exist", "admits a pool whose nodes declare routable addresses", the four unusable-address rejections). Deliberate: see `DECISIONS.md` §4 |
| 10 | `internal/probe/probe_test.go:308` | false confidence | cosmetic | `LatencyMillis < 0`: the value comes from `time.Since(start).Milliseconds()`, which cannot be negative. Unfailable. | **Not fixed** (cosmetic, no risk) |
| 11 | `internal/probe/probe_test.go:429` | false confidence | cosmetic | Accepts `401` **or** `400` because coturn and the pion test server differ; the in-process server always answers 400, so the 401 arm is dead and the assertion cannot detect a wrong code in either direction. | **Not fixed** (documented in the test's own comment; the e2e layer covers the real coturn code) |
| 12 | `internal/vantage/vantage_test.go:103` | false confidence | cosmetic | Four table cases, three distinct refusals implemented, one `wantErr` sentinel: "no observation time", "not json" and "another pool" are indistinguishable. | **Not fixed** |
| 13 | `internal/relay/fuzz_test.go:54` | false confidence | cosmetic | Every `Render` error is swallowed with `continue`, so a renderer that refuses *all* relays leaves the Render half of the body with no assertion executed. | **Not fixed** |
| 14 | `internal/relay/render_test.go:504` | false confidence | cosmetic | Asserts the absence of `no-tcp-relay` but not `min-port`/`max-port`, which is what the doc comment's capacity argument is about. | **Not fixed** |
| 15 | `make test-fuzz` | missing coverage | wrong | Neither fuzz target runs in CI: `grep -rn fuzz .github/workflows/` matches nothing, and `make ci` is `verify helm-verify lint test vuln licenses dco`. The renderer and validator invariants are only exercised when someone remembers to run it. | **Not fixed** — wiring it in needs a time budget decision (30s per target) that is the maintainer's, not the audit's |
| 16 | `internal/controller/address_conflict_test.go:20` | fragility | cosmetic | The suite's own comment: "envtest never finishes terminating a test namespace, so pools from the other tests in this package are still there". Every test leaks its namespace and its pools, and the cross-pool conflict check scans the whole cluster, so each test's reconciles walk the leftovers of every earlier test. The workaround (a unique address per test file) is correct but load-bearing and undocumented outside that comment. | **Not fixed** — quantifying it needs a probe into the API server at the end of a run |
| 17 | `internal/webhook/v1alpha1/turnpool_webhook_envtest_test.go:66` | wrong layer | cosmetic | The `scale` subresource spec is CRD plumbing, not admission. It belongs with the API/CRD tests rather than the webhook suite. | **Not fixed**: it is already at the right *layer* (envtest), only in the wrong file, and it costs 0.02s |
| 18 | `test/e2e/e2e_test.go:620` | fragility | cosmetic | `controllerMetrics` reads the controller's own metrics through the API server as `services/http:…:8443/proxy/metrics`, and the comment above it says why: the e2e overlay passes `--metrics-secure=false`. The chart installs metrics over HTTPS (`charts/turnfleet/values.yaml:51` `secure: true`), so against a chart-installed cluster the call fails with `Error from server (BadRequest): the server rejected our request for an unknown reason` and `TestSaturationBecomesObservable` fails **before** any assertion about saturation runs. Found the hard way: 5/5 reruns after a `make test-e2e-chart` left the chart installed. CI never sees it because the chart step runs only `TestProbeCLIFromTheHost`. | **Not fixed**: the coupling is documented in the comment and `make test-e2e` always deploys that overlay. Worth a line in `docs/limitations.md`: the suite cannot run against the chart's default install |

No test asserts on list order, timestamps, generated names or event counts; no
test uses `time.Sleep`; no test opens a real network dependency outside
`internal/probe` and the e2e suite, which are testing exactly that.

---

## 5. Flaky specs

**Before**: 0 spec failures in every run I could inspect.

* 50 CI e2e runs: 36 success, 10 failure, 4 cancelled. All 10 failures are in
  **setup, before any spec runs** (8 at `make deploy`, 2 at `docker-build`), and
  all 10 are on 2026-10-13. No spec has failed in CI in the last 50 runs.
* 1 local e2e run: 8/8 specs passed, exit 0.
* CI `ci` job: the unit and envtest suites have not failed in that window; the
  two ci failures on 2026-10-13 were at the `Lint` step.

**The flake is real but it is not a spec.** 8 of those 10 runs failed at:

```
kubectl ... rollout status deployment/turnfleet-controller-manager --timeout=180s
Waiting for deployment "turnfleet-controller-manager" rollout to finish: 1 old replicas are pending termination...
error: timed out waiting for the condition
```

Failure rate: **8/50 runs (16%)**, all between 06:48Z and 09:17Z on
2026-10-13, none in the two days since. The same step takes 11-12s when it
succeeds, so the 180s budget is not short and the pod that never finished
terminating is not slow — it is stuck. Root cause **unproven**: the log records
nothing about the pods, the nodes or the events. Not fixed, by design
(`DECISIONS.md` §2): the gate is unchanged and now dumps that state on failure.

The remaining 2 failures were `docker build` on renovate branches, i.e. a real
dependency break, not flakiness.

**After**: 5 reruns of the e2e specs and 5 reruns of the unit + envtest layers
with randomized Ginkgo order and a different seed per iteration.

### E2E specs, 5 reruns

**0 failures, 5/5 exit 0.** Wall time per run: 70.4s, 75.8s, 73.3s, 75.5s, 79.0s
(mean 74.8s, sd 3.0s). The specs are byte-identical before and after this
branch — it changes the Makefile, the Dockerfiles, CI and the unit/envtest
tests, and touches no file under `test/e2e/` — so these five runs are as valid a
measurement of the spec code's stability as a rerun of the parent commit would
be.

Per-spec spread over the 5 runs:

| Spec | Min | Max | Spread |
|---|---|---|---|
| `TestVantageRunnerInCluster` | 8.09s | 13.18s | **63%** |
| `TestHostNetworkMode` | 8.04s | 11.04s | 37% |
| `TestScaleDownDrainsRelayWithAnActiveAllocation` | 12.06s | 13.08s | 8% |
| `TestSaturationBecomesObservable` | 7.53s | 8.05s | 7% |
| `TestProbeCLIFromTheHost` | 6.55s | 8.06s | 23% |
| `TestPartitionedIPMode` | 6.54s | 8.07s | 23% |
| `TestExternalIPPoolMode` | 7.98s | 8.48s | 6% |
| `TestPeerPolicyRefusesClusterAddresses` | 7.04s | 8.04s | 14% |

The spec that varies most is the one that waits for a `Job` to be scheduled and
run (`TestVantageRunnerInCluster`, `test/e2e/vantage_test.go:184`): its
convergence is however long the cluster takes to schedule a pod, not a fixed
wait, so a spread of 5s is expected rather than a defect. The suite-level
number is the same shape: 70.4s to 116.5s across every run I took, the 116.5s
being the first run after a cold build and a `docker builder prune`.

### Unit + envtest, 5 reruns

`--ginkgo.randomize-all` with seeds 1101/2202/3303/4404/5505, `-race`, one
`go test` per iteration:

**0 failures, 5/5 exit 0**, 30 package runs. Per-package, mean of 5:

| Package | Before (mean, 5 runs) | After (mean, 5 runs) |
|---|---|---|
| `internal/controller` (envtest, 56 tests) | 20.04s (19.70-20.23) | 19.78s (19.52-20.40) |
| `internal/webhook/v1alpha1` (22 Ginkgo specs) | 8.76s (8.43-9.04) | 8.96s (8.56-9.29) |
| `internal/relay`, `internal/probe`, `internal/vantage`, `internal/version`, `cmd/…`, `api/…` | ~1.0-1.1s each | ~1.0-1.1s each |

Iteration wall time 41.1-44.2s after, 34.9-39.2s before; the two are not
comparable, because the before-runs measured only the two envtest packages and
the after-runs added the six pure-unit packages to each iteration.

The "before" numbers are from a `git worktree` at the parent commit `9ba0193`,
run in exactly the same shape: one `go test` per package, sequential, `-race`,
same seeds. **The `internal/controller` improvement is inside the run-to-run
spread, so this branch claims no envtest speedup** — an earlier claim of
21.9s → 20.6s in the first version of that commit message compared a parallel
run with a sequential one and was withdrawn; the commit now says so.

### Five runs that failed for a reason worth writing down

Before the reruns above, five runs of the suite failed 5/5 on
`TestSaturationBecomesObservable`. They were run against a cluster left
chart-installed by `make test-e2e-chart`, and the chart serves metrics over
HTTPS while the suite reads them over HTTP through the API server
(`test/e2e/e2e_test.go:620`). The spec failed at the metrics read, before any
saturation assertion. Recorded in full in §4 row 18 and `DECISIONS.md` §9;
the raw log is `baseline/flake-e2e-invalid-chart-install.log`. It is a
measurement mistake, not a defect, but it is also the one hidden coupling the
suite has: it cannot run against the chart's default install.

### The one flake that is real

Not a spec: the `await-rollout` step, 8 of 50 CI runs, root cause unproven,
described above and in §9. Its failure rate after this branch is **unknown**
(the change does not touch the wait, only what is printed when it times out) —
the next CI runs are the measurement.

---

## 6. Layer migrations

**Nothing moved between layers, and that is the finding.**

The E2E suite is 8 specs for 8 behaviours that only a real cluster can show: a
real coturn process binding the node's address, a real TURN `Allocate`
answering from the host, port exhaustion answering 508, a drain that holds a
pod while an allocation is live, a peer policy refusing the cluster's own
service IP, a `Job` running the shipped runner manifest, the probe CLI from
outside the cluster, and the chart install serving a relay. Every one of them
either talks to a relay or depends on node networking; moving any of them down
to envtest would delete the thing it proves. The address arithmetic behind them
(disjoint ranges, listener-port offsets, capacity totals) is *already* covered
at the unit and envtest layers — `internal/relay/plan_test.go`,
`render_test.go`, `podip_test.go` and `internal/controller/*_test.go` — so the
e2e specs are checking the wiring, not re-deriving the logic.

What did change is the *setup* around them: the E2E layer no longer pays for
artefacts another step already produced, and the chart step no longer re-proves
`TestProbeCLIFromTheHost` from a fresh build. Coverage is unchanged: the chart
step still installs CRDs and the controller from `helm` and asserts a relay
served by that install answers a real Allocate.

The one layer question the audit raises is the opposite direction: three
reconciler contracts (the requeue intervals, §4 row 8) are asserted *nowhere*,
and the cheapest place to catch them is the existing envtest `reconcile()`
helper, not the e2e suite.

---

## 7. Untested CI changes needing a real CI run

Both are in separate commits so they can be dropped on their own.

1. **`d4822e6` — `paths-ignore` on the e2e workflow.** GitHub's filter semantics
   cannot be exercised locally. Risk: a PR that touches only `docs/` but should
   run E2E is skipped. Mitigated by keeping the list to `**.md`, `docs/**`,
   `LICENSE` and `.github/pull_request_template.md`.
2. **`059971d` — `REUSE_IMAGES=1` in the chart step.** The *Makefile* behaviour
   is tested locally (48.1s, exit 0, and the default path was dry-run verified to
   still build and load); what is untested is that the images the first step
   built are the ones the second step's `kind load` put on the nodes in CI. They
   are built from the same tree by the same target minutes earlier on the same
   runner, and the first step fails the job if that build fails.

The `docker build` change (`673adeb`) uses BuildKit cache mounts, which need a
BuildKit builder; that is the default for `docker build` since Docker 23 and was
verified here on docker 26.1.5. A build with `DOCKER_BUILDKIT=0` would fail on
the Dockerfile syntax — worth knowing, and correct: that mode is removed in
Docker 25+ anyway.

---

## 8. Unverified items

* **CI wall times after the change are derived, not measured.** The E2E job
  cannot be run on GitHub Actions from here. The derivation is: the phases that
  changed are exactly those measured in run 38719831132, scaled by the ratio
  measured locally between before and after for that phase. Everything else
  (cluster create 46s, CRD apply 6s, kind load 29s, deploy 12s, `go test` 81s)
  is left at its CI value.
* **The 16% rollout flake's root cause.** Unproven; the evidence is the step,
  the message and the window. See §5 and §9.
* **Whether the BuildKit cache mount survives the two CI *steps*.** Both steps
  run on one runner with one docker daemon, so it should; not verified in CI.
* **Ginkgo's randomization** is `RandomizeAllSpecs: false`, i.e. only top-level
  containers shuffle, verified in the pinned source (v2.33.1
  `types/config.go:285-288`) and in this repo's own report
  (`baseline/ginkgo-webhook-before.json`). I did not verify Ginkgo's
  `--probe`/`--repeat` behaviour beyond that; the reruns use `go test` and vary
  `--ginkgo.seed` with `--ginkgo.randomize-all`.
* **`kind load docker-image` timing** is from the CI log's per-image lines, not
  from a controlled measurement.
* **The envtest namespace leak** (§4 row 16) is documented by the repository's
  own comment and by envtest's design (no namespace controller); the *cost* of
  the leftovers was not measured.

---

## 9. Suspicions

Each with what it would take to confirm.

1. **The rollout flake was node pressure on the runner.** Hypotheses:
   `kind` on a 4-vCPU/7GB runner with 4 nodes plus three images per node, a
   node going `NotReady`, and the old pod stuck `Terminating` because its
   kubelet stopped. **Confirm**: the `await-rollout` dump now in `Makefile` on
   the next occurrence — `get nodes -o wide` and `get pods -o wide` on failure.
2. **The probe image build is now nearly free in CI.** The measured local drop
   (the probe build reusing the controller's cache) predicts the CI probe image
   build goes from 53s to a few seconds. **Confirm**: the next e2e run's log,
   where the two `docker build` blocks are timestamped.
3. **The `make build` step could be removed from the e2e job entirely** by
   extracting the probe binary the image already contains
   (`docker create` + `docker cp`). Prize: the whole host build. **Confirm**: a
   local change and one e2e run; ranked in `PLAN.md` §9.
4. **The e2e specs would parallelise.** They use disjoint relay port ranges
   (51000-51020, 51100-51120, ...), disjoint listener ports (34780-34787) and
   per-pool label selectors, and only `TestExternalIPPoolMode` mutates anything
   outside a pool (secondary addresses on two nodes). **Confirm**: `t.Parallel()`
   plus unique namespaces, then 10 clean runs; the two allocation-sensitive
   specs (`TestSaturationBecomesObservable`, `TestScaleDownDrains...`) are the
   ones to watch.
5. **The two envtest control planes cost more than the 4.4s** measured for the
   webhook suite's own `BeforeSuite`/`AfterSuite`, because the controller suite
   starts one inside `TestMain`, outside any Ginkgo reporting. **Confirm**:
   instrument `TestMain` and compare with the package's reported elapsed.
6. **`-race` roughly doubles the envtest layer's cost.** ~28s in CI for a layer
   whose specs are mostly API-server round trips. **Confirm**: one local run
   with and without; not done because dropping it would trade real detection.
7. **The `docker build` cache would help CI's *first* build too**, if the
   BuildKit cache were persisted across runs (`buildx` + `type=gha`). Ranked in
   `PLAN.md` §10. **Confirm**: a CI run with the cache exported.
8. **The BadRequest on `services/http:…/proxy/metrics` is a proxy-level refusal,
   not an upstream failure.** Observed: with the endpoint serving HTTPS, the
   `http:` proxy path returns `BadRequest … unknown reason` while the `https:`
   path returns `You must be logged in to the server` — so the scheme is being
   honoured and the endpoint is reachable. What is *not* proven is which layer
   (the API server's proxy, or the TLS server receiving plaintext) produces the
   400. **Confirm**: `curl` the same proxy path against a cluster with
   `--metrics-secure=true` and read the API server's audit log entry.
9. **The e2e suite would be order-sensitive under parallel execution.** Every
   spec shares one namespace and `deletePool` waits for relay pods to go, on the
   assumption that the next spec may want those node ports. **Confirm**:
   `t.Parallel()` plus unique namespaces, then 10 runs.

---

## 10. Remaining opportunities, ranked

1. **Assert the requeue contracts** (§4 row 8). Three behaviours, no test, and
   the helper is one line away from exposing `ctrl.Result`.
2. **Run `make test-fuzz` in CI** (§4 row 15), with a fixed time budget.
3. **Extract the probe binary from the image instead of building it**
   (`PLAN.md` §9): the largest remaining *setup* item, ~48s of CI.
4. **Persist the BuildKit cache across CI runs** (`PLAN.md` §10): ~100s of the
   first image build, every run.
5. **Parallelise the e2e specs** (`PLAN.md` §8): ~40s of CI, the most delicate.
6. **Share one envtest control plane** between the two envtest packages
   (`PLAN.md` §11): 4.4s measured, medium effort.
7. **Preload the coturn image** instead of `kind load` per run (`PLAN.md` §12):
   ~7s.
8. **Quantify and fix the envtest namespace leak** (§4 row 16).

---

## A. Inventory

| Layer | Framework | Location | Invoked by | Specs |
|---|---|---|---|---|
| Pure unit | `testing`, table-driven | `internal/relay/{plan,render,podip}_test.go`, `internal/probe/probe_test.go`, `internal/vantage/vantage_test.go`, `cmd/turnfleet-probe/vantage_test.go`, `internal/version/version_test.go` | `make test` (ci job) | 133 `Test*` functions |
| Fuzz | `testing.F` | `internal/relay/fuzz_test.go`, `internal/webhook/v1alpha1/fuzz_test.go` | `make test-fuzz` — **no CI job** | 2 targets |
| envtest, real API server, in-process `Reconcile` | `testing` + `envtest` | `internal/controller/*_test.go` | `make test` (ci job) | 56 |
| envtest + webhook server over TLS | Ginkgo v2.33.1, Gomega v1.44.0, `envtest` | `internal/webhook/v1alpha1/*_test.go` | `make test` (ci job) | 22 Ginkgo specs + 9 `Test*` functions |
| E2E, real kind cluster, real coturn, real TURN | `testing` | `test/e2e/*_test.go` (build tag `e2e`) | `make test-e2e`, `make test-e2e-chart` (e2e job) | 8 |

There is **no fake-client test in the repository** (`grep -rn
'fake.NewClientBuilder\|client/fake' --include='*_test.go'` → nothing).
Everything that talks to an API server talks to a real one.

**How the E2E cluster is made.** `make kind-up` creates **one** cluster per CI
job (`turnfleet`) from `hack/kind-config.yaml`: 1 control plane + 3 workers
labelled `turnfleet.hzmi.dev/relay-node=true`, node image pinned by tag *and*
digest. Both e2e steps share it (the second prints "kind cluster turnfleet
already exists"). Installed: kind's own CNI and CSI, the CRDs from
`config/crd`, the controller from `config/e2e` (kustomize; `config/default` plus
`--metrics-secure=false`), and Helm's copy of the same in the second step.
**No cert-manager, no Prometheus, no admission webhooks** on this path —
`config/default` runs the controller without webhooks, and the suite creates
pools that are already valid. Relay images: `coturn/coturn:4.18.0-r0`
(tag pinned in the Makefile, digest verified before loading) plus the two images
built from this tree.

**How the image reaches the cluster.** `make docker-build` (local docker
daemon) → `kind load docker-image` into all four nodes. No registry. The `dev`
tag is constant, so each deploy follows the load with `rollout restart`.

**Where the controller runs.** As a **deployed pod** in e2e, with the suite on
the host talking to relays over the node network. As an **in-process
`Reconcile` call** in envtest — no manager, no watches, no requeue loop; the
watch predicates are unit-tested directly.

**Timeouts and polling.**

| Where | Value |
|---|---|
| e2e `waitFor` | 500ms poll; explicit timeout per call; the context error is swallowed so the loop reports the timeout, not `context deadline exceeded` |
| e2e waits | 4 min `waitForRelays`, 3 min pool deletion, 2 min vantage/drain, 90s capacity pressure |
| e2e `-timeout` | 30 min (`test-e2e`), 15 min (`test-e2e-chart`) |
| Gomega defaults | `Eventually` 1s / 10ms, `Consistently` 100ms — gomega v1.44.0 `internal/duration_bundle.go:31`. **Was** inherited by the webhook readiness gate; now explicit (30s / 100ms) |
| Ginkgo order | seed = `time.Now().Unix()` per run; only top-level containers shuffle; `FlakeAttempts` 0 (no retries) — ginkgo v2.33.1 `types/config.go:50,285-288` |
| Controller intervals | `secretRetryInterval` 10s, `drainPollInterval` 1s, probe interval/timeout from the spec (`turnpool_controller.go:44-57,215-240`) |

No `time.Sleep` anywhere in the repository's Go code.

**Parallelism.**

| Level | Setting |
|---|---|
| `go test -p` | unset → `GOMAXPROCS`; the two envtest packages run concurrently, each with its own control plane (2 per `make test`) |
| `t.Parallel()` | used in the unit and envtest suites, **not** in `test/e2e` |
| Ginkgo procs | none |
| CI matrix/sharding | none: one `ci` job, one `e2e` job |

---

## B. Baseline

See `baseline/README.md` for the raw numbers and their sources.

**CI, per phase (run 38719831132).** `make test-e2e` 386s = cluster 46 + image
builds 154 + host builds 62 + CRDs 6 + load 29 + deploy 12 + `go test` 81
(67.7s reported by the package). `make test-e2e-chart` 174s = image builds 137 +
host builds 3 + load 7 + undeploy/uninstall 6 + helm install 12 + `go test` 9
(7.5s reported). Job 607s.

**CI, per job.** `ci` 2.2-2.5 min; `e2e` median 12.2 min over 50 runs.

**Local.** `make test` equivalent (unit + envtest, `-race`) 33.7s wall;
`internal/controller` 21.9s; `internal/webhook/v1alpha1` 10.3s, of which the
Ginkgo suite is 7.4s and 4.5s of that is `BeforeSuite` + `AfterSuite`.
`make test-e2e` setup 484.1s + specs 116.5s.

**Local per-spec** (the 8 slowest, since there are only 8): vantage 38.10,
host-network 21.60, scale-down/drain 13.08, saturation 8.04, peer-policy 8.04,
probe CLI 8.07, partitioned 8.03, external-IP-pool 7.95.

**Local docker build.** Cold, committed Dockerfiles: 399.4s. The 10 slowest
setup steps are the phases above: four `docker build` invocations, `make build`,
kind create, three `kind load`s, and the two rollout waits.

---

## C. Reproduce

```sh
# Baseline, from the parent commit (9ba0193):
#   the suite layer
KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" make test
#   one e2e run, setup and specs timed separately
make kind-up docker-build build install load-images      # 484.1s here
make deploy KUSTOMIZE_OVERLAY=config/e2e
go test -tags=e2e -count=1 -v -timeout 30m ./test/e2e/   # 116.5s here
#   the image build alone (any run is a cache miss: DATE is in LDFLAGS)
docker builder prune -af && make docker-build            # 399.4s here

# After, on test-audit/2026-10-15:
docker builder prune -af && make docker-build            # 256.7s here, 27.1s warm
make kind-up docker-build probe-bin install load-images  # 294.5s here, all phases
make deploy KUSTOMIZE_OVERLAY=config/e2e
go test -tags=e2e -count=1 -v -timeout 30m ./test/e2e/   # 90.8s here
make test-e2e-chart REUSE_IMAGES=1                       # 48.1s here
```
