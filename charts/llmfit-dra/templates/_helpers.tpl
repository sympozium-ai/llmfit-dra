{{- define "llmfit-dra.name" -}}
{{- .Chart.Name -}}
{{- end -}}

{{- define "llmfit-dra.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "llmfit-dra.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "llmfit-dra.name" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "llmfit-dra.controllerServiceAccountName" -}}
{{- include "llmfit-dra.serviceAccountName" . -}}-modelclaim
{{- end -}}

{{- define "llmfit-dra.labels" -}}
app.kubernetes.io/name: {{ include "llmfit-dra.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "llmfit-dra.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}
