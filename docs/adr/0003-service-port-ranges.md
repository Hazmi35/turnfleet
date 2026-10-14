# 3. Relay port ranges are never expressed as Service ports

* Status: accepted
* Date: 2026-10-13

## Context

The obvious design for "expose a relay port range on one public IP" is a
Service (or cloud load balancer) in front of the relay pods. That design does
not work, and the reasons are worth recording because the failure mode is
silent.

**Kubernetes Services have no port ranges.** In the pinned API
(`k8s.io/api@v0.37.1`, `core/v1/types.go`), `ServicePort` has a single
`int32 port` and an optional `NodePort`; there is no `endPort` field. Port
ranges exist in Kubernetes only in NetworkPolicy
(`NetworkPolicyPort.EndPort`, KEP-2079). Expressing a 16k-port relay range as a
Service therefore means 16k `ServicePort` entries — beyond the object size
limits, and still load balanced per flow.

**Cloud load balancers are per-port or single-range at best.**

* AWS NLB listeners are one port per listener: "It is not possible to configure
  port range on a single listener for an AWS Network Load Balancer (NLB) at
  this time. NLB listeners are designed to work with a single port per
  listener." — <https://www.repost.aws/questions/QUA_GotXcWRnaA0yLBBmNr2Q/nlb-listener-for-port-range>
* GCP forwarding rules do accept `portRange`, but one range per rule and it is
  mutually exclusive with `ports` and `allPorts`; a rule also front-ends a
  backend service whose endpoints are load balanced per flow, not per
  `(address, port)` pair. — <https://docs.cloud.google.com/compute/docs/reference/rest/v1/forwardingRules>

**And even a perfect port-range load balancer would be wrong**, because the
load balancing decision must be `(relay IP, relay port) → relay process`, while
L4 load balancers hash on the whole 5-tuple: peer-originated packets arrive
from an arbitrary source address:port and would be sprayed across relays.

## Decision

No Service, Ingress or cloud load balancer is placed in the relay data path.
Relay pods run with `hostNetwork: true` and bind their relay port range
directly on a node address, so the kernel delivers traffic addressed to
`(node address, port)` to the one process that owns that port (ADR 1).

Consequently the `<pool>-turn-uris` ConfigMap — the list of per-relay
`turn:`/`turns:` URIs the credential issuer hands out — is the only discovery
mechanism, and it only ever contains relays that are ready and not draining.
The issuer returns a URI for a relay that actually has capacity, instead of
one address that fronts a fleet.

## Consequences

* Capacity scales by adding relay addresses (nodes or secondary IPs), never by
  adding replicas behind one address.
* There is no Service to observe; per-relay reachability is checked by the
  probe (ADR 7), not by a Service endpoint.
* Firewall/security-group rules on relay nodes must allow the listener ports
  and the relay range, plus ICMP/UDP as required by the client network. This is
  documented in `docs/capacity-model.md` and is the operational cost of the
  design.
