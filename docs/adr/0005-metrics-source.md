# 5. Metrics source: scraped from coturn, with probe-derived failure codes

* Status: accepted
* Date: 2026-10-13

## Context

Capacity exhaustion has to become an observable signal. Two numbers matter:
how many allocations a relay currently holds, and how many relay ports it has.
The first one lives inside coturn; the operator cannot count allocations
itself without parsing the TURN protocol from the outside.

coturn could also be instrumented by the operator (a sidecar, a wrapper), but
that would mean either patching coturn (rejected, ADR 2) or running a second
process that cannot see the relay's internal state.

## Decision

The renderer enables coturn's own Prometheus endpoint
(`--prometheus`, `--prometheus-port`, `--prometheus-path` — all present in the
pinned image's `turnserver --help`) and the controller scrapes
`http://<relay pod IP>:9641/metrics` from inside the cluster.

* `coturn_turn_total_allocations{type="udp|tcp|tls"}` is a **gauge** of the
  current allocation count per socket type (registered as
  `prom_gauge_new("turn_total_allocations", "Represents current allocations
  number", ...)` in `src/apps/relay/prom_server.c` of the pinned tag). The
  controller sums the label values and reports them as
  `turnfleet_relay_allocations` and `turnfleet_relay_ports_in_use` — one
  allocation occupies exactly one relay port.
* `spec.relayPortRange` gives `turnfleet_relay_ports_total` (the slice a relay
  owns, not the configured range of the pool).
* coturn exposes **no per-error-code allocation failure counters**, so
  `turnfleet_allocation_failures_total{code}` is derived from the probe: every
  probe performs a real `Allocate` and records the TURN error code it observes,
  with `508 Insufficient Capacity` called out because it is the saturation
  signal. This is the same code path the e2e saturation test asserts on.
* Metrics labels are limited to `namespace`, `pool` and `relay`. Client
  addresses and usernames are never labels: they are unbounded and private.

Scrape failures are not fatal: a relay whose metrics endpoint cannot be reached
keeps its last known counts, its probe result decides readiness, and
`CapacityPressure` is only raised from fresh samples so a scrape outage cannot
fake capacity pressure.

## Consequences

* `portsInUse`/`portsTotal` in the status are real numbers from the data plane,
  so consumers can compute a ratio themselves (the API has no floats).
* `CapacityPressure` is triggered when `portsInUse / portsTotal` crosses the
  configured threshold *and* a probe observed a 508, which is the only
  unambiguous proof that the relay is actually refusing allocations.
* The probe's own allocation briefly occupies a port. The controller releases
  it (`Refresh` with lifetime 0) at the end of every probe, and the threshold
  logic tolerates the transient by using coturn's steady-state gauge rather
  than a delta.
