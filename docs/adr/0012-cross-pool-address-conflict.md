# 12. Cross-pool relay address conflicts are a rejected state

* Status: accepted
* Date: 2026-10-13

## Context

A relay identity is `(relayAddress, portRange)`, and it has to be unique across
the cluster: two TURN processes that own the same ports on one address bind the
same sockets, so the second one serves a fraction of what it reports and the
first one loses ports it is relaying on. ADR 1
(`docs/adr/0001-addressing-modes.md`) makes the identities inside one pool
disjoint by construction, and the validating webhook rejects a pool whose own
addressing would collide.

Across pools there was nothing. ADR 1 recorded that as a known limitation,
because a validating webhook sees one object: the other pool may not exist yet,
may be mid-edit, and the cache the webhook could read is not a trustworthy
picture of the cluster. The limitation asked the operator to use distinct
addresses or disjoint ranges, and nothing detected a violation, which is the
part that matters: the failure was silent — both pools reported `Ready` while
they served less than the sum of their relays.

## Decision

The reconciler compares the relay identities pools hold and reports every
overlap as a condition on each pool that holds one. Admission is not touched: a
single-object webhook cannot be atomic across objects, and a cache of the other
pools is not authoritative enough to reject on.

**What is compared is what pools hold, never what they request.**

* For the pool being reconciled, the identities are the ones its own states for
  this pass carry: the relays it plans plus the ones still draining. They are
  exactly what its status is about to publish, and the plan already reflects
  what the nodes can actually give the pool.
* For every other pool, the identities are `status.relays[]`.

A pool that requests an address but holds no relay on it therefore conflicts
with nobody. A relay that is draining still holds its ports until its pod is
gone, so it still takes part: the condition clears once the relay is gone, not
when its owner is asked to leave.

**Overlap, not equality.** Two identities conflict when the addresses are equal
and the port ranges share at least one port. Disjoint ranges on one address stay
legal across pools for the same reason they are legal inside one: several
processes may share an address as long as each owns its own slice, and the
address's capacity is then divided rather than multiplied
(`docs/adr/0001-addressing-modes.md`).

**Both conditions.** A conflict raises `AddressConflict` (new, abnormal-true)
and `Degraded`, both through `meta.SetStatusCondition`, both naming the other
pool and its ports. `Degraded` is the pool-level fact an operator already
watches; `AddressConflict` is the specific cause. Status stays derived: nothing
about another pool is cached, and no spec field carries the answer.

**The other pool has to be woken.** The pool that has to clear its condition is
the one that did *not* change, so the reconciler watches TURNPools. The map
function enqueues every pool holding a relay on one of the changed pool's
addresses — controller-runtime maps both the old and the new object of an
update, so the peers a pool just left are enqueued too — and the predicate
passes for a create or a delete and, on an update, only when the set of held
identities changed. A status write that only reports a probe result or a port
count wakes no peer, so a pool does not take on the probe cadence of every pool
it shares an address with. The requeue interval is unchanged: a peer's identity
change adds one reconcile to a pool, it does not give the pool a faster
cadence. An index over held addresses would narrow the lookup further; it is
not worth maintaining an index over status content for a fan-out bounded by the
pools that share an address, and the map function reuses the shape the node
watch already has.

**Rejected, not resolved.** The controller reports; it does not move relays to
avoid the conflict. Picking which pool gives way is a capacity decision only the
operator can make, and the condition names both sides.

## Consequences

* A cross-pool conflict appears on the status of both pools, with
  `AddressConflict=True` and `Degraded=True`, and clears on both when either
  pool stops holding the address.
* A pool with a conflicting identity is still admitted. The rejection is in
  status, which is what makes it observable and attributable to both pools.
* The condition survives a drain on purpose: it clears when the last relay on
  the shared address is gone, which is when the ports are actually free.
* Cost is one cached List of TURNPools per reconcile of a pool that holds at
  least one relay, plus one watch whose handler only fires when a pool's held
  identities change.
* The operator's move is bounded by immutability: `relayPortRange` and
  addressing mode cannot change in place, so clearing a conflict means removing
  a declared address (`ExternalIPPool`) or creating a new pool. The condition
  message names the pool to move.
* This supersedes the "Cross-pool overlap is not validated" limitation in
  `docs/adr/0001-addressing-modes.md` and the entry in `docs/limitations.md`.
