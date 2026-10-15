# Test suite audit — Turnfleet

Branch: `test-audit/2026-10-15` (17 commits), pushed and open as **draft PR
#62** so the CI wall times in Session 2 could be measured rather than derived.

Everything below is measured on this workstation (2 vCPU, 15 GiB, docker 26.1.5,
buildx 0.13.1, kind v0.33.0) or read out of real CI runs of this repository
(GitHub Actions, `ubuntu-latest`, 4 vCPU). Each number says which. Local and CI
absolute times are not comparable — the local box is 2-3x slower — so local
numbers are used for *ratios* between phases, and CI numbers for the wall time
the plan is about. Raw data is in `baseline/`; the ranked plan is in `PLAN.md`;
the judgment calls are in `DECISIONS.md`.

---

## Session 2

Second pass on the audit: the open items from §10 item 1 (requeue contracts), §4
row 18 (the metrics-scheme coupling), the 8-in-50 rollout flake, and the numbers
that were derived rather than measured. Branch `test-audit/2026-10-15`, now on
PR #62 (draft, pushed to measure the CI wall times).

### What was fixed

**1. The requeue contract is asserted instead of discarded** (`edeb70b`).

`reconcile()` threw the pass's `ctrl.Result` away, so nothing in the suite
looked at what the controller asked the work queue to do next. The fixture now
has `reconcileResult`, and eight specs pin the four outcomes. The inventory, with
the line that produces each:

| Outcome | Where | Spec |
|---|---|---|
| `ctrl.Result{}` | `turnpool_controller.go:101` (pool gone, `client.IgnoreNotFound`) | `TestReconcileOnAMissingPoolAsksForNothing` |
| `ctrl.Result{}` | `status.go:333` (deleted, no finalizer), `status.go:361` (drained) | `TestRequeueWhileDeletingAPoolThatStillHasRelays` |
| `RequeueAfter: 10s` | `turnpool_controller.go:154` (`secretRetryInterval`) | `TestRequeueWhileTheAuthSecretIsMissing` |
| `RequeueAfter: 1s` | `turnpool_controller.go:210`→`:217` (`drainPollInterval`) | `TestRequeueWhileDrainingAScaledDownPool` |
| `RequeueAfter: 1s` | `status.go:402` (`drainPollInterval`) | `TestRequeueWhileDeletingAPoolThatStillHasRelays` |
| `RequeueAfter: 10s` / the pool's | `turnpool_controller.go:210`→`:219` (`probeInterval`) | `TestRequeueInSteadyStateUsesTheDefaultProbeInterval`, `…FollowsTheSpecProbeInterval` |

There is **no immediate requeue**: `grep -rn 'Requeue:' internal/ cmd/` finds no
`Requeue: true`, and the helpers assert `Requeue` stays false alongside
`RequeueAfter`.

The intervals are literals, not the constants they mirror. The first draft of
these specs asserted `secretRetryInterval` against itself and survived retuning
the retry to 11s — the same shape as the vacuous assertions this audit was
looking for, introduced by the audit itself. Each literal names its constant.

Proven by eight breaks, one per contract, each restored immediately (matrix in
`baseline/session2-requeue-breaks.txt`): A `secretRetryInterval 10s->11s` fails
only the Secret spec; B `drainPollInterval 1s->2s` fails only the two drain
specs; C ignoring the pool's own probe interval fails only the override spec; D
making steady state ask for nothing fails five; G returning an error for a
missing pool fails only that spec; H requeueing a drained deletion fails only the
deletion spec.

**2. The controller no longer rewrites a pool's status on every pass** (`813d64e`)
— a product bug the requested idempotency check exposed.

`buildStatus` builds its conditions from nothing and `meta.SetStatusCondition`
stamps `time.Now()` on a condition it does not find (apimachinery v0.37.1,
`pkg/api/meta/conditions.go`), so the freshly built status never compared equal
to the stored one: the equality guard at `status.go:81` could not fire, and every
pass issued an `Update` the API server then discarded. The visible half is that
`LastTransitionTime` — the API's "when did this condition last transition" —
walked forward while nothing transitioned, because the server stores whole
seconds and the incoming nanosecond value differed every pass.

`carryTransitionTimes` copies the stored transition time onto every condition the
new status reproduces unchanged, which is what lets the guard work.

