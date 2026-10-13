# AGENTS.md

Turnfleet is a Kubernetes operator (`turnfleet-controller`) for TURN relay
fleets and a standalone relay probe (`turnfleet-probe`). A `TURNPool` declares
realm, auth, listeners, relay port range and an addressing mode; the operator
places **relays** — one TURN process per public relay identity `(IP, port
range)` — on nodes labelled `turnfleet.hzmi.dev/relay-node=true`, renders
coturn configuration for each, publishes per-relay `turn:` URIs, and keeps
them reachable and observable. Capacity is bounded per public IP by the port
space, never by the number of pods: adding replicas behind one address adds
nothing. This file is the only instruction file for every agent; Claude Code
reads it natively when no `CLAUDE.md` exists (see "Never").

## Glossary

* **Pool** — one `TURNPool` resource: relays sharing a realm and auth.
* **Relay** — one TURN process with its own relay identity. Never "instance",
  "replica" or "server" in API, code, docs or metrics.
* **Allocation** — a TURN allocation held on a relay; it occupies one port.
* **Relay address** — the public IP a relay advertises in
  `XOR-RELAYED-ADDRESS`.
* **Relay node** — a node labelled `turnfleet.hzmi.dev/relay-node=true`.
* **Draining** — a relay on its way out: removed from `turnURIs`, readiness
  false, deleted only when allocations reach zero or `drainTimeout` expires.

Conventions that are load-bearing: API group `turnfleet.hzmi.dev`, version
`v1alpha1`, kind `TURNPool` (plural `turnpools`, short `tpool`); label,
annotation and finalizer prefix `turnfleet.hzmi.dev/`; metric prefix
`turnfleet_`; pod names `<pool>-relay-<5-char hash>`; URI ConfigMap
`<pool>-turn-uris`; kind cluster `turnfleet`, context `kind-turnfleet`.

## Commands

```sh
make help                 # every target, one line each
make fmt vet              # format (gofumpt+gci) and go vet
make lint                 # golangci-lint (builds bin/golangci-lint, plugins included)
make lint-fix             # lint with the fixes it can apply
make verify-fast          # what the pre-stop hook runs: fmt, vet changed, lint new
make generate manifests   # deepcopy, CRDs, RBAC, webhooks  (never hand-edit the output)
make api-docs             # docs/api.md from api/v1alpha1
make test                 # unit + envtest (KUBEBUILDER_ASSETS is set for you)
make test-fuzz            # fuzz the renderer and the webhook validators
make build                # bin/turnfleet-controller, bin/turnfleet-probe
make kind-up kind-down    # local 1 control-plane + 3 relay-node workers
make install deploy undeploy
make test-e2e             # full kind suite; loads images, installs, deploys
make verify               # generated files, headers, no CLAUDE.md, no diff
make vuln licenses ci     # govulncheck, AGPL-3.0-only dependency check, everything CI runs
make dco                  # every commit in the range is signed off
```

Run `make ci` before considering any change done. Run `make test-e2e` when a
change affects relay behaviour, rendering, drain, or the probe.

## Architecture map

| Path | Responsibility |
| --- | --- |
| `api/v1alpha1/` | `TURNPool` types. Markers only, no logic. Ordering and naming follow the Kubernetes API conventions; `kubeapilinter` enforces them. |
| `internal/relay/` | `Planner` assigns relay identities (address + disjoint port range) per addressing mode; `Renderer` turns one relay into coturn config. Pure functions: no client, no clock, no I/O. |
| `internal/probe/` | Real TURN `Allocate` against a relay listener on `pion/turn`; result type shared by controller and CLI. |
| `internal/metrics/` | `turnfleet_` metric registration and the coturn scrape/parse helpers. |
| `internal/controller/` | Reconcile: planner + renderer + pods + ConfigMaps + status, drain state machine, config-hash rollout. |
| `internal/webhook/v1alpha1/` | Defaulting and validating webhooks, immutability, mode-specific validation. |
| `cmd/turnfleet-controller/`, `cmd/turnfleet-probe/` | Binaries; flag parsing and wiring only. |
| `test/e2e/` | kind end-to-end suite (build-tagged `e2e`), `test/fixtures/` holds cluster fixtures. |
| `config/` | kustomize manifests. `config/default` (what `make deploy` applies) runs the controller without admission webhooks; `config/with-webhook` adds them and their cert-manager certificates. |
| `docs/` | `architecture.md`, `capacity-model.md`, `api.md` (generated), `adr/`, `versions.md`, `status.md`. |

