---
name: api-change
description: Change the TURNPool API types safely - edit types and markers, regenerate, update webhooks, tests and docs. Use whenever a field is added, removed or re-marked in api/v1alpha1.
---

# Changing the TURNPool API

Work in this order; each step catches a different class of mistake.

1. **Edit `api/v1alpha1/turnpool_types.go`.**
   - Every field is explicitly `+optional` or `+required`. No floats; ratios are
     `portsInUse`/`portsTotal` integers that consumers divide.
   - Lists get `+listType=map` + `+listMapKey=<key>` (or `+listType=atomic` with
     a one-line reason), maps are avoided.
   - Status fields are `+optional` and derived from the spec, never cached
     from it. Conditions stay the first status field, set with
     `meta.SetStatusCondition`.
   - New enum values: extend the Go type, the `+kubebuilder:validation:Enum`
     marker, and every `switch` over it (the `exhaustive` linter fails the
     build otherwise). Add the docs line in the same edit.
2. **Decide mutability.** Anything that would invalidate live allocations is
   immutable: relay port range, addressing mode and its per-node split, realm.
   Adding it to the immutability check in
   `internal/webhook/v1alpha1/turnpool_webhook.go` is part of the change, with
   the reason in the validation message.
3. **Regenerate** (`make generate manifests api-docs`). Never hand-edit
   `zz_generated.deepcopy.go`, `config/crd/bases/*`, `config/rbac/role.yaml` or
   `docs/api.md`.
4. **Update the webhooks**: defaulting for anything with a sensible default,
   validating for the rules the CRD schema cannot express (mode-specific
   config, listener port uniqueness, ports outside the relay range, overlap
   between relays sharing an address).
5. **Update tests**:
   - `internal/webhook/v1alpha1/turnpool_webhook_test.go` for defaulting and
     each rejection (envtest, table-driven).
   - `internal/relay` golden files if the rendered coturn config changes; a new
     field that reaches the data plane always changes the goldens.
   - `test/e2e/` if the field is observable from outside.
6. **Update docs**: `docs/api.md` is generated; `docs/architecture.md` and
   `docs/capacity-model.md` are not. A new addressing mode also needs an ADR
   (`add-addressing-mode`).
7. **Prove it**: `make ci`. If the change touches the data plane,
   `make test-e2e`.

Report which files changed and which command proved the change.
