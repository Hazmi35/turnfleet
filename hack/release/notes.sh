#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# notes.sh writes the release notes the release workflow publishes: the digests
# of the images and of the chart it signed, and the exact commands that install,
# pull and check them. The notes are generated rather than written by hand so a
# release cannot document a digest or an identity that is not the one the
# workflow signed.
#
# Usage:
#   notes.sh --tag <tag> --commit <sha> --repository <owner>/<repo> \
#            --identity <url> --issuer <url> --image <ref> [--image <ref>] \
#            --chart <oci://host/ns/chart@sha256:digest> \
#            --chart-version <X.Y.Z> --output <file>
#
# The identity is the signing certificate's, as the run reported it
# (GITHUB_WORKFLOW_REF): a tag push signs under that tag's ref, and the notes
# have to record the value the release actually used.
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: notes.sh --tag <tag> --commit <sha> --repository <owner>/<repo> \
                --identity <url> --issuer <url> --output <file> \
                --chart <oci://host/ns/chart@sha256:digest> \
                --chart-version <X.Y.Z> [--image <ref> ...]

  --tag            release tag (required)
  --commit         commit the tag points at, full sha (required)
  --repository     owner/repo the release lives in, for the download commands
                   (required)
  --identity       certificate identity the artifacts were signed with (required)
  --issuer         OIDC issuer the identity was verified against (required)
  --image          a signed image reference to document; repeat per image
                   (required)
  --chart          the published chart digest to document (required)
  --chart-version  the chart's version, which is the release version (required)
  --output         file to write the notes to (required)
USAGE
  exit "${1:-2}"
}

tag=''; commit=''; repository=''; identity=''; issuer=''; output=''
chart=''; chart_version=''
images=()

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag) tag="${2:-}"; shift 2 ;;
    --commit) commit="${2:-}"; shift 2 ;;
    --repository) repository="${2:-}"; shift 2 ;;
    --identity) identity="${2:-}"; shift 2 ;;
    --issuer) issuer="${2:-}"; shift 2 ;;
    --image) images+=("${2:-}"); shift 2 ;;
    --chart) chart="${2:-}"; shift 2 ;;
    --chart-version) chart_version="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) die "notes: unknown argument '$1'" ;;
  esac
done

for required in tag commit repository identity issuer output chart chart_version; do
  [ -n "${!required}" ] || die "notes: --${required//_/-} is required"
done
[ "${#images[@]}" -gt 0 ] || die 'notes: at least one --image is required'
for ref in "${images[@]}"; do
  case "$ref" in
    *@sha256:*) ;;
    *) die "notes: --image '$ref' is not a digest reference; the notes record the digest, never only the tag" ;;
  esac
done
case "$chart" in
  oci://*@sha256:*) ;;
  *) die "notes: --chart '$chart' is not a digest reference (oci://host/ns/chart@sha256:<digest>); the notes record the digest, never only the tag" ;;
esac

# The chart's reference without its digest is what helm takes, with the version
# beside it; cosign takes the reference on its own.
chart_oci="${chart%@*}"
chart_digest="${chart#*@}"
chart_ref="${chart#oci://}"

# The file names the release carries are derived from the image name, so the
# notes point at the file the workflow uploaded.
image_name() {
  local ref="${1%@*}"
  printf '%s\n' "${ref%:*}" | sed 's%^.*/%%'
}

