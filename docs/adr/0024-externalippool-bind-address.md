# 24. An ExternalIPPool entry declares the address its relay binds apart from the one it advertises

* Status: accepted
* Date: 2026-11-03
* Amends [0001](0001-addressing-modes.md) on `ExternalIPPool`, which bound and
  advertised one declared address. Builds on [0023](0023-relay-bind-decided-on-the-node.md),
  which made the same split for the node addresses of `HostNetwork` and
  `PartitionedIP`. The identity `(address, port range)`, the drain rules and the
  conflict check of ADR 12 stand.

## Context

A public address on a managed cloud is a 1:1 NAT address that the instance does
not hold: an Elastic IP on AWS, a public IP on a NIC IP configuration on Azure,
an external IP on an access config on GCP, a reserved IP behind its anchor
address on DigitalOcean. The instance holds a private address, and the network
translates the public one to it.

`ExternalAddress.address` was both the address coturn binds and the address it
advertises in `XOR-RELAYED-ADDRESS`. A pool could therefore not name such an
address: coturn failed with `Cannot assign requested address`. The #11 spike on
EKS showed the data path works once coturn binds the private address and
advertises the public one (`external-ip=public/private`), and the renderer
already emits that mapping whenever a relay's advertised and bound address
differ (`writeAddressing`, ADR 23). `docs/capacity-model.md` claimed an entry
could declare a separate advertised address, which was wrong.

## Decision

**An entry has an optional `bindAddress`.** It is the local address the relay
binds when `address` is an address the node does not hold. The relay advertises
`address` in both cases. Absent `bindAddress`, the relay binds `address`, as
before, so no existing pool changes.

```yaml
addressing:
  mode: ExternalIPPool
  externalIPPool:
    addresses:
      - nodeName: ip-10-0-0-10
        address: 203.0.113.20      # the Elastic IP, advertised
        bindAddress: 10.0.0.20     # a private address on the node, bound
```

The name follows the glossary term "Bind address" (ADR 23). The list key stays
`address`, so one public address is still one entry.

**The relay's identity stays `(address, port range)`.** The planner sets the
relay's `Address` to `address` and its `BindAddress` to `bindAddress`, or to
`address` when it is absent. The relay name, the `turn:` URIs, `status.relays[]`
and the conflict check of ADR 12 are all keyed on the advertised address, so a
client is never told the private address.

**The pair is validated twice, because the webhook is optional.** The CRD
carries CEL rules on the entry, which the API server evaluates on every install,
including `config/default`, which deploys no webhook. The validating webhook
repeats them with clearer messages and adds the check across entries, which CEL
cannot do without a bound on the list. When `bindAddress` is set it must be:

* an IP address in its plain spelling: no surrounding space, no zone and no
  IPv4-in-IPv6 form (what CEL's `isIP` accepts), at most 45 characters;
* of the same family as `address`, because coturn maps an advertised address to
  a bound one only within a family;
* not unspecified, loopback, link-local or multicast, the set that
  `relay.IndividuallyRoutable` refuses for a `PodIP` address (CEL:
  `isGlobalUnicast`). A private address is the expected value and is allowed;
* different from `address`. An equal value is **rejected, not normalised**: the
  spec would have two spellings of "the node holds this address", and the API
  has one, which is to leave `bindAddress` out. A webhook that rewrites it would
  also make the stored spec differ from the manifest the author applied;
* unique per node among the effective bind addresses (the webhook only) of the pool's entries (the
  `bindAddress`, or the `address` when there is none). Two public addresses
  cannot translate to one private address, and two relays on one node cannot
  bind one local address and listener port. The key includes the node because a
  private address is unique only inside its network, so two nodes may hold the
  same one.

The CRD gives `bindAddress` a minimum length of one, so an empty string is
rejected by the schema and absence means "bind `address`". The CEL library the
rules use (`isIP`, `ip()`, `isGlobalUnicast`) is in Kubernetes 1.31 and later,
and the repository targets 1.37 (the envtest control plane and the kind node
image).

**The planner skips an entry it cannot render.** An object can reach the
controller without either check, for instance one stored before the rules
existed. The renderer refuses a `bindAddress` that is not an IP address or is of
another family than `address`, and a render error stops the whole pool's
reconcile. The planner therefore skips such an entry, and one whose
`bindAddress` is unspecified, loopback, link-local or multicast, with a note
that reaches the pool's Ready condition while the pool is short of relays. The
other entries keep their relays, as they do beside an `address` that is not an
IP address.

**Nothing checks that the node holds `bindAddress`.** The webhook cannot see a
node's interfaces. The node's `status.addresses` is reported by the kubelet and
does not list a secondary address. A relay on a node that does not hold it
fails to bind, as an `ExternalIPPool` relay on a missing address always did.

**Mutability.** `bindAddress` is not part of the identity and is not in the
immutability check: no allocation is bound to it, and a client only ever sees
`address`. Changing it changes the rendered configuration, so the config hash
changes and the relay is replaced through the normal rollout (ADR 21). The
rollout is not a new relay: the pod keeps its name, the URIs do not change, and
it happens one relay at a time. It is a replacement, so while it drains the
relay leaves `turnURIs` and its allocations are waited for, up to
`drainTimeout`, as for any configuration change. Adding or removing the field
on an entry rolls its relay the same way.

