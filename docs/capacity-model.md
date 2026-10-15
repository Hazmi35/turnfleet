# Capacity model

This is the document to read before scaling a pool. It explains what a relay
can actually serve, why adding pods behind one address does not help, and what
to watch when a pool runs out.

## One allocation, one port

A TURN allocation hands the client a relayed transport address: a public
`(IP, port)` pair the relay will receive peer traffic on. The relay keeps a
socket bound on that port for the lifetime of the allocation.
WebRTC clients typically request one allocation each for the session.

So the hard limit is **ports, not processes and not bandwidth**:

```
relay capacity (allocations) = ports in the relay's range
pool capacity (allocations)  = Σ over relays of that relay's range
                             = distinct relay addresses × ports per address
```

`spec.relayPortRange` is the pool's port space. A relay owning the whole range
serves `end - start + 1` concurrent allocations. coturn allocates the relay
port for *both* relay transports from that range, and its UDP and TCP port maps
are built from the same bounds. A TCP relay endpoint therefore counts against
the same capacity as a UDP one, verified against the pinned image.

The UDP port space is 65536 ports total, minus what the node needs for the
listener ports, ephemeral traffic and anything else bound on that address.
In practice operators use a few thousand to ~16k ports per address. The API
validates `1 <= start <= end <= 65535`, and the webhook rejects a range that
leaves a relay no ports.

## Why replicas behind one IP add nothing

Peer packets arrive at the relayed address from an arbitrary source
address/port. Nothing in the packet identifies which relay process the
allocation belongs to, so anything that load balances by 5-tuple (a Service, a
cloud NLB, an L4 proxy) will spray those packets across replicas. The peer's
traffic reaches the wrong process, and the relay candidate fails silently while
every pod is healthy.

Kubernetes Services cannot express a port range at all (`ServicePort` has a
single `port`; ranges exist only in NetworkPolicy). Cloud load balancers are
per-port or single-range with the same 5-tuple behaviour. The findings are in
`docs/adr/0003-service-port-ranges.md`.

The only correct scaling axis is therefore **more relay addresses**, each with
its own port range, delivered without a load balancer.

## Per addressing mode

| Mode | Relays per node | Address per relay | Ports per relay | Ports per node |
| --- | --- | --- | --- | --- |
| `HostNetwork` | 1 (required anti-affinity) | the node's `ExternalIP` (or the `turnfleet.hzmi.dev/relay-address` annotation) | the whole range | `end - start + 1` |
| `PartitionedIP` | `addressing.partitionedIP.relaysPerNode` | the node address, shared by all relays on that node | `(end - start + 1) / relaysPerNode`, remainder to the first slices | still `end - start + 1`: **splitting an IP does not create ports** |
| `ExternalIPPool` | 1 (required anti-affinity, as in `HostNetwork`) | the declared secondary address | the whole range | `end - start + 1` |
| `PodIP` | 1 | the address the pod holds on the default network or on a Multus attachment, declared per node in `turnfleet.hzmi.dev/relay-pod-addresses` | the whole range | `end - start + 1` |

Every mode but `PodIP` binds an address that belongs to the node. `PodIP` gives
the address to the pod itself, which is what makes it usable on clusters whose
nodes have no address a peer can route to (Azure Kubernetes Service with
Cilium, and Multus secondary networks). See
`docs/adr/0018-addressing-mode-podip.md`. Note that its per-address ingress
rules, routable pod network and stable IPAM are prerequisites the operator
owns. A private address is allowed: admission refuses only the addresses that
cannot work at all. Making it routable from the peers is therefore the
operator's responsibility, and the external vantage point is the check.

Growing capacity:

* `HostNetwork`: add nodes with the relay-node label. Each new node is a new
  public address and a fresh port space.
* `ExternalIPPool`: declare an address on another node and add it to
  `addressing.externalIPPool.addresses`, or declare a second address on a node
  that already runs a relay. Every relay owns its address, so a node with two
  declared addresses runs two relays. Attaching cloud secondary addresses to
  existing nodes is what this mode exists for.
* `PodIP`: declare another address on a relay node in
  `turnfleet.hzmi.dev/relay-pod-addresses`. Each declared address is one relay
  with its own port space, exactly as in `ExternalIPPool`; the routes and
  ingress rules for the new address are the operator's to add.
* `PartitionedIP`: this mode does **not** add capacity on a node. It exists so
  several relays can share an address when the address is a scarce resource.
  It gives more processes, more concurrency and per-process blast-radius
  isolation, but the sum of the slices is still one IP's port space. If you
  need more allocations, use `HostNetwork` or `ExternalIPPool`.

  Relays that share an address also need listener ports of their own. A
  listening socket binds once per address, so relay *n* on a node serves
  `spec.listeners[].port + n`. Its coturn metrics port is `9641 + n` for the
  same reason (see `docs/adr/0009-partitioned-listener-ports.md`). Those
  `relaysPerNode` consecutive listener ports must sit outside
  `spec.relayPortRange`. The webhook rejects a pool where they do not.

