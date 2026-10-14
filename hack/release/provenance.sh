#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# provenance.sh writes the SLSA provenance predicate the release workflow
# attests with `cosign attest --type slsaprovenance`.
#
# What the document claims: this image digest is the one the tag published, the
# tag points at this commit, the digest was built by the image workflow at that
# tag, and these are the pins the tagged tree carried.
#
# What it does not claim: a complete parameter list, the runner environment, or
# a reproducible build. image.yaml already attaches the build's own provenance
# (`provenance: true`) to the image index, and that document records the build
# args and the run; duplicating it here and getting a detail wrong would be
# worse than pointing at it. SLSA v0.2 predicate, because that is the type
# `cosign verify-attestation --type slsaprovenance` looks for.
#
# Usage:
#   provenance.sh --image <ref> --tag <tag> --commit <sha> --repository-url <url>
#                 --workflow-ref <owner/repo/.github/workflows/release.yaml@ref>
#                 --run-id <id> --run-url <url> --output <file>
#                 [--build-workflow image.yaml]
#                 [--cosign-version vX.Y.Z] [--syft-version vX.Y.Z]
set -euo pipefail
# shellcheck disable=SC1091 # the source path is this script's own directory, which shellcheck cannot resolve
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

usage() {
  cat <<'USAGE'
usage: provenance.sh --image <ref> --tag <tag> --commit <sha> --output <file> \
                     --repository-url <url> --workflow-ref <ref> \
                     --run-id <id> --run-url <url> [options]

  --image            the published reference this provenance is about (required)
  --tag              release tag the image was published for (required)
  --commit           commit the tag points at, full sha (required)
  --repository-url   https://github.com/<owner>/<repo> (required)
  --workflow-ref     the release workflow's ref, as GITHUB_WORKFLOW_REF gives it
                     (required: recorded as the invocation environment)
  --run-id           the release run's id (required)
  --run-url          the release run's URL (required)
  --output           file to write the predicate to (required)
  --build-workflow   workflow that built the digest (default image.yaml)
  --cosign-version   cosign version that signed it, recorded as a pin
  --syft-version     syft version that generated the SBOM, recorded as a pin
USAGE
  exit "${1:-2}"
}

image=''; tag=''; commit=''; repository_url=''; workflow_ref=''; run_id=''; run_url=''
output=''; build_workflow='image.yaml'; cosign_version=''; syft_version=''

while [ "$#" -gt 0 ]; do
  case "$1" in
    --image) image="${2:-}"; shift 2 ;;
    --tag) tag="${2:-}"; shift 2 ;;
    --commit) commit="${2:-}"; shift 2 ;;
    --repository-url) repository_url="${2:-}"; shift 2 ;;
    --workflow-ref) workflow_ref="${2:-}"; shift 2 ;;
    --run-id) run_id="${2:-}"; shift 2 ;;
    --run-url) run_url="${2:-}"; shift 2 ;;
    --output) output="${2:-}"; shift 2 ;;
    --build-workflow) build_workflow="${2:-}"; shift 2 ;;
    --cosign-version) cosign_version="${2:-}"; shift 2 ;;
    --syft-version) syft_version="${2:-}"; shift 2 ;;
    -h|--help) usage 0 ;;
    *) die "provenance: unknown argument '$1'" ;;
  esac
done

for required in image tag commit repository_url workflow_ref run_id run_url output; do
  [ -n "${!required}" ] || die "provenance: --${required//_/-} is required"
done
case "$commit" in
  *[!0-9a-f]*|'') die "provenance: --commit must be a full hexadecimal commit sha" ;;
esac
[ "${#commit}" -eq 40 ] || die "provenance: --commit must be the full 40-character sha, not an abbreviation"

require_tool jq

# The build definition: the tag's tree, built by the image workflow at that tag.
tag_ref="refs/tags/${tag}"
build_workflow_url="${repository_url}/.github/workflows/${build_workflow}@${tag_ref}"

jq -n \
  --arg builder "$build_workflow_url" \
  --arg repository "$repository_url" \
  --arg tag_ref "$tag_ref" \
  --arg tag "$tag" \
  --arg commit "$commit" \
  --arg image "$image" \
  --arg build_workflow "$build_workflow" \
  --arg workflow_ref "$workflow_ref" \
  --arg run_id "$run_id" \
  --arg run_url "$run_url" \
  --arg go_image "$(makefile_pin GO_IMAGE)" \
  --arg runtime_image "$(makefile_pin RUNTIME_IMAGE)" \
  --arg coturn_image "$(makefile_pin COTURN_IMAGE)@$(makefile_pin COTURN_IMAGE_DIGEST)" \
  --arg platforms "$(makefile_pin PLATFORMS)" \
  --arg cosign "$cosign_version" \
  --arg syft "$syft_version" \
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
        image: $image,
        buildWorkflow: $builder,
        goImage: $go_image,
        runtimeImage: $runtime_image,
        coturnImage: $coturn_image,
        platforms: $platforms
      } + (if $cosign == "" then {} else {cosign: $cosign} end)
        + (if $syft == "" then {} else {syft: $syft} end)),
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
      {uri: ("git+" + $repository), digest: {sha1: $commit}},
      {uri: $go_image, digest: {sha256: ($go_image | split("@")[1] | sub("^sha256:"; ""))}},
      {uri: $runtime_image, digest: {sha256: ($runtime_image | split("@")[1] | sub("^sha256:"; ""))}}
    ]
  }' >"$output"

jq -e '.buildType != null and .invocation.configSource.digest.sha1 != null' "$output" >/dev/null \
  || die "provenance: $output is not a usable SLSA provenance predicate"
printf 'provenance: %s -> %s (commit %s, built by %s, recorded by %s)\n' \
  "$tag" "$output" "$commit" "$build_workflow_url" "$workflow_ref"