**No init container.** The relay of an entry states its bind address, so the
node is not asked which address it holds (ADR 23). The pod records
`bindAddress` in the `turnfleet.hzmi.dev/relay-bind-address` annotation, and its
readiness probe connects to it. A node with an `InternalIP` and a different
`ExternalIP` does not change that: an entry never reads the node's addresses.

**Two pools that bind one private address are not reported.** ADR 12 compares
the relay identities pools hold, and a pool's status carries the advertised
address of a relay, not its bind address. Reporting this conflict would need a
new status field for a state that is also visible where it matters: the relay
that loses cannot bind, and its probe fails. The webhook already refuses the
case inside one pool. `docs/limitations.md` records the gap.

## What the operator still owns on a cloud

`bindAddress` is the declaration. The operator has to make it true, and the
operator does so before the pool is created. The controller does not attach,
detach or configure addresses (#89 tracks a provider path, and #90, #91 and #92
are the AWS, Azure and GCP sub-issues). The #11 spike on EKS is the worked
example of what that includes:

* **Attaching the public address** to the instance, and the private address it
  maps to.
* **An interface the CNI does not manage.** The AWS VPC CNI fills a node's
  interfaces with pod addresses. A relay address goes on a separate interface
  that the CNI leaves alone (`node.k8s.amazonaws.com/no_manage=true`), and an
  instance type has a limit on how many it can have.
* **The operating system's configuration of that interface**: bringing it up,
  giving it the address, and a routing rule so traffic from that address leaves
  by it.
* **An exemption from outbound NAT.** The VPC CNI rewrites traffic that leaves
  the VPC to the node's main address. Without an exemption for the relay
  address, replies leave from the wrong address and the round trip fails.
* **The firewall**: the listener port and the relay range open to the peers.

## The NAT in the e2e suite

kind nodes hold no public address, so `make test-e2e` stands in for the cloud
with the host (`test/fixtures/nat-standin.sh`):

* A secondary address on a kind node is the private address. It is attached with
  the same script as the other `ExternalIPPool` case.
* A documentation address that no kind node holds is the public address. The
  host translates it to the private one with a DNAT, in the `OUTPUT` chain for
  the host's own traffic and in `PREROUTING` for a pod's or a node's. A
  `MASQUERADE` on translated traffic keeps replies on the path through the host,
  and a host route sends the public address out of the kind bridge.
* A relay that bound the public address could not start, because no node holds
  it. A relay that answers on it was bound to the private address, and the case
  asserts that from the pod annotation and the rendered configuration.
* `turnfleet-probe` from the host allocates on the public address and must get
  the public address back in `XOR-RELAYED-ADDRESS`.

The translation needs root. The script uses `sudo -n`, which the GitHub-hosted
runners allow, and removes its rules when the case ends. The case skips, and
says why, on a host that has no `iptables` or no passwordless `sudo`. What kind
cannot show is a cloud's own NAT, the attachment of an address to an instance,
or a CNI that fills the node's interfaces.

## Rejected

* **Decide on the node, as in ADR 23.** There the node reports an `ExternalIP`
  and an `InternalIP`, and only an interface listing tells which one it holds.
  Here the operator declares the address, and the private address that goes with
  it is not in the node's status at all. The declared pair is the fact.
* **Validate in the webhook only.** `config/default` and the chart deploy no
  webhook, and a bad value then reached the renderer, whose error stopped the
  whole pool. The CRD rules and the planner's skip cover that install.
* **Validate nothing and let the controller fail.** The renderer refuses a
  mixed-family pair in the reconcile loop, where the author sees only a pool
  that does not come up. Admission says so, with the field.
* **Normalise `bindAddress == address` to absent.** See the validation rules:
  one way to say it.
* **Make `bindAddress` part of the identity.** A client holds an allocation
  against the public address. The private address is an implementation detail of
  the node, and an identity that moved with it would drain a relay that no
  client can tell apart.
* **A cross-pool conflict on the bind address.** It needs the bind address in
  the status and says nothing a failing probe does not.
* **Another name.** `privateAddress` and `internalAddress` would name the
  address by what it usually is. `bindAddress` names what it does, in the
  glossary's term, and stays right for an entry whose `bindAddress` is public.

## Consequences

* A pool can name an Elastic IP or its equivalent. The operator still attaches
  and configures it.
* Existing pools do not change: with no `bindAddress` the configuration, its
  hash and the relay's identity are what they were.
* Issue #11 said a changed bind address "does not drain" the relay. It does:
  the replacement goes through the config-hash rollout, which drains (ADR 6 and
  ADR 21). Nothing moves the relay's identity, so no client sees another URI,
  but the relay leaves `turnURIs` while it drains.
* Setting, changing or removing `bindAddress` rolls that relay once, under the
  drain of ADR 21, and does not move its identity or its URIs.
* A relay is ready only after the controller's own probe, from inside the
  cluster, allocates on the advertised address. On a cloud the controller's pod
  has to reach the public address of its own cluster, which works when the
  traffic leaves the instance and comes back through the cloud's NAT. The e2e
  case relies on the same path, through the host's translation.
* A wrong `bindAddress` that is valid, such as a private address the node does
  not hold, is not caught by admission. The relay does not become reachable, and
  the pool's probe says so.
* The e2e case needs `iptables` and a passwordless `sudo` on the host, and
  modifies the host's NAT table for the length of one test.
