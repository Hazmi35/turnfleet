# 20. The rendered relay configuration is delivered as a Secret

* Status: accepted
* Date: 2026-10-15
* Supersedes [0008](0008-coturn-config-rendering.md) on the object the
  configuration is stored in. Everything 0008 decides about *what* is rendered
  — the flags, the golden files, the help-text check — stands.

## Context

A relay's rendered configuration carries the listener addresses and ports, the
relay address and port range, the realm, the peer policy and
`--static-auth-secret`, the shared HMAC secret the credential issuer mints
with. Whoever can read it can mint credentials for the pool's relays.

coturn as pinned (`coturn/coturn:4.18.0-r0`) takes that secret as a string and
nothing else: the pinned `--help` — the file every rendered flag is checked
against (ADR 8) — has `--static-auth-secret <secret>` and no flag that reads
the value from a file. The secret therefore has to reach the process either in
the configuration file or on the command line.

ADR 8 put the file in a ConfigMap keyed by relay name. Two consequences of that
are avoidable:

* Reading the rendered configuration needs `get` on ConfigMaps in the pool's
  namespace, which is a much wider permission than reading one Secret: a
  namespace with a ConfigMap-reading workload or a cluster-wide dump exposes
  the shared secret of every pool.
* The secret sits in the object type that gets copied around — `kubectl get cm
  -A -o yaml`, a debugging bundle, a GitOps diff — because nothing marks it as
  sensitive.

## Decision

**The rendered configuration lives in a Secret named `<pool>-relay-config`,
with one key per relay.** The layout, the name, the labels, the owner reference
and the mount path are what they were; the pod projects its own relay's key to
`/etc/turnfleet/turnserver.conf` from a `secret` volume instead of a
`configMap` one, and coturn is still started with `-c`. Only the object the file
comes from changes.

* **Not a Secret-backed environment variable with the secret on the command
  line.** coturn would then get `--static-auth-secret=$(TURN_SECRET)` as an
  argument rather than through the rendered file. That moves a flag out of the
  file whose flags are verified against the pinned help text, puts the secret in
  the process arguments where any `ps` in the container sees it, and does not
  remove the need for the configuration hash to follow the secret: an updated
  Secret does not restart a container, so a rotation would still have to roll
  the relay from the hash.
* **Not an init container or entrypoint shell that writes the file.** The same
  trade with a wrapper script in the data path, and the pinned image needs no
  shell to run coturn.
* **The object the configuration is read from is part of the relay's config
  hash** (`ConfigSource` in the projection, deliberately not `omitempty`). A
  pod that still mounts the ConfigMap an earlier release wrote reads exactly
  the same bytes, so without it no input would change and the fleet would keep
  both objects alive indefinitely. With it, an operator upgrade rolls every
  fleet once, one relay at a time, under drain (ADR 6).
* **The superseded ConfigMap is deleted in the same pass**, when it exists and
  the pool controls it. A running relay keeps the file it read at startup, and
  the roll from the hash replaces it; a ConfigMap of that name the pool does not
  own is left alone. The cleanup goes away once no supported upgrade starts from
  a release that wrote it.
* **The secret is watched** the way the published ConfigMaps are (`Owns`), so a
  configuration Secret deleted under a running relay comes back on the next
  pass rather than at the next resync.

What this does not buy: etcd still stores the object unencrypted unless the
cluster runs with encryption at rest. The gain is the permission a reader needs,
the object type a namespace-wide dump exposes, and the process arguments.

## Consequences

* Reading a relay's configuration needs `get` on Secrets in the pool's
  namespace: `kubectl get secret <pool>-relay-config -o
  jsonpath='{.data.<relay>}' | base64 -d`. `e2e-debug`, `docs/architecture.md`
  and ADR 8's reviewability bullet say so.
* The controller's ClusterRole gains `create` and `update` on Secrets — it
  already had `get`/`list`/`watch` for the shared and serving secrets. Clusters
  whose policy allows Secrets to be written only by named controllers need that
  policy extended to `turnfleet-controller`.
* `spec.replicas` stays bounded at 256: a Secret is capped at 1 MiB like a
  ConfigMap, a rendered configuration is ~1 KB, and base64 costs a third on top
  of that. `docs/limitations.md` records the object type.
* The rendered configuration is no longer visible to a reader who can only read
  ConfigMaps. That is the point, and it is the one operational cost.