Measured: with it, a steady-state pass issues **zero** mutating requests and the
pool, its pod, its relay configuration Secret and its URI ConfigMap all keep
their resourceVersion; without it, exactly one request per pass
(`[statusUpdate object/<pool>]`). The count comes from a `mutationCounter`
wrapping the fixture's client, because the API server cannot witness a write it
discards: the resourceVersion probe alone passed while the write happened.

Proven both ways: removing the carry-over fails only the idempotency spec;
applying it to changed conditions too fails only the unit spec, on
"a changed condition's transition time = 2020-01-01 …, want it left unset".

**3. The e2e metrics read follows the install that is running** (`6a05a0d`).

`controllerMetrics` hard-coded `services/http:…/proxy/metrics`, true of
`config/e2e` alone. Metrics are secure by default (`-metrics-secure` defaults to
true; `config/manager` sets nothing; the chart emits `--metrics-secure=false`
only when `metrics.secure` is false — `_helpers.tpl:90`, `values.yaml:51`).
Against a chart-installed cluster the read failed with `BadRequest … unknown
reason` and `TestSaturationBecomesObservable` failed before any saturation
assertion ran.

The scheme is now read from the running Deployment's arguments, and the
assertion is gated on it: a secure metrics endpoint cannot be read through the
proxy at all (the metrics server authenticates its caller; the proxy forwards no
credential), so the spec skips with that reason rather than failing on a
BadRequest that names nothing. Where the read is possible nothing is skipped.

Measured, six full cycles:

| | cycles | stages | result |
|---|---|---|---|
| order A: `make test-e2e` then `make test-e2e-chart` | 3 | 6 | all exit 0 |
| order B: `make test-e2e-chart` then `make test-e2e` | 3 | 6 | all exit 0 |
| raw `go test -run TestSaturationBecomesObservable` against the chart install | 1 | 1 | `--- SKIP` with the reason above |
| the same spec against `config/e2e`, verbosely | 1 | 1 | `--- PASS (21.59s)` |

The deployments do not disagree with each other — both install paths serve HTTPS
and the e2e overlay is the deliberate exception — so there is no product
inconsistency to report. What was wrong was the helper assuming its own overlay.

### What is still open, and why

* **The rollout flake**: see "The rollout flake" below. Honest state: *not
  fixed*; root cause still unproven, with a classification of what the dumps
  show.
* **`make test-fuzz` still runs in no CI job** (§4 row 15). Wiring it in needs a
  time budget from the maintainer; unchanged.
* **The e2e specs are still serial** (`PLAN.md` §8, ~40s of CI), and the envtest
  control planes are still two (§11, 4.4s). Both ranked, neither attempted.

### Commit list (this session)

| Commit | Change | Measured effect |
|---|---|---|
| `edeb70b` | `reconcileResult` + eight requeue specs | no timing claim; the three intervals and the no-requeue paths are asserted where none was |
| `813d64e` | `carryTransitionTimes`: stop rewriting an unchanged status | one mutating request per steady-state pass → zero |
| `6a05a0d` | metrics scheme derived from the deployed Deployment | chart step's spec no longer fails on a BadRequest; both orders pass |
| `…` | revert of the `probe-bin` change | see "Reverted" below |

### The CI numbers, measured instead of derived

§8 of the session-1 report listed the E2E job time (~6.7 min) and the chart step
(~35s) as derived. They are now measured: the branch is pushed and PR #62 (draft)
is open, which runs the real workflow. Five runs — the first push plus four
`gh run rerun`s of the same run, one at a time, because the workflow's
concurrency group cancels an in-progress run on the same ref.

| | before | after (5 runs) | change |
|---|---|---|---|
| E2E job wall | 12.15 min median of 36 successful runs (10.0-13.5) | **7.55 min median** (6.67-7.70) | **-38%** |
| `make test-e2e` step | 386s | 366s median (310-374) | -5% |
| `make test-e2e-chart` step | 174s | **46s median** (45-48) | **-74%** |
| probe image build | 53s | **5-6s in all five** | -90% |
| controller image build | 101s | 92-122s | none (variance) |
| host probe build | 62s for both binaries | 48-77s for the probe alone | none (variance) |
| `go test` in the suite step (package) | 67.7s | 64.3-73.2s | none, as expected |

The before and after ranges do not overlap (10.0-13.5 vs 6.67-7.70).

