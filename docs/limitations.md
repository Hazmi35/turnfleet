# Known limitations

What Turnfleet deliberately does not do, and the traps that follow from it. Each
entry constrains something described in `docs/architecture.md` or
`docs/capacity-model.md`; read those first.

This file is not a backlog. Work that is planned but not scheduled lives in the
issue tracker (`gh issue list --label roadmap`; see
`docs/agents/triage-labels.md`); an entry here that becomes planned work keeps
its statement and gains a pointer to the issue.

* **Cross-pool overlap is not validated.** The webhook rejects overlapping relay
  port ranges within a pool, but two `TURNPool`s claiming the same address and
  range cannot be checked atomically by a single-object webhook. Use distinct
  addresses or disjoint ranges, and treat the planner's disjointness invariant
  as intra-pool only. Tracked in #10.
* **Secondary addresses must be attached already.** `ExternalIPPool` never
  attaches addresses to nodes; it binds to what is there. Attaching cloud
  elastic IPs stays the operator's job. Tracked in #11.
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
  (`docs/adr/0007-probe-vantage-point.md`). A runner can now publish its result
  for the controller to report (`ReachableFromVantage`,
  `docs/adr/0015-vantage-result-delivery.md`), but **nothing shipped here puts
  the vantage point outside the cluster**: `config/vantage/runner.yaml` runs in
  a pod, and the controller cannot tell where a runner ran. Making the check
  authoritative needs relay addresses that route from outside and ingress rules
  per address, which the operator builds. Until then the condition reports what
  a runner saw, not that a peer can connect.
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
  addresses only. Tracked in #13.
* **`HostNetwork` needs a node address that clients can actually route to.** It
  binds the node's `ExternalIP`, or the `turnfleet.hzmi.dev/relay-address`
  annotation when the node has no `ExternalIP` or it is not the address clients
  should use. On a cluster where the nodes have no public address at all —
  Azure Kubernetes Service with Cilium is the named example, where inbound
  traffic arrives only through a load balancer and outbound goes through a NAT
  gateway — this mode is unusable: the relay would advertise the node's
  `InternalIP`, an RFC 1918 address no peer can reach, while the allocation and
  the in-cluster probe both look healthy. `PodIP`/`SecondaryNetwork` addressing
  (#15) is what closes that gap.
* **The e2e suite runs on amd64.** The kind node image and the loaded coturn
  image are amd64; arm64 is built in CI but not exercised end to end.
