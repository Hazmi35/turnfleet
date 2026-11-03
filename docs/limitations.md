# Known limitations

What Turnfleet deliberately does not do, and the traps that follow from it. Each
entry constrains something described in `docs/architecture.md` or
`docs/capacity-model.md`; read those first.

This file is not a backlog. Work that is planned but not scheduled lives in the
issue tracker (`gh issue list --label roadmap`; see
`docs/agents/triage-labels.md`); an entry here that becomes planned work keeps
its statement and gains a pointer to the issue.

* **Cross-pool overlap is reported, not resolved.** Two pools that hold the same
  relay address with overlapping ports are detected and flagged on both pools
  (`AddressConflict` and `Degraded`; see
  `docs/adr/0012-cross-pool-address-conflict.md`). Nothing moves a relay to make
  room, because which pool gives way is a capacity decision. `relayPortRange`
  and addressing mode are immutable, so clearing the condition means removing a
  declared address (`ExternalIPPool`) or creating a new pool. Such a pool is
  still admitted, because a single-object webhook cannot see the other pool's
  addresses atomically.
* **Secondary addresses must be attached already.** `ExternalIPPool` never
  attaches addresses to nodes, and never configures them. It binds what is
  there: `address`, or `bindAddress` when the public address is a 1:1 NAT
  address the node does not hold. Attaching a cloud public address to an
  instance, any interface the CNI does not manage, the operating system's
  configuration of that interface and its routing, and the exemption of the
  address from the cluster's outbound NAT stay the operator's job
  (`docs/adr/0024-externalippool-bind-address.md`). A cluster that does it for
  you is tracked in #89, #90, #91 and #92.
* **Two pools can bind one local address.** The cross-pool conflict check
  compares advertised addresses. Any two relays from different pools that bind
  one local address are not reported, and they compete for the sockets on it.
  That covers two entries that map different public addresses to one private
  address, an entry whose `bindAddress` is another pool's `address`, and an
  entry whose `bindAddress` is a node's `InternalIP` that a `HostNetwork` or
  `PartitionedIP` relay binds. The webhook checks one pool only
  (`docs/adr/0024-externalippool-bind-address.md`).
* **No Service in the data path means firewall work.** Listener ports and the
  relay range must be reachable on each relay node. The operator cannot create
  security-group or firewall rules for an address it does not own.
