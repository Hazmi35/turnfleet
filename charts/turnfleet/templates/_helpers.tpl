# SPDX-License-Identifier: AGPL-3.0-only
# Copyright (C) 2026 Turnfleet Authors
#
# Names and labels every template shares.
{{/*
The prefix on every object this chart creates. It is not derived from the
release name: the names have to be the ones the project's own manifests give the
controller, so that `make helm-drift` can hold the two against each other and a
`helm install` puts an object where a `kubectl get turnfleet-controller-manager`
expects it.
*/}}
{{- define "turnfleet.name" -}}
{{- default "turnfleet" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Labels that name the application. `app.kubernetes.io/name` is the project, not
the release: the metrics Service selects the manager by these two labels and the
Deployment's selector is immutable, so they are not configurable.
*/}}
{{- define "turnfleet.selectorLabels" -}}
app.kubernetes.io/name: turnfleet
control-plane: controller-manager
{{- end -}}

{{/*
Selector labels plus the ones that name the installer and the release. The
drift guard ignores the installer-specific labels on both sides, because
kustomize writes `app.kubernetes.io/managed-by: kustomize` and Helm writes the
release name, the chart version and `managed-by: Helm`.
*/}}
{{- define "turnfleet.labels" -}}
{{ include "turnfleet.selectorLabels" . }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
podLabels cannot override the selector labels. spec.selector is fixed, so setting
either of them on the pods would stop the Deployment's selector from matching its
own pod template, and the API server rejects the object.
*/}}
{{- define "turnfleet.checkPodLabels" -}}
{{- range $key := list "app.kubernetes.io/name" "control-plane" }}
{{- if hasKey $.Values.podLabels $key }}
{{- fail (printf "podLabels must not set %q: the Deployment selects on that label" $key) }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "turnfleet.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- printf "%s-controller-manager" (include "turnfleet.name" .) -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create is false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The port part of a bind address (`:8081`, `0.0.0.0:8081`). The manager takes the
address as a flag, while the container port, the probes and the Service target
want the number, and a chart that hard-coded 8081 next to a configurable address
would probe a port the manager no longer binds.
*/}}
{{- define "turnfleet.bindPort" -}}
{{- regexFind "[0-9]+$" . -}}
{{- end -}}

{{/*
The manager's arguments, in the order config/ ends up with them: the metrics
flag the default overlay inserts first, then the flags config/manager carries,
then the webhook flags the webhook overlay appends, then anything the user adds.
*/}}
{{- define "turnfleet.managerArgs" -}}
{{- if .Values.metrics.enabled }}
- --metrics-bind-address={{ .Values.metrics.bindAddress }}
{{- end }}
{{- if .Values.leaderElection.enabled }}
- --leader-elect
{{- end }}
- --health-probe-bind-address={{ .Values.healthProbe.bindAddress }}
{{- if .Values.webhook.enabled }}
- --webhook-cert-path={{ .Values.webhook.certPath }}
- --webhook-port={{ .Values.webhook.port }}
{{- end }}
{{- if and .Values.metrics.enabled (not .Values.metrics.secure) }}
- --metrics-secure=false
{{- end }}
{{- range .Values.extraArgs }}
- {{ . | quote }}
{{- end }}
{{- end -}}
