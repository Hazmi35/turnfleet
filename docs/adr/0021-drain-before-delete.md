# 21. A drain is recorded in one pass and its pod deleted in a later one

* Status: accepted
* Date: 2026-10-16
* Refines [0006](0006-drain-semantics.md) on the ordering between a relay
  leaving `turnURIs` and its pod being deleted. Everything 0006 decides about
  *why* a relay is drained, and about the wait, stands.

## Context

ADR 6 fixes the sequence: a relay that is going away is removed from
`turnURIs` and from the `<pool>-turn-uris` ConfigMap before its pod goes away,
its readiness fails, the controller waits for the allocation count to reach zero
or for `spec.drainTimeout`, and only then is the pod deleted.

`reconcilePods` converged a pool in three loops: record a drain, delete every
relay whose drain is complete, create the pods that should exist. Two
properties ADR 6 relies on did not follow from that shape.

* The delete loop asked whether a relay has to go and whether its drain is
  complete, never whether its drain was *recorded*. `drainComplete` answers yes
  for a pod that is not running, and for a relay holding no allocation. An idle
  relay was therefore deleted in the pass that recorded its drain, and the loop
  had no per-pass limit, so every idle relay of the pool went in that one pass.
  That pass then wrote the status and the ConfigMap from the states it had built
  before the deletion, so the relays it had just deleted were still published as
  ready, with their URIs, in both places a credential issuer reads. A consumer
  minted a credential for a relay whose pod was already gone, and the pool
  replaced several relays at once.
* A relay whose drain was complete but whose pod was still terminating no
  longer "has to go". Its configuration hash is current, so `drainRequired` is
  false, and nothing serialized the next relay behind it. The next relay started
  draining while the previous relay's replacement could not exist yet, so a
  whole fleet could be terminating with nothing replacing it.

## Decision

**A relay's pod is deleted only once a pass has read the relay's drain back from
that pod, and a relay on its way out keeps the next relay from being marked.**

* **A drain is recorded on the pod, and only a later pass acts on it.** A pass
  lists the pods before it changes anything, so it distinguishes a relay whose
  pod already carries the drain annotation, meaning an earlier pass recorded the
  drain and wrote the relay out of `turnURIs`, from one it marks in this pass.
  Only the first kind is eligible to be deleted. The pass that records a drain
  is therefore the pass that writes the status and the URI ConfigMap, and no
  consumer can read a URI for a relay whose pod is being deleted.
* **The drain slot is held until the relay's pod is gone.** While a relay is
  draining, or still has to go, and its pod exists, no other relay is marked.
  The next relay's capacity leaves service only once the previous relay's
  replacement can be created, and that replacement carries the relay's identity
  and therefore its name, so it cannot be created while the old pod object is
  still being finalized.

## Consequences

* A rollout of an idle pool removes one relay's capacity from service at a
  time by construction rather than by the scrape answering zero: relay A leaves
  `turnURIs` in one pass, its pod is deleted in the next, and relay B is marked
  only once A's pod is gone.
* A relay pod that never goes away, which is what a node that is gone but whose
  pods have not been reaped looks like, holds the drain slot for the rest of the
  rollout. That relay's own drain is still bounded by `spec.drainTimeout`, but
  the relays behind it wait for the pod object. Removing that object is the
  kubelet's job, or the node lifecycle controller's. This is the price of never
  putting two relays out of service at once, and it is the state ADR 6 already
  describes under node loss: the pool reports a relay that is not converging
  instead of silently losing a second one.
* A relay is deleted one pass later than it used to be, so a rollout costs one
  extra reconcile per relay. That pass is one `drainPollInterval`, and the
  controller already re-checked an in-flight drain at that interval.
* `status.relays[]` publishes at most one draining relay at a time, and the
  drain regression test in `internal/controller` asserts the ordering pass by
  pass: at most one pod deleted per pass, every deleted pod's relay already out
  of the published URI list, and no URI published for a relay whose pod is gone.
