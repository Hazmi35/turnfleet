# 18. The PodIP addressing mode

* Status: accepted
* Date: 2026-10-14

## Context

`HostNetwork` binds the node's `ExternalIP` (or its
`turnfleet.hzmi.dev/relay-address` override) and `ExternalIPPool` binds
secondary addresses declared in the pool. Both assume a relay address that
belongs to the node and is reachable from the peers' networks on its own.

There is a whole class of clusters where that is false. Azure Kubernetes
Service with Cilium is the named example (issue #15): nodes have no public
address, inbound traffic arrives only through a load balancer, and outbound
goes through a NAT gateway. A `hostNetwork` relay falls back to the node's
`InternalIP`, advertises an RFC 1918 address no peer can route to, allocates
and probes healthy in-cluster, and works for nobody. Pod addresses, on the
other hand, can be public there: Cilium can allocate pod IPs from a public
prefix, and a Multus secondary interface (`azure-ipam`, SR-IOV, macvlan) can
hand the pod an address with its own IPAM.

What such a relay needs is its **own** address, held by the pod rather than by
the node, with no Service and no load balancer in the path. A load balancer in
front of one public address is the shape this project removes (ADR 1, ADR 3,
ADR 4).

## Decision

`spec.addressing.mode: PodIP` runs one relay per address reserved for the
pool, in a pod with `hostNetwork: false`, and the relay binds and advertises
the address the pod itself holds.

**Where the address comes from.** The operator declares the reserved addresses
**on the relay node**, in the annotation

```
turnfleet.hzmi.dev/relay-pod-addresses: "203.0.113.21/24, 203.0.113.22/24"
```

one entry per relay, each optionally with the prefix length its attachment
should ask its IPAM for. The declaration is per node and not in the pool spec,
and that is what separates this mode from `ExternalIPPool`. There the address
must already be attached to the node and the pool only names it; here the
operator reserves an address for a pod, and the pod is where it lands. The
operator owns the IPAM: the operator attaches the addresses, routes them and
opens the ingress rules, so the operator declares them.

**Why the address is declared, not observed.** A relay's identity is
`(address, portRange)` and its pod name is the hash of that identity. An
address that only became known after scheduling would leave the pod unnameable,
so the address has to exist before the pod does. The declaration is also what
keeps the identity stable: the same node and the same declaration always plan
the same relays.

**How the pod receives it.** `addressing.podIP` names the Multus
`NetworkAttachmentDefinition` the pod attaches, in the pool's namespace or as
`namespace/name`, through `attachmentRef`. The block is immutable, like `mode`
and `family`, and so is the reference. Omitting the block is how a pool says the
relay uses the pod's own address on the cluster's default network, and that zero
value is the mode's only other spelling. The block cannot be present and name
nothing. Its one field must be set whenever the block is, a one-field
`AtLeastOneOf`, so `podIP: {}` is refused; and the reference must be non-empty,
so `podIP: {attachmentRef: ""}` is refused too. The controller asks for the
declared address in the pod's Multus annotation, which is what an IPAM that
honours a request needs:

```
k8s.v1.cni.cncf.io/networks: '[{"name":"relay-pods","ips":["203.0.113.21/24"]}]'
```

**Why the attachment's IPAM is not read instead.** Reading the address out of
the `NetworkAttachmentDefinition` cannot portably yield the address a given pod
will own: `azure-ipam` and SR-IOV attachments declare no addresses at all, and
a macvlan attachment may use a `host-local` or `whereabouts` range whose
allocation depends on the node and on what is already taken. So the address the
operator declares is the planning input, and the pod's own report is the check.

**The read-back.** The controller reads the address the scheduled pod actually
holds: from the attachment's entry in Multus'
`k8s.v1.cni.cncf.io/network-status` when the pool names an attachment, and from
`status.podIP`/`status.podIPs` when it does not (the pod's primary address is
never the attachment's). A pod that came up on another address is drained
(ADR 6) rather than re-advertised: silently moving the relay would strand every
allocation bound to the old address. The drain is observable: a
`RelayAddressChanged` warning event names both addresses, and the relay leaves
`turnURIs` in the same pass.

**Two halves of one rule.** Admission checks the declared addresses and the
attachment against the cluster state, but node annotations change after
admission, so the reconciler re-checks exactly the same rules on every pass.
An address that is not individually routable is dropped from the plan and
reported as the `PodAddressUnusable` condition (with `Degraded`), for the same
reason ADR 12 reports a cross-pool overlap from the reconciler rather than
trusting the webhook.

**Capacity is addresses × ports**, exactly as in `ExternalIPPool`: every relay
owns the whole relay port range on its own address, and `status.portsTotal`
keeps its meaning. The mode adds no capacity behind one address, and it never
can.

What the mode forbids:

* A Service, Ingress or cloud load balancer in the relay data path, as in every
  other mode.
* An address shared by two relays: the declaration is per node and each entry
  is one relay, and two nodes declaring the same address is a planner note and
  an unnecessary collision.
* Changing `attachmentRef` in place: a different attachment changes which
  address every relay holds, so it strands live allocations exactly as changing
  `mode` or `family` does. `mode`, `family` and the attachment are immutable;
  a new pool is the move.
* Addresses that cannot work at all: unspecified, loopback, link-local unicast
  and multicast. A peer could not reach them even with every firewall open. A
  private address (RFC 1918), a carrier-grade NAT address (RFC 6598) or an IPv6
  unique local address (RFC 4193) is **not** refused. Whether the peers route it
  is the operator's judgement (a hub-and-spoke cluster whose pod CIDR is peered
  while the node subnet is not routes one), and the external vantage point
  (ADR 7) is what proves it.

## Consequences

* Prerequisites the operator owns and this operator does not create:
  per-address ingress rules (NSG or security group), a routable pod network,
  and reserved or static IPAM. Without stable IPAM the pod comes up on another
  address and the relay drains in a loop: the events and the
  `PodAddressUnusable` condition are what makes that visible, and the fix is
  IPAM, not the operator.
* An IPAM that ignores the `ips` request (azure-ipam, SR-IOV with
  `host-local`) works only where its own assignment happens to be stable: the
  read-back then confirms it or drains the relay. An IPAM that honours the
  request (static IPAM, whereabouts) gets the declared address by construction.
* The declaration is node-wide, so two `PodIP` pools with relays on one node
  collide on the addresses offered there; the cross-pool check (ADR 12) reports
  it, because the identities carry the address.
* Kind as `make kind-up` configures it has no Multus and no secondary address
  for a pod, so this mode's data path is not exercised by `make test-e2e` yet.
  It is proven by unit, envtest and the renderer's golden files, and its data
  path shares the flag set ADR 8 verified against the pinned image. That is a
  gap in the local cluster, not in the mode. #19 tracks the fixture (Multus
  plus a macvlan attachment, whose host side the suite already probes for
  `ExternalIPPool` in `test/fixtures/attach-secondary-address.sh` and
  `hostProbe`) that would let the suite exercise it.
