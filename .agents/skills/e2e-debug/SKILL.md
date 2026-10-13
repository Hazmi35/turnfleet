---
name: e2e-debug
description: Debug a failing relay end to end - inspect the kind cluster, relay logs and rendered coturn config, and run turnfleet-probe by hand. Use when a relay never forms, a probe fails, or an e2e case breaks.
---

# Debugging a relay

Symptom first, then the narrowest check that can distinguish the candidates.
The failure this project exists to remove is silent, so work from evidence, not
from "the pod is Running".

## 1. Cluster and pool state

```sh
kubectl --context kind-turnfleet get turnpools -A -o wide
kubectl --context kind-turnfleet -n <ns> get turnpool <pool> -o yaml
```

Read `status.relays[]`: `ready`, `draining`, `relayAddress`, `portRange`,
`portsInUse`/`portsTotal`, and `lastProbe.{succeeded,message}`. A relay that is
`ready: false` with a probe message is a *data plane* problem; a relay that is
missing from `status.relays` is a *planner* problem. `kubectl --context
kind-turnfleet describe turnpool <pool>` shows the conditions:
`Ready`, `ProbeSucceeded`, `CapacityPressure`, `Degraded`.

## 2. Rendered config

```sh
kubectl --context kind-turnfleet -n <ns> get cm <pool>-relay-<hash> -o yaml
```

Check the listening address, `--relay-ip`/`--external-ip`, `--min-port`/
`--max-port`, realm, and that `--static-auth-secret` matches the Secret the
issuer uses. If the config looks right but the relay does not, the pod is
probably not running on a labelled node.

## 3. Relay process

```sh
kubectl --context kind-turnfleet -n <ns> get pods -o wide | grep relay
kubectl --context kind-turnfleet -n <ns> logs <relay-pod> --tail=100
kubectl --context kind-turnfleet -n <ns> exec <relay-pod> -- \
  curl -s localhost:9641/metrics | grep turn_total_allocations
```

A relay that started but cannot bind its range logs a bind error and exits:
that means another process on the node already owns those ports (see the
partitioning rules in `docs/adr/0001-addressing-modes.md`).

## 4. Probe by hand

```sh
kubectl --context kind-turnfleet -n <ns> get secret <auth-secret> \
  -o jsonpath='{.data.<key>}' | base64 -d > /tmp/turn-secret
bin/turnfleet-probe --uri "turn:<relayAddress>:<port>?transport=udp" \
  --secret-file /tmp/turn-secret --expect-address <relayAddress> \
  --expect-port-range <start>-<end> --json
```

A failure from inside the cluster isolates configuration from reachability:
what it cannot see is hairpin NAT and firewalls in front of the node, which is
why `docs/capacity-model.md` says the external vantage point is the
authoritative check. Run the same command from the host against the node
address to test the external path.

## 5. Saturation

`turnfleet_allocation_failures_total{code="508"}` going up, with
`CapacityPressure` true, means the relay's range is genuinely exhausted and the
other relays are still serving. To reproduce it, allocate in a loop from a pion
client until the relay answers 508 — the e2e saturation case does exactly this.

Never widen a range or restart a relay to make a symptom disappear without
naming the cause; that is how the capacity illusion comes back.
