{{- define "iscsi.image" -}}
{{- $repository := required "image repository is required" .image.repository -}}
{{- if hasPrefix "/" $repository -}}
{{- $repository = printf "%s%s" .baseRepo $repository -}}
{{- end -}}
{{- if contains "@" $repository -}}
{{- $repository | quote -}}
{{- else if .image.tag -}}
{{- printf "%s:%s" $repository .image.tag | quote -}}
{{- else -}}
{{- $repository | quote -}}
{{- end -}}
{{- end -}}

{{- define "iscsi.labels" -}}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
app.kubernetes.io/name: {{ .Chart.Name | quote }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end -}}
