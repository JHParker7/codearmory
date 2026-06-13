{{/* Expand the name of the chart. */}}
{{- define "outpost.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "outpost.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "outpost.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "outpost.labels" -}}
app.kubernetes.io/name: {{ include "outpost.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "outpost.serviceAccountName" -}}
{{- default (include "outpost.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/* hasModule returns "true" if the named module is in .Values.modules (csv). */}}
{{- define "outpost.hasModule" -}}
{{- $module := index . 1 -}}
{{- $root := index . 0 -}}
{{- $found := "" -}}
{{- range (splitList "," $root.Values.modules) -}}
{{- if eq (trim .) $module -}}{{- $found = "true" -}}{{- end -}}
{{- end -}}
{{- $found -}}
{{- end -}}
