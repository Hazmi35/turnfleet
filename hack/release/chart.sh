#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# chart.sh packages charts/turnfleet, pushes it to the OCI registry the release
# publishes the chart in, and resolves the digest that push reported.
#
# The version guard runs before the package is built. The release tag is
# `vX.Y.Z` and the chart's `version` and `appVersion` are `X.Y.Z` (ADR 17), so a
# release whose checklist bump was missed stops here, naming both numbers,
# instead of publishing a chart that does not carry the released version. Both
# fields are read from the chart as helm's own parser reports them, so a comment
# in Chart.yaml cannot be mistaken for the value.
#
# The digest is what the push reported, not what the tag is assumed to point at,
# and it is proven before anything signs it: `helm show chart` at that digest has
# to serve the chart back. A tag can be re-pointed; a digest cannot, and the
# digest is what the release signs and the notes record.
#
# It does not log in: `helm push` reads the credentials `docker login` wrote,
# which is the login the release workflow already makes for cosign.
#
# Usage:
#   chart.sh --tag <vX.Y.Z> --chart <dir> --registry <host/namespace>
#            --output <file> [--helm <path>] [--plain-http]
#
#   --tag         the release tag, `vX.Y.Z` (required)
#   --chart       the chart directory in the checkout (required)
#   --registry    where the chart is published, as `host/namespace` with no
#                 scheme and no chart name, e.g.
#                 `ghcr.io/hazmi35/turnfleet-chart` (required)
#   --output      file to write the published record to (required)
#   --helm        the helm binary (default: the pinned one the Makefile's
#                 `helm` target installs, under LOCALBIN)
#   --plain-http  push and fetch over plain HTTP, for a registry on localhost
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: chart.sh --tag <vX.Y.Z> --chart <dir> --registry <host/namespace> \
                --output <file> [--helm <path>] [--plain-http]

  --tag         release tag, vX.Y.Z (required)
  --chart       chart directory to package and push (required)
  --registry    host/namespace to publish in, no scheme, no chart name (required)
  --output      file to write the published record to (required)
  --helm        helm binary (default: the pinned one under LOCALBIN)
  --plain-http  use plain HTTP, for a registry on localhost
USAGE
  exit "${1:-2}"
}

tag=''; chart=''; registry=''; output=''; helm=''; plain_http=0

while [ "$#" -gt 0 ]; do
  case "$1" in
    --tag) tag="${2:-}"; shift 2 ;;
    --chart) chart="${2:-}"; shift 2 ;;
    --registry) registry="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --helm) helm="${2:-}"; shift 2 ;;
    --plain-http) plain_http=1; shift ;;
    -h|--help) usage 0 ;;
    *) die "chart: unknown argument '$1'" ;;
  esac
done

for required in tag chart registry output; do
  [ -n "${!required}" ] || die "chart: --${required//_/-} is required"
done
case "$tag" in
  v*) ;;
  *) die "chart: --tag '$tag' is not a release tag; a release tag starts with v (vX.Y.Z)" ;;
esac
[ -d "$chart" ] || die "chart: --chart '$chart' is not a directory; run this from the repository root"
case "$registry" in
  *://*) die "chart: --registry '$registry' carries a scheme; it is host/namespace, with a port if the registry needs one, and no chart name" ;;
  */) die "chart: --registry '$registry' ends in a slash; the chart name is appended to it" ;;
esac

[ -n "$helm" ] || helm="$(makefile_pin LOCALBIN)/helm"
[ -x "$helm" ] || die "chart: '$helm' is not executable; run 'make helm' first, or pass --helm"
require_tool jq

# The guard. helm prints the chart's metadata with the comments stripped, which
# is also the parse it will package from.
metadata="$("$helm" show chart "$chart")"
metadata_field() {
  printf '%s\n' "$metadata" | awk -v field="$1" '$1 == field ":" { print $2; exit }'
}
name="$(metadata_field name)"
version="$(metadata_field version)"
app_version="$(metadata_field appVersion)"
if [ -z "$name" ] || [ -z "$version" ]; then
  die "chart: $chart has no name or version; helm could not read it"
fi

expected="${tag#v}"
[ "$version" = "$expected" ] || die "chart: $chart is version '$version' but the release is $tag, which is version '$expected'; bump version and appVersion in the tagged commit and create the release again (release-check step 4), or the chart is published under a version the release does not carry"
[ "$app_version" = "$expected" ] || die "chart: $chart has appVersion '$app_version' but the release is $tag, which is version '$expected'; the chart would install the image of another release — bump version and appVersion in the tagged commit and create the release again (release-check step 4)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

"$helm" package "$chart" --destination "$work"
package_file="${work}/${name}-${version}.tgz"
[ -f "$package_file" ] || die "chart: helm package wrote no ${name}-${version}.tgz into $work"

push_args=()
fetch_args=()
if [ "$plain_http" -eq 1 ]; then
  push_args+=(--plain-http)
  fetch_args+=(--plain-http)
fi

pushed="$("$helm" push "${push_args[@]}" "$package_file" "oci://${registry}" 2>&1)" || {
  printf '%s\n' "$pushed" >&2
  die "chart: helm push to oci://${registry} failed"
}
printf '%s\n' "$pushed"
digest="$(printf '%s\n' "$pushed" | awk '/^Digest: /{ print $2 }')"
case "$digest" in
  sha256:*) ;;
  *) die "chart: helm push reported no digest for ${registry}/${name} (got '${digest}'); the release signs a digest, never a tag" ;;
esac
[ "${#digest}" -eq 71 ] || die "chart: helm push reported '$digest', which is not a full sha256 digest"

# The digest is only worth signing if the registry serves it back.
oci_digest="oci://${registry}/${name}@${digest}"
served=''
served="$("$helm" show chart "${fetch_args[@]}" "$oci_digest" 2>&1)" || {
  printf '%s\n' "$served" >&2
  die "chart: ${oci_digest} is published but not fetchable"
}
served_version="$(printf '%s\n' "$served" | awk '$1 == "version:" { print $2; exit }')"
[ "$served_version" = "$version" ] || die "chart: ${oci_digest} serves version '${served_version}', not '${version}'"

jq -n \
  --arg name "$name" \
  --arg version "$version" \
  --arg digest "$digest" \
  --arg oci "$oci_digest" \
  --arg ref "${registry}/${name}@${digest}" \
  '{name: $name, version: $version, digest: $digest, oci: $oci, ref: $ref}' >"$output"

jq -e '.digest as $d
  | .name != "" and .version != ""
    and ($d | startswith("sha256:"))
    and (.oci | startswith("oci://") and endswith($d))
    and (.ref | endswith($d))' "$output" >/dev/null \
  || die "chart: $output is not a usable record of the published chart"

printf 'chart: %s %s (release %s) -> %s (digest %s, fetchable)\n' \
  "$name" "$version" "$tag" "$oci_digest" "$digest"
