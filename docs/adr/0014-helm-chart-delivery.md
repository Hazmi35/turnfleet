# 14. The chart installs the controller, config/ stays the source of truth

* Status: accepted
* Date: 2026-10-13

## Context

Until now the project shipped one installer: the kustomize overlays
(`config/default`, `config/with-webhook`, `config/e2e`), applied by `make deploy`
with the Makefile patching the manager image afterwards. A controller with CRDs,
RBAC, metrics and admission webhooks is exactly what people expect to install
from a Helm chart, so a chart is the second consumer-facing path this project
has.

A second path forks the definition of the same controller. The failure mode is
quiet: the chart keeps installing yesterday's objects while `config/` moves on,
and nothing in `make ci` notices, because the chart is not rendered by anything
that also renders `config/`. The failure is worse than a cosmetic drift. A chart
whose Deployment does not match `config/manager/manager.yaml` can install a
controller with the wrong args, the wrong webhook port or a missing RBAC rule,
and it looks installed.

The work also exposed a bug in the first path. `config/with-webhook` adds the
webhook Service, the cert-manager Issuer and Certificates and the two webhook
configurations outside `../default`, and kustomize's transformers only reach the
resources of the kustomization that declares them. Those objects therefore
landed in a namespace called `system`: cert-manager created the serving Secret
there and the manager in `turnfleet-system` mounted a Secret that did not
exist. Writing the chart against that overlay is what surfaced it.

## Decision

`charts/turnfleet` installs the controller. `config/` remains the source of
truth for what the controller is, and `make helm-drift` holds the chart to it.

* `hack/helm-drift` renders `config/default`, `config/with-webhook` and the
  chart, and compares them object by object
  (`apiVersion`/`kind`/`namespace`/`name`) and then field by field, on JSON with
  sorted keys. A missing or extra object is a failure, and so is any field that
  differs. It ignores only installer bookkeeping: the `Namespace` object
  kustomize creates and Helm does not, top-level `metadata.labels`, the
  installer's keys in the label maps that remain (`managed-by`, `instance`,
  `version`, `helm.sh/*`), `meta.helm.sh/*` annotations, and empty maps and lists
  where `config/` carries placeholders. The labels the cluster acts on are
  compared in full: the Deployment's selector, its pod template, the Services'
  selectors.
* The chart owns the ServiceAccount, RBAC, the manager Deployment, the metrics
  Service, the CRDs and, behind `webhook.enabled`, the webhook Service, the
  cert-manager objects and the webhook configurations. It never templates relay
  pods, and no Service, Ingress or load balancer enters the relay data path
  (ADR 3, ADR 4 stand unchanged).
* The CRDs under `charts/turnfleet/crds/` are a copy of `config/crd/bases`, and
  two things hold them to it. `helm-drift` renders the chart with
  `--include-crds` and compares the CustomResourceDefinition it finds against
  `config/crd/bases`, so a copy that is a release behind fails the check.
  `make manifests` also regenerates the copy and `make verify` runs the same
  regeneration, because the file is generated: regeneration is the answer for a
  generated file, and a chart whose CRDs are stale installs an old API.
* `helm` is a pinned Makefile tool (`HELM_VERSION`), downloaded from the release
  with the checksum published beside it and tracked by Renovate like the image
  pins (ADR 11). `make helm-verify` (`helm-lint` + `helm-drift`) runs in
  `make ci`.
* The chart is exercised as an installer, not only as a render: the e2e workflow
  runs `make test-e2e-chart`, which removes the kustomize deployment, installs
  the CRDs and the controller from the chart, and probes a relay from a pool
  created against that install. What kustomize deployed is removed first because
  Helm will not adopt objects it did not create, and the CRDs have to belong to
  the release that installs them.

## Consequences

* A change to `config/` that the chart does not follow fails `make ci`. That is
  the whole point of the guard, and it means the chart cannot be updated "later".
* Two installers have to keep working. The e2e job installs the controller twice
  and its timeout went from 60 to 90 minutes.
* The guard compares the controller's objects, not the relay path: relays come
  from the planner on both paths, so what a chart install cannot change is where
  relays run.
* The image is rendered from the same value on both sides. `config/` carries the
  builder's placeholder `controller:latest` and the Makefile patches the image on
  the cluster after applying; the guard renders the chart with that placeholder,
  and the chart's own default image and tag are covered by the chart install in
  the e2e run.
* The webhook overlay is now a real standalone overlay (its own `namespace`),
  which is a fix to `config/` that the chart work forced, not a chart-side
  workaround.
