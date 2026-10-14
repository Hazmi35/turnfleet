#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# chart-provenance.sh writes the SLSA provenance predicate the release workflow
# attests to the published chart with `cosign attest --type slsaprovenance`.
#
# What the document claims: this chart digest is the one the release pushed, the
# chart's version is the release's version, and the digest was packaged and
# pushed by the release workflow at the tag the commit points at.
#
# It is a sibling of provenance.sh rather than a mode of it because the two
# documents describe different builds: an image's provenance points at
# image.yaml, which built and pushed the digest before the release ran, and
# carries the image pins as materials, while the chart is packaged and pushed by
# the release workflow itself and its only material is the tagged tree. The
# parameters and materials of the two documents have nothing in common but the
# tag and the commit.
#
# What it does not claim: a complete parameter list, the runner environment, or
# a reproducible build. `helm package` is byte-reproducible for a given tree and
# helm version, but nothing in the release pipeline proves the published digest
# came from this tree, so `reproducible` stays false as it does for the images.
# SLSA v0.2 predicate, because that is the type `cosign verify-attestation
# --type slsaprovenance` looks for.
#
# Usage:
#   chart-provenance.sh --chart <oci://host/ns/chart@sha256:digest> \
#                       --chart-version <X.Y.Z> --tag <tag> --commit <sha> \
#                       --repository-url <url> --workflow-ref <owner/repo/...> \
#                       --run-id <id> --run-url <url> --output <file>
#                       [--build-workflow release.yaml]
#                       [--helm-version vX.Y.Z] [--cosign-version vX.Y.Z]
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: chart-provenance.sh --chart <oci://...@sha256:digest> --chart-version <X.Y.Z> \
                           --tag <tag> --commit <sha> --output <file> \
                           --repository-url <url> --workflow-ref <ref> \
                           --run-id <id> --run-url <url> [options]

  --chart            the published chart reference this provenance is about,
                     pinned to its digest (required)
  --chart-version    the chart's version, which is the release version (required)
  --tag              release tag the chart was published for (required)
  --commit           commit the tag points at, full sha (required)
  --repository-url   https://github.com/<owner>/<repo> (required)
  --workflow-ref     the release workflow's ref, as GITHUB_WORKFLOW_REF gives it
                     (required: recorded as the invocation environment)
  --run-id           the release run's id (required)
  --run-url          the release run's URL (required)
  --output           file to write the predicate to (required)
  --build-workflow   workflow that packaged and pushed the chart
                     (default release.yaml)
  --helm-version     helm version that packaged it, recorded as a pin
                     (default HELM_VERSION from the Makefile)
  --cosign-version   cosign version that signed it, recorded as a pin
USAGE
  exit "${1:-2}"
}

chart=''; chart_version=''; tag=''; commit=''; repository_url=''; workflow_ref=''
run_id=''; run_url=''; output=''; build_workflow='release.yaml'
helm_version=''; cosign_version=''

while [ "$#" -gt 0 ]; do
  case "$1" in
    --chart) chart="${2:-}"; shift 2 ;;
    --chart-version) chart_version="${2:-}"; shift 2 ;;
    --tag) tag="${2:-}"; shift 2 ;;
    --commit) commit="${2:-}"; shift 2 ;;
    --repository-url) repository_url="${2:-}"; shift 2 ;;
    --workflow-ref) workflow_ref="${2:-}"; shift 2 ;;
    --run-id) run_id="${2:-}"; shift 2 ;;
    --run-url) run_url="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --build-workflow) build_workflow="${2:-}"; shift 2 ;;
    --helm-version) helm_version="${2:-}"; shift 2 ;;
    --cosign-version) cosign_version="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) die "chart-provenance: unknown argument '$1'" ;;
  esac
done

for required in chart chart_version tag commit repository_url workflow_ref run_id run_url output; do
  [ -n "${!required}" ] || die "chart-provenance: --${required//_/-} is required"
done
case "$chart" in
  oci://*@sha256:*) ;;
  *) die "chart-provenance: --chart must be a digest reference (oci://host/ns/chart@sha256:<digest>); a tag can be re-pointed, a digest cannot" ;;
esac
case "$commit" in
  *[!0-9a-f]*|'') die "chart-provenance: --commit must be a full hexadecimal commit sha" ;;
esac
[ "${#commit}" -eq 40 ] || die "chart-provenance: --commit must be the full 40-character sha, not an abbreviation"

require_tool jq

# The helm pin comes from the Makefile, which holds one copy of it
# (docs/versions.md); --helm-version exists so the script can be run against a
# checkout that does not carry that pin.
if [ -z "$helm_version" ]; then
  helm_version="$(makefile_pin HELM_VERSION)"
fi

# The build definition: the tag's tree, packaged and pushed by the release
# workflow at that tag. The release workflow is the builder for the chart,
# where image.yaml is the builder for an image digest.
tag_ref="refs/tags/${tag}"
build_workflow_url="${repository_url}/.github/workflows/${build_workflow}@${tag_ref}"

jq -n \
  --arg builder "$build_workflow_url" \
  --arg repository "$repository_url" \
  --arg tag_ref "$tag_ref" \
  --arg tag "$tag" \
  --arg commit "$commit" \
  --arg chart "$chart" \
  --arg chart_version "$chart_version" \
  --arg build_workflow "$build_workflow" \
  --arg workflow_ref "$workflow_ref" \
  --arg run_id "$run_id" \
  --arg run_url "$run_url" \
  --arg helm "$helm_version" \
  --arg cosign "$cosign_version" \
  '{
    builder: {id: $builder},
    buildType: $builder,
    invocation: {
      configSource: {
        uri: ("git+" + $repository + "@" + $tag_ref),
        digest: {sha1: $commit},
        entryPoint: $build_workflow
      },
      parameters: ({
        tag: $tag,
        commit: $commit,
        chart: $chart,
        chartVersion: $chart_version
      } + (if $helm == "" then {} else {helm: $helm} end)
        + (if $cosign == "" then {} else {cosign: $cosign} end)),
      environment: {
        githubWorkflowRef: $workflow_ref,
        githubRunId: $run_id,
        githubRunUrl: $run_url
      }
    },
    metadata: {
      completeness: {parameters: false, environment: false, materials: false},
      reproducible: false
    },
    materials: [
      {uri: ("git+" + $repository), digest: {sha1: $commit}}
    ]
  }' >"$output"

jq -e '.buildType != null and .invocation.configSource.digest.sha1 != null and .invocation.parameters.chartVersion != null' "$output" >/dev/null \
  || die "chart-provenance: $output is not a usable SLSA provenance predicate"
printf 'chart-provenance: %s %s -> %s (commit %s, packaged and pushed by %s, recorded by %s)\n' \
  "$chart_version" "$output" "$chart" "$commit" "$build_workflow_url" "$workflow_ref"
