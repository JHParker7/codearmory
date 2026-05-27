{{/*
Expand the name of the chart.
*/}}
{{- define "codearmory.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "codearmory.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "codearmory.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "codearmory.labels" -}}
helm.sh/chart: {{ include "codearmory.chart" . }}
app.kubernetes.io/name: {{ include "codearmory.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Resolve the image tag for a service. Falls back to .Values.imageTag.
Usage: {{ include "codearmory.imageTag" (list . .Values.gatekeeper) }}
*/}}
{{- define "codearmory.imageTag" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- coalesce $svc.image.tag $root.Values.imageTag "latest" -}}
{{- end }}

{{/*
Construct a PostgreSQL DSN for a named database.
Usage: {{ include "codearmory.postgresql.dsn" (list . "gatekeeper") }}
*/}}
{{- define "codearmory.postgresql.dsn" -}}
{{- $root := index . 0 -}}
{{- $db := index . 1 -}}
{{- printf "postgres://postgres:%s@%s-postgresql:5432/%s" ($root.Values.postgresql.auth.postgresPassword | urlquery) $root.Release.Name $db -}}
{{- end }}

{{/*
Forge service account name.
*/}}
{{- define "codearmory.forge.serviceAccountName" -}}
{{- if .Values.forge.serviceAccount.create }}
{{- default (printf "%s-forge" (include "codearmory.fullname" .)) .Values.forge.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.forge.serviceAccount.name }}
{{- end }}
{{- end }}