* **Some networks cannot be served by any mode.** When inbound traffic arrives
  only through a load balancer and the pod addresses are private, no relay can
  own an address the peers reach directly. An overlay network without Multus,
  on nodes whose addresses the peers cannot route to, leaves no address to
  give a relay at all. `docs/capacity-model.md` ("Which environments can be
  served") maps the environments that work and what each needs first.
* **A peer's own egress policy is invisible from the cluster.** A peer whose
  network lets only TCP/443 out reaches a relay only through a listener on port
  443: `tcp`, or `tls` where the network inspects what it lets through. The
  peer at the other end must still reach the relay range, or use a relay of its
  own. Nothing inside the cluster can see either constraint; a vantage runner
  placed in that network is the check.
* **An attachment's IPAM is trusted, not verified.** Turnfleet cannot tell in
  advance whether a NetworkAttachmentDefinition gives a `PodIP` relay pod the
  same address every time. It reads the address back from the pod and drains
  the relay when it differs (ADR 18), so an unstable IPAM shows up as a drain
  loop after the fact.
* **Probes hold a port for their duration, and release it asynchronously.**
  Closing an allocation sends `Refresh` with lifetime zero and does not wait for
  the answer, so the port comes back a moment later. The saturation threshold
  therefore uses coturn's steady-state gauge rather than deltas. A relay with
  exactly one free port can answer 508 to a probe while the previous probe's
  release is in flight.
* **coturn exposes no per-code allocation failure counters.** `code="508"` is
  counted from probe observations, not from coturn. A relay that is full but
  never probed (probe disabled, or the controller down) will not report 508.
* **An in-cluster probe can pass while external reachability fails.** Hairpin
  NAT and firewalls in front of a node are invisible from inside. The external
  `turnfleet-probe` run is the authoritative check
  (`docs/adr/0007-probe-vantage-point.md`). A runner can now publish its result
  for the controller to report (`ReachableFromVantage`,
  `docs/adr/0015-vantage-result-delivery.md`). But **nothing shipped here puts
  the vantage point outside the cluster**: `config/vantage/runner.yaml` runs in
  a pod, and the controller cannot tell where a runner ran. Making the check
  authoritative needs relay addresses that route from outside and ingress rules
  per address, which the operator builds. Until then the condition reports what
  a runner saw, not that a peer can connect.
* **Webhooks need certificates in-cluster, so they live in their own overlay.**
  `config/default` (what `make deploy` applies) runs the controller without
  admission webhooks. `config/with-webhook` adds the webhook configurations, the
  cert-manager `Certificate`/`Issuer` and the serving certificate mount
  (`make deploy KUSTOMIZE_OVERLAY=config/with-webhook`). Admission behaviour is
  covered by envtest, which serves its own certificates.
* **The manager's lease keeps controller-runtime's timings by default.** The
  leader renews its lease every 2s (`--leader-elect-retry-period`), which is one
  write to the API server each time, and it exits when it cannot renew for 10s
  (`--leader-elect-renew-deadline`). A restart of a single API server that takes
  longer than that restarts the controller. Longer timings, such as 60s, 40s and
  15s, fit one replica or one API server better. They are flags rather than the
  default because they also slow a standby's takeover after the leader dies.
* **`nodurations` is disabled in kube-api-linter.** `drainTimeout` and the probe
  timings are `metav1.Duration` by design; see `.golangci.yml`.
* **One Secret holds every relay's configuration per pool.** Each rendered
  config is ~1 KB, and a Secret is capped at 1 MiB like a ConfigMap (`base64`
  costs a third on top). So `spec.replicas` is bounded at 256: a pool that needs
  more relays needs more than one pool. Reading a relay's configuration needs
  `get` on Secrets in the pool's namespace. The operator needs `create` and
  `update` on Secrets there, because the rendered configuration embeds the
  shared HMAC secret (`docs/adr/0020-relay-config-secret.md`).
* **Partitioned relays need consecutive listener ports.** `relaysPerNode`
  relays on one address serve `spec.listeners[].port` up to
  `port + relaysPerNode - 1`, and that span must stay outside
  `spec.relayPortRange`. The webhook rejects a pool where it does not
  (`docs/adr/0009-partitioned-listener-ports.md`).
* **IPv6 relay addresses are the node's own.** A pool with
  `spec.addressing.family` set to `IPv6` or `DualStack` plans relays on a node's
  IPv6 address, and the operator neither creates nor routes it. The node has to
  have a globally reachable IPv6 address (or a declared one). A node without one
  hosts no IPv6 relay, and the pool reports the replicas it could not place
  instead of falling back to IPv4. Kind nodes cannot provide an IPv6 address, so
  the IPv6 data path is proven against the pinned coturn image rather than by
  `make test-e2e` (`docs/capacity-model.md`).
* **`HostNetwork` needs a node address that clients can actually route to.** It
  advertises the node's `ExternalIP`, or the `turnfleet.hzmi.dev/relay-address`
  annotation when the node has no `ExternalIP`, or when the `ExternalIP` is not
  the address clients should use. When the node has an `InternalIP` and a
  different `ExternalIP`, the relay binds the `ExternalIP` if the node holds it
  on an interface and the `InternalIP` if it does not (1:1 NAT, as on EKS with
  public nodes). The relay pod decides when it starts
  (`docs/adr/0023-relay-bind-decided-on-the-node.md`), and the node annotation
  `turnfleet.hzmi.dev/relay-bind` (`InternalIP` or `ExternalIP`) skips the
  decision. On a cluster where the nodes have no public address at all, this
  mode is unusable. Azure Kubernetes Service with Cilium is the named example:
  inbound traffic arrives only through a load balancer and outbound goes
  through a NAT gateway. The relay would advertise the node's `InternalIP`, an
  RFC 1918 address no peer can reach, while the allocation and the in-cluster
  probe both look healthy. `PodIP` addressing is what closes that gap
  (`docs/adr/0018-addressing-mode-podip.md`).
* **A relay on a node with two different addresses carries an init container.**
  It runs the controller's own image, so the node has to be able to pull it, and
  the controller has to know it: it reads it from its own pod, or from
  `--relay-helper-image` outside a pod. Without either, a pool that plans such a
  relay fails to reconcile with an error that names the flag and the node
  annotation. Relay pods get no `imagePullSecrets`, so a controller image in a
  private registry leaves the init container in `ImagePullBackOff` unless the
  node holds a pull credential or the image. Setting `relay-bind` on the node
  avoids the init container. The relay also listens on the loopback address of its family,
  which its readiness probe uses, because the probe is fixed before the choice
  is made. A node that sets `turnfleet.hzmi.dev/relay-bind`, or that has one
  address per family, gets neither. kind nodes cannot hold an `ExternalIP` the
  kubelet does not report, so `make test-e2e` runs the path without the init
  container. The choice itself is covered by unit and envtest tests and was run
  by hand against the pinned coturn image.
* **Upgrading restarts the relays on nodes with two different addresses.** A
  relay on a node whose `ExternalIP` and `InternalIP` differ and that has no
  `turnfleet.hzmi.dev/relay-bind` gains the init container and the loopback
  listener, so its pod spec and config hash change and the relay rolls once,
  under drain, one relay at a time. Its identity `(address, port range)` does
  not change. The hash holds the version of the decision, not the init
  container's image, so a later controller release does not roll these relays
  again unless the decision changes. On a node that holds its `ExternalIP`, the relay binds and
  advertises what it did before. On a NATed node it starts serving, where it
  bound an address the node did not hold. A relay on a node with one address,
  with equal addresses, or with `relay-bind=ExternalIP` hashes as it did and is
  not restarted. With `relay-bind=InternalIP` the bind address changes, so the
  relay rolls once.
* **`PodIP` needs the operator to reserve the addresses.** The relay advertises
  the address its own pod holds, on the default network or on a Multus
  attachment (`spec.addressing.podIP.attachmentRef`). The operator declares the
  reserved addresses per relay node in `turnfleet.hzmi.dev/relay-pod-addresses`.
  Per-address ingress rules (NSG or security group), a routable pod network and
  stable IPAM are prerequisites the operator owns. Without stable IPAM the pod
  comes up on another address and the relay drains in a loop
  (`RelayAddressChanged` events and the `PodAddressUnusable` condition say so).
  Admission proves routability only negatively. It refuses the addresses that
  cannot work at all (unspecified, loopback, link-local, multicast) and lets a
  private address through, because routing that address to the peers is the
  operator's job. That job covers peering, routes and per-address ingress rules.
  The external vantage point is the check that it was done. A network whose
  inbound traffic arrives only through a load balancer is a hard blocker unless
  the pod addresses are themselves reachable from the peers and permitted. Kind
  as `make kind-up` configures it carries Multus and the reference CNI plugins
  (macvlan and the static IPAM plugin), so `make test-e2e` runs the data path of
  the mode: a relay on a macvlan attachment advertises the attachment's address
  in its status, `turnURIs` and the URI ConfigMap, and answers a TURN `Allocate`
  from the test host (`TestPodIPRelayOnAMultusAttachment`). It also covers the
  failure half: a node that declares an address the pod cannot hold, where the
  relay is drained and never published
  (`TestPodIPRelayWhoseDeclaredAddressThePodCannotHold`). What kind proves is
  the mechanism, and not a cloud's IPAM. The attachment uses the static IPAM
  plugin, which honours the address the pod asks for. `azure-ipam`, SR-IOV and
  `whereabouts` are not exercised, and neither are the per-address ingress rules
  and the routing a real cloud network adds. The case probes from the test
  host, which sits on the same bridge as the nodes, so it shows the address
  reaches the pod and not that a peer behind a NAT or a firewall can reach it.
  The case skips, with the reason in its output, on a host that has no interface
  on the kind nodes' network, such as a remote container engine.
* **A macvlan relay is unreachable from its own node.** The kernel does not pass
  traffic between a macvlan interface and its parent. A `PodIP` relay on a
  macvlan attachment therefore cannot be probed from a controller pod that runs
  on the same node, and it never turns ready, although the host and every other
  node reach it. The e2e case adds a second macvlan interface on the relay's
  node and routes the relay's address through it (`test/fixtures/macvlan-shim.sh`).
  An operator on macvlan needs the same shim, or a controller placed on another
  node. The case does not show whether other interface types share the limit.
* **The e2e suite runs on amd64.** The kind node image and the loaded coturn
  image are amd64; arm64 is built in CI but not exercised end to end.
