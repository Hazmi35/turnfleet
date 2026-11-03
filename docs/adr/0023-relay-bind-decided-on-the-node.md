# 23. A relay on a node with two addresses decides on the node which one it binds

* Status: accepted
* Date: 2026-11-03
* Amends [0001](0001-addressing-modes.md) and
  [0013](0013-addressing-family.md) on which node address a relay binds. Both
  said a relay on a node binds the node's `ExternalIP`, falling back to its
  `InternalIP`. Everything else they decide stands: the modes, the identity
  `(address, port range)`, and one relay per family per node.

## Context

`HostNetwork` and `PartitionedIP` plan relays on node addresses. The controller
took one address per family, the `ExternalIP` or the `InternalIP` when there was
no `ExternalIP`, and used it as both the address the relay advertises and the
address it binds.

That is wrong on a cloud that gives a node a public address by 1:1 NAT: the
instance holds only its private address, and the provider translates the
`ExternalIP`. This was seen on EKS 1.37 in `ap-southeast-1`, with nodes in a
public subnet (the #11 spike,
<https://github.com/Hazmi35/turnfleet/issues/11#issuecomment-6600851975>). The
node reported `InternalIP=10.42.0.134` and `ExternalIP=13.212.26.172`. The sample
pool rendered `listening-ip=13.212.26.172` and `relay-ip=13.212.26.172`, and
coturn retried forever:

```text
Cannot bind DTLS/UDP listener socket to addr 13.212.26.172:3478. Error: Cannot assign requested address
```

The pool reported the relay `ready: false` with `AllocationRefused`, and the
relay pod reported `Ready=True`. The same node served a full relayed round trip
from outside AWS with a hand-run coturn that bound the private address and
advertised the public one:

```text
listening-ip=10.42.0.134
relay-ip=10.42.0.134
external-ip=13.212.26.172/10.42.0.134
```

The renderer already emits that mapping when the advertised and the bound
address differ (`writeAddressing` in `internal/relay/render.go`, golden file
`hostnetwork-nat.conf`). The `turnfleet.hzmi.dev/relay-address` annotation did not
help: it replaces the advertised address only.

The opposite case is as common. DigitalOcean, Linode, Vultr and Hetzner Cloud
nodes, most bare-metal nodes, and k3s with `--node-external-ip` on a public
interface hold the `ExternalIP` on an interface (the maintainer's own k3s
cluster is one: `InternalIP` 10.110.0.x, the public `ExternalIP` on `eth0`).
Binding the `InternalIP` there would leave coturn listening on the private
address only, which no client reaches. The Node object has the same two
addresses in both cases, so no rule over the Node object alone is right for
both.

## Decision

**When a node has an `InternalIP` and an `ExternalIP` in a family and they
differ, the relay advertises the `ExternalIP` and the relay pod decides what it
binds when it starts.** The `ExternalIP` is bound if it is assigned to an
interface of the node that is up, with no `external-ip` mapping. Otherwise the
`InternalIP` is bound and `external-ip=<ExternalIP>/<InternalIP>` maps one to
the other. With one address in a family, or two equal ones, the relay binds and
advertises it, as before. The rule is per family, so a dual-stack node can be
NATed in IPv4 and not in IPv6. It applies to `HostNetwork` and `PartitionedIP`,
the modes that plan on node addresses.

The mechanics, for a relay that needs the decision:

* The controller renders two configurations, one per case, and stores both in
  the pool's relay Secret under `<relay>.local.conf` and `<relay>.mapped.conf`.
  Both go into the config hash, so a change to either rolls the relay.
* The pod gets an init container that runs `turnfleet-controller relay-init`,
  from the controller's own image. It lists the node's interfaces with
  `net.Interfaces()`, which needs no privilege, copies the matching
  configuration to an in-memory `emptyDir` coturn reads, and writes the bind
  address it chose to its termination message. It fails, with the reason in
  that message, when the node holds neither address.
* The controller reads the termination message from the pod status, records the
  address in the existing pod annotation `turnfleet.hzmi.dev/relay-bind-address`,
  and emits a `RelayBindDetected` event. It needs no new RBAC.
* The image is the controller's own, found by reading its own pod (the manager
  container, by the hostname and the namespace file the service account
  mounts). `--relay-helper-image` overrides it, for a controller that runs
  outside a pod. A relay that needs the decision is refused with an error that
  names the flag and the node annotation when the controller knows neither.
* The dataplane image stays the user's pinned coturn. Nothing runs in it but
  `turnserver`: it has no shell and no `ip`.
* Both configurations also set `listening-ip` to the loopback address of the
  relay's family. A pod's TCP readiness probe is fixed when the pod is created,
  before the choice, so it probes loopback, and the probe is the same in both
  cases. `relay-ip` stays the bound node address, so nothing is relayed through
  loopback. A relay that does not need the decision keeps its configuration and
  its probe on its bind address, unchanged.

