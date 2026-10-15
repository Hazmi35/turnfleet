# 2. coturn stays a separate, unmodified upstream image

* Status: accepted
* Date: 2026-10-13

## Context

Turnfleet needs a TURN data plane. Writing one is a multi-year project (RFC 5766
plus 5780, 6062, 6156, 8656 and the ICE/RTP interop matrix that follows). The
operator's job is placement, addressing, capacity accounting and observability,
not the relay itself.

The project is licensed AGPL-3.0-only. Vendoring a third-party data plane into
the repository would have three results:

* the vendored code would come under the project's license header conventions;
* the project would have to patch it and track upstream;
* relay behaviour would be defined in a second place.

## Decision

The data plane is the upstream `coturn/coturn` image, pinned by tag **and**
digest:

```
coturn/coturn:4.18.0-r0@sha256:bbefd3e1fdfdc0d58770fe01b581fd8b00d9f3a5580d00acb77cf719a6bc78e3
```

It is used as-is. Nothing in this repository builds, patches or vendors coturn
sources, and the image keeps its own BSD-3-Clause license. The operator only
writes coturn configuration (mounted from a Secret, see ADR 20) and passes a
small set of flags verified against that exact image.

`spec.dataplane.image` must be a digest-pinned reference. The field is required
and has no default, and the validating webhook rejects an empty or tag-only
image. A pool therefore always names the image it runs, and can never silently
float to a new relay version.

## Consequences

* Relay upgrades are explicit: bump the digest in the pool spec, relays roll
  one at a time with drain (ADR 6).
* The operator cannot work around a coturn limitation by patching coturn. Flags
  that do not exist in the pinned version are not available, because the
  renderer only emits flags that appear in the pinned image's `turnserver
  --help` output. That output is captured as
  `internal/relay/testdata/coturn-4.18.0-r0-help.txt` and asserted by a unit
  test.
* Metrics are whatever coturn exposes (ADR 5). Allocation failure codes that
  coturn does not expose are derived from the probe instead.
