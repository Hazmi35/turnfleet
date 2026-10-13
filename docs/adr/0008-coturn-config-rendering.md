# 8. coturn configuration is rendered into a ConfigMap, from verified flags

* Status: accepted
* Date: 2026-10-13

## Context

A relay needs: listener addresses and ports, relay address and port range,
external address mapping, realm, the shared secret, denied/allowed peer ranges,
and TLS material. There are two ways to get that into coturn: a long argument
list on the container command, or a rendered configuration file.

Arguments are not reviewable, cannot be diffed, and cannot carry a comment
explaining why a flag is there. They also cannot be validated as a unit, which
matters because a wrong `--min-port`/`--max-port` silently changes capacity.

There is a second trap: coturn's flag set has changed across releases, and a
flag that does not exist in the pinned version is either ignored or fatal
depending on the version. Guessing is not acceptable.

## Decision

`internal/relay` renders one coturn configuration file per relay and the
controller stores it in a ConfigMap keyed by relay name; the pod mounts it
read-only and coturn is started with `-c /etc/turnfleet/turnserver.conf`.

* Every flag emitted is checked against the pinned image: `turnserver --help`
  of `coturn/coturn:4.18.0-r0` is captured verbatim in
  `internal/relay/testdata/coturn-4.18.0-r0-help.txt`, and a unit test asserts
  that every flag the renderer can emit appears in that file. Adding a flag
  therefore requires re-capturing the help output of a new pin.
* The flags used are: `--listening-port`, `--tls-listening-port`,
  `--listening-ip` (`-L`), `--relay-ip` (`-E`), `--external-ip` (`-X`),
  `--min-port`, `--max-port`, `--realm`, `--lt-cred-mech`,
  `--use-auth-secret`, `--static-auth-secret`, `--no-multicast-peers`,
  `--denied-peer-ip`, `--allowed-peer-ip`, `--prometheus`,
  `--prometheus-port`, `--prometheus-path`, `--no-cli`, `--simple-log`,
  `--log-file`, and `--cert`/`--pkey` for TLS listeners.
* The rendered config is content-hashed into the pod's
  `turnfleet.hzmi.dev/config-hash` annotation. A change to the config or to the
  referenced Secret changes the hash, and the relay rolls (one at a time, with
  drain — ADR 6).
* Rendering is a pure function of `(pool spec, relay identity, node address)`
  and is covered by golden files, one per addressing mode, plus a fuzz target
  that asserts arbitrary specs never produce overlapping port ranges and never
  panic.

`spec.dataplane.extraArgs` exists as an escape hatch. Its entries are appended
verbatim, are part of the hash, and are **not** validated against the pinned
image — a documented, deliberate hole for operators who need a flag the CRD
does not model.

## Consequences

* Configuration is reviewable (`kubectl get cm <pool>-relay-<hash> -o yaml`) and
  is the first thing `e2e-debug` looks at.
* The "consider it a bug if you render a flag that does not exist" rule is
  mechanical, not a review habit.
* Denied peer ranges are asserted by a unit test that the default deny list
  covers RFC 1918, RFC 6598, loopback, link-local (including
  `169.254.169.254`) and the cluster pod/service CIDRs.
