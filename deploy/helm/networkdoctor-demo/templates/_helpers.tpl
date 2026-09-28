{{- define "demo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo.labels" -}}
helm.sh/chart: {{ include "demo.chart" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: networkdoctor-demo
{{- end }}

{{/* The demo app object name is the app name itself (e.g. "catshop"). */}}
{{- define "demo.app.name" -}}
{{- .Values.app.name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo.app.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demo.app.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: app
{{- end }}

{{- define "demo.loadgen.name" -}}
{{- printf "%s-loadgen" (include "demo.app.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo.loadgen.selectorLabels" -}}
app.kubernetes.io/name: {{ include "demo.loadgen.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: loadgen
{{- end }}