**The node annotation `turnfleet.hzmi.dev/relay-bind` skips the decision.** Its
value is `InternalIP` or `ExternalIP`. `InternalIP` binds the `InternalIP` and
advertises the `ExternalIP`. `ExternalIP` binds and advertises the `ExternalIP`.
Any other value is a reconcile error that names the node, the way an unusable
`turnfleet.hzmi.dev/relay-address` is. Absent means decide. The name is not
`turnfleet.hzmi.dev/relay-bind-address`: that is the pod annotation above.

**`turnfleet.hzmi.dev/relay-address` keeps replacing the advertised address of
its family and nothing else.** The comment on the annotation said it also bound
the address, which was never true. The bind decision is made against the node's
`ExternalIP`, and the override is what coturn then maps.

The node watch compares the planned addresses, so a change to either annotation
that changes the plan re-plans the node.

### Rejected

* **Bind the `InternalIP` by default, with an annotation for a node that holds
  its `ExternalIP` (option 1 in #88).** It fixes every 1:1 NAT cloud and breaks
  every node that holds its public address, silently and on upgrade: the relay
  starts, listens on the private address, and nobody reaches it. The pod is
  `Ready` and only the pool status shows it. That is the failure this decision
  removes, moved to other nodes.
* **Keep binding the `ExternalIP` by default, with an annotation for NAT (option
  2 in #88).** Nothing existing changes, but a relay on every NAT cloud is broken
  until its operator finds the annotation, and the annotation shares its name
  with the pod annotation.
* **Guess from the Node object.** Provider IDs, address ranges or labels could
  stand in for the interfaces, with a table per cloud to keep current. The
  interfaces are the fact, and the pod can read them.
* **Test whether the `ExternalIP` can be bound.** A node with
  `net.ipv4.ip_nonlocal_bind=1` binds an address it does not hold, so the test
  would pass on the node it must fail on.
* **Listen on the `InternalIP` too, so the probe has one target.** The listener
  would then also exist on the private address of a node that holds its
  `ExternalIP`, which is new exposure on a node whose behaviour should not
  change. Loopback is reachable only from the node.
* **Probe by `exec`, from a copy of the helper in the relay container.** It runs
  a Go binary every few seconds in the data plane container, from a volume that
  a hardened node may mount `noexec`.
* **A flag in the manifests for the helper image.** `make deploy` sets the
  controller image with `kubectl set image`, which changes the container's image
  and no argument or environment value, and the chart would need the same value
  spelled twice. The controller reading its own pod cannot drift from the image
  it runs.
* **Detect in the controller.** It does not run in the relay's network
  namespace, so it cannot see the node's interfaces.

## Consequences

* A `HostNetwork` or `PartitionedIP` relay on a 1:1 NAT node starts serving
  without configuration, and a relay on a node that holds its `ExternalIP`
  binds it as before. Both need no annotation.
* **Upgrade effect.** A relay on a node whose `ExternalIP` and `InternalIP`
  differ and that has no `relay-bind` annotation gains the init container, the
  candidates and the loopback listener, so its pod spec and config hash change.
  It rolls once, under drain, one relay at a time (ADR 21). Its identity
  `(address, port range)` does not change, because the advertised address is
  the `ExternalIP` in both cases, so no `turnURIs` entry changes. A node that
  holds its `ExternalIP` binds and advertises what it did before. A NATed node
  goes from broken to working. A relay on a node with one address per family,
  equal addresses, or `relay-bind` set hashes as it did and does not roll
  (a test pins the value).
* A relay pod on such a node has an init container, and the node has to be able
  to pull the controller's image for it. The pod's memory-backed `emptyDir`
  holds the configuration, which embeds the shared secret, so it never reaches
  the node's disk.
* The relay listens on loopback as well. Any process on the node can reach that
  listener, and credentials still apply.
* A relay on a node that holds neither address cannot start: the init container
  fails and the pod does not become ready.
* `kind` nodes report one address, and the kubelet owns `status.addresses`, so
  `make test-e2e` cannot give a node an `ExternalIP` that stays. The
  decision is covered by unit tests of the helper with a fixed interface list,
  by envtest tests of the pod the controller builds and what it does with the
  init container's report, and by a manual run of the helper and the pinned
  coturn on a node that holds its address.
* The pod readiness gap stays: a relay pod reports `Ready=True` while coturn
  cannot bind, when it has no TCP listener to probe, and the pool status is the
  only signal. It is a separate issue.
* The ExternalIPPool text that said an entry can declare an advertised address
  other than the bound one was wrong, because `ExternalAddress.address` is both.
  That belongs to #11.
