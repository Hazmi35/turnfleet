# Decisions

Judgment calls made while the audit ran, with the reasoning. Ambiguous calls
were resolved conservatively: keep behaviour, prefer the smaller change, and
prove the effect.

## 1. Cache mounts, not a removed build-date stamp

The two container images in the E2E job rebuilt from scratch on every
invocation because `DATE` in the Makefile changes per `make` run, `DATE` is part
of `LDFLAGS`, `LDFLAGS` is a `docker build --build-arg`, and the `RUN go build
-a` layer therefore always missed.

The obvious fix — drop `date` from `LDFLAGS` so the layer cache hits — was
rejected: the stamp is documented behaviour (`.github/workflows/image.yaml`
explains it, ADR 17 and `docs/versions.md` record the pins), and removing it
changes what a released binary reports. BuildKit cache mounts give the same
speedup and change nothing about the produced artefact: a cache mount is not a
layer, so the images stay byte-for-byte what they were apart from the stamp
that is meant to vary. `-a` was dropped for the same reason it was useless
here: with no cache to reuse it compiles everything anyway, and with a cache it
only forces work that is already done.

## 2. The rollout gate keeps its semantics; it gains diagnostics

Eight of the last 50 E2E runs failed in `make deploy` with `error: timed out
waiting for the condition` after `1 old replicas are pending termination`, all
on 2026-10-13 and none since. The successful path takes 11-12s against a 180s
budget, so the timeout is not short and raising it is not a fix: by the rule
that a timeout may only grow when data shows the old value was below real
convergence, this one stays at 180s.

Switching the gate to `kubectl wait --for=condition=Available` would make the
symptom disappear, because it does not wait for old pods to terminate. That is
a guess at the root cause, and it would weaken a check that currently catches a
stuck rollout. Instead the failure now dumps the deployment, the pods, the
nodes and recent events, so the next occurrence can be diagnosed from the log
rather than by re-running. Recorded as a suspicion in `REPORT.md`, not as a
fixed flake.

## 3. `REUSE_IMAGES` is opt-in, not automatic

`make test-e2e-chart` now skips the image build only when `REUSE_IMAGES=1`, and
the CI workflow sets it. Automatic detection ("is the image already in docker?")
would make a standalone chart run silently test whatever image happened to be
lying around instead of the tree it was pointed at. Defaulting to build keeps
the target honest for local use, and the CI job's two steps provably share one
tree.

## 4. No fake client for the PodIP webhook branch

`validatePodIPDeployment` returns early when the validator has no `Client`, so
`TestValidateCreate`'s two "valid PodIP pool" cases assert nothing about
attachment existence or declared-address routability. The available fixes were
a `fake.NewClientBuilder()` in the unit test, or leaving it.

Left it, deliberately: this repository has no fake client anywhere, and the
same rules are already proven through a real API server in
`turnpool_webhook_envtest_test.go` — "rejects a declared attachment that does
not exist", "admits a pool whose nodes declare routable addresses", and the
four unusable-address rejections. A fake client would be a second, weaker
implementation of cluster reads. The gap is recorded in `REPORT.md` so the
choice is visible rather than implied.

## 5. `TestValidateDeleteIsUnconditional` stays

DELETE is not a served admission operation (`verbs=create;update` in the
webhook marker, `operations: [CREATE, UPDATE]` in `config/webhook/
manifests.yaml`), so no API server ever calls `ValidateDelete`, and the test
asserts that a `return nil, nil` body returns no error. Removing it was
tempting. It stays because it pins the interface contract an admission
`Validator` owes — "deletion is guarded by the drain finalizer, not by
admission" — and costs milliseconds; there is no other test that would notice
if that method started rejecting.

## 6. E2E specs are not parallelised in this change

The E2E suite's eight specs are serial and take 67.7s on CI (112.9s locally).
Even a perfect parallelisation saves ~40s of CI, against the ~350s the setup
changes in this branch recover, and it is the change with the most ways to be
subtly wrong: the specs share one namespace, three relay nodes and the host
network, and two of them assert on allocation counts and drain timing. It is
ranked in `REPORT.md` as the next opportunity with its measured prize, and not
attempted, rather than attempted and half-verified.

## 7. Test namespaces come from a counter

`time.Now().UnixNano() % 1_000_000` repeats every millisecond, so two fixtures
could pick one name and the second `Create` fails with `AlreadyExists` — a
failure with nothing to do with the code under test. An atomic counter is
strictly better and removes a real clock from a suite that otherwise uses the
injected fake clock.

## 8. The version fallback test says what it cannot observe

