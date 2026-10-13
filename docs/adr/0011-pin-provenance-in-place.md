# 11. Version pins live next to what they pin

* Status: accepted
* Date: 2026-10-13

## Context

`docs/versions.md` used to carry the whole pin set as tables: the Go toolchain,
the lint toolchain, every container image with its digest, every GitHub Action
with its SHA, transitive floors, the agent-tooling floor. Every number in those
tables was a copy of a number that already existed in `go.mod`,
`tools/go.mod`, the `Makefile`, the Dockerfiles, the workflows or
`.custom-gcl.yml` — five pin sources, roughly thirty duplicated values, plus a
"verification log" section whose job was to argue that the copies still agreed.

Nothing kept them in step. `make verify` runs the generated-file diff, the
header check, the harness-link check and the no-`CLAUDE.md` check; no script
compared a table row against its source, because none existed (`hack/` holds
`verify-dco.sh`, `verify-headers.sh`, `verify-harness-links.sh`,
`verify-no-claude-md.sh`). The release-check skill could only tell a human to
"diff them, do not trust the table", which is the tell: a document that cannot
be trusted is a document that will be wrong, and a stale digest or a stale
Action SHA in a table is worse than no table, because it reads as verified.

The tables also carried almost no information the manifests lacked. What could
not be recovered from the tree was small and prose-shaped: which pins are
deliberately *held* and why.

## Decision

A pin lives next to what it pins, and `docs/versions.md` keeps only what the
manifests cannot express.

* The numbers: `go.mod` `go`/`tool`, `tools/go.mod`, the `Makefile`,
  `.custom-gcl.yml`, the Dockerfiles, `.github/workflows/*.yaml`. The
  `docs/versions.md` copy is gone.
* The reasons: held modules (`k8s.io/kube-openapi`), vulnerability floors
  (`google.golang.org/grpc`, `go.opentelemetry.io/otel/*`, with the GO-2026 IDs
  that make them floors), the `tag@sha256:digest` policy and why the coturn tag
  exists beside its digest, the Action-SHA policy, the agent-tooling floor, and
  a provenance section for the cross-checks that were actually run.
* A bump moves every copy in one pull request. `renovate.json` groups the pins
  that must move as a unit, `.github/pull_request_template.md` asks for the
  rest, and a bump that moves a held pin updates the note in the same change
  (`AGENTS.md`, "Definition of done").

No `hack/verify-*.sh` was added to police the numbers: with one copy each there
is nothing to compare, and a checker for a document that no longer restates
anything would be the same ceremony in a new place.

## Consequences

* Duplication drops to the copies that are forced by reality — a pin that a
  workflow, a Dockerfile and the `Makefile` all need — and `docs/versions.md`
  no longer competes with `go.mod` as "the" answer for what version something is.
* `GOLANGCI_LINT_VERSION` was declared in the `Makefile` and never read by any
  target (`make golangci-lint` builds the base binary from `tools/go.mod` and
  the plugin set from `.custom-gcl.yml`); it was deleted as part of this change
  rather than carried as a fourth copy of a pin nothing used.
* The held-pin knowledge now has to be written down deliberately when a hold is
  introduced, instead of appearing as one more table row. That is the intended
  cost: a hold without a reason is the thing that gets silently reverted.
* A reader who expects a central table has to be told where the pins are, which
  is why the reason is stated in `AGENTS.md`, the pull request template, the
  `Makefile` header and the release-check skill, not only here.
