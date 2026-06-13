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
Build a fully-qualified image reference for a codearmory service.
Prepends imageRegistry when set: "<registry>/<repo>:<tag>" or "<repo>:<tag>".
Usage: {{ include "codearmory.image" (list . .Values.gatekeeper) }}
*/}}
{{- define "codearmory.image" -}}
{{- $root := index . 0 -}}
{{- $svc := index . 1 -}}
{{- $repo := $svc.image.repository -}}
{{- $tag := include "codearmory.imageTag" (list $root $svc) -}}
{{- if $root.Values.imageRegistry -}}
{{- printf "%s/%s:%s" $root.Values.imageRegistry $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $repo $tag -}}
{{- end -}}
{{- end }}

{{/*
Derive a deterministic bootstrap service key from the release name, namespace,
and service name. Both gatekeeper-secret and the service's own secret use this
helper as their fallback when no existing Secret is found in the cluster (i.e.
on first install). Because the same inputs produce the same sha256sum output,
both sides always agree — eliminating the first-install key mismatch that
occurs when each template calls randAlphaNum independently.

The key is only valid until the first rotation (~25 minutes post-boot), after
which the service replaces it with a cryptographically random rotated key.

Usage: {{ include "codearmory.bootstrapServiceKey" (list . "containers") }}
*/}}
{{- define "codearmory.bootstrapServiceKey" -}}
{{- $root := index . 0 -}}
{{- $svcName := index . 1 -}}
{{- printf "%s|%s|%s|codearmory-bootstrap-key" $root.Release.Name $root.Release.Namespace $svcName | sha256sum -}}
{{- end -}}

{{/*
Construct a PostgreSQL DSN for a named database. Used only by test pods that need
a render-time URL; prefer codearmory.postgresql.env for service deployments.
Usage: {{ include "codearmory.postgresql.dsn" (list . "gatekeeper") }}
*/}}
{{- define "codearmory.postgresql.dsn" -}}
{{- $root := index . 0 -}}
{{- $db := index . 1 -}}
{{- printf "postgres://postgres:%s@%s-postgresql:5432/%s" ($root.Values.postgresql.auth.postgresPassword | urlquery) $root.Release.Name $db -}}
{{- end }}

{{/*
Inject DATABASE_URL (and PGPASSWORD) for a service container.
When postgresql.enabled=true the Bitnami subchart auto-generates the password and
stores it in <release>-postgresql; PGPASSWORD sources it at pod startup so no
password is embedded in values.yaml or in any rendered Secret template.
When postgresql.enabled=false the value comes from the service-specific Secret.
Usage: {{- include "codearmory.postgresql.env" (list . "gatekeeper" "gatekeeper" (include "codearmory.fullname" .)) | nindent 12 }}
  args: root, service-name, db-name, fullname
*/}}
{{- define "codearmory.postgresql.env" -}}
{{- $root := index . 0 -}}
{{- $svc  := index . 1 -}}
{{- $db   := index . 2 -}}
{{- $full := index . 3 -}}
{{- if $root.Values.postgresql.enabled }}
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $root.Release.Name }}-postgresql
      key: postgres-password
- name: DATABASE_URL
  value: {{ printf "postgres://postgres:$(PGPASSWORD)@%s-postgresql:5432/%s" $root.Release.Name $db | quote }}
{{- else }}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ $full }}-{{ $svc }}
      key: database-url
{{- end }}
{{- end }}

{{/*
OpenTelemetry environment variables for a service: the per-service OTLP endpoint
plus the chart-global trace sampler. The sampler vars are emitted only when an
endpoint is set (a service with no endpoint exports nothing, so the sampler is
moot). Sampler names follow the OTEL_TRACES_SAMPLER spec — set
otel.sampler=parentbased_traceidratio with otel.samplerArg (e.g. "0.1") to sample
down high-volume deployments instead of tracing every request.
Usage: {{- include "codearmory.otel.envs" (list . .Values.conductor.env) | nindent 12 }}
  args: root, service-env-map
*/}}
{{- define "codearmory.otel.envs" -}}
{{- $root := index . 0 -}}
{{- $env := index . 1 -}}
{{- if $env.otelEndpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ $env.otelEndpoint | quote }}
{{- with $root.Values.otel }}
{{- if .sampler }}
- name: OTEL_TRACES_SAMPLER
  value: {{ .sampler | quote }}
{{- end }}
{{- if .samplerArg }}
- name: OTEL_TRACES_SAMPLER_ARG
  value: {{ .samplerArg | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
TLS environment variables for a service.
Usage: {{ include "codearmory.tls.envs" .Values.someservice.tls }}
*/}}
{{- define "codearmory.tls.envs" -}}
{{- if .certSecret }}
- name: TLS_CERT_FILE
  value: /etc/tls/server/tls.crt