## Agent harness

Skills, subagent definitions and hook policy live once and are wired into each
harness instead of being maintained per harness:

| Path | What it is |
| --- | --- |
| `.agents/skills/<name>/SKILL.md` | Canonical skills; `.claude/skills/<name>/SKILL.md` is a symlink to it. |
| `.agents/agents/<name>.md` | Canonical subagent definitions; `.claude/agents/<name>.md` and `.omp/agents/<name>.md` are symlinks to them. |
| `hack/agent/*.sh` | Hook policy (kubectl guard, Go formatting, `make verify-fast`), harness-neutral: arguments in, exit code out. See `hack/agent/README.md`. |
| `.claude/hooks/*.sh` | Claude Code adapters: stdin JSON in, Claude's decision JSON or exit 2 out. `.claude/settings.json` holds Claude's permission lists, which are harness-specific. |
| `.omp/hooks/{pre,post}/*.ts` | omp adapters over the same scripts, on `tool_call` and `tool_result`. |

Edit the canonical file under `.agents/`; `hack/verify-harness-links.sh` (part of
`make verify`) fails when an alias is missing, points elsewhere, or was replaced
by a copy.

## Conventions

* Errors: wrap with `%w` and enough context to act on; never return a bare
  third-party error from a reconcile path.
* Logging: `logr` key-value pairs, lower-camel keys, odd number of args is a
  bug (`logcheck` enforces it). Log the relay identity, not the whole object.
* API: no floats, no booleans where a string enum reads better, every field
  explicitly `+optional` or `+required`, lists carry `+listType`, conditions
  are set with `meta.SetStatusCondition`. Status is derived, never a cache of
  the spec.
* Context: every client call and every probe takes and honours the `context`
  from `Reconcile`; never `context.Background()` inside a reconcile path.
* Reconcile is level-based and idempotent: compute the desired relay set, then
  converge. Owners and the `turnfleet.hzmi.dev/config-hash` annotation decide
  what rolls.
* Tests: table-driven where it fits, golden files for the renderer (one per
  addressing mode), `Eventually` with explicit timeouts, no `time.Sleep` for
  synchronisation, fixed seeds in fuzz seeds.
* Naming lives in the "Glossary" section above. If you need a new term, add it
  there in the same change.

## Definition of done

1. `make ci` is green (verify, lint, test, vuln, licenses, dco).
2. Behavioural changes are exercised: a unit/envtest test that fails without
   the change, or `make test-e2e` for anything touching a real relay.
3. Generated files are regenerated and committed (`make generate manifests
   api-docs`), and `docs/api.md` reflects API changes.
4. Docs and an ADR are updated: `docs/architecture.md`, `docs/capacity-model.md`
   or `docs/status.md` as applicable; a decision that was not obvious gets
   `docs/adr/NNNN-title.md`.
5. A version bump that moves a pin `docs/versions.md` records as held (a held
   module, a vulnerability floor, an image digest) updates that note in the same
   pull request.
6. Commits are Conventional Commits with `git commit -s` (DCO).

## Never

* Hand-edit generated files (`zz_generated.*`, `config/crd/bases/*`,
  `config/rbac/role.yaml`, `docs/api.md`). Regenerate them.
* Disable a linter without a `//nolint:<linter> // <reason>` scoped to the
  line, or add a broad exclusion to `.golangci.yml`.
* Change API types without regenerating and updating `docs/api.md`.
* Bump a version that `docs/versions.md` records as held (a held module, a
  vulnerability floor, an image digest) without moving that note with it.
* Run `kubectl` against any context other than `kind-turnfleet`
  (`hack/agent/guard-kubectl.sh`, wired into every harness, blocks it); `make`
  sets the context for you.
* Create `CLAUDE.md` or `CLAUDE.local.md`: either shadows this file, and
  `make verify` fails if one exists.
* Edit a `.claude/` or `.omp/` alias, or replace it with a copy: the content
  under `.agents/` is canonical, and `make verify` checks the links.
* Put a Service, Ingress or load balancer in the relay data path, or move the
  relay range behind one public IP: that is the bug this project removes.
* Change a field that invalidates live allocations (relay port range,
  addressing mode, realm) in place; the webhook rejects it, and the correct
  move is a new pool.
* Use floats in the API or in status; consumers compute ratios.
