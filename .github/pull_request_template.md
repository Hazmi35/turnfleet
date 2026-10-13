# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Pull request template. The checklist is the short version of docs/status.md
# and AGENTS.md: what has to be true before a change lands.

## What and why

<!-- One paragraph. Link the issue if there is one. -->

## Checklist

- [ ] `make ci` passes locally.
- [ ] If the change affects relay behaviour, `make test-e2e` passes.
- [ ] Any version bump in this pull request moves every copy of that pin
      (`go.mod`, `tools/go.mod`, the `Makefile`, `.custom-gcl.yml`, the
      Dockerfiles, the workflows) and updates `docs/versions.md` when the pin
      was deliberately held.
- [ ] API changes: `make generate manifests api-docs` was run and the generated
      files are part of this pull request.
- [ ] New or changed behaviour is covered by a test that would fail without the
      change.
- [ ] Documentation reflects the change (`README.md`, `docs/*.md`), and a
      decision that was not obvious got an ADR in `docs/adr/`.
- [ ] No `CLAUDE.md` or `CLAUDE.local.md` was added: `AGENTS.md` is the single
      agent instruction file.

## Notes for reviewers

<!-- Anything surprising: a tradeoff, a follow-up, a known limitation. -->
