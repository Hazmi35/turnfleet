# Status

What exists today, what it does not do, and what comes next. Last updated with
the initial implementation of all phases; update it in the same pull request as
any change to behaviour or scope.

## Done

**Operator and API.** `TURNPool` (`turnfleet.hzmi.dev/v1alpha1`, `tpool`) with
`/status` and `/scale` subresources, defaulting, validating and immutability
webhooks, generated CRDs and a generated API reference.

**Addressing modes.** All three, end to end and covered by e2e cases:

* `HostNetwork` — one relay per labelled node on the node's `ExternalIP` (or the
  `turnfleet.hzmi.dev/relay-address` override), with required anti-affinity.
* `PartitionedIP` — several relays per node on one address, each owning a
  disjoint slice of the relay port range; the status reports the real
  `portsTotal`, which is one IP's port space.
* `ExternalIPPool` — one relay per declared `{nodeName, address}` secondary
  address, assigned stably.

**Relay lifecycle.** Planner with stable relay identities; coturn config
rendered per relay into a ConfigMap; pods named `<pool>-relay-<5-char hash>`;
rolling update on config or Secret change via the
`turnfleet.hzmi.dev/config-hash` annotation, one relay at a time; drain state
machine with the `turnfleet.hzmi.dev/drain` finalizer; `turnURIs` limited to
ready, non-draining relays and mirrored into `<pool>-turn-uris`.

**Probe.** `internal/probe` performs a real TURN `Allocate` with a
TURN-REST-API credential derived from the shared secret, asserts
`XOR-RELAYED-ADDRESS` against the relay's expected address and range, and
releases the allocation afterwards. The controller runs it per relay on
`spec.probe.interval` and feeds readiness, `lastProbe` and `ProbeSucceeded`.
`turnfleet-probe` exposes the same check as a CLI with JSON output.

**Observability.** `turnfleet_relay_allocations`, `turnfleet_relay_ports_in_use`,
`turnfleet_relay_ports_total`, `turnfleet_allocation_failures_total{code}`,
`turnfleet_probe_success`, `turnfleet_probe_duration_seconds`, labelled only by
`namespace`, `pool` and `relay`. Conditions: `Ready` and `ProbeSucceeded`
normal-true, `CapacityPressure` and `Degraded` abnormal-true.

**Tests.** Unit tests for planner stability, disjointness and partitioning,
golden-file renderer tests per mode, credential derivation, condition
transitions and the coturn flag set; fuzz targets driving the renderer and the
webhook with arbitrary specs (never panic, never plan overlapping ranges on one
address); envtest for per-mode reconcile, the drain state machine with and
without the drain timeout, secret rotation rolled one relay at a time, node loss,
the missing-secret path, webhook rejections and the scale subresource; e2e on
kind with a real coturn data plane for the three modes against a real TURN
`Allocate`, global-address saturation (`508` observed, `CapacityPressure` true,
`turnfleet_allocation_failures_total{code="508"}` counted, the other relay still
allocating), the peer policy refusing an in-cluster address, scale-down with an
active allocation, and `turnfleet-probe` run from the host.

**Tooling.** `make ci` (verify, lint, test, vuln, licenses, dco) and
`make test-e2e`; every version pinned and recorded in `docs/versions.md`;
GitHub Actions pinned by SHA with least-privilege permissions; DCO enforced by
`hack/verify-dco.sh`.

## Known limitations

* **Cross-pool overlap is not validated.** The webhook rejects overlapping relay
  port ranges within a pool, but two `TURNPool`s claiming the same address and
  range cannot be checked atomically by a single-object webhook. Use distinct
  addresses or disjoint ranges, and treat the planner's disjointness invariant
  as intra-pool only.
* **Secondary addresses must be attached already.** `ExternalIPPool` never
  attaches addresses to nodes; it binds to what is there. Attaching cloud
  elastic IPs stays the operator's job.
* **No Service in the data path means firewall work.** Listener ports and the
  relay range must be reachable on each relay node; the operator cannot create
  security-group or firewall rules for an address it does not own.
* **Probes hold a port for their duration, and release it asynchronously.**
  Closing an allocation sends `Refresh` with lifetime zero and does not wait for
  the answer, so the port comes back a moment later. The saturation threshold
  therefore uses coturn's steady-state gauge rather than deltas, and a relay with
  exactly one free port can answer 508 to a probe while the previous probe's
  release is in flight.
* **coturn exposes no per-code allocation failure counters.** `code="508"` is
  counted from probe observations, not from coturn. A relay that is full but
  never probed (probe disabled, or the controller down) will not report 508.
* **An in-cluster probe can pass while external reachability fails.** Hairpin
  NAT and firewalls in front of a node are invisible from inside; the external
  `turnfleet-probe` run is the authoritative check
  (`docs/adr/0007-probe-vantage-point.md`).
* **Webhooks need certificates in-cluster, so they live in their own overlay.**
  `config/default` (what `make deploy` applies) runs the controller without
  admission webhooks; `config/with-webhook` adds the webhook configurations, the
  cert-manager `Certificate`/`Issuer` and the serving certificate mount
  (`make deploy KUSTOMIZE_OVERLAY=config/with-webhook`). Admission behaviour is
  covered by envtest, which serves its own certificates.
* **`nodurations` is disabled in kube-api-linter.** `drainTimeout` and the probe
  timings are `metav1.Duration` by design; see `.golangci.yml`.
