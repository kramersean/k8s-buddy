{{/*
Standard Helm chart name helpers, following the same convention `helm create`
scaffolds (and every widely-used community chart follows), so this chart's
naming is unsurprising to anyone who has installed a Helm chart before.
*/}}

{{/*
Expand the name of the chart.
*/}}
{{- define "k8s-buddy.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name. Truncated at 63 chars because
some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "k8s-buddy.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "k8s-buddy.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels. All five standard app.kubernetes.io/* keys, on every object
this chart creates -- the same invariant Global Constraints requires of
every kustomize-generated object in this repo.
*/}}
{{- define "k8s-buddy.labels" -}}
helm.sh/chart: {{ include "k8s-buddy.chart" . }}
{{ include "k8s-buddy.selectorLabels" . }}
app.kubernetes.io/part-of: k8s-buddy
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
Selector labels: app.kubernetes.io/name, /instance, and /component -- the
three-key selector every Deployment's spec.selector.matchLabels here uses,
matching the shape (if not the exact keys) every kustomize-generated
Deployment in this repo already uses. Immutable once a Deployment exists, so
these three keys alone (never helm.sh/chart or app.kubernetes.io/version,
both of which change on every upgrade) are what spec.selector pins.
*/}}
{{- define "k8s-buddy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "k8s-buddy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end -}}

{{/*
The ServiceAccount name the operator's Deployment runs as.
*/}}
{{- define "k8s-buddy.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "k8s-buddy.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The namespace every namespaced object in this chart targets: always
`.Release.Namespace` (never a name independent of it), which is exactly what
keeps `helm install <release> -n <ns> --create-namespace` fully
self-contained -- see values.yaml's own `namespace.create` comment for why
this matters (no collision with any other release's or kustomize's own
identically-named objects).
*/}}
{{- define "k8s-buddy.namespace" -}}
{{- .Release.Namespace -}}
{{- end -}}

{{/*
The namespace the optional sample Plant (and, if requested, its own
Namespace object) lands in: `.Values.plant.namespace.name` if set, else the
release namespace. See values.yaml's own `plant.namespace` comment for why
the default is deliberately NOT `k8s-buddy-plants`.
*/}}
{{- define "k8s-buddy.plantNamespace" -}}
{{- default (include "k8s-buddy.namespace" .) .Values.plant.namespace.name -}}
{{- end -}}
