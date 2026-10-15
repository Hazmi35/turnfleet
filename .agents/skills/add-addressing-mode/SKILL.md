---
name: add-addressing-mode
description: Add a new addressing mode to Turnfleet - implement Planner and Renderer, golden tests, an e2e case and an ADR. Use when a new way of assigning relay addresses or port ranges is needed.
---

# Adding an addressing mode

An addressing mode answers two questions: which node does a relay run on, and
which public relay identity `(address, port range)` does it own. Anything that
does not change capacity accounting is not a new mode.

1. **Extend the enum.** `api/v1alpha1/AddressingMode` plus the
   `+kubebuilder:validation:Enum` marker. Adding a value breaks every `switch`
   over the type by design, so fix them all (`exhaustive` enforces it).
2. **Implement `internal/relay.Planner`** for the mode:
   - Deterministic ordering (sort nodes and addresses) so identities are stable
     across restarts and controller failover.
   - A candidate set of relay identities: `(address, disjoint port range)`.
     Prove disjointness, do not assume it.
   - A relay moves only when its node or its address disappears. Anything else
     is a capacity bug the e2e suite will not catch.
   - Capacity honesty: if the mode shares one IP between relays, the plan
     must say so (`portsTotal` sums to one IP's port space, not more).
3. **Implement `internal/relay.Renderer`** for the mode: listening address,
   relay address, `-E`/`-X` mapping, port range, listeners, realm, secret,
   denied peers. Every coturn flag must already appear in
   `internal/relay/testdata/coturn-4.18.0-r0-help.txt`. Before adding a flag,
   re-capture that file from the pinned image (`docker run --rm <pinned coturn>
   turnserver --help`) and update `docs/adr/0008-coturn-config-rendering.md`.
4. **Golden tests**, one file per mode under `internal/relay/testdata/`, plus a
   planner test for stability, rebalancing and disjointness. The `FuzzRenderer`
   target must still pass.
5. **Webhook validation** for mode-specific config in
   `internal/webhook/v1alpha1/turnpool_webhook.go`: required fields present,
   forbidden fields absent, ranges partitioned without overlap.
6. **An e2e case** in `test/e2e/` that proves the mode with real relays: the
   advertised address is the expected one, the ranges are disjoint, and each
   relay answers a real TURN `Allocate`. Fixtures go in `test/fixtures/`.
7. **An ADR** `docs/adr/NNNN-addressing-mode-<name>.md`: what the mode is for,
   what it costs, what capacity it actually adds (often none), and what it
   forbids. Update the modes table in `docs/adr/0001-addressing-modes.md` and
   `docs/capacity-model.md` in the same change.
8. **Prove it**: `make ci` then `make test-e2e`.
