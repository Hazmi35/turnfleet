# 3. Relay port ranges are never expressed as Service ports

* Status: accepted
* Date: 2026-10-13

## Context

The obvious design for exposing a relay port range on one public IP is a
Service (or a cloud load balancer) in front of the relay pods. That design does
not work, and the reasons are worth recording, because the failure mode is
silent.

**Kubernetes Services have no port ranges.** In the pinned API
(`k8s.io/api@v0.37.1`, `core/v1/types.go`), `ServicePort` has a single
`int32 port` and an optional `NodePort`, and there is no `endPort` field. Port
ranges exist in Kubernetes only in NetworkPolicy
(`NetworkPolicyPort.EndPort`, KEP-2079). Expressing a 16k-port relay range as a
Service therefore means 16k `ServicePort` entries, which is beyond the object
size limits and is still load balanced per flow.

**Cloud load balancers are per-port or single-range at best.**

* AWS NLB listeners are one port per listener: "It is not possible to configure
  port range on a single listener for an AWS Network Load Balancer (NLB) at
  this time. NLB listeners are designed to work with a single port per
  listener."
  (<https://www.repost.aws/questions/QUA_GotXcWRnaA0yLBBmNr2Q/nlb-listener-for-port-range>)
* GCP forwarding rules do accept `portRange`, but one range per rule, and
  `portRange` is mutually exclusive with `ports` and `allPorts`. A rule also
  front-ends a backend service whose endpoints are load balanced per flow, not
  per `(address, port)` pair.
  (<https://docs.cloud.google.com/compute/docs/reference/rest/v1/forwardingRules>)

**And even a perfect port-range load balancer would be wrong**, because the
load balancing decision must be `(relay IP, relay port) → relay process`. L4
load balancers hash on the whole 5-tuple instead, and peer-originated packets
arrive from an arbitrary source address:port, so they would be sprayed across
relays.

*(Corrected by ADR 19.)* The 5-tuple hashing above is the common L4 design, not
the only one. A GCP forwarding rule can front one port range per backend
service, and Global Accelerator custom routing keys on the destination port
alone. The durable reasons to keep a load balancer out of the relay data path
are narrower, and ADR 19 records them:

* it puts a load balancer in the peer's path;
* the address the peer dials belongs to the balancer and not to the relay;
* it cannot carry the drain semantics of ADR 6.

## Decision

No Service, Ingress or cloud load balancer is placed in the relay data path.
Relay pods run with `hostNetwork: true` and bind their relay port range
directly on a node address, so the kernel delivers traffic addressed to
`(node address, port)` to the one process that owns that port (ADR 1).

Consequently the `<pool>-turn-uris` ConfigMap is the only discovery mechanism.
It holds the list of per-relay `turn:`/`turns:` URIs that the credential issuer
hands out, and it only ever contains relays that are ready and not draining.
The issuer returns a URI for a relay that actually has capacity, instead of
one address that fronts a fleet.

## Consequences

* Capacity scales by adding relay addresses (nodes or secondary IPs), never by
  adding replicas behind one address.
* There is no Service to observe, so per-relay reachability is checked by the
  probe (ADR 7), not by a Service endpoint.
* Firewall/security-group rules on relay nodes must allow the listener ports
  and the relay range, plus ICMP/UDP as required by the client network. This is
  documented in `docs/capacity-model.md` and is the operational cost of the
  design.
