#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# chart-sbom.sh writes the SPDX document the release workflow attests to the
# published chart with `cosign attest --type spdxjson`.
#
# The chart is a package of templates, CRDs and values: it has no modules or
# base images to catalogue, so what its SBOM can describe is what the artifact
# contains. It pulls the chart by digest and records every file in it with its
# SHA256, the archive's own SHA256 (which is the digest of the layer the OCI
# manifest names) and the SPDX package verification code over those files. The
# document therefore describes the bytes the release signed, and the same shape
# — SPDX 2.3, attached as a cosign attestation on the digest — is what the
# images' SBOMs use, so a consumer verifies the chart the way it verifies them.
#
# The scan is by digest, never by tag: a tag can be re-pointed, and a document
# that describes one build while the signature covers another is worse than no
# document. The digest is required for that reason.
#
# Usage:
#   chart-sbom.sh --chart <oci://host/ns/chart@sha256:digest> --output <file>
#                 [--helm <path>] [--plain-http]
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: chart-sbom.sh --chart <oci://host/ns/chart@sha256:digest> --output <file> \
                     [--helm <path>] [--plain-http]

  --chart       published chart digest to describe (required)
  --output      file to write the SPDX 2.3 document to (required)
  --helm        helm binary (default: the pinned one under LOCALBIN)
  --plain-http  use plain HTTP, for a registry on localhost
USAGE
  exit "${1:-2}"
}

chart=''; output=''; helm=''; plain_http=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --chart) chart="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --helm) helm="${2:-}"; shift 2 ;;
    --plain-http) plain_http=1; shift ;;
    -h|--help) usage 0 ;;
    *) die "chart-sbom: unknown argument '$1'" ;;
  esac
done

[ -n "$chart" ] || die 'chart-sbom: --chart is required'
[ -n "$output" ] || die 'chart-sbom: --output is required'
case "$chart" in
  oci://*@sha256:*) ;;
  *) die "chart-sbom: --chart must be a digest reference (oci://host/ns/chart@sha256:<digest>); a tag can be re-pointed, a digest cannot" ;;
esac
digest="${chart##*@}"

[ -n "$helm" ] || helm="$(makefile_pin LOCALBIN)/helm"
[ -x "$helm" ] || die "chart-sbom: '$helm' is not executable; run 'make helm' first, or pass --helm"
require_tool jq

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

fetch_args=()
if [ "$plain_http" -eq 1 ]; then
  fetch_args+=(--plain-http)
fi

# The published artifact, pulled by the digest that was signed.
pulled=''
pulled="$("$helm" pull "${fetch_args[@]}" "$chart" --destination "$work" 2>&1)" || {
  printf '%s\n' "$pulled" >&2
  die "chart-sbom: ${chart} is not fetchable"
}

