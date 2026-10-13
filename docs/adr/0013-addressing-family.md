# 13. Address families: IPv4, IPv6 and DualStack

* Status: accepted
* Date: 2026-10-13

## Context

A TURN allocation is a public `(IP, port)` pair, and peers send to that pair. A
relay on an IPv4 address is unreachable from an IPv6-only network and the other
way round, so a pool can only serve clients whose path to the relayed address
uses a family the relay has an address in. Every relay planned before this
decision bound the node's IPv4 address.

The addressing modes decide how many relays a node runs and where their
addresses come from (ADR 1), but they said nothing about the family of those
addresses. The obvious extension — one process per node listening on both
families — does not fit the model: a relay is one process owning one
`(address, port range)` identity, its rendered configuration binds one relay
address, and coturn's `-X`/`-E` mapping is defined per family ("that ip will be
reported as relay IP address of all allocations of its own address family", from
the pinned image's `--help`).

## Decision

`spec.addressing.family` selects the address family of a pool's relay
addresses: `IPv4` (the default), `IPv6`, or `DualStack`.

* The family is part of an address, not an axis of capacity of its own. A node's
  IPv4 and IPv6 addresses are two relay addresses with two port spaces; a second
  family on one address would add no ports at all.
* `DualStack` plans the mode's relay set once per family. On a node that has
  both addresses that is one relay identity per family: one relay per family in
  `HostNetwork`, one split of the port range per family in `PartitionedIP`, and
  one relay per declared address of each family in `ExternalIPPool`.
* The family is immutable, exactly like `mode`, and for the same reason:
  changing it changes the address, or half the addresses, of every relay, and
  every allocation is bound to one. The webhook rejects the change; the correct
  move is a new pool.
* An address's family is read from the address, never declared next to it, so
  the two can never disagree. A declared `externalIPPool.addresses` entry of a
  family the pool does not plan is rejected rather than left unroutable, and a
  `DualStack` pool must declare at least one address per family.
* Peer ranges get the same treatment: a deny or allow range of a family the
  relays cannot reach is rejected, because it would read as policy while denying
  nothing.
* The default peer deny list follows the family. An IPv6 pool gets the IPv6
  ranges — unspecified, loopback, link-local `fe80::/10`, unique local
  `fc00::/7`, multicast, and `::ffff:0:0/96`, the whole IPv4-mapped space, which
  covers the metadata address in its mapped form `::ffff:169.254.169.254`. A
  `DualStack` pool gets both lists.
* Node addresses are the node's own addresses, one per family (`ExternalIP`,
  falling back to `InternalIP`). The `turnfleet.hzmi.dev/relay-address`
  annotation overrides the advertised address of the family it names. A node
  without an address in a planned family hosts no relay there, and the planner
  reports the replicas it could not place instead of falling back to the other
  family.

An unset `family` is `IPv4`, which is what the CRD default and the defaulting
webhook set, so every pool that existed before this field keeps planning exactly
the relays it planned before, with the same identities.

## Consequences

* `DualStack` runs two relays on a dual-stack node, and they need distinct
  metrics ports: coturn's prometheus exporter does not bind the relay's own
  address, it binds every address on the node ("prometheus exporter server will
  listen on [::]:9641", verified against the pinned image). Metrics ports are
  therefore offset per node, while listener ports stay offset per address — two
  relays on one address cannot both bind one listening port, two relays on
  different addresses of one node can.
* Two relays of one node also mean the required pod anti-affinity has to be
  relaxed for `DualStack`, the way `PartitionedIP` already relaxes it. The rule
  exists to keep two relays from competing for one address's port range, and two
  families are two addresses.
* A `hostNetwork` pod has one pod IP on a dual-stack node, so the readiness
  probe targets the relay's own bind address rather than the pod IP. The same
  change fixes a NATed relay, whose pod IP is never the address it advertises.
* Published URIs bracket an IPv6 authority (`turn:[2001:db8::1]:3478`) as
  RFC 7065 requires; the probe's URI parser rejects an unbracketed IPv6
  authority instead of guessing where the port starts.
* No Service or load balancer appears in either family's path, and none is
  added. Each family's relay range must be reachable directly, exactly as for
  IPv4, and an IPv6 relay node needs a routable IPv6 address and route that the
  operator does not create.
* A kind node cannot provide an IPv6 address, so the IPv6 data path is proven
  against the pinned coturn image on `::1` rather than in the end-to-end suite;
  the commands are in `docs/capacity-model.md`.
