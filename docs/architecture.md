# Architecture

Turnfleet turns a `TURNPool` into a set of TURN relays, each with its own public
relay identity, and keeps that set reachable, drained and observable.

## The problem it solves

A TURN allocation is a public `(IP, port)` pair. Peer traffic sent to that pair
must reach the exact process holding the allocation. Running N relays behind one
load-balanced public IP cannot guarantee that: an L4 load balancer hashes on the
5-tuple, and a peer's source port is arbitrary. Adding replicas behind one
address therefore adds processes without adding *relay capacity*, while every
pod reports healthy — the failure is silent. Turnfleet replaces that with
per-relay identities: one address plus a disjoint port range, delivered without
a load balancer. See `docs/adr/0001-addressing-modes.md` and
`docs/adr/0003-service-port-ranges.md`.

## Data path

```mermaid
flowchart LR
  client["TURN client"]
  issuer["credential issuer<br/>(not in this repo)"]
  agent["turnfleet-controller"]
  pool["TURNPool"]
  cm["ConfigMap<br/>pool-turn-uris"]
  subgraph node["relay node (label turnfleet.hzmi.dev/relay-node=true)"]
    relay["relay pod (hostNetwork)<br/>coturn: listener + relay range"]
  end

  issuer -->|reads URIs| cm
  client -->|gets turn: URI| issuer
  client -->|Allocate on listener| relay
  peer["WebRTC peer"] -->|UDP to relayed address| relay
  pool --> agent
  agent -->|creates/owns| relay
  agent -->|mirrors ready, non-draining URIs| cm
  agent -->|probe: TURN Allocate| relay
```

Relays run with `hostNetwork: true` on nodes labelled
`turnfleet.hzmi.dev/relay-node=true` and bind their listener ports and relay
port range directly on a node address. There is no Service, Ingress or cloud
load balancer in the relay data path (ADR 3, ADR 4). Relays that share an address
(PartitionedIP) each get their own listener port — `spec.listeners[].port` plus
the relay's slot on its node — because a listening socket binds once per address
(ADR 9).

## Components

| Component | Responsibility |
| --- | --- |
| `api/v1alpha1` | `TURNPool` spec and status. No logic. |
| `internal/relay.Planner` | Computes the desired relay set: one identity `(relayAddress, portRange)` per relay, disjoint within a pool, stable across restarts and failover. Implements the three addressing modes. |
| `internal/relay.Renderer` | Renders one relay's coturn configuration from the spec, the relay identity and the node address. Pure; golden-tested. |
| `internal/probe` | A real TURN `Allocate` (plus optional `CreatePermission`/`Send` round trip) against a relay listener on `pion/turn`, asserting the relayed address and range. |
| `internal/vantage` | The result a vantage runner publishes and where it publishes it: the `<pool>-vantage` ConfigMap, its labels and its JSON payload. |
| `internal/metrics` | `turnfleet_` collectors and the coturn scrape/parse helpers. |
| `internal/controller` | Reconciles a `TURNPool`: planner + renderer + relay pods + config ConfigMap + `<pool>-turn-uris` ConfigMap + status; runs the drain state machine, the rolling config-hash update and the periodic probe. |
| `internal/webhook/v1alpha1` | Defaulting, validation, immutability, mode-specific rules. |
| `cmd/turnfleet-probe` | The same probe as a CLI with JSON output, so the check can run from outside the cluster. With `--uris-configmap`/`--publish-configmap` it is also the vantage runner: it probes every URI the pool publishes and writes the result where the controller reads it. |
| `config/` | The kustomize manifests for the controller: `config/default` runs it without admission webhooks, `config/with-webhook` adds them and their cert-manager certificates. Source of truth for what the controller is deployed as. |
| `charts/turnfleet` | The Helm chart for the same controller — CRDs, RBAC, the manager, the metrics Service and, opt-in, the webhooks — for a consumer who does not want to run kustomize. It never templates relays: relay pods come from the planner, and nothing in the relay data path goes through it. `make helm-drift` renders the chart and `config/` and fails on drift. |
| `config/vantage` | The runner manifest: a CronJob, its ServiceAccount, its Role and RoleBinding. Not part of `config/default` — a runner is per pool and the operator does not schedule it. |

## Reconcile flow

```mermaid
stateDiagram-v2
  [*] --> Observe
  Observe: list labelled nodes, relay pods, Secret, ConfigMaps, the published vantage result
  Observe --> Plan: plan desired relays
  Plan --> Render: per relay: coturn config + config hash
  Render --> Converge: create/update pods, ConfigMap
  Converge --> Drain: relays that must go: mark draining, drop from turnURIs
  Drain --> Wait: wait allocations == 0 or drainTimeout
  Wait --> Converge: delete drained pods
  Converge --> Probe: probe ready relays on probe.interval
  Probe --> Status: conditions, relays, portsInUse/portsTotal, turnURIs
  Status --> [*]
```

* **Relay identity and naming.** `(relayAddress, portRange)`; the pod name is
  `<pool>-relay-<5-char hash of the identity>`, so the same identity always maps
  to the same pod and a controller restart or failover does not shuffle relays.
