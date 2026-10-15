---
name: release-check
description: What must be true before tagging a Turnfleet release. Use when preparing a tag or when asked whether the repository is releasable.
---

# Release check

Do not create the release until every line below is true. Report the command
output as evidence for each one. "looks fine" is not evidence.

## Required

1. **Clean history on `main`**: no merge commits, Conventional Commits, every
   commit signed off. `make dco` and `git log --oneline main`.
2. **`make ci`** green on the exact commit being tagged: verify, helm-verify,
   lint, test, vuln, licenses, dco.
3. **`make test-e2e`** green: all three addressing modes, saturation, peer
   policy, drain with an active allocation, and `turnfleet-probe` against a real
   relay. **`make test-e2e-chart`** green too: the chart has to install the CRDs
   and the controller and probe a relay from that install, which `make
   helm-verify` does not exercise (it lints and compares renders, and installs
   nothing).
4. **`charts/turnfleet`'s `version` and `appVersion` are the release version**,
   on the commit being released: `version` is the chart's semver (`X.Y.Z`) and
   `appVersion` is the image tag the chart installs by default (`X.Y.Z`, the
   number beside the git tag's `vX.Y.Z`). They are the same number on purpose.
   Bumping either after the release leaves it carrying the previous one. The
   Release workflow checks both against the release tag before it publishes the
   chart, so a bump missed here fails the release instead of publishing a chart
   the release does not carry.
5. **Version pins agree**: each pin's copies in `go.mod`, `tools/go.mod`, the
   `Makefile`, `.custom-gcl.yml`, the Dockerfiles and the workflows hold the same
   value. The held modules, vulnerability floors and the held cosign pin
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
10. **The release is what starts the pipeline**: create it for the version being
    released (`gh release create vX.Y.Z --title "Turnfleet vX.Y.Z" --notes …`),
    which is also what creates the tag. `image.yaml` then builds and pushes both
    images for that tag. `release.yaml` waits for their digests, signs them
    keyless, attaches the SPDX SBOM and the SLSA provenance, publishes the chart
    to `oci://ghcr.io/hazmi35/turnfleet-chart/turnfleet` at the release's
    version and signs it at its own digest, and writes the release body
    (ADR 16, ADR 17). A tag push with no release publishes nothing. Release notes
    may claim the chart: it ships in this tree, and `make test-e2e-chart` proves
    it installs.

## After the release

* Confirm the release body carries the digest of both published images and the
  chart's version and digest.
* Watch the Release workflow for the tag. It waits for the Image workflow to
  publish `ghcr.io/hazmi35/turnfleet-{controller,probe}:<tag>`. It then signs
  both index digests keyless, attaches an SPDX SBOM and SLSA provenance to each,
  publishes and signs the chart at the digest its own push reported, verifies
  all of it with the identity it records, and writes the release body from the
  digests and the commands. It fails loudly if the digests never appear (look
  at the Image run for the tag), if the tag resolves to an image built from
  another commit (the check reads `org.opencontainers.image.revision` against
  the tagged commit, so a tag still carrying a previous release's build fails
  the run instead of being signed), if the chart's `version` or `appVersion`
  is not the release version (nothing is pushed in that case), or if one of its
  own verifications fails.
* Check what it published rather than trusting it. The release notes carry the
  digests, the certificate identity and the exact commands. `identity` is
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
  instead of trusting the Fulcio identity would need `cosign verify --key
  cosign.pub <ref>`, which requires a key-based signature this repository does
  not produce.
* The chart was published with the same identity and verifies the same way, at
  `oci://ghcr.io/hazmi35/turnfleet-chart/turnfleet`, version `<tag without its v>`.
  Pull it and check its digest, which the release body records, rather than the
  tag:

  ```sh
  helm pull oci://ghcr.io/hazmi35/turnfleet-chart/turnfleet --version <tag without v>
  cosign verify --certificate-identity "$identity" --certificate-oidc-issuer "$issuer" \
    ghcr.io/hazmi35/turnfleet-chart/turnfleet@sha256:<chart digest>
  ```

  Its provenance and its SPDX document verify with the `slsaprovenance` and
  `spdxjson` commands above, against the same reference. #39's acceptance,
  installing from that published reference on a fresh cluster and probing a
  relay, needs a release to have happened, the way the image digest checks above
  do.
* Re-run `make verify` on the tag: a tag is a commit, not a branch state.
