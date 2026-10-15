# 1. Addressing modes and relay capacity

* Status: accepted
* Date: 2026-10-13

## Context

A TURN relay allocation is a public `(IP, port)` pair. Once allocated, peer
traffic sent to that pair must reach the exact process that holds the
allocation. If it does not, the relay candidate the client advertised never
works: signalling succeeds, media never flows, and every pod reports healthy.

Running N relays behind one Kubernetes Service that exposes one public IP
therefore does not add relay capacity. A Service load balances per flow, and it
cannot express a port range, so a peer that was told `203.0.113.7:52134` has no
way to influence which backend receives its packet. Capacity per public IP is
bounded by the port space, not by the number of processes: 65536 ports, of which
at most ~16k are usable in practice.

The alternative is to give each relay process a public relay identity of its
own: an address plus a disjoint port range. That range's traffic must then
arrive at the process directly, without a load balancer.

## Decision

Relays run with `hostNetwork: true` on nodes labelled
`turnfleet.hzmi.dev/relay-node=true`, and bind both their listener ports and
their relay port range on a node address. A relay is identified by
`(relayAddress, portRange)`, which is stable across restarts and controller
failover. Four addressing modes decide how many relays a node runs and where
their addresses come from. `PodIP` is the one exception to `hostNetwork: true`:
its relay address belongs to the pod, so its pod has a network namespace of its
own (ADR 18). Every other mode binds an address that belongs to the node, and
needs the host's network.

| Mode | Relays per node | Relay address | Capacity per node |
| --- | --- | --- | --- |
| `HostNetwork` | exactly one (required anti-affinity) | the node's `ExternalIP`, or the `turnfleet.hzmi.dev/relay-address` annotation | the whole configured range |
| `PartitionedIP` | `addressing.partitionedIP.relaysPerNode` | the node address, shared | still one IP's port space: the configured range is split into disjoint slices, so **adding relays per node does not add ports on that node** |
| `ExternalIPPool` | one per address in `addressing.externalIPPool.addresses` | the declared secondary address, one per relay | addresses × the whole range |
| `PodIP` | one per address declared on the node in `turnfleet.hzmi.dev/relay-pod-addresses` | the address the pod itself holds, on the default network or on a Multus attachment (`addressing.podIP.attachmentRef`) | addresses × the whole range |

`spec.addressing.family` decides which address family the addresses above are
taken from: `IPv4` (the default), `IPv6`, or `DualStack`. `DualStack` plans each
row once per family, so a dual-stack node runs two relays, one per family, and
offers two port spaces. A family is part of an address, not extra ports on one
address. See ADR 13 and `docs/capacity-model.md`.

`PartitionedIP` exists because several processes can share one IP, as long as
each one owns a disjoint range and receives that range's traffic. It raises
*concurrency* (more processes, more CPU) but not capacity, and the operator says
so in the status: `portsTotal` sums the slices, and `docs/capacity-model.md`
states that the sum is bounded by one IP's port space. Total port space per pool
is `relays × slice`, and the slices never overlap.

The planner moves a relay only when its node or its address disappears. In
every other case it keeps the identity stable. A controller restart re-derives
the same identities from the same inputs: the sorted node list, the sorted
address list, and a stable hash of the identity for the pod name.

## Consequences

* No Service or load balancer is in the relay data path, so the relay range
  never has to be expressed as Service ports (see ADR 3).
* Relay nodes need their listener ports and the relay range open in whatever
  firewall sits in front of them. `docs/capacity-model.md` lists exactly which
  ranges.
* `HostNetwork` capacity scales linearly with the number of labelled nodes,
  which is the intended way to scale the pool. `PartitionedIP` is for
  under-provisioned nodes that cannot be added to, not for scaling.
* Two relays in one pool can only share a relay address in `PartitionedIP`
  mode, where the port slices are disjoint by construction. Overlap between
  *different* pools cannot be validated by an admission webhook, because it
  cannot see other TURNPools' addresses atomically. The reconciler therefore
  compares the identities that pools hold and reports every overlap as a
  condition on the pools involved: see
  `docs/adr/0012-cross-pool-address-conflict.md`. Admission itself is
  unchanged.
