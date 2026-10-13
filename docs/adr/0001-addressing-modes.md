# 1. Addressing modes and relay capacity

* Status: accepted
* Date: 2026-10-13

## Context

A TURN relay allocation is a public `(IP, port)` pair. Once allocated, peer
traffic sent to that pair must reach the exact process that holds the
allocation, otherwise the relay candidate the client advertised never works:
signalling succeeds, media never flows, and every pod reports healthy.

Running N relays behind one Kubernetes Service exposing one public IP therefore
does not add relay capacity. The Service load balances on the 5-tuple; a peer
that was told `203.0.113.7:52134` has no way to influence which backend
receives its packet. Capacity is bounded per public IP by the port space
(64 ports per 1/1024th, but at most ~16k usable ports in practice), not by the
number of processes.

The alternative is to give each relay process a public relay identity of its
own — an address plus a disjoint port range — and to make sure that the range's
traffic arrives at that process directly, without a load balancer.

## Decision

Relays run with `hostNetwork: true` on nodes labelled
`turnfleet.hzmi.dev/relay-node=true` and bind both their listener ports and
their relay port range on a node address. A relay is identified by
`(relayAddress, portRange)`, which is stable across restarts and controller
failover. Three addressing modes decide how many relays a node runs and where
their addresses come from:

| Mode | Relays per node | Relay address | Capacity per node |
| --- | --- | --- | --- |
| `HostNetwork` | exactly one (required anti-affinity) | the node's `ExternalIP`, or the `turnfleet.hzmi.dev/relay-address` annotation | the whole configured range |
| `PartitionedIP` | `addressing.partitionedIP.relaysPerNode` | the node address, shared | still one IP's port space: the configured range is split into disjoint slices, so **adding relays per node does not add ports on that node** |
| `ExternalIPPool` | one per address in `addressing.externalIPPool.addresses` | the declared secondary address, one per relay | addresses × the whole range |

`PartitionedIP` exists because several processes may share one IP as long as
each owns a disjoint range and receives that range's traffic. It raises
*concurrency* (more processes, more CPU) but not capacity, and the operator says
so in the status: `portsTotal` sums the slices, and `docs/capacity-model.md`
states that the sum is bounded by one IP's port space. Total port space per pool
is `relays × slice`, and the slices never overlap.

The planner moves a relay only when its node or its address disappears, and it
keeps the identity stable otherwise; a controller restart re-derives the same
identities from the same inputs (sorted node list, sorted address list, stable
hash of the identity for the pod name).

## Consequences

* No Service or load balancer is in the relay data path; the relay range never
  has to be expressed as Service ports (see ADR 3).
* Relay nodes need their listener ports and the relay range open in whatever
  firewall sits in front of them. `docs/capacity-model.md` lists exactly which
  ranges.
* `HostNetwork` capacity scales linearly with the number of labelled nodes,
  which is the intended way to scale the pool. `PartitionedIP` is for
  under-provisioned nodes that cannot be added to, not for scaling.
* Two relays in one pool can only share a relay address in `PartitionedIP`
  mode, where the port slices are disjoint by construction. Overlap between
  *different* pools cannot be validated by an admission webhook (it cannot see
  other TURNPools' addresses atomically); it is recorded as a known limitation
  in `docs/status.md`.