`status.portsTotal` sums the slices actually assigned, so a pool whose
`PartitionedIP` split is misconfigured shows the real number instead of the
declared range.

## Address families

`spec.addressing.family` picks the address family of a pool's relay addresses:
`IPv4` (the default), `IPv6`, or `DualStack`.

A family is part of an address, not a separate pool of ports. Two relay
addresses on one node are two port spaces; the same address never gains ports
from a family:

```
ports per node = distinct relay addresses on that node × ports per address
capacity       = Σ over relays of that relay's port range
```

So `DualStack` on a dual-stack node plans the mode's relay set once per family:

| Mode | `IPv4` | `IPv6` | `DualStack` on a dual-stack node |
| --- | --- | --- | --- |
| `HostNetwork` | one relay on the node's IPv4 address | one relay on its IPv6 address | two relays, one per family, each with the whole range |
| `PartitionedIP` | `relaysPerNode` slices of the IPv4 address | `relaysPerNode` slices of the IPv6 address | `relaysPerNode` slices of each address: the split is per address, and each split still sums to one port space |
| `ExternalIPPool` | one relay per declared IPv4 address | one relay per declared IPv6 address | one relay per declared address of either family |
| `PodIP` | one relay per IPv4 address declared on the node | one relay per IPv6 address declared on the node | one relay per declared address of either family |

`DualStack` therefore doubles a dual-stack node's port space, because it doubles
its relay addresses. It adds nothing to a node that has only one family. Ports
per address are the same in both families: an IPv6 address has the same
65536-port space, minus what the node needs for listeners, ephemeral traffic and
everything else bound on it.

Relays of both families on one node need distinct metrics ports, because
coturn's prometheus exporter binds every address on the node rather than the
relay's own address (`prometheus exporter server will listen on [::]:9641`).
They need distinct listener ports only when they share an address. That is why a
relay's `prometheus-port` is `9641 + its index on the node` while its listener
port is offset per address.

An IPv6 pool binds IPv6 addresses only. A client reaches a relay on the address
its own path uses, so a v6 relay has v6 peers and a v4 relay has v4 peers. The
default peer deny list follows the family for the same reason (see ADR 13).
Both the defaulting webhook and the controller's own fallback for a pool that
never passed admission (see `config/default`) pick it from `family`. A peer
range of the other family is rejected rather than kept as decoration.

A node without an address in a planned family hosts no relay there. An IPv6 pool
on IPv4-only nodes places nothing. It reports the shortfall on the `Ready` and
`Degraded` conditions, whose messages carry the planner's notes, rather than
quietly falling back to IPv4.

The `turnfleet.hzmi.dev/relay-address` annotation overrides the advertised
address of the family it names. One annotation covers one family, and the other
family keeps the node's own address. The value has to be an IP address. A value
that is not one, or that names a family the node has no address in, is reported
as a reconcile error rather than advertising an address no peer can reach.

### Checking IPv6 against a real relay

The kind node image cannot give a relay node an IPv6 address, so the IPv6 path
is not covered by `make test-e2e`. It is checked against the pinned coturn image
instead, in a throwaway container that shares no network with the cluster:

```sh
# Render a v6 configuration, point it at IPv6 loopback, and let coturn parse it.
sed -e 's/^listening-ip=.*/listening-ip=::1/' -e 's/^relay-ip=.*/relay-ip=::1/' \
  -e '/^external-ip=/d' internal/relay/testdata/hostnetwork-v6.conf > /tmp/turnserver-v6.conf
docker run -d --name tf-v6 -v /tmp/turnserver-v6.conf:/etc/turnserver.conf:ro \
  coturn/coturn:4.18.0-r0 -c /etc/turnserver.conf

# Probe it with the real probe, from the same network namespace.
docker run --rm --network container:tf-v6 -v "$PWD/bin/turnfleet-probe:/turnfleet-probe:ro" \
  coturn/coturn:4.18.0-r0 /turnfleet-probe \
  --uri 'turn:[::1]:3478?transport=udp' --secret "$SECRET" \
  --expect-address ::1 --expect-port-range 49152-49199 --json
```

The container's `/proc/net/tcp6` shows the listeners, and coturn's own log lines
(`relay ::1 initialization`, `prometheus exporter server will listen on
[::]:9641`) show what it bound. The same probe with `--peer-echo '[::1]:0'`
proves the deny list is enforced: coturn answers `403` to the permission,
because `::1/128` is in the default IPv6 deny list.

