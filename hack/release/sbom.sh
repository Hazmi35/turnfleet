#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# sbom.sh writes the SBOM for one published image.
#
# It scans the image the registry serves at the digest it is given, never a
# checkout: the document has to describe the bytes the release signs, and a
# build tree would describe neither the pinned base images nor the modules the
# binary actually carries. The digest is required, so the scan cannot describe
# one build while the signature covers another.
#
# coturn is added as an external component afterwards. It is the relay data
# plane, it is pinned in the Makefile, and it is not a layer of either image, so
# nothing in the layers can name it; the release SBOM is the document that has
# to (issue #6: "name the pinned coturn digest and the Go modules in the build").
#
# Usage:
#   sbom.sh --image <registry>/<image>:<tag>@sha256:<digest> --output <file>
#           [--platform linux/amd64] [--coturn-ref <image>:<tag>@sha256:<digest>]
#
# The platform matters: syft reports the packages of the manifest it scans, and
# an image index is resolved to the host platform otherwise. The Go modules are
# the same either way, so one document per image is what the release publishes.
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: sbom.sh --image <ref> --output <file> [--platform <platform>] [--coturn-ref <ref>]

  --image       registry/image:tag@sha256:digest to scan (required)
  --output      file to write the SPDX 2.3 document to (required)
  --platform    platform to scan when the reference is an image index
                (default linux/amd64)
  --coturn-ref  pinned relay data plane to record as an external component
                (default: COTURN_IMAGE@COTURN_IMAGE_DIGEST from the Makefile)
USAGE
  exit "${1:-2}"
}

image=''
output=''
platform='linux/amd64'
coturn_ref=''

while [ "$#" -gt 0 ]; do
  case "$1" in
    --image) image="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --platform) platform="${2:-}"; shift 2 ;;
    --coturn-ref) coturn_ref="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) die "sbom: unknown argument '$1'" ;;
  esac
done

[ -n "$image" ] || die 'sbom: --image is required'
[ -n "$output" ] || die 'sbom: --output is required'
case "$image" in
  *@sha256:*) ;;
  *) die "sbom: --image must be a digest reference (<image>:<tag>@sha256:<digest>); a tag can be re-pointed, a digest cannot" ;;
esac

require_tool syft
require_tool jq

# The coturn pin comes from the Makefile, which holds one copy of it
# (docs/versions.md); --coturn-ref exists so the script can be run against a
# checkout that does not carry that pin.
if [ -z "$coturn_ref" ]; then
  coturn_ref="$(makefile_pin COTURN_IMAGE)@$(makefile_pin COTURN_IMAGE_DIGEST)"
fi

raw="$(mktemp)"
trap 'rm -f "$raw"' EXIT

syft "$image" --platform "$platform" -o "spdx-json=$raw"

coturn_digest="${coturn_ref#*@}"
coturn_component="$(jq -n --arg ref "$coturn_ref" '
  ($ref | split("@")) as $parts
  | ($parts[0] | split(":")) as $name_tag
  | ($name_tag[0] | split("/") | last) as $name
  | {
      SPDXID: "SPDXRef-Package-coturn-data-plane",
      name: $name,
      versionInfo: $name_tag[1],
      downloadLocation: "NOASSERTION",
      filesAnalyzed: false,
      licenseConcluded: "NOASSERTION",
      licenseDeclared: "NOASSERTION",
      copyrightText: "NOASSERTION",
      primaryPackagePurpose: "CONTAINER",
      externalRefs: [{
        referenceCategory: "PACKAGE-MANAGER",
        referenceType: "purl",
        referenceLocator: ("pkg:oci/" + $name + "@" + $parts[1] + "?repository_url=" + $name_tag[0] + "&tag=" + $name_tag[1])
      }],
      comment: ("The relay data plane, pinned as " + $ref + " in the Makefile. It is not a layer of this image: the controller renders relay configuration for it and never vendors it (docs/adr/0002-coturn-data-plane.md).")
    }')"

jq --argjson coturn "$coturn_component" '
  .packages += [$coturn]
  | .relationships += [{
      spdxElementId: "SPDXRef-DOCUMENT",
      relationshipType: "DESCRIBES",
      relatedSpdxElement: $coturn.SPDXID
    }]
' "$raw" >"$output"

# The SBOM is evidence, so it fails loudly rather than publishing a document
# that does not describe the build: an empty scan, a missing coturn component or
# a module list that syft could not read from the binary are all reasons to stop
# the release instead of attaching a hollow SBOM to it.
jq -e '.spdxVersion == "SPDX-2.3"' "$output" >/dev/null \
  || die "sbom: $output is not an SPDX 2.3 document"
jq -e --arg digest "$coturn_digest" \
  '[.packages[] | select(.SPDXID == "SPDXRef-Package-coturn-data-plane"
     and any(.externalRefs[]?; .referenceLocator | contains($digest)))] | length == 1' "$output" >/dev/null \
  || die "sbom: $output does not name the pinned coturn digest ${coturn_digest}"
jq -e '[.packages[] | select(any(.externalRefs[]?; .referenceLocator | startswith("pkg:golang/")))] | length > 0' "$output" >/dev/null \
  || die "sbom: $output lists no Go module, so it does not describe the modules this image was built from"

printf 'sbom: %s (%s) -> %s: %s packages (%s Go modules), coturn %s\n' \
  "$image" "$platform" "$output" \
  "$(jq '.packages | length' "$output")" \
  "$(jq '[.packages[] | select(any(.externalRefs[]?; .referenceLocator | startswith("pkg:golang/")))] | length' "$output")" \
  "$coturn_digest"