**Where the derivation missed, and why.** The chart step was derived at ~35s and
measures 46s — 31%, over the 20% the task allows. The gap is that step's own
single spec: `TestProbeCLIFromTheHost` reports 22-26s in all five runs against
7.5s in the one baseline sample, and 23.1s in the local measurement of
`make test-e2e-chart` on this branch. The spec's work is unchanged by this
branch (the only change to that step is that it no longer rebuilds images), so
what the derivation missed is that spec's own duration, not removed work.

A controlled experiment settles whether the reuse is involved: on one cluster,
three cycles with `REUSE_IMAGES=1` give spec times 20.56s, 22.55s and 23.55s, and
three with `REUSE_IMAGES=0` give 19.55s, 23.56s and 20.56s — no difference. The
floor is the spec itself at ~21s, not the build the step no longer does.

*Why* that spec takes ~21s against a chart install and 6.5-8s inside the
kustomize suite is not pinned. The shape points at cold state rather than at the
spec: the chart step deletes the CRDs immediately before (`make uninstall`), so
the controller's caches and watches are being established when the pool appears,
and `createPool` plus `waitForRelays` pay for it. The duration is measured; the
mechanism is a hypothesis, and either way it is the step's own shape rather than
anything this branch changed. One CI run of the chart step with `REUSE_IMAGES=0`
would confirm the whole step, not just the spec; the local experiment above
already covers the part the derivation depended on.

The suite step's *saving* is much smaller than derived (386→366 measured,
against ~66s predicted) for a reason the phase timings show: the probe image
build saved the predicted 48s, and then the controller image build and the host
build came out 20-30s slower than the single baseline sample, in a pattern that
tracks the runner rather than the change — attempts 1/3/4 have both builds slow
(119-122s image, 76-78s host) and attempts 2/5 both fast (92-94s, 48-49s). The
one-run baseline sits between them, so the runs cluster by runner, not by
change.

### The rollout flake: not reproduced, not classified, not fixed

The task asked for five failures with dumps or 100 runs. **200 cycles ran
locally, with zero failures**, across five pressures (`baseline/
session2-flake-hunt.log`, `session2-flake-hunt-constrained.log`,
`session2-flake-hunt-v6.log`):

| variant | cycles | shape | failures |
|---|---|---|---|
| V1 | 100 | `make deploy` alone — the failing step | 0 |
| V2 | 20 | after `make load-images`: three images imported into four nodes immediately before the rollout, the I/O shape CI has | 0 |
| V3 | 20 | with two CPU-burning processes on a 2-vCPU box | 0 |
| V4+V5 | 40 | node limits **intended** — docker rejected `--memory` without `--memory-swap`, rejects the whole invocation, and I did not check, so neither CPU nor memory took effect. These are 40 more bare cycles and prove nothing about node pressure; kept in the log rather than quietly dropped | 0 |
| V6 | 20 | kind nodes limited to 0.4 CPU / 1.5 GiB, **verified in force on all four nodes before cycling** (`NanoCpus=400000000 Memory=1572864000`). The control plane sat at 67% of its memory limit during the run, the workers at 15-18% | 0 |

The healthy distribution is tight and far from the gate: for V1-V5 cycle time
min 12.5s, p50 13.4s, p95 17.3s, max 17.8s; with the nodes limited, min 13.5s,
p50 14.3s, max 16.4s. The old pod finishes terminating within three `kubectl`
poll intervals every single time (420 "1 old replicas are pending termination"
lines over 140 cycles = 3.0 per cycle). The CI failure sat at exactly that
message for 180s. **The failure is therefore a stall, not a rollout that was
progressing slowly** — which is the test the task sets for raising the timeout,
and it fails it. The gate stays at 180s.

No classification is possible from the evidence that exists: the eight original
failures predate the `await-rollout` dump, so their logs contain only the
timeout. What the dumps will show on the next occurrence is already decided by
the categories they print (`get nodes -o wide` for a `NotReady`/pressure node,
`get pods -o wide` for a pod stuck `Terminating`, events for evictions and
probe failures). The mechanism the one available clue points at — the new pod
became ready and only the old pod's deletion hung, because `kubectl` prints
"1 old replicas are pending termination" only after the new ReplicaSet has a
ready pod — is the kubelet of the old pod's node no longer completing the
deletion, which is what node I/O or memory pressure produces.

Four things CI has that this box cannot reproduce, recorded so the next attempt
does not re-derive them:

1. a **fresh cluster per run**: four node images pulled and three relay images
   imported into four nodes immediately before the rollout starts;
