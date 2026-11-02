# 22. A relay's probe status records its outcome, not its last probe

* Status: accepted
* Date: 2026-11-02
* Refines [0007](0007-probe-vantage-point.md) on where the in-cluster probe's
  result is kept. Everything 0007 decides about what the probe checks and what
  it feeds stands.

## Context

The controller probes every ready relay once per `spec.probe.interval`
(default 10s). Each probe used to replace `status.relays[].lastProbe` with
`{time, succeeded, latencyMillis, message}`. `time` and `latencyMillis` change
on every probe, so the status that writeStatus computed never matched the stored
one, and a pool with nothing happening to it was written once per probe
interval (issue #76). On one real cluster that was one `TURNPool` status
update every 10s, about 10,100 in 28 hours, each one an etcd write and an event
for every watcher of the pool.

The status also served as the probe schedule. The controller read
`lastProbe.time` back on the next pass to decide whether a probe was due, so the
timestamp could not leave the status without the schedule going somewhere else.

Kubernetes has made the same move twice. Node heartbeats left `NodeStatus` for a
Lease object (KEP-589), and kubelet leaves `PodCondition.lastProbeTime` unset.
A field that changes on a timer costs a write per tick, whatever it reports.

## Decision

**`status.relays[].lastProbe` is the relay's current probe outcome.** It holds
`succeeded`, `reason`, `message` and `lastTransitionTime`, and it changes only
when the outcome does.

* **`lastTransitionTime` replaces `time`.** It is when `succeeded` last changed,
  which is the meaning the field name has on conditions. The old name is not
  kept with the new meaning: a consumer that read `time` as "how fresh is this"
  would see a stale timestamp on a healthy pool and nothing to tell it why.
* **`latencyMillis` is removed.** `turnfleet_probe_duration_seconds` carries it.
* **`reason` names the step the probe ended at**: `Allocated`,
  `ListenerUnreachable`, `ClientFailed`, `AllocationRefused`,
  `RelayedAddressUnreadable`, `RelayedAddressUnexpected`, or `AuthUnavailable`
  when the probe could not run without the pool's auth secret. It is an enum
  in the schema. The probe reports the same reasons as a typed
  `probe.Reason`, and the controller maps one to the other in a switch over
  every probe reason, so the `exhaustive` linter fails a probe reason that is
  added without a decision about the status.
* **The message is replaced when `succeeded` or `reason` changes, or when the
  TURN error code does.** It is not compared as text, because it ends in the
  error the probe saw, and that text can differ between two probes that failed
  the same way (pion's retransmission error names the transaction). With the
  reason in the status, the status is compared with itself, which holds across
  a controller restart. The TURN error code is the one part of the outcome kept
  only in memory: a code that changes across a restart, under the same reason,
  leaves the old message until the next change.
* **The probe schedule lives in the controller's memory**, one record per relay
  with when it was probed, how long it took, its error code and the probe
  status the controller reports for it. Every pass reports the recorded status,
  so a status write that fails is made again by the next pass, which is not due
  to probe. The probe metrics are published from the same record. The record is
  dropped when its relay or its pool is gone.

## Consequences

* A pool whose relays keep answering the same way writes no status between
  probes. A relay that starts or stops failing costs one write at the change.
* The status no longer says when a relay was last probed. Whether the
  controller is still probing is the controller's health (its Deployment and
  its leader election Lease) and the probe metrics, not the pool.
* A restarted controller, or a new leader, has no probe records. Its first pass
  probes every relay with a ready pod, as it would a relay never probed. When
  `succeeded` and `reason` match the status, nothing is written.
* Until that first probe, the probe gauges of a relay are absent rather than
  carried over from the status, because the status no longer holds a latency.
* `lastProbe.time` and `lastProbe.latencyMillis` disappear from the API. They
  are status fields of a `v1alpha1` resource, and nothing in this repository
  reads them, but an external reader of either needs to move to
  `lastTransitionTime` or to the metrics. A relay reported before the upgrade
  has no `lastTransitionTime` and no `reason`, so its first probe after the
  upgrade sets both, even when `succeeded` did not change.
* `turnfleet-probe --json` and the vantage payload gain `reason` beside
  `message`.