{
  cat <<EOF
<!-- Written by hack/release/notes.sh when the tag was signed; a re-run replaces this file. -->

Turnfleet \`${tag}\`, built from commit \`${commit}\` by the image workflow at
\`refs/tags/${tag}\`.

## Images

Both images are multi-arch (\`linux/amd64\`, \`linux/arm64\`). The digest below is
the index digest: it is what this release signed, and what the SBOM and
provenance attestations are attached to.

| Image | Digest |
| --- | --- |
EOF
  for ref in "${images[@]}"; do
    printf "| \`%s\` | \`%s\` |\n" "${ref%@*}" "${ref#*@}"
  done

  cat <<EOF

## Chart

The controller chart ships beside the images as its own OCI artifact, one
artifact per release, at the release's version: installed without flags it runs
\`turnfleet-controller:${chart_version}\`, the released image.

| Chart | Version | Digest |
| --- | --- | --- |
| \`${chart_oci}\` | \`${chart_version}\` | \`${chart_digest}\` |

\`\`\`sh
# Install it. The chart creates the CRDs, the RBAC and the manager; a cluster
# gets the controller from this chart or from config/, never both.
helm install turnfleet ${chart_oci} \\
  --version ${chart_version} --namespace turnfleet-system --create-namespace

# Or pull the artifact and read it.
helm pull ${chart_oci} --version ${chart_version}
\`\`\`

## Verifying this release

Signing is keyless. Each signature carries a short-lived Fulcio certificate for
the identity below, issued for this repository's release workflow at this tag;
there is no key to fetch, and cosign checks the certificate against the OIDC
issuer.

    identity: ${identity}
    issuer:   ${issuer}

\`\`\`sh
# The image signature.
EOF
  for ref in "${images[@]}"; do
    cat <<EOF
cosign verify \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${ref}
EOF
  done

  cat <<EOF

# The SLSA provenance attestation. Its invocation.configSource.digest.sha1 is
# ${commit}, the commit above.
EOF
  for ref in "${images[@]}"; do
    cat <<EOF
cosign verify-attestation --type slsaprovenance \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${ref}
EOF
  done

  cat <<EOF

# The SBOM attestation, written out as the file attached to this release.
EOF
  for ref in "${images[@]}"; do
    cat <<EOF
cosign verify-attestation --type spdxjson \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${ref} | jq -r .payload | base64 -d | jq .predicate > $(image_name "$ref").spdx.json
EOF
  done

  cat <<EOF
\`\`\`

### The chart

The chart carries the same three things at its digest: a keyless signature, the
SLSA provenance and an SPDX document of the files the artifact contains.

\`\`\`sh
# Its signature.
cosign verify \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${chart_ref}

# Its SLSA provenance. invocation.configSource.digest.sha1 is ${commit}, the
# commit above, and parameters.chartVersion is ${chart_version}.
cosign verify-attestation --type slsaprovenance \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${chart_ref}

# Its SPDX document: every file in the artifact with its SHA256.
cosign verify-attestation --type spdxjson \\
  --certificate-identity '${identity}' \\
  --certificate-oidc-issuer '${issuer}' \\
  ${chart_ref} | jq -r .payload | base64 -d | jq .predicate
\`\`\`

### Verifying against a public key instead

cosign also takes \`--key cosign.pub\`, which skips Fulcio and the OIDC issuer:

\`\`\`sh
cosign verify --key cosign.pub ${images[0]}
\`\`\`

That needs a key-based signature, and this release has none: it is signed
keyless so that there is no long-lived key to hold or rotate. A publisher who
needs the key-based path generates a pair with \`cosign generate-key-pair\`,
keeps the private key and its password in the repository's secrets, signs with
\`cosign sign --key cosign.key\`, and publishes the public key next to the
release. Trust a \`cosign.pub\` only when it arrived over a channel you already
trust.

## SBOMs

One SPDX 2.3 document per image. syft generated it from the published digest, so
it describes the bytes that were signed rather than a checkout, and it is
attached twice: as the release asset below, and as a cosign attestation on the
image, which is what the verification above fetches by digest.

The chart has one too, written from the artifact pulled by its digest: it lists
the files the chart contains with their SHA256 values and the package
verification code over them. It is attached to the chart's digest as a cosign
attestation and not as a file on this release, so write it out with the
\`spdxjson\` command under "The chart" above.

| File | Image |
| --- | --- |
EOF
  for ref in "${images[@]}"; do
    printf "| \`%s.spdx.json\` | \`%s\` |\n" "$(image_name "$ref")" "${ref%@*}"
  done

  cat <<EOF

\`\`\`sh
gh release download ${tag} --repo ${repository} --pattern '*.spdx.json'
\`\`\`

The scan describes \`linux/amd64\`; the Go modules are the same on both
platforms. The relay data plane (coturn) is pinned in the repository and is not
a layer of either image, so each document names it as an external component
instead, with the digest the repository pins it to.
EOF
} >"$output"

printf 'notes: %s -> %s (%s images and chart %s, identity %s)\n' \
  "$tag" "$output" "${#images[@]}" "$chart_version" "$identity"