## Rejected approaches

**One address spread over several nodes.** Cloud routing matches destination
prefixes only. A port-range front end is a load balancer, whatever its shape:

* GCP: a forwarding rule per range, 50 ports per protocol on a shared internal
  VIP.
* AWS: 50 listeners per NLB, not adjustable.
* AWS Global Accelerator custom routing: no health checks or failover,
  immutable port map.

ADR 3 and ADR 4 therefore exclude that approach, and the peer-facing address
stays off the node. A floating address or an anycast prefix is one address per
slice, which is `ExternalIPPool` again. Per-flow ECMP hashes the peer's source
port, so a NAT rebind moves one peer's flow onto a node that holds no allocation
for it. See ADR 19. `PartitionedIP` remains the one way to put several relays on
one address, and it keeps them on one node.

## Running out

Exhaustion is visible in three places:

* `turnfleet_relay_ports_in_use` / `turnfleet_relay_ports_total` per relay, and
  `status.relays[].portsInUse` / `portsTotal`.
* `turnfleet_allocation_failures_total{code="508"}`: coturn answering
  `508 Insufficient Capacity` to a client `Allocate`. The probe performs a real
  allocation on every interval, so this counter moves as soon as a relay is
  full, even if no client is unlucky yet.
* The `CapacityPressure` condition, which becomes true when a relay crosses the
  configured ratio or a probe observed a 508.

Allocation counts come from coturn's own Prometheus endpoint. Its gauge is
`turn_total_allocations{type}`, summed over the socket types, and coturn's
exposition carries no `coturn_` prefix. The source of these metrics, and the
reason the per-code counters are probe-derived, are in
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

An **in-cluster probe can pass while external reachability fails**. This is the
most confusing failure in a TURN deployment:

* **Hairpin NAT.** Traffic from a pod on the relay node to the node's own
  public address may never leave the host, or may be rewritten. The probe then
  succeeds without proving anything about the path a real client takes.
* **Firewalls and security groups.** The relay range and listener ports may be
  blocked on the node's public interface; nothing inside the cluster sees that.
* **NAT mapping that is not 1:1.** If `-X` maps a public address to a private
  one and the NAT does not preserve ports, a peer's response to
  `publicIP:port` is dropped. An in-cluster check that talks to the private
  address never notices.

The **authoritative check is external**: run `turnfleet-probe` from outside the
cluster (CI, a laptop, a synthetic monitoring agent) against every URI in
`<pool>-turn-uris`. Treat its exit code as the signal users experience.

```sh
bin/turnfleet-probe --uri "turn:<relayAddress>:<listener-port>?transport=udp" \
  --secret-file ./turn-secret --expect-address <relayAddress> \
  --expect-port-range <start>-<end> --json
```

A runner can put that answer where the operator can see it, instead of leaving
it in a terminal. `--uris-configmap` probes every URI the pool publishes, and
`--publish-configmap` writes the pass into the pool's `<pool>-vantage`
ConfigMap. The controller reads that object into the pool's
`ReachableFromVantage` condition, and a failing result also raises `Degraded`:

```sh
bin/turnfleet-probe --uris-configmap <pool>-turn-uris \
  --publish-configmap <pool>-vantage --pool <pool> \
  --namespace <pool-namespace> --secret-file ./turn-secret
```

`config/vantage/runner.yaml` is that command as a CronJob, with the role that
lets it write its result and nothing else. **Running it in a pod proves the
delivery path, not the vantage point**: a pod is on the cluster's network, so it
sees exactly what the controller sees. Publish from a machine outside, and per
pool, for the answer to be authoritative
(`docs/adr/0015-vantage-result-delivery.md`).

## Firewall and network prerequisites

On every relay node, allow:

* each listener port/protocol from `spec.listeners[]` (UDP/TCP 3478, TLS 5349 by
  default), inbound;
* the relay range `spec.relayPortRange` for UDP, inbound: this is the range
  peers send to;
* outbound to the peers' networks (the resolved relay addresses), subject to
  `spec.peerPolicy`.

The rules are per address: a `DualStack` node needs them on its IPv4 and its
IPv6 address. A client that reaches the node over IPv6 is only served by the
relay bound to the IPv6 address.

Turnfleet deliberately does not create a Service or security-group rule for the
relay range: it cannot know how the node's address is exposed. When a relay
advertises a public address that differs from the address it binds, the
annotation `turnfleet.hzmi.dev/relay-address` or an `ExternalIPPool` entry
declares the advertised address. The renderer then emits `-X public/private` so
coturn advertises the public one. coturn maps one address to another within one
family, so an IPv6 relay renders an IPv6 pair.