tgz=''
for candidate in "$work"/*.tgz; do
  [ -f "$candidate" ] || continue
  [ -z "$tgz" ] || die "chart-sbom: helm pull wrote more than one chart archive into $work"
  tgz="$candidate"
done
[ -n "$tgz" ] || die "chart-sbom: helm pull wrote no chart archive into $work"

# helm reads the archive's own Chart.yaml, so the name and version in the
# document are the ones the released chart carries.
metadata="$("$helm" show chart "$tgz")"
name="$(printf '%s\n' "$metadata" | awk '$1 == "name:" { print $2; exit }')"
version="$(printf '%s\n' "$metadata" | awk '$1 == "version:" { print $2; exit }')"
if [ -z "$name" ] || [ -z "$version" ]; then
  die "chart-sbom: ${chart} carries no chart name or version"
fi

unpacked="$work/unpacked"
mkdir -p "$unpacked"
tar -xzf "$tgz" -C "$unpacked"

files_json="$work/files.json"
sha1s="$work/sha1s.txt"
: >"$files_json"
: >"$sha1s"
count=0
while IFS= read -r path; do
  sum="$(sha256sum "${unpacked}/${path}" | cut -d' ' -f1)"
  # SPDX ids take letters, digits, `.` and `-`; the path is the only thing that
  # makes an id unique here.
  id="SPDXRef-File-$(printf '%s' "${path#./}" | sed 's/[^A-Za-z0-9.-]/-/g')"
  jq -n --arg id "$id" --arg name "$path" --arg sum "$sum" \
    '{SPDXID: $id, fileName: $name,
      checksums: [{algorithm: "SHA256", checksumValue: $sum}],
      licenseConcluded: "NOASSERTION", copyrightText: "NOASSERTION"}' >>"$files_json"
  sha1sum "${unpacked}/${path}" | cut -d' ' -f1 >>"$sha1s"
  count=$((count + 1))
done < <(cd "$unpacked" && find . -type f | LC_ALL=C sort)

# The package verification code is what SPDX defines for "these are the files of
# this package": the SHA1 of the concatenation of the files' SHA1s, in ascending
# order of the hash value (7.9.1). It is computed over SHA1s whatever the file
# entries record, which is why the loop keeps both.
verification="$(LC_ALL=C sort "$sha1s" | tr -d '\n' | sha1sum | cut -d' ' -f1)"
# The layer the OCI manifest names is the archive itself, so this SHA256 is the
# one a consumer gets from the registry for the chart content.
archive_sha="$(sha256sum "$tgz" | cut -d' ' -f1)"

jq -n \
  --arg name "$name" \
  --arg version "$version" \
  --arg ref "$chart" \
  --arg namespace "${chart}#spdx" \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg archive "$archive_sha" \
  --arg verification "$verification" \
  --argjson files "$(jq -s '.' "$files_json")" \
  '{
    spdxVersion: "SPDX-2.3",
    dataLicense: "CC0-1.0",
    SPDXID: "SPDXRef-DOCUMENT",
    name: ($name + "-" + $version + "-chart"),
    documentNamespace: $namespace,
    creationInfo: {
      created: $created,
      creators: ["Tool: turnfleet-release", "Organization: Turnfleet Authors"]
    },
    packages: [{
      SPDXID: "SPDXRef-Package-chart",
      name: $name,
      versionInfo: $version,
      downloadLocation: $ref,
      filesAnalyzed: true,
      packageVerificationCode: {packageVerificationCodeValue: $verification},
      checksums: [{algorithm: "SHA256", checksumValue: $archive}],
      licenseConcluded: "AGPL-3.0-only",
      licenseDeclared: "AGPL-3.0-only",
      copyrightText: "Copyright (C) 2026 Turnfleet Authors",
      primaryPackagePurpose: "APPLICATION",
      hasFiles: [$files[].SPDXID]
    }],
    files: $files,
    relationships: [{
      spdxElementId: "SPDXRef-DOCUMENT",
      relationshipType: "DESCRIBES",
      relatedSpdxElement: "SPDXRef-Package-chart"
    }]
  }' >"$output"

# The document is evidence, so it fails loudly rather than being attached
# hollow: an empty file list, a document that does not name the digest that was
# signed, or one without the chart's own Chart.yaml are all reasons to stop the
# release instead of attesting a document that describes nothing.
jq -e '.spdxVersion == "SPDX-2.3"' "$output" >/dev/null \
  || die "chart-sbom: $output is not an SPDX 2.3 document"
jq -e --arg digest "$digest" \
  '[.packages[] | select(.SPDXID == "SPDXRef-Package-chart" and (.downloadLocation | contains($digest)))] | length == 1' "$output" >/dev/null \
  || die "chart-sbom: $output does not name the published digest ${digest}"
jq -e --argjson count "$count" \
  '(.files | length) == $count
   and ([.files[] | select(.checksums[0].checksumValue | test("^[0-9a-f]{64}$"))] | length) == $count
   and (.packages[0].hasFiles | length) == $count' "$output" >/dev/null \
  || die "chart-sbom: $output does not describe all ${count} files of the chart with their SHA256"
jq -e '.files | map(.fileName) | index("./'"${name}"'/Chart.yaml") != null' "$output" >/dev/null \
  || die "chart-sbom: $output does not carry the chart's own Chart.yaml"

printf 'chart-sbom: %s -> %s: %s files, archive %s, verification code %s\n' \
  "$chart" "$output" "$count" "$archive_sha" "$verification"