* Admission remains advisory for the node-declared half of the rule, and the
  reconciler is what enforces it. The vantage point (ADR 7) remains the only
  proof that the address is reachable from outside.

### Verified against the CNI's own documentation

* Multus' network selection element carries a per-pod IP request:
  `NetworkSelectionElement.IPRequest []string json:"ips,omitempty"` is copied
  into `RuntimeConfig.IPs`, the CNI `ips` capability
  (`k8snetworkplumbingwg/multus-cni`, `pkg/types/types.go`).
* The static IPAM plugin declares that capability and expects a prefixed
  address: "`ips`: Pass IP addresses for CNI interface … A list of custom IPs
  to attempt to allocate, with prefix (e.g. '10.10.0.1/24')" (`cni.dev`,
  plugins/ipam/static). This is why a declaration may carry a prefix and why
  the prefix is stripped from the relay identity.
* Where the read-back comes from: Multus publishes the addresses it attached in
  the pod's `k8s.v1.cni.cncf.io/network-status` annotation, an array of
  `{"name": …, "interface": …, "ips": […], …}` entries where the default
  network is one entry of its own (`"name": "cbr0", "default": true`). That is
  why the attachment's address is never read from `status.podIP`
  (`k8snetworkplumbingwg/multus-cni`, `docs/quickstart.md`).
* Whereabouts is built on the static IPAM plugin ("I utilized his static CNI
  IPAM plugin as a basis for this project", `k8snetworkplumbingwg/whereabouts`
  README) and allocates from a range tracked cluster-wide, which is the
  reserved-address form the mode relies on when the attachment carries no
  explicit address.