- name: TLS_KEY_FILE
  value: /etc/tls/server/tls.key
{{- if .clientAuth }}
- name: TLS_CLIENT_AUTH
  value: {{ .clientAuth | quote }}
{{- end }}
{{- if .caSecret }}
- name: TLS_CLIENT_CA_FILE
  value: /etc/tls/ca/ca.crt
{{- end }}
{{- end }}
{{- if .clientCertSecret }}
- name: TLS_CLIENT_CERT_FILE
  value: /etc/tls/client/tls.crt
- name: TLS_CLIENT_KEY_FILE
  value: /etc/tls/client/tls.key
{{- end }}
{{- if .caBundle }}
- name: TLS_CA_FILE
  value: /etc/tls/ca-bundle/ca.crt
{{- end }}
{{- end }}

{{/*
TLS volume mounts for a service.
Usage: {{ include "codearmory.tls.volumeMounts" .Values.someservice.tls }}
*/}}
{{- define "codearmory.tls.volumeMounts" -}}
{{- if .certSecret }}
- name: tls-server
  mountPath: /etc/tls/server
  readOnly: true
{{- end }}
{{- if .caSecret }}
- name: tls-ca
  mountPath: /etc/tls/ca
  readOnly: true
{{- end }}
{{- if .clientCertSecret }}
- name: tls-client
  mountPath: /etc/tls/client
  readOnly: true
{{- end }}
{{- if .caBundle }}
- name: tls-ca-bundle
  mountPath: /etc/tls/ca-bundle
  readOnly: true
{{- end }}
{{- end }}

{{/*
TLS volumes for a service.
Usage: {{ include "codearmory.tls.volumes" .Values.someservice.tls }}
*/}}
{{- define "codearmory.tls.volumes" -}}
{{- if .certSecret }}
- name: tls-server
  secret:
    secretName: {{ .certSecret }}
{{- end }}
{{- if .caSecret }}
- name: tls-ca
  secret:
    secretName: {{ .caSecret }}
{{- end }}
{{- if .clientCertSecret }}
- name: tls-client
  secret:
    secretName: {{ .clientCertSecret }}
{{- end }}
{{- if .caBundle }}
- name: tls-ca-bundle
  secret:
    secretName: {{ .caBundle }}
{{- end }}
{{- end }}

{{/*
initContainer that polls gatekeeper's /healthz until it returns HTTP 200.
Inject into every service that calls StartKeyRotation so it never races gatekeeper on startup.
Usage: {{- include "codearmory.initContainer.waitForGatekeeper" . | nindent 8 }}
*/}}
{{- define "codearmory.initContainer.waitForGatekeeper" -}}
- name: wait-for-gatekeeper
  image: {{ .Values.waitContainerImage | default "busybox:1.36" }}
  command:
    - sh
    - -c
    - >-
      until wget -qO- http://{{ include "codearmory.fullname" . }}-gatekeeper:{{ .Values.gatekeeper.port }}/healthz
      2>/dev/null; do echo "waiting for gatekeeper"; sleep 2; done
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    runAsNonRoot: true
    runAsUser: 65534
    capabilities:
      drop: ["ALL"]
{{- end }}

{{/*
initContainer that polls registry's /healthz until it returns HTTP 200.
Inject into conductor so it never polls the registry before it is ready.
Usage: {{- include "codearmory.initContainer.waitForRegistry" . | nindent 8 }}
*/}}
{{- define "codearmory.initContainer.waitForRegistry" -}}
- name: wait-for-registry
  image: {{ .Values.waitContainerImage | default "public.ecr.aws/docker/library/busybox:1.36" }}
  command:
    - sh
    - -c
    - >-
      until wget -qO- http://{{ include "codearmory.fullname" . }}-registry:{{ .Values.registry.port }}/healthz
      2>/dev/null; do echo "waiting for registry"; sleep 2; done
  securityContext:
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: true
    runAsNonRoot: true
    runAsUser: 65534
    capabilities:
      drop: ["ALL"]
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
