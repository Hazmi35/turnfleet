# Triage Labels

The skills use five canonical triage roles. This file maps each role to the label string this repo's issue tracker uses.

| Label in mattpocock/skills | Label in our tracker | Meaning |
|---|---|---|
| `needs-triage` | `needs-triage` | Maintainer needs to evaluate this issue |
| `needs-info` | `needs-info` | Waiting on reporter for more information |
| `ready-for-agent` | `ready-for-agent` | Fully specified, ready for an AFK agent |
| `ready-for-human` | `ready-for-human` | Requires human implementation |
| `wontfix` | `wontfix` | Will not be actioned |

When a skill mentions a role (e.g. "apply the AFK-ready triage label"), use the matching label string from this table.

## This repo

All five labels exist on `Hazmi35/turnfleet` with these strings; `wontfix` predates this file. The five roles are triage state, not category: a triaged issue carries exactly one of them plus a category label (`bug`, `enhancement`, `documentation`, …).

## Area labels

Work that is planned but not scheduled also carries the `roadmap` label and exactly one `area:*` label. Both are category metadata, not triage state, so they never replace the role.

| Label | Covers |
|---|---|
| `roadmap` | The backlog that is planned but not scheduled; `gh issue list --label roadmap` is the whole of it. The initial set was extracted from the retired `docs/status.md` next-steps list. |
| `area:release` | Release automation and packaging: signing, SBOM, provenance, chart. |
| `area:addressing` | Addressing modes and relay identity placement. |
| `area:capacity` | Capacity sources, address supply and the capacity model. |
| `area:observability` | Metrics, probes and external vantage points. |
| `area:docs` | Examples and documentation deliverables. |

An issue without `roadmap` is still work in the tracker: the label marks what came out of the retired next-steps list, not the only work worth doing. Add a new `area:*` string only when no row above covers that family of work, and record the row here in the same change.