2. a **fresh controller image** whose ID changes every run (`DATE` is in
   `LDFLAGS`), so the rollout is a real image change rather than a restart of
   identical content, and containerd unpacks the new image on first use;
3. a **14 GiB runner disk** shared by four kind node images, the docker build
   cache and the BuildKit cache mounts this branch added;
4. four kind nodes on a 4-vCPU runner, with the import traffic of (1) hitting the
   same API server the rollout is polling.

What I did **not** try: recreating the cluster per cycle (the first of those,
~6 minutes each, and the one I would try first with more time), and running the
step inside a CI-like cgroup for the whole job rather than per node.

Honest state: **open, unproven, unfixed.** `REPORT.md` §9 keeps the hypothesis
list; the diagnostic is in place so the next CI failure classifies itself.

---

## 1. Summary

**The E2E job is the pull request's critical path: 10.0-13.5 min, median
12.2 min, against 2.2 min for the ci job.** Almost none of it was the tests.
`make test-e2e` spent 305s of its 386s on setup and 67.7s running specs; the
chart step spent 165s of its 174s on setup and 7.5s on its one spec. Half of the
time inside the two steps (291s of 560s) was four `docker build` invocations:
the two in the chart step rebuilt a tree the step above had just built, and
each of the four compiled every dependency from scratch, because `DATE` in
`LDFLAGS` changes on every `make` run and invalidates the build layer.

