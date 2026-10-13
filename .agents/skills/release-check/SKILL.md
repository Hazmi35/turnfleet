---
name: release-check
description: What must be true before tagging a Turnfleet release. Use when preparing a tag or when asked whether the repository is releasable.
---

# Release check

Do not tag until every line below is true. Report the command output as
evidence for each one; "looks fine" is not evidence.

## Required

1. **Clean history on `main`**: no merge commits, Conventional Commits, every
   commit signed off. `make dco` and `git log --oneline main`.
2. **`make ci`** green on the exact commit being tagged: verify, lint, test,
   vuln, licenses, dco.
3. **`make test-e2e`** green: all three addressing modes, saturation, peer
   policy, drain with an active allocation, and `turnfleet-probe` against a real
   relay.
4. **Version pins agree**: each pin's copies in `go.mod`, `tools/go.mod`, the
   `Makefile`, `.custom-gcl.yml`, the Dockerfiles and the workflows hold the same
   value, and the held modules and vulnerability floors described in
   `docs/versions.md` still describe what is in the tree. Diff them.
5. **`docs/api.md` and the CRDs** are regenerated from the tagged commit
   (`make verify` is the check).
6. **`docs/status.md`** reflects reality: what is done, known limitations,
   next steps. Anything that changed behaviour in this release is described
   there.
7. **Images build for both architectures** (`make docker-buildx` locally or the
   `image.yaml` workflow on `main`), and both carry
   `org.opencontainers.image.licenses=AGPL-3.0-only` and
   `org.opencontainers.image.source`.
8. **License obligations**: `make licenses` is green; `LICENSE` is the verbatim
   AGPL-3.0 text; the coturn image is still the unmodified upstream image
   (ADR 2).
9. **No `CLAUDE.md`/`CLAUDE.local.md`** anywhere (`make verify`).

## Out of scope today

Signing, SBOM publication, provenance attestations and a Helm chart are listed
in `docs/status.md` as not yet implemented. Do not claim them in release notes.

## After tagging

* Record the tag and the digest of both published images in the release notes.
* Bump the image tags referenced from `config/` if the chart uses fixed tags.
* Re-run `make verify` on the tag: a tag is a commit, not a branch state.
