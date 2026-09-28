{{/*
Chart name.
*/}}
{{- define "networkdoctor.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name (release-aware, 63 char limit).
*/}}
{{- define "networkdoctor.fullname" -}}
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

{{- define "networkdoctor.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels shared by every object.
*/}}
{{- define "networkdoctor.labels" -}}
helm.sh/chart: {{ include "networkdoctor.chart" . }}
app.kubernetes.io/name: {{ include "networkdoctor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: networkdoctor
{{- end }}

{{/*
Agent selector labels / names.
*/}}
{{- define "networkdoctor.agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "networkdoctor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end }}

{{- define "networkdoctor.agent.fullname" -}}
{{- printf "%s-agent" (include "networkdoctor.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Backend selector labels / names.
*/}}
{{- define "networkdoctor.backend.selectorLabels" -}}
app.kubernetes.io/name: {{ include "networkdoctor.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: backend
{{- end }}

{{- define "networkdoctor.backend.fullname" -}}
{{- printf "%s-backend" (include "networkdoctor.fullname" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "networkdoctor.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "networkdoctor.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Backend webhook URL as Alertmanager should see it.
*/}}
{{- define "networkdoctor.backend.webhookURL" -}}
{{- printf "http://%s.%s.svc:%d/webhooks/alertmanager" (include "networkdoctor.backend.fullname" .) .Release.Namespace (int .Values.backend.service.port) }}
{{- end }}
