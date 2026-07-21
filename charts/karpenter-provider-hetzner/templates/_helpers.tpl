{{/*
Expand the name of the chart.
*/}}
{{- define "karpenter-provider-hetzner.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "karpenter-provider-hetzner.fullname" -}}
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
{{- define "karpenter-provider-hetzner.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "karpenter-provider-hetzner.labels" -}}
helm.sh/chart: {{ include "karpenter-provider-hetzner.chart" . }}
app.kubernetes.io/name: {{ include "karpenter-provider-hetzner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "karpenter-provider-hetzner.selectorLabels" -}}
app.kubernetes.io/name: {{ include "karpenter-provider-hetzner.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Fully qualified Secret name.
*/}}
{{- define "karpenter-provider-hetzner.secretName" -}}
{{- printf "%s-%s" (include "karpenter-provider-hetzner.fullname" .) (.Values.auth.secretRef.name | default "hcloud-token") -}}
{{- end -}}
