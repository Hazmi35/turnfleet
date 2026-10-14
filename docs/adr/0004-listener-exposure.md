# 4. Listener exposure: per-relay node ports, not a shared Service

* Status: accepted
* Date: 2026-10-13

## Context

Relay listeners (UDP/TCP 3478, TLS 5349 by default) are single, well-known
ports, so unlike the relay range they *can* be expressed as Service ports. The
question is whether they should be.

A client authenticates against the listener, then receives an allocation and a
relayed transport address. If the listener is a load-balanced Service, the
client's first packets may land on relay A, while the allocation it ends up
holding is on relay B. Worse, a client that is handed a URI for a relay that is
draining or out of ports gets a 508 only after the ICE gathering phase, which
is exactly the silent failure this project exists to remove.

## Decision

Listeners are bound on the relay pod's node address, alongside the relay range,
and every relay advertises its own listener URIs. There is no Service in front
of the listeners. Clients are handed the exact URI of the relay that will hold
their allocation.

The rendering rules that follow from this:

* `spec.listeners[].port` is per relay and must not fall inside
  `spec.relayPortRange` (rejected by the validating webhook: binding a listener
  inside the relay range means the first allocation can fail to bind).
* Listener ports are unique within a pool, and the same port is reused on every
  relay (`name` + `protocol` identifies a listener, not an address).
* TLS listeners reference a Secret in the pool's namespace
  (`tlsSecretRef`), which is mounted into the relay pod and read by coturn when
  it starts. The pod's `turnfleet.hzmi.dev/config-hash` covers the rendered
  configuration, the image, the resources, the placement and the content of that
  Secret, so rotating a serving certificate rolls the relays one at a time with
  drain, the way any other change to what a relay runs does.

## Consequences

* `turnURIs` in the status and in the `<pool>-turn-uris` ConfigMap is
  per-relay, and only contains ready, non-draining relays. The credential
  issuer can therefore hand out a URI that is known to allocate.
* Relaying for a client is confined to one relay for the lifetime of the
  allocation, which is what the address/range partitioning requires anyway.
* Operators must make the listener ports reachable on the relay nodes, the same
  way they make the relay range reachable. No extra Kubernetes object is
  needed, and there is nothing to debug in a Service path.
