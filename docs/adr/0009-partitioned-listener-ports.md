# 9. Partitioned relays get their own listener ports

* Status: accepted
* Date: 2026-10-13

## Context

`PartitionedIP` runs several relays on one node address, each owning a disjoint
slice of the pool's relay port range. The relay port range is handled: the
planner partitions it and the webhook rejects a pool whose range cannot be
split.

The *listener* ports were not. `spec.listeners[].port` is one port per pool
(3478, say), and a listening socket can only be bound once per address. Two
relays on one address both asking coturn to bind `3478` means the second one
fails to start: the pool would report relays that exist on paper while serving
a fraction of the capacity it claims — the exact silent failure this project
exists to remove.

## Decision

In `PartitionedIP` mode, a relay's listener port is the pool's base port plus
the relay's slot index on its node: relay 0 serves 3478, relay 1 serves 3479,
and so on. `Relay.ListenerPortOffset` carries the slot, and the renderer, the
`turnURIs`, the probe and the pod's ports all apply it. In `HostNetwork` and
`ExternalIPPool` mode each relay owns its address, so the offset is always zero
and nothing changes.

Clients never see this: they are handed the URI of a specific relay
(`turn:203.0.113.10:3479?transport=udp`), so the offset is an implementation
detail of sharing an address. The webhook rejects a pool whose listener port
span (`port` .. `port + relaysPerNode - 1`) intersects `relayPortRange`, or
runs past 65535, which is the other half of keeping listeners and allocations
from colliding.

## Consequences

* `PartitionedIP` actually works: every relay binds its own listening port and
  the first relay to start no longer starves the rest.
* The pool's listener ports occupy `relaysPerNode` consecutive ports per
  address. That is a bounded, declared cost — the validation rule names it.
* The rendered configuration, the status URIs and the probe all derive the port
  from the relay, not from the pool, so there is one place (`Relay`) where the
  offset lives and one function (`ListenerPort`) that applies it.
* A pool that partitions an address must reserve a listener span outside its
  relay range. `docs/capacity-model.md` states it, and the webhook enforces it.
