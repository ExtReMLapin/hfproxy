{{- define "hfproxy.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{- else -}}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{- end -}}
{{- end -}}

{{- define "hfproxy.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "hfproxy.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hfproxy.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
{{- end -}}

{{/* Environment shared by the server and the migration job. */}}
{{- define "hfproxy.env" -}}
{{- $cfg := mergeOverwrite (dict) .Values.config .Values.extraConfig -}}
{{- if not (hasKey $cfg "STAGING_DIR") }}{{ $_ := set $cfg "STAGING_DIR" "/staging" }}{{ end -}}
{{- if .Values.metrics.enabled }}{{ $_ := set $cfg "METRICS_ENABLED" "true" }}{{ $_ := set $cfg "METRICS_ADDR" (printf "0.0.0.0:%d" (int .Values.metrics.port)) }}{{ end -}}
{{- $_ := set $cfg "LISTEN_ADDR" "0.0.0.0:8080" -}}
{{- range $name, $value := $cfg }}
{{- if ne (toString $value) "" }}
- name: HFPROXY_{{ $name }}
  value: {{ toString $value | quote }}
{{- end }}
{{- end }}
{{- range $name, $key := .Values.secrets.files }}
- name: HFPROXY_{{ $name }}_FILE
  value: /run/secrets/hfproxy/{{ $key }}
{{- end }}
{{- end -}}

{{- define "hfproxy.secretVolume" -}}
- name: secrets
  secret:
    secretName: {{ required "secrets.existingSecret is required" .Values.secrets.existingSecret }}
    defaultMode: 0440
    items:
    {{- range $name, $key := .Values.secrets.files }}
      - key: {{ $key }}
        path: {{ $key }}
    {{- end }}
{{- end -}}
