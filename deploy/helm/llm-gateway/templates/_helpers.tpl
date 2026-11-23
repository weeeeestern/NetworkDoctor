{{- define "llm-gateway.labels" -}}
app.kubernetes.io/name: llm-gateway
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/part-of: networkdoctor
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "llm-gateway.selectorLabels" -}}
app.kubernetes.io/name: llm-gateway
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
