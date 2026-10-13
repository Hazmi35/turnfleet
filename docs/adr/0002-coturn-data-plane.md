# 2. coturn stays a separate, unmodified upstream image

* Status: accepted
* Date: 2026-10-13

## Context

Turnfleet needs a TURN data plane. Writing one is a multi-year project (RFC 5766
plus 5780, 6062, 6156, 8656 and the ICE/RTP interop matrix that follows). The
operator's job is placement, addressing, capacity accounting and observability,
not the relay itself.

The project is licensed AGPL-3.0-only. Vendoring a third-party data plane into
the repository would pull that code under the project's license header
conventions, force us to patch and track upstream, and create a second place
where relay behaviour is defined.

## Decision

The data plane is the upstream `coturn/coturn` image, pinned by tag **and**
digest:

```
coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3
```

It is used as-is. Nothing in this repository builds, patches or vendors coturn
sources, and the image keeps its own BSD-3-Clause license. The operator only
writes coturn configuration (mounted as a ConfigMap, see ADR 8) and passes a
small set of flags verified against that exact image.

`spec.dataplane.image` defaults to the pin above and must be a digest-pinned
reference; the validating webhook rejects a tag-only image so a pool can never
silently float to a new relay version.

## Consequences

* Relay upgrades are explicit: bump the digest in the pool spec, relays roll
  one at a time with drain (ADR 6).
* The operator cannot work around a coturn limitation by patching coturn. Flags
  that do not exist in the pinned version are simply not available: the
  renderer only emits flags that appear in the pinned image's `turnserver
  --help` output, which is captured as
  `internal/relay/testdata/coturn-4.18.0-r0-help.txt` and asserted by a unit
  test.
* Metrics are whatever coturn exposes (ADR 5); allocation failure codes that
  coturn does not expose are derived from the probe instead.
