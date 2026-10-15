# 19. One address is never split across nodes

* Status: accepted
* Date: 2026-10-15

## Context

`PartitionedIP` splits one address's port range across relays on one node. #12
asked whether the same range can span nodes. Delivering `(address, port)` needs
a layer that reads the destination port, and only two exist: the node the
address is bound on (ADR 1), and a load balancer in the peer's data path, which
ADR 3 and ADR 4 exclude. Cloud routing matches destination prefixes only; a
shared cloud IP is bounded far below a relay range (GCP: 50 ports per protocol
on an internal VIP; AWS: 50 listeners per NLB, not adjustable); and AWS Global
Accelerator custom routing, the one product keyed on the destination port alone,
does no health checks and no failover, has an immutable port mapping, is
IPv4-only, and fronts the address itself. The remaining options are one address
per relay by another name: a floating address serves one node at a time, and an
anycast prefix cannot encode a port, needs a covering /24, and fails stateful
transports (RFC 7094).

## Decision

No addressing mode places one address on more than one node. A relay's port
range is served by the node that holds the address. Capacity scales by more
relay addresses: another relay node (`HostNetwork`), another declared address on
a node already used (`ExternalIPPool`, #11), or a routable pod or secondary
address (#15). The alternatives are recorded as a rejected approach in
`docs/capacity-model.md`.

## Consequences

* Two relays share one address only in `PartitionedIP`, and only on one node.
* No mode depends on a cloud feature outside the address/port model, so the data
  path stays a socket bound on a node (ADR 3, ADR 4).
* `status.portsTotal` keeps its meaning: distinct addresses × ports per address.
* This rejects the mechanisms that exist today, not the idea. A layer that
  routes `(address, port)` would have to carry per-target health checks (a drain
  must black-hole nothing, ADR 6), leave the peer-facing address with the relay
  that binds and advertises it (ADR 3), preserve the relayed port, and cover
  both address families. None of the products below does all four, and one that
  did would reopen this decision.
* ADR 3's claim that a port-range load balancer would be wrong *because* L4 load
  balancers hash the 5-tuple was too strong; the correction is recorded there.

## Evidence

* Routing has no port dimension, and an address is on one node at a time: an AWS
  route table entry "specifies a destination (CIDR block or prefix list)" and
  traffic is routed "based on its destination IP address"
  (<https://docs.aws.amazon.com/vpc/latest/userguide/VPC_Route_Tables.html>);
  on GCP "only one resource at a time can use a static external IP address"
  (<https://docs.cloud.google.com/vpc/docs/reserve-static-external-ip-address>).
* Port-aware front ends exist and are load balancers: a GCP forwarding rule
  forwards "only packets addressed to ports in the specified range", and a
  shared internal VIP carries 50 ports per protocol
  (<https://docs.cloud.google.com/compute/docs/reference/rest/v1/forwardingRules>,
  <https://docs.cloud.google.com/load-balancing/docs/internal/multiple-forwarding-rules-same-ip>);
  an NLB has "Listeners per Network Load Balancer: 50", Adjustable "No"
  (<https://docs.aws.amazon.com/elasticloadbalancing/latest/network/load-balancer-limits.html>).
* Global Accelerator custom routing keys on the destination port, and "does not
  perform health checks" and "does not failover", with a port mapping that
  "never changes"
  (<https://docs.aws.amazon.com/global-accelerator/latest/dg/about-custom-routing-how-it-works.html>,
  <https://docs.aws.amazon.com/global-accelerator/latest/dg/about-custom-routing-guidelines.html>).
* Anycast cannot express a port, and stateful transports "fail
  probabilistically, but possibly catastrophically" over it (RFC 7094), while
  RFC 4786 §4.1 requires the routing decision to outlast the transaction — a
  TURN allocation being exactly that long-lived state
  (<https://www.rfc-editor.org/rfc/rfc7094.txt>,
  <https://www.rfc-editor.org/rfc/rfc4786.txt>).
* coturn needs the relayed port preserved: "NAT must forward ports directly
  (relayed port 12345 must be always mapped to the same 'external' port 12345)"
  (<https://github.com/coturn/coturn/blob/master/README.turnserver>).
