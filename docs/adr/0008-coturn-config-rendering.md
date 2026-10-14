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
  `--min-port`, `--max-port`, `--realm`, `--use-auth-secret`,
  `--static-auth-secret`, `--no-multicast-peers`, `--denied-peer-ip`,
  `--allowed-peer-ip`, `--prometheus`, `--prometheus-port`,
  `--prometheus-path`, `--log-file`, `--simple-log`, `--fingerprint`,
  `--no-udp`/`--no-tcp`/`--no-tls` (only the transports the pool declares), and
  `--cert`/`--pkey` for TLS listeners.
* Three behaviours of the pinned image were verified against it rather than
  assumed, and they shape the rendering:
  * **`--denied-peer-ip` does not accept CIDR.** coturn aborts with "invalid
    denied-peer-ip value 203.0.113.0/24. Use IP or IP-IP range (CIDR is not
    supported)", so every prefix is expanded to `first-last` by the renderer.
  * **`--no-cli` is deprecated** in this version, so neither it nor `--cli` is
    rendered; the CLI is off by default.
  * **TCP relay endpoints are allocated from `--min-port`/`--max-port` too**
    (`turnports_create` builds the UDP and TCP port maps from the same bounds),
    so `--no-tcp-relay` is *not* rendered: TCP relays stay available for clients
    that need them and still count against the pool's capacity. Verified by
    allocating over TCP against the pinned image and observing a relayed port
    inside the configured range.
  * **An IPv6 relay uses the same flags with IPv6 values, and they were verified
    the same way.** `--listening-ip`, `--relay-ip` and `--external-ip` take a
    bare IPv6 address (`-X` is split on `/`, so a public/private pair needs no
    brackets), and `--denied-peer-ip` takes IPv6 ranges. Running the pinned
    image with a rendered v6 configuration starts it; a probe allocates on
    `::1` and reads the v6 `XOR-RELAYED-ADDRESS`; `-X 2001:db8:ffff::77/fd00::11`
    is reported as the relayed address instead of the bound one; and a peer in a
    denied v6 range is refused with TURN error 403. The v6 values are rendered
    from `::/128`, `::1/128`, `::ffff:0:0/96`, `fc00::/7`, `fe80::/10`,
    `ff00::/8` and the pool's own ranges.
* Authentication uses `--use-auth-secret` with `--static-auth-secret` only.
  `--lt-cred-mech` is not rendered: coturn warns when both are set, and the
  shared-secret mechanism is the one the credential issuer uses. This was
  verified end to end against the pinned image — a probe using credentials
  derived from the shared secret allocates successfully, and the same probe with
  a wrong secret is refused with TURN error 401.
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

* Configuration is reviewable (`kubectl get cm <pool>-relay-config -o yaml`,
  one key per relay) and is the first thing `e2e-debug` looks at.
* The "consider it a bug if you render a flag that does not exist" rule is
  mechanical, not a review habit.
* Denied peer ranges are asserted by the golden render tests: the IPv4 golden
  files carry the default list (RFC 1918, RFC 6598, loopback, link-local
  including `169.254.169.254`, benchmarking, multicast, reserved), and the IPv6
  ones the family-specific list (loopback, link-local, unique local addresses,
  multicast and the IPv4-mapped space `::ffff:0:0/96`, which is where the
  metadata address lives for an IPv6 peer). That a relay denies its own
  cluster's addresses is an e2e case, not a unit test.
