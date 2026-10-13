# Capacity model

This is the document to read before scaling a pool. It explains what a relay
can actually serve, why adding pods behind one address does not help, and what
to watch when a pool runs out.

## One allocation, one port

A TURN allocation hands the client a relayed transport address: a public
`(IP, port)` pair the relay will receive peer traffic on. The relay keeps a
socket bound on that port for the lifetime of the allocation, and WebRTC
clients typically request one allocation each for the session.

So the hard limit is **ports, not processes and not bandwidth**:

```
relay capacity (allocations) = ports in the relay's range
pool capacity (allocations)  = Σ over relays of that relay's range
                             = distinct relay addresses × ports per address
```

`spec.relayPortRange` is the pool's port space. A relay owning the whole range
serves `end - start + 1` concurrent allocations. coturn allocates the relay
port for *both* relay transports from that range (its UDP and TCP port maps are
built from the same bounds), so a TCP relay endpoint counts against the same
capacity as a UDP one — verified against the pinned image.

The UDP port space is 65536 ports total, minus what the node needs for the
listener ports, ephemeral traffic and anything else bound on that address. In
practice operators use a few thousand to ~16k ports per address; the API
validates `1 <= start <= end <= 65535` and the webhook rejects a range that
leaves a relay no ports.

## Why replicas behind one IP add nothing

Peer packets arrive at the relayed address from an arbitrary source
address/port. Nothing in the packet identifies which relay process the
allocation belongs to, so anything that load balances by 5-tuple — a Service, a
cloud NLB, an L4 proxy — will spray those packets across replicas. The peer's
traffic reaches the wrong process and the relay candidate fails silently while
every pod is healthy.

Kubernetes Services cannot express a port range at all (`ServicePort` has a
single `port`; ranges exist only in NetworkPolicy), and cloud load balancers
are per-port or single-range with the same 5-tuple behaviour. The findings are
in `docs/adr/0003-service-port-ranges.md`.

The only correct scaling axis is therefore **more relay addresses**, each with
its own port range, delivered without a load balancer.

## Per addressing mode

| Mode | Relays per node | Address per relay | Ports per relay | Ports per node |
| --- | --- | --- | --- | --- |
| `HostNetwork` | 1 (required anti-affinity) | the node's `ExternalIP` (or the `turnfleet.hzmi.dev/relay-address` annotation) | the whole range | `end - start + 1` |
| `PartitionedIP` | `addressing.partitionedIP.relaysPerNode` | the node address, shared by all relays on that node | `(end - start + 1) / relaysPerNode`, remainder to the first slices | still `end - start + 1` — **splitting an IP does not create ports** |
| `ExternalIPPool` | one per declared address | the declared secondary address | the whole range | `addresses on that node × (end - start + 1)` |

Every mode here binds a node address. A fourth mode for routable, stable pod or
secondary addresses is listed in `docs/status.md`.

Growing capacity:

* `HostNetwork`: add nodes with the relay-node label. Each new node is a new
  public address and a fresh port space.
* `ExternalIPPool`: attach more secondary addresses to existing nodes (cloud
  elastic IPs) and add them to `addressing.externalIPPool.addresses`.
* `PartitionedIP`: this mode does **not** add capacity on a node. It exists so
  several relays can share an address when the address is a scarce resource —
  more processes, more concurrency and per-process blast-radius isolation, but
  the sum of the slices is still one IP's port space. If you need more
  allocations, use `HostNetwork` or `ExternalIPPool`.

  Relays that share an address also need listener ports of their own: a
  listening socket binds once per address, so relay *n* on a node serves
  `spec.listeners[].port + n`, and its coturn metrics port is `9641 + n` for the
  same reason (see `docs/adr/0009-partitioned-listener-ports.md`). Those
  `relaysPerNode` consecutive listener ports must sit outside
  `spec.relayPortRange`, and the webhook rejects a pool where they do not.

`status.portsTotal` sums the slices actually assigned, so a pool whose
`PartitionedIP` split is misconfigured shows the real number instead of the
declared range.

## Running out

Exhaustion is visible in three places:

* `turnfleet_relay_ports_in_use` / `turnfleet_relay_ports_total` per relay, and
  `status.relays[].portsInUse` / `portsTotal`.
* `turnfleet_allocation_failures_total{code="508"}` — coturn answering
  `508 Insufficient Capacity` to a client `Allocate`. The probe performs a real
  allocation on every interval, so this counter moves as soon as a relay is
  full, even if no client is unlucky yet.
* The `CapacityPressure` condition, which becomes true when a relay crosses the
  configured ratio or a probe observed a 508.

Allocation counts come from coturn's own Prometheus endpoint
(`coturn_turn_total_allocations{type}` is a gauge of current allocations); the
source and the reason per-code counters are probe-derived are in
`docs/adr/0005-metrics-source.md`.

**Saturation is per relay, not per pool.** When one relay is full, clients
handed a URI for it fail while every other relay still serves. That is why
`turnURIs` only lists relays that are ready and not draining, and why the
issuer must re-read it instead of caching a URI list.

## Probe vantage points

The controller probes each relay from inside the cluster every
`spec.probe.interval` (default 10s, timeout 3s). It performs a real TURN
`Allocate` with credentials derived from the shared secret and asserts that
`XOR-RELAYED-ADDRESS` matches the relay's expected address and range. It catches
wrong address, wrong range, wrong secret, missing realm, misconfigured listeners
and capacity exhaustion.

An **in-cluster probe can pass while external reachability fails**, and this is
the most confusing failure in a TURN deployment:

* **Hairpin NAT.** Traffic from a pod on the relay node to the node's own
  public address may never leave the host (or may be rewritten), so the probe
  succeeds without proving anything about the path a real client takes.
* **Firewalls and security groups.** The relay range and listener ports may be
  blocked on the node's public interface; nothing inside the cluster sees that.
* **NAT mapping that is not 1:1.** If `-X` maps a public address to a private
  one and the NAT does not preserve ports, a peer's response to
  `publicIP:port` is dropped. An in-cluster check that talks to the private
  address never notices.

The **authoritative check is external**: run `turnfleet-probe` from outside the
cluster (CI, a laptop, a synthetic monitoring agent) against every URI in
`<pool>-turn-uris`, and treat its exit code as the signal users experience.

```sh
bin/turnfleet-probe --uri "turn:<relayAddress>:<listener-port>?transport=udp" \
  --secret-file ./turn-secret --expect-address <relayAddress> \
  --expect-port-range <start>-<end> --json
```

## Firewall and network prerequisites

On every relay node, allow:

* each listener port/protocol from `spec.listeners[]` (UDP/TCP 3478, TLS 5349 by
  default), inbound;
* the relay range `spec.relayPortRange` for UDP, inbound — this is the range
  peers send to;
* outbound to the peers' networks (the resolved relay addresses), subject to
  `spec.peerPolicy`.

Turnfleet deliberately does not create a Service or security-group rule for the
relay range: it cannot know how the node's address is exposed. When a relay
advertises a public address that differs from the address it binds, the
annotation `turnfleet.hzmi.dev/relay-address` or an `ExternalIPPool` entry
declares the advertised address, and the renderer emits `-X public/private` so
coturn advertises the public one.
