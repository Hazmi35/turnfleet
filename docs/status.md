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
golden-file renderer tests per mode, credential derivation, and condition
transitions; fuzz targets for the renderer and webhook validation; envtest for
per-mode reconcile, the drain state machine, secret rotation and webhook
rejections; e2e on kind for the three modes, saturation (508 counted,
`CapacityPressure` true, other relays still serving), peer policy, scale-down
with an active allocation, and `turnfleet-probe` from outside the cluster.

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
* **Probes hold a port for their duration.** An in-flight probe allocation is
  visible in `portsInUse`; the probe releases it immediately afterwards, so the
  saturation threshold uses coturn's steady-state gauge rather than deltas.
* **coturn exposes no per-code allocation failure counters.** `code="508"` is
  counted from probe observations, not from coturn. A relay that is full but
  never probed (probe disabled, or the controller down) will not report 508.
* **An in-cluster probe can pass while external reachability fails.** Hairpin
  NAT and firewalls in front of a node are invisible from inside; the external
  `turnfleet-probe` run is the authoritative check
  (`docs/adr/0007-probe-vantage-point.md`).
* **Webhooks need certificates in-cluster.** `config/default` keeps the
  cert-manager integration; the e2e overlay (`config/e2e`) runs the controller
  with `ENABLE_WEBHOOKS=false`. Admission behaviour is covered by envtest
  instead. A production install needs cert-manager or pre-provisioned serving
  certificates.
* **`nodurations` is disabled in kube-api-linter.** `drainTimeout` and the probe
  timings are `metav1.Duration` by design; see `.golangci.yml`.
* **IPv6 and dual-stack relays are not implemented.** Relays bind IPv4
  addresses only.
* **`HostNetwork` needs a node address that is reachable by clients.** On
  clusters where the node has no `ExternalIP` (or it is not the address clients
  should use), set the `turnfleet.hzmi.dev/relay-address` annotation, otherwise
  the relay advertises an address clients cannot reach.
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
