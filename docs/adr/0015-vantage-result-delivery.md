# 15. The vantage point's result is a ConfigMap the controller reads

* Status: accepted
* Date: 2026-10-13

## Context

ADR 7 splits the probe in two: the controller proves a relay's configuration
and capacity from inside the cluster every interval, and `turnfleet-probe` run
from outside is the authoritative check for reachability, because hairpin NAT, a
firewall in front of the relay node and a NAT mapping that is not 1:1 are all
invisible from a pod. That left the authoritative check manual — a CI job, a
laptop, someone remembering to run it — and issue #9 asks to bring its answer
into the control loop.

Two things have to be decided before a runner can move the status: where the
runner runs, and how its answer reaches the pool. `status` is written by the
controller, so the runner cannot write the verdict itself, and the controller
must not be able to derive the verdict from anything it observes in-cluster —
otherwise an in-cluster observation can stand in for an outside one, which is
the failure ADR 7 exists to prevent.

The runner also has to be able to say "unreachable" and be believed, while the
in-cluster probe keeps passing. Any design where one result can be computed from
the other is wrong: the two vantage points answer different questions.

## Decision

**Delivery path: a ConfigMap named `<pool>-vantage`.** The runner creates or
updates it; the controller watches ConfigMaps by label and reads it in every
reconcile.

* *Not an annotation on the `TURNPool`.* Writing one would mean granting the
  runner `update` on pools, which carries the whole spec: a runner that can
  patch a pool can change its relay port range, its addressing mode and its
  auth. The authoritative reachability check would be the most privileged
  component in the cluster.
* *Not a Lease.* A Lease is built for one holder identity and a renewal time;
  the payload here is a list of per-listener results, and `coordination.k8s.io`
  has no field for a structured document. Stuffing JSON into `holderIdentity`
  would make `kubectl get lease -o yaml` unreadable and turn a coordination
  object into a data store.
* *A ConfigMap* is what the controller already watches and what an operator can
  inspect: `kubectl get configmap demo-vantage -o yaml` is the entire published
  result, in the same shape the probe CLI prints. It holds the payload under
  `vantage.json`, carries `app.kubernetes.io/component: vantage` and
  `app.kubernetes.io/instance: <pool>`, and has **no owner reference**: the
  runner does not own the pool, and the operator does not own the runner. The
  controller deletes the result when it removes its finalizer.

**The payload** (`internal/vantage`) is the pool name, the observation time and
one `probe.Result` per listener URI, which is the same type the controller's own
probe produces. A payload that names another pool, or that carries no observation
time, is refused: the first is what a copied runner manifest produces, and
without a time there is no way to tell a current success from a stale one.

**What the runner checks** is every URI in the pool's list —
`<pool>-turn-uris`, which is exactly what a client is handed — one probe per
listener. From a vantage point the only assertion available without reading the
pool's spec is that the relay advertises the address the URI names, so that is
what the runner asserts; the port range stays the in-cluster check's job.

**Where the runner runs is the operator's decision, and the controller cannot
verify it.** The shipped manifest (`config/vantage/runner.yaml`, a CronJob the
operator does not schedule) runs in a pod, which demonstrates the delivery path
and nothing more: the vantage point that ADR 7 makes authoritative has to run
outside the cluster's network. Nothing in the API claims otherwise, and the
condition is named for the vantage point rather than for externality.

**Freshness is bounded.** A published result counts for
`controller.VantageResultMaxAge` (15 minutes, three passes of the shipped
five-minute CronJob). Older than that, the condition is unknown, because a
runner that stopped running must not leave "reachable" standing. The constant
and the manifest's schedule are a pair.

**The condition is `ReachableFromVantage`** (normal-true), set through the same
`meta.SetStatusCondition` helper as the others: true when every published result
succeeded, false when one did not, unknown while nothing current has been
published. It is computed from the published payload alone — no in-cluster
observation takes part in it. A failing observation also raises `Degraded`
(reason `UnreachableFromVantage` when it is the only thing wrong), which is what
an operator alerts on.

`Ready` keeps its in-cluster meaning: the relays exist, are ready and are not
draining. A pool whose relays cannot be reached from outside is not "not ready"
— it is ready and unusable, and `ReachableFromVantage: false` plus `Degraded:
true` says so without conflating the two.

**The publisher lives in `turnfleet-probe`** (`--uris-configmap`,
`--publish-configmap`, `--pool`), not in a second binary. The runner is the
probe: same code, same credentials, same `Allocate`, same JSON. A second binary
would have to be kept identical to this one by hand, and the reason ADR 7 gives
for the CLI — that an operator runs what the operator trusts — argues against
splitting it. The client it needs is controller-runtime's, which reads
in-cluster configuration or `KUBECONFIG`, so the same binary publishes from a
laptop or a CI job.

**RBAC**: the runner's Role grants `get, list, create, update, patch` on
ConfigMaps in the pool's namespace and nothing else. It is not scoped by
`resourceNames`: `create` cannot be restricted that way, so a name-scoped rule
would be documentation rather than a limit. The shared secret reaches the runner
as a read-only volume mount of the pool's own Secret; an out-of-cluster runner
has to be given the value out of band.

## Consequences

* Issue #9's acceptance holds: a pool whose relays answer in-cluster and nowhere
  else reports `ReachableFromVantage: false` and `Degraded: true`, and a passing
  in-cluster probe cannot clear either. `internal/controller/vantage_test.go`
  fails when the condition is computed from the in-cluster probe instead.
* The runner is a single point of truth that can also be wrong: a runner whose
  credentials are stale, whose `--uris-configmap` points at the wrong ConfigMap,
  or which is run from a network with its own filtering reports a failure the
  pool's relays do not have. The published result is inspectable and the
  condition says when it was observed, which is all the controller can offer.
* Until a runner runs outside the cluster's network, the external reachability
  property ADR 7 makes authoritative is *not* demonstrated by anything in this
  repository: `config/vantage/runner.yaml` runs in a pod, and
  `TestVantageRunnerInCluster` proves the delivery path, not the vantage point.
  Demonstrating it needs a machine outside the cluster, routable relay
  addresses, and ingress rules per address — the operator's infrastructure, not
  the operator's code.
* A pool with no result published forever reports `ReachableFromVantage:
  unknown`, which is visible in `kubectl get turnpool -o yaml` but raises no
  event. Monitoring should alert on unknown, not only on false.