* **One ConfigMap holds every relay's configuration per pool.** Each rendered
  config is ~1.5 KB and a ConfigMap is capped at 1 MiB, which is why
  `spec.replicas` is bounded at 256. A pool that needs more relays needs more
  than one pool.
* **Partitioned relays need consecutive listener ports.** `relaysPerNode`
  relays on one address serve `spec.listeners[].port` up to
  `port + relaysPerNode - 1`, and that span must stay outside
  `spec.relayPortRange`; the webhook rejects a pool where it does not
  (`docs/adr/0009-partitioned-listener-ports.md`).
* **IPv6 and dual-stack relays are not implemented.** Relays bind IPv4
  addresses only.
* **`HostNetwork` needs a node address that clients can actually route to.** It
  binds the node's `ExternalIP`, or the `turnfleet.hzmi.dev/relay-address`
  annotation when the node has no `ExternalIP` or it is not the address clients
  should use. On a cluster where the nodes have no public address at all —
  Azure Kubernetes Service with Cilium is the named example, where inbound
  traffic arrives only through a load balancer and outbound goes through a NAT
  gateway — this mode is unusable: the relay would advertise the node's
  `InternalIP`, an RFC 1918 address no peer can reach, while the allocation and
  the in-cluster probe both look healthy. Next step 7 below
  (`PodIP`/`SecondaryNetwork` addressing) is what closes that gap.
* **The e2e suite runs on amd64.** The kind node image and the loaded coturn
  image are amd64; arm64 is built in CI but not exercised end to end.

## Next steps

1. **Release automation**: image signing (cosign), SBOM publication and
   provenance attestations, plus a Helm chart. Out of scope for the initial
   implementation and listed here so the gap is explicit.
2. **External synthetic monitoring**: run `turnfleet-probe` from outside the
   cluster on a schedule and expose the result to the pool's status, so the
   authoritative vantage point is part of the control loop.
3. **Cluster-wide address allocation**: a cluster-scoped cooperating controller
   that makes cross-pool overlap a rejected state instead of a documented
   limitation.
4. **More capacity sources**: cloud IP allocation (attach secondary addresses
   automatically), and a mode that spans multiple nodes per address pair.
5. **IPv6/dual-stack**: bind and advertise IPv6 relay addresses, including the
   `-X` mapping rules. <!-- markdownlint-disable-line -->
6. **KEDA/HPA examples** built on the existing `/scale` subresource and
   `turnfleet_relay_ports_in_use` / `turnfleet_relay_ports_total`, including the
   caveat that scaling relays does not add ports to an address.
7. **`PodIP` / `SecondaryNetwork` addressing (`hostNetwork: false`).**
   Motivating environment: Azure Kubernetes Service with Cilium, where nodes
   have no public IP, inbound traffic arrives only through a load balancer, and
   outbound goes through a NAT gateway. Today that is unusable: `HostNetwork`
   falls back to the node's `InternalIP`, so a relay advertises an RFC 1918
   address no peer can route to, while the allocation and the in-cluster probe
   both look healthy.

   Requirements for the mode (each one is a prerequisite, not an
   implementation detail):
   - **Individually routable addresses.** The address a relay advertises must be
     reachable from the peers' networks with no load balancer in the path.
     `HostNetwork` uses a node address; this mode uses the relay's own pod IP
     (Cilium on Azure with public pod IPs from a prefix, NSG and route table
     ingress rules per address) or a Multus secondary interface with its own
     IPAM (`azure-ipam`, SR-IOV, macvlan) — the explicit form, and the one an
     operator can reason about.
   - **Stable addresses.** A relay's identity is `(address, portRange)`, so an
     address that changes when the pod is recreated strands every allocation on
     it. The mode must require reserved/static IPAM (or StatefulSet-style
     addressing) and treat "my address changed" as a drain, never as a silent
     re-advertise.
   - **No Service or cloud load balancer in the data path.** `ServicePort` has
     no port range, Azure Standard LB rules and NAT rules are per-port, and an
     L4 LB still hashes the 5-tuple for peer-originated packets, so a peer's
     packet can land on the wrong relay (ADR 3).
   - **Honest capacity accounting.** Capacity stays addresses × ports, exactly
     as in `ExternalIPPool`: the renderer binds the pod or secondary address,
     the planner treats each address as one relay identity, and
     `status.portsTotal` keeps reporting the real port space.
   - **Validated from outside.** Admission cannot prove routability, so the mode
     must be gated on the external vantage point: `turnfleet-probe` run against
     the advertised address from outside the cluster (`ProbeSucceeded`), with
     the in-cluster probe explicitly documented as insufficient (ADR 7).

   Prerequisites the operator owns and the operator cannot create: per-address
   ingress rules (NSG/security group), routable pod CIDRs, and stable IPAM.
   `docs/capacity-model.md` needs the matching paragraph stating that
   "routable" means individually addressable from the peer's network, which on
   that platform is something the operator must build — and that an
   "inbound-only-via-LB" network is a hard blocker unless pod IPs are themselves
   public and permitted.

   Not a `hostNetwork: false` flag on the existing modes: it is a fourth
   addressing mode, with its own planner candidate source, webhook validation
   (reject addresses that are not individually reachable), a golden-file
   renderer case, an e2e case, and an ADR recording the routability and
   stability prerequisites.
