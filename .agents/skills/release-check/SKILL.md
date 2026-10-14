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
2. **`make ci`** green on the exact commit being tagged: verify, helm-verify,
   lint, test, vuln, licenses, dco.
3. **`make test-e2e`** green: all three addressing modes, saturation, peer
   policy, drain with an active allocation, and `turnfleet-probe` against a real
   relay. **`make test-e2e-chart`** green too: the chart has to install the CRDs
   and the controller and probe a relay from that install, which `make
   helm-verify` does not exercise (it lints and compares renders, it installs
   nothing).
4. **`charts/turnfleet`'s `appVersion` is the image tag being released**, on the
   commit being tagged, not `dev`. The chart installs that tag by default, so
   bumping it after the tag leaves the tag carrying the previous one.
5. **Version pins agree**: each pin's copies in `go.mod`, `tools/go.mod`, the
   `Makefile`, `.custom-gcl.yml`, the Dockerfiles and the workflows hold the same
   value, and the held modules, vulnerability floors and the held cosign pin
   described in `docs/versions.md` still describe what is in the tree. Diff them.
6. **`docs/api.md` and the CRDs** are regenerated from the tagged commit
   (`make verify` is the check).
7. **The docs reflect reality**: `docs/architecture.md` and
   `docs/capacity-model.md` describe what the tree actually does,
   `docs/limitations.md` lists what it does not, and no document claims a
   feature that is not implemented. Anything that changed behaviour in this
   release is described by the document that owns it.
8. **Images build for both architectures** (`make docker-buildx` locally or the
   `image.yaml` workflow on `main`), and both carry
   `org.opencontainers.image.licenses=AGPL-3.0-only` and
   `org.opencontainers.image.source`.
9. **License obligations**: `make licenses` is green; `LICENSE` is the verbatim
   AGPL-3.0 text; the coturn image is still the unmodified upstream image
   (ADR 2).
10. **`.github/workflows/release.yaml` is on the commit being tagged**: a tag push
   runs that file as the tag's tree has it, and it is what signs the digests,
   attaches the SBOM and the provenance and writes the release notes (ADR 16).
   Release notes may claim the chart: it ships in this tree, and
   `make test-e2e-chart` proves it installs.

## After tagging

* Record the tag and the digest of both published images in the release notes.
* Watch the Release workflow for the tag. It waits for the Image workflow to
  publish `ghcr.io/hazmi35/turnfleet-{controller,probe}:<tag>`, signs both index
  digests keyless, attaches an SPDX SBOM and SLSA provenance to each, verifies
  all three with the identity it records, and writes the release body from the
  digests and the commands. It fails loudly if the digests never appear (look at
  the Image run for the tag) or if one of its own verifications fails.
* Check what it published rather than trusting it. The release notes carry the
  digests, the certificate identity and the exact commands; `identity` is
  `https://github.com/Hazmi35/turnfleet/.github/workflows/release.yaml@refs/tags/<tag>`:

  ```sh
  ref="ghcr.io/hazmi35/turnfleet-controller:<tag>@sha256:<digest>"
  identity="https://github.com/Hazmi35/turnfleet/.github/workflows/release.yaml@refs/tags/<tag>"
  issuer=https://token.actions.githubusercontent.com

  cosign verify --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$ref"
  cosign verify-attestation --type slsaprovenance --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$ref" |
    jq -r .payload | base64 -d | jq -r .predicate.invocation.configSource.digest.sha1
  cosign verify-attestation --type spdxjson --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" "$ref" |
    jq -r .payload | base64 -d | jq -r '.predicate.packages[] | select(.name == "coturn") | .versionInfo'
  ```

  The first command proves the signature, the second prints the commit the
  provenance names (it must be the tagged commit) and the third prints the pinned
  coturn digest. Repeat for `turnfleet-probe`. A consumer that holds a public key
  instead of trusting the Fulcio identity uses `cosign verify --key cosign.pub
  <ref>`, which needs a key-based signature this repository does not produce.
* Re-run `make verify` on the tag: a tag is a commit, not a branch state.
