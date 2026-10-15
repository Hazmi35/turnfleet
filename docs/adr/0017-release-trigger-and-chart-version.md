# 17. The release is the trigger, and the chart carries the application's version

* Status: accepted
* Date: 2026-10-14

## Context

The pipeline used to start on a tag push: `.github/workflows/image.yaml` and
`.github/workflows/release.yaml` both listened for `push: tags: ["v*"]`, so
pushing a tag built the images, signed them, attached an SBOM and provenance and
wrote the release body. That made the release an *output* of a git act: the tag
was the input, and the workflow created the release it then wrote the notes for.
A tag pushed for any other reason (a marker, a rehearsal, a mistake) started the
whole thing, and a tag whose build failed left a release behind anyway.

The chart had the same shape of problem in miniature. Its `version` was `0.1.0`
and its `appVersion` was `dev`, the Makefile's `IMAGE_TAG` for a local build.
The chart therefore carried two numbers with no relation to each other and no
relation to the images it installs. A consumer reading a chart version learned
nothing about which controller image it would run, and once the chart is
published as an artifact of its own (#39) that question becomes the first one a
consumer asks.

What made a single number possible: `docker/metadata-action` publishes `0.1.0`
beside the tag's `v0.1.0` for every release. An image tag therefore exists that
is the release version exactly, without the `v` that a chart's `version` field
does not accept.

## Decision

The GitHub release is the trigger. The chart's version is the application's.

* `image.yaml` runs on `release: published`, beside the push to `main` that
  builds the `main` tag for day-to-day work, and `release.yaml` runs on the same
  event. A tag push publishes nothing. The release event is the one act that
  publishes a version, and it is deliberate by construction.
* The tag a release points at is still the ref both workflows work from, because
  `github.ref` is `refs/tags/<tag>` for a release event. The identity the
  signatures carry (`…/release.yaml@refs/tags/<tag>`) and the digests the notes
  record are unchanged.
* `release.yaml` keeps its wait for the images to appear in the registry. The
  ordering makes that wait load-bearing rather than defensive: the release now
  exists before its images do, and both workflows start together.
* `release.yaml` edits the release it was triggered by, so the notes are written
  onto the release the maintainer created. The create path stays for a manual
  run, where no release exists yet.
* `charts/turnfleet`'s `version` and `appVersion` are the release version: for
  the tag `v0.1.0` the chart is `0.1.0` and its default image tag is `0.1.0`,
  which is the tag `image.yaml` publishes beside `v0.1.0`. `make helm-install`
  still passes the Makefile's `IMAGE_REGISTRY` and `IMAGE_TAG`, so a local
  install runs the image the Makefile just built.
* The release checklist is framed around creating the release (which is what
  creates the tag) and requires both chart fields to be the release version
  before that.

## Consequences

* A tag is inert. Publishing requires creating the release, which is a
  deliberate act with a version attached, and a failed image build now fails
  inside a release that already exists rather than producing one as a side
  effect. That failure is visible, because the signing job waits for digests
  that never appear and says so.
* One number covers the chart, the images and the OCI artifact of #39. A chart
  install without flags pulls the image released beside it.
* A local install of the chart no longer gets a locally loaded image by default:
  the chart's `appVersion` is a released tag, so a local install passes
  `make helm-install` (or `--set image.tag=dev`). That is the intended trade:
  the chart's default belongs to a consumer, not to this repository's workflow.
* The trigger itself is unverified until the first release runs it. Every static
  check passes (`actionlint`, and the checks GitHub starts on a pull request are
  proof that it accepts the workflow files), but "a release publishes the
  artifacts" is a claim only a release can settle.
