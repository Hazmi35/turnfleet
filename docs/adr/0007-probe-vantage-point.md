# 7. Probe vantage point: in-cluster for configuration, external for reachability

* Status: accepted
* Date: 2026-10-13

## Context

The failure this project exists to remove is silent. The signalling path
succeeds, the relay candidate never forms, and every pod is healthy. A
readiness probe that only checks "is the TURN port open" does not catch it,
because a port can be open and still be useless to the client that needs it.

The strongest check available is a real `TURN Allocate` against the relay's
listener with credentials derived from the shared secret, asserting that the
returned `XOR-RELAYED-ADDRESS` matches the `(address, port range)` the relay is
supposed to own. That catches wrong address, wrong range, bad secret, missing
realm, and saturation (error 508).

What it cannot catch from inside the cluster is external reachability: hairpin
NAT on the relay's own node, a firewall in front of the node, or a NAT rule
that does not map the advertised port 1:1. All of those let an in-cluster probe
succeed while no client can reach the relay.

## Decision

Two vantage points, one implementation:

* `internal/probe` implements the check on `pion/turn`: `Allocate` with a
  TURN-REST-API credential derived from the shared secret, assertion on
  `XOR-RELAYED-ADDRESS`, and an optional `CreatePermission` + `Send`/`Data`
  round trip to prove the relayed path works end to end.
* The controller runs the probe against every relay every `spec.probe.interval`
  (default 10s, `spec.probe.timeout` default 3s) and feeds the result into
  readiness, `status.relays[].lastProbe`, the `ProbeSucceeded` condition and
  `turnfleet_probe_success` / `turnfleet_probe_duration_seconds`.
* `turnfleet-probe` exposes the same code as a CLI
  (`--uri`, `--secret-file`, `--expect-address`, `--expect-port-range`, JSON
  output) so the authoritative check can be run from outside the cluster, from
  a CI job or from a laptop, against the public address of a relay.

The probe releases its allocation at the end of every check (`Refresh` with
lifetime 0). Leaving it behind would consume exactly the capacity the probe is
measuring.

## Consequences

* `ProbeSucceeded: false` on a relay is the honest "relay is broken" signal,
  and `Degraded` is raised for the pool when fewer relays than
  `spec.replicas` probe successfully.
* `docs/capacity-model.md` states plainly that an in-cluster probe can pass
  while external reachability fails, and that the external vantage point
  (`turnfleet-probe` from outside) is the authoritative check. Operators are
  expected to run it as a synthetic check against every relay URI.
* The probe's credential is derived the same way the issuer derives it, so a
  probe failure also catches a rotated or wrong `auth.secretRef`.