| Layer / job | Before | After | How |
|---|---|---|---|
| `ci` job (unit + envtest, lint, vuln) | 2.2-2.5 min | unchanged | nothing here was the bottleneck |
| `internal/controller` envtest | 20.04s local, 5 runs **[measured]** | 19.78s local, 5 runs **[measured]** | one `List` per `markPodsReady` instead of one per pod — within the run-to-run spread, so a correctness fix, not a timing win |
| `make test-e2e` setup | 484.1s local **[measured]** | 294.5s local **[measured]** (-39%) | cacheable images, probe-only host build |
| `make test-e2e` specs | 116.5s local, single run | 74.8s mean over 5 runs, 70.4-79.0s **[measured]** | the spec code is unchanged; the spread is scheduling (§5) |
| `make test-e2e` total | 600.6s local **[measured]** | 385.3s local **[measured]** (-36%), of which the spec phase was a 90.8s run; with the 5-run mean it is ~370s | |
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
| 8 | `internal/controller/suite_test.go:179` | missing coverage | wrong | `_, err := f.reconciler.Reconcile(...)`: the `ctrl.Result` is discarded in every controller test, so **no test asserts any of the three requeue contracts** — `secretRetryInterval` (`turnpool_controller.go:154`), `drainPollInterval` (`status.go:402`), `probeInterval` (`turnpool_controller.go:210`). A regression that made the controller never requeue, or spin with a zero delay, passes the whole suite. | **Fixed** `edeb70b` (Session 2): `reconcileResult` returns the result, and eight specs pin all three intervals, both no-requeue paths and the absence of an immediate requeue — each proven by breaking the contract it pins |
| 9 | `internal/webhook/v1alpha1/turnpool_webhook_test.go:557` | missing coverage | cosmetic | Every table case builds `&TURNPoolValidator{}` with a nil `Client`, and `validatePodIPDeployment` returns `nil` immediately when `c == nil` (`turnpool_webhook.go:401`), so the two "valid PodIP pool" cases assert nothing about attachment existence or address routability. | **Not fixed** — the coverage lives at the API-server level in `turnpool_webhook_envtest_test.go` ("rejects a declared attachment that does not exist", "admits a pool whose nodes declare routable addresses", the four unusable-address rejections). Deliberate: see `DECISIONS.md` §4 |
| 10 | `internal/probe/probe_test.go:308` | false confidence | cosmetic | `LatencyMillis < 0`: the value comes from `time.Since(start).Milliseconds()`, which cannot be negative. Unfailable. | **Not fixed** (cosmetic, no risk) |
| 11 | `internal/probe/probe_test.go:429` | false confidence | cosmetic | Accepts `401` **or** `400` because coturn and the pion test server differ; the in-process server always answers 400, so the 401 arm is dead and the assertion cannot detect a wrong code in either direction. | **Not fixed** (documented in the test's own comment; the e2e layer covers the real coturn code) |
| 12 | `internal/vantage/vantage_test.go:103` | false confidence | cosmetic | Four table cases, three distinct refusals implemented, one `wantErr` sentinel: "no observation time", "not json" and "another pool" are indistinguishable. | **Not fixed** |
| 13 | `internal/relay/fuzz_test.go:54` | false confidence | cosmetic | Every `Render` error is swallowed with `continue`, so a renderer that refuses *all* relays leaves the Render half of the body with no assertion executed. | **Not fixed** |
| 14 | `internal/relay/render_test.go:504` | false confidence | cosmetic | Asserts the absence of `no-tcp-relay` but not `min-port`/`max-port`, which is what the doc comment's capacity argument is about. | **Not fixed** |
| 15 | `make test-fuzz` | missing coverage | wrong | Neither fuzz target runs in CI: `grep -rn fuzz .github/workflows/` matches nothing, and `make ci` is `verify helm-verify lint test vuln licenses dco`. The renderer and validator invariants are only exercised when someone remembers to run it. | **Not fixed** — wiring it in needs a time budget decision (30s per target) that is the maintainer's, not the audit's |
| 16 | `internal/controller/address_conflict_test.go:20` | fragility | cosmetic | The suite's own comment: "envtest never finishes terminating a test namespace, so pools from the other tests in this package are still there". Every test leaks its namespace and its pools, and the cross-pool conflict check scans the whole cluster, so each test's reconciles walk the leftovers of every earlier test. The workaround (a unique address per test file) is correct but load-bearing and undocumented outside that comment. | **Not fixed** — quantifying it needs a probe into the API server at the end of a run |
| 17 | `internal/webhook/v1alpha1/turnpool_webhook_envtest_test.go:66` | wrong layer | cosmetic | The `scale` subresource spec is CRD plumbing, not admission. It belongs with the API/CRD tests rather than the webhook suite. | **Not fixed**: it is already at the right *layer* (envtest), only in the wrong file, and it costs 0.02s |
| 18 | `test/e2e/e2e_test.go:620` | fragility | cosmetic | `controllerMetrics` reads the controller's own metrics through the API server as `services/http:…:8443/proxy/metrics`, and the comment above it says why: the e2e overlay passes `--metrics-secure=false`. The chart installs metrics over HTTPS (`charts/turnfleet/values.yaml:51` `secure: true`), so against a chart-installed cluster the call fails with `Error from server (BadRequest): the server rejected our request for an unknown reason` and `TestSaturationBecomesObservable` fails **before** any assertion about saturation runs. Found the hard way: 5/5 reruns after a `make test-e2e-chart` left the chart installed. CI never sees it because the chart step runs only `TestProbeCLIFromTheHost`. | **Fixed** `6a05a0d` (Session 2): the scheme is read from the running Deployment's arguments, and the spec skips with that reason when the install serves HTTPS. Both e2e targets pass in either order (3 cycles each) |
| 19 | `internal/controller/status.go:79` | product bug | wrong | Found by the idempotency spec Session 2 asked for: `buildStatus` builds its conditions from nothing, so `meta.SetStatusCondition` stamped `time.Now()` on every one of them and the freshly built status never compared equal to the stored one. The guard at `status.go:81` could not fire, every pass issued an `Update` the API server discarded, and the stored `LastTransitionTime` — the API's "when did this condition last transition" — walked forward while nothing transitioned. | **Fixed** `813d64e`: `carryTransitionTimes` carries the stored time over for unchanged conditions. One mutating request per steady-state pass → zero |

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

Not a spec: the `await-rollout` step, 8 of 50 CI runs, root cause unproven.

Session 2 attempted a local reproduction and could not force it: **200 cycles
across five pressures, zero failures**, including kind nodes limited to 0.4 CPU
and 1.5 GiB with the limits verified in force. The healthy distribution has a
10x margin to the gate — p50 13.4s, max 17.8s against the 180s timeout — and the
old pod finished terminating within three `kubectl` polls in every one of the
140 cycles that were measured in detail. The CI failure is therefore a *stall*,
not a rollout that was progressing slowly, which is the test the task sets for
raising the timeout: it fails it, so the gate stays at 180s. Classification needs
a dump from CI and the eight original failures predate `await-rollout`; the next
occurrence classifies itself. Details and the four CI-only differences in
Session 2.

The five CI runs on this branch show **no rollout failures at all** (5/5 green).
Five samples against a 16% base rate cannot show a change in the rate either
way; the bar for claiming a fix was 0/50.

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

**No spec was lost.** `git grep -c '^func Test'` over every `*_test.go` returns
113 on `main` and 113 on this branch: no test function was added or removed, and
the 22 Ginkgo specs are untouched (`turnpool_webhook_envtest_test.go` changed
only its node-cleanup `DeferCleanup` and an import; the specs in
`turnpool_webhook_test.go` kept their names, cases and count). The E2E suite's 8 specs are
byte-identical: this branch does not touch `test/e2e/` at all.

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

* ~~**CI wall times after the change are derived, not measured.**~~ **Measured
  in Session 2**, on five runs of the real workflow (PR #62): E2E job 7.55 min
  median (6.67-7.70) against 12.15 min for the 36 successful runs before; chart
  step 46s median (45-48) against 174s; suite step 366s median (310-374) against
  386s. The one derivation that missed by more than 20% is the chart step (~35s
  derived, 46s measured) — Session 2 records what it is and what would confirm
  the attribution.
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
   Session 2 tried to force it locally (200 cycles, node limits verified in
   force) and could not; the four CI-only differences worth trying next are
   listed in Session 2. **Still unconfirmed.**
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

1. ~~**Assert the requeue contracts** (§4 row 8).~~ **Done** in `edeb70b`, with
   eight proven breaks. What that item leaves behind is idempotency in the wider
   sense: the specs assert that one steady-state pass writes nothing, not that
   two passes over the same state converge on identical output in every state
   the controller can be in.
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
9. **Let the Ginkgo suite run with a fixed seed in CI.** `make test` passes all
   packages to one `go test`, and `-args` reaches *every* test binary in that
   invocation, so `--ginkgo.seed` kills `internal/controller` with "flag
   provided but not defined: -ginkgo.seed". A fixed seed in CI needs the two
   envtest packages split into two invocations (or a `GINKGO_SEED` env var).
   Found the hard way while writing the rerun script.

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

# The repo's own gates, all green on the final tree:
make lint && make verify && make test && make ci

# The envtest before/after (§5), 5 runs each, one package per invocation:
git worktree add --detach /tmp/before 9ba0193
for s in 1101 2202 3303 4404 5505; do
  for tree in /tmp/before "$PWD"; do
    ( cd "$tree"
      KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" \
        go test -race -count=1 -timeout 20m ./internal/controller/
      KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" \
        go test -race -count=1 -timeout 20m ./internal/webhook/v1alpha1/ \
        -args --ginkgo.seed=$s --ginkgo.randomize-all )
  done
done

# The e2e flake reruns (§5), 5 times, against the install `make test-e2e` makes:
for i in 1 2 3 4 5; do
  KUBE_CONTEXT=kind-turnfleet KIND_CLUSTER=turnfleet COTURN_IMAGE=coturn/coturn:4.18.0-r0 \
  PROBE_BIN="$PWD/bin/turnfleet-probe" PROBE_IMG=ghcr.io/hazmi35/turnfleet-probe:dev \
    go test -tags=e2e -count=1 -v -timeout 30m ./test/e2e/
done
```

### Session 2

```sh
# The requeue and idempotency specs, verbosely
KUBEBUILDER_ASSETS="$PWD/bin/k8s/1.37.0-linux-amd64" go test -count=1 -v \
  -run 'TestReconcileOnAMissingPoolAsksForNothing|TestRequeue|TestReconcileIsIdempotentOnUnchangedState|TestCarryTransitionTimes' \
  ./internal/controller/

# The eight-break proof matrix (it edits the two controller files and restores
# them; the run leaves the tree clean)
bash test-audit/baseline/scripts/requeue-breaks.sh      # output: baseline/session2-requeue-breaks.txt

# Both e2e orders, three cycles each (~23 min here, needs the local kind cluster)
bash test-audit/baseline/scripts/e2e-orders.sh

# The rollout-flake hunt: 180 cycles, then 20 with the kind nodes limited
# (~33 min, then ~5 min)
bash test-audit/baseline/scripts/flake-hunt.sh
bash test-audit/baseline/scripts/flake-v6.sh

# The chart step's single spec with and without image reuse
bash test-audit/baseline/scripts/chart-spec-timing.sh

# The five CI runs are the e2e workflow on PR #62: the first push, then four
# reruns one at a time (the concurrency group cancels a run on the same ref)
gh run view 38750384973
gh run rerun 38750384973 && gh run watch 38750384973
gh api repos/Hazmi35/turnfleet/actions/runs/38750384973/jobs \
  --jq '.jobs[] | .steps[] | [.name, .started_at, .completed_at, .conclusion] | @tsv'
```