* **Rollout.** The rendered config and the referenced Secret are hashed into the
  pod's `turnfleet.hzmi.dev/config-hash` annotation. A change rolls the relays
  **one at a time**, each with drain, so capacity loss is bounded to one relay.
* **Drain.** Draining relays are removed from `turnURIs` and from the
  `<pool>-turn-uris` ConfigMap *before* the pod goes away, then readiness fails,
  then the controller waits for the allocation count to reach zero or for
  `spec.drainTimeout`. The finalizer `turnfleet.hzmi.dev/drain` runs the same
  sequence for pool deletion. See `docs/adr/0006-drain-semantics.md`.
* **`turnURIs`.** Only ready, non-draining relays, mirrored into the
  `<pool>-turn-uris` ConfigMap so the credential issuer hands out per-relay
  URIs (and never a URI for a relay that is saturated or going away).
* **Vantage result.** A runner publishes its pass over `<pool>-turn-uris` into
  the `<pool>-vantage` ConfigMap; the controller watches ConfigMaps by label and
  reads it every pass. The result is the only input to `ReachableFromVantage`,
  and the in-cluster probe is not an input to it at all. See
  `docs/adr/0015-vantage-result-delivery.md`.

## Status and conditions

| Condition | Polarity | Meaning |
| --- | --- | --- |
| `Ready` | normal-true | The desired number of relays exist, are ready and non-draining. |
| `ProbeSucceeded` | normal-true | Every ready relay last probed successfully. |
| `CapacityPressure` | abnormal-true | A relay is near or at its port limit (`portsInUse`/`portsTotal`), or a probe observed error 508. |
| `ReachableFromVantage` | normal-true | Every listener URI the pool publishes answered a TURN `Allocate` where the vantage runner ran. False when one did not; unknown while nothing current has been published — nothing at all, an unreadable payload, or a result older than 15 minutes. |
| `Degraded` | abnormal-true | Fewer relays than desired, a relay failing to converge, or a vantage point that cannot reach the relays. |

`ReachableFromVantage` is a separate condition on purpose. `Ready` and
`ProbeSucceeded` describe what the controller sees from inside the cluster, and
a relay can be perfectly healthy there while no peer can reach it — hairpin NAT,
a firewall in front of the relay node, a NAT mapping that is not 1:1. The
vantage condition reports what a runner saw and takes no part in the others, so
a passing in-cluster probe cannot clear a failing observation.

`status.relays[]` carries per-relay truth: `relayAddress`, `portRange`,
`ready`, `draining`, `allocations`, `portsInUse`, `portsTotal` and
`lastProbe{time,succeeded,latencyMillis,message}`. The API has no floats:
consumers compute the ratio themselves.

## Metrics

Every `turnfleet_` metric carries exactly `namespace`, `pool` and `relay` — the
relay identity, never the whole object. `turnfleet_relay_ports_total` is the
capacity number; the pod count is not.

| Metric | Type | Source |
| --- | --- | --- |
| `turnfleet_relay_allocations` | gauge | coturn's own endpoint (`turn_total_allocations`, summed over socket types) |
| `turnfleet_relay_ports_in_use` | gauge | the same scrape; one allocation holds one port |
| `turnfleet_relay_ports_total` | gauge | the planned relay identity — the ports that relay owns |
| `turnfleet_allocation_failures_total{code}` | counter | allocations the probe observed failing, by TURN error code (ADR 5) |
| `turnfleet_probe_success` | gauge (1/0) | the last `Allocate` probe of that relay |
| `turnfleet_probe_duration_seconds` | gauge | the same probe |

The gauges are republished on every reconcile and dropped when a relay or pool
disappears; the failure counter is cumulative, so read it as a rate.
`docs/capacity-model.md` says what to watch when a pool runs out.

## Peer policy

A relay must never reach in-cluster or metadata addresses unless a pool
explicitly allows it. The default deny list (rendered as `--denied-peer-ip`)
covers RFC 1918, RFC 6598 (CGNAT), loopback, link-local including
`169.254.169.254`, and the cluster pod and service CIDRs. `allowedCIDRs` emits
`--allowed-peer-ip` entries. This is what stops a relay from being turned into
an SSRF pivot into the cluster (see the e2e peer-policy case).

A pool that declares no `peerPolicy`, or an empty `deniedCIDRs` list, gets that
default: the defaulting webhook applies it, and so does the controller, because
`config/default` runs without admission webhooks and a spec that arrives from
there has been defaulted by nobody. A pool that declares its own list gets
exactly that list, never the default added to it.

## What is deliberately not here

The full list, including what each omission costs operationally, is in
`docs/limitations.md`.

* The direct/STUN path, signalling, and the credential-issuing endpoint are out
  of scope. Turnfleet only consumes the shared HMAC secret the issuer uses.
* No Service/LoadBalancer exposure, no ingress controller. The controller is
  installed with kustomize (`config/`) or with the Helm chart
  (`charts/turnfleet`); both are the same controller, and neither is in the relay
  data path.
