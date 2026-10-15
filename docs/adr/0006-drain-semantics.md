# 6. Drain semantics

* Status: accepted
* Date: 2026-10-13

## Context

A relay holds allocations that are invisible to Kubernetes. Deleting the pod
tears every allocation down at once: peers keep sending to a relayed transport
address that no longer exists, and the client only discovers this on its next
consent refresh. A pool that is scaled down, rolled or deleted must therefore
stop handing out its relays **before** it stops serving them.

## Decision

A relay is drained when it is going away: scale-down, a rollout caused by a
config/secret change, a node that lost its relay label, or pool deletion.

The state machine, applied by the controller and observable in
`status.relays[].draining`:

1. `draining: true` is set on the relay's status entry.
2. The relay is removed from `status.turnURIs` and from the
   `<pool>-turn-uris` ConfigMap, so the credential issuer stops handing out
   URIs for it. This happens before any pod change.
3. The pod is labelled `turnfleet.hzmi.dev/draining=true` and annotated with
   the time the drain started. The relay's readiness is the controller's own
   `status.readyRelays`, which excludes a draining relay. A pod whose pool has a
   TCP listener also carries a TCP readiness probe, and the drain does not fail
   that by itself.
4. The controller waits for the relay's allocation count (from coturn metrics,
   ADR 5) to reach zero, or for `spec.drainTimeout` to expire, whichever comes
   first. Draining is bounded: a stuck client cannot block a rollout forever.
5. Only then is the pod deleted.

Deletion of the TURNPool object is guarded by the finalizer
`turnfleet.hzmi.dev/drain`, which runs the same sequence for every relay in the
pool before the finalizer is removed. Pods carry an ordered graceful shutdown,
so the coturn process itself has the chance to send `Refresh`-style teardown
rather than being SIGKILLed.

Node loss is the exception the timeout exists for: when the node is gone, the
allocations are already dead and the relay can never reach zero, so the wait
ends at `drainTimeout`. In that case the status records the relay as drained
with a message saying the wait timed out, instead of reporting a healthy pool.

Rollouts are serialized: one draining relay per pool at a time. A roll
therefore never removes more than one relay's worth of capacity from service,
and the `turnURIs` list is never empty while at least one ready relay exists.

## Consequences

* Consumers of `turnURIs` see a strictly shrinking set during a scale-down: no
  URI appears twice, and none disappears before the relay stopped accepting.
* `spec.drainTimeout` (default 30s) bounds the wait and is part of the rollout
  latency budget. It is a `metav1.Duration`, not a bare integer, so the unit is
  in the manifest.
* The e2e suite proves the ordering: with an active allocation on the relay
  being removed, the relay disappears from `turnURIs` while its pod still
  exists. The pod is deleted only after the allocation is released (or
  `drainTimeout` expires).
