#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# notes.sh writes the release notes the release workflow publishes: the digests
# of the images it signed, and the exact commands that check them. The notes are
# generated rather than written by hand so a release cannot document a digest or
# an identity that is not the one the workflow signed.
#
# Usage:
#   notes.sh --tag <tag> --commit <sha> --repository <owner>/<repo> \
#            --identity <url> --issuer <url> --image <ref> [--image <ref>] \
#            --output <file>
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
                [--image <registry>/<image>:<tag>@sha256:<digest> ...]

  --tag         release tag (required)
  --commit      commit the tag points at, full sha (required)
  --repository  owner/repo the release lives in, for the download commands
                (required)
  --identity    certificate identity the images were signed with (required)
  --issuer      OIDC issuer the identity was verified against (required)
  --image       a signed reference to document; repeat for each image (required)
  --output      file to write the notes to (required)
USAGE
  exit "${1:-2}"
}

tag=''; commit=''; repository=''; identity=''; issuer=''; output=''
images=()

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag) tag="${2:-}"; shift 2 ;;
    --commit) commit="${2:-}"; shift 2 ;;
    --repository) repository="${2:-}"; shift 2 ;;
    --identity) identity="${2:-}"; shift 2 ;;
    --issuer) issuer="${2:-}"; shift 2 ;;
    --image) images+=("${2:-}"); shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) die "notes: unknown argument '$1'" ;;
  esac
done

for required in tag commit repository identity issuer output; do
  [ -n "${!required}" ] || die "notes: --${required//_/-} is required"
done
[ "${#images[@]}" -gt 0 ] || die 'notes: at least one --image is required'
for ref in "${images[@]}"; do
  case "$ref" in
    *@sha256:*) ;;
    *) die "notes: --image '$ref' is not a digest reference; the notes record the digest, never only the tag" ;;
  esac
done

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

printf 'notes: %s -> %s (%s images, identity %s)\n' \
  "$tag" "$output" "${#images[@]}" "$identity"
