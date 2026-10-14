# Architecture Decision Records

One file per decision that was not obvious from the code. Format: context,
decision, consequences. Numbered, never renumbered, never deleted; a superseded
decision gets a new ADR that says so.

| ADR | Decision |
| --- | --- |
| [0001](0001-addressing-modes.md) | Addressing modes and relay capacity: per-relay identities instead of replicas behind one IP. |
| [0002](0002-coturn-data-plane.md) | coturn stays a separate, unmodified upstream image (BSD-3-Clause), never vendored. |
| [0003](0003-service-port-ranges.md) | Relay port ranges are never expressed as Service ports. |
| [0004](0004-listener-exposure.md) | Listener exposure: per-relay node ports, not a shared Service. |
| [0005](0005-metrics-source.md) | Metrics come from coturn's Prometheus endpoint; per-code allocation failures come from the probe. |
| [0006](0006-drain-semantics.md) | Drain semantics: out of `turnURIs` first, delete after allocations reach zero or `drainTimeout`. |
| [0007](0007-probe-vantage-point.md) | The probe runs in-cluster for configuration and from outside for reachability. |
| [0008](0008-coturn-config-rendering.md) | coturn configuration is rendered into a ConfigMap, from flags verified against the pinned image. |
| [0009](0009-partitioned-listener-ports.md) | Partitioned relays get their own listener ports: a listening socket binds once per address. |
| [0010](0010-harness-agnostic-agent-config.md) | The agent harness is harness-agnostic: canonical content under `.agents/`/`hack/agent/`, thin per-harness wiring. |
| [0011](0011-pin-provenance-in-place.md) | Version pins live next to what they pin; `docs/versions.md` records only the pins that are held, and why. |
| [0016](0016-release-signature-storage.md) | Release signatures and attestations live in the registry on cosign's tag-based storage, so the cosign pin is held on the v2 line. |
| [0014](0014-helm-chart-delivery.md) | The chart installs the controller; `config/` stays the source of truth, held together by `make helm-drift`. |

Related documents that are not ADRs: [`../architecture.md`](../architecture.md),
[`../capacity-model.md`](../capacity-model.md),
[`../limitations.md`](../limitations.md),
[`../versions.md`](../versions.md).