A `go test` binary carries no `vcs.revision` setting (verified: its build info
holds only `-buildmode`, `-compiler`, `CGO_*`, `GOARCH`, `GOOS`), so the commit
half of `Info()`'s fallback is unreachable from the unit layer. Rather than
`t.Skip` the whole test — which would hide the two assertions that do hold — it
asserts them and uses `t.Log` for the unreachable half, so the gap is visible
in the output instead of silent.

## 9. The e2e suite's dependence on the e2e metrics overlay is documented, not removed

`test/e2e/e2e_test.go:620` (`controllerMetrics`) reaches the controller's
metrics as `services/http:…:8443/proxy/metrics`, which only works because
`config/e2e` passes `--metrics-secure=false`. Against the chart's default
install (`charts/turnfleet/values.yaml:51`, `metrics.secure: true`) the call
fails with `BadRequest` and `TestSaturationBecomesObservable` fails before any
assertion about saturation runs.

I found this by leaving the chart installed after measuring
`make test-e2e-chart` and then rerunning the suite: 5 runs, 5 failures, all at
the metrics read. Left as is, because `make test-e2e` always deploys that
overlay, the coupling is already explained in the comment above the function,
and the chart install path is covered by a spec that does not read metrics. The
finding, and the suggestion to write it down, are in `REPORT.md` §4 row 18 and
§9; what I did *not* do is make the metrics read scheme-configurable, because
that would change a spec whose behaviour I have no evidence is wrong.

## 10. `probe-bin` is reverted: five CI runs do not show the saving it was derived from

`make test-e2e` depended on `build`, which builds both host binaries. Session 1
replaced it with a `probe-bin` target on the argument that the controller host
binary is dead weight — the controller under test runs from the container image —
and derived ~14s of saving from a local cold-cache ratio (controller 194.8s,
probe alone 151.4s).

The measured CI runs do not show it. The host-build step takes 48-78s with
`probe-bin` (attempts: 78, 48, 77, 76, 49) against 62s for `make build` in the
one baseline sample, and the spread tracks the runner rather than the change:
attempts 1, 3 and 4 have both the controller image build and the host build slow
(119-122s and 76-78s), attempts 2 and 5 have both fast (92-94s and 48-49s).

The reason is visible in the baseline log: `make build` compiles the controller
first (61.8s) and the probe then takes 1.1s, because the two share the 281
client-go/controller-runtime packages the probe pulls in through
`internal/vantage`. The probe's own dependency graph is most of the cost, so
building it alone is not cheaper — and building the controller's binary alongside
it is nearly free.

Reverted. The change saved nothing measurable, it was the one item in the branch
whose effect was derived rather than measured, and `PLAN.md` §3 already said so.
What remains from that commit is `-o $(PROBE_BIN)` instead of `-o
bin/turnfleet-probe`, which is the same path through the variable the Makefile
already exports for the e2e suite.

## 11. The rollout flake is left unfixed, with the diagnostics in place

160 local cycles across five pressures (bare, after an image import, under host
CPU saturation, and twice with the kind nodes resource-limited) produced no
failure. The healthy distribution has a 10x margin to the gate — p50 13.4s, max
17.8s against a 180s timeout — and the old pod finished terminating within three
kubectl polls every time, so the CI failure is a stall rather than a rollout that
was progressing slowly.

That matters for the one fix the task allowed me to consider: raising the timeout
counts only when the dumps show the rollout was still progressing normally when
it expired, with measured convergence times to justify the new value. The
measurements say the opposite of "still progressing" — nothing local ever came
within 10x of the gate — so the timeout stays at 180s.

Two further reasons not to guess at a fix. The eight original failures predate
`await-rollout`, so their logs contain no state to classify; and the fix that
would make the symptom disappear (`kubectl wait --for=condition=Available`, which
does not wait for old pods) would also hide a genuinely stuck rollout. Recorded
as unfixed in `REPORT.md` §9 with the four CI-only differences to try next.

## 12. The node-constraint experiment is reported twice, once as a failed attempt

The first run of the node-pressure variant applied `docker update --cpus=0.5
--memory=2g`; docker rejects a memory limit unless memory-swap is set at the same
time, rejects the whole invocation, and I did not check — so those 40 cycles were
unconstrained and prove nothing about node pressure. The second run applies both
and verifies `NanoCpus`/`Memory` are non-zero on all four nodes before cycling.

Both are in the report: the 40 unconstrained cycles as evidence of a mistake I
made, the constrained ones as the real test. The distinction is the whole point
of "every claim needs evidence" — the first run would have read as a clean null
result.
