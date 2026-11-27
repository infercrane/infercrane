{{- define "infercrane-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "infercrane-agent.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "infercrane-agent.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "infercrane-agent.labels" -}}
app.kubernetes.io/name: {{ include "infercrane-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "infercrane-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "infercrane-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "infercrane-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "infercrane-agent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "infercrane-agent.executorNamespace" -}}
{{- default .Release.Namespace .Values.executor.namespace -}}
{{- end -}}

{{- define "infercrane-agent.hashKeySecret" -}}
{{- default (printf "%s-hash-key" (include "infercrane-agent.fullname" .)) .Values.agent.hashKeySecret.name -}}
{{- end -}}

{{- define "infercrane-agent.workerSecret" -}}
{{- default (printf "%s-worker" (include "infercrane-agent.fullname" .)) .Values.executor.workerSecret.name -}}
{{- end -}}

{{- define "infercrane-agent.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- /* Release images are tagged with the git tag, "v" + the version. */ -}}
{{- printf "%s:%s" .Values.image.repository (default (printf "v%s" .Chart.AppVersion) .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{- /* Sources that may reach candidate pods: by default this release's agent
pods, in the release namespace, so the policy holds when candidates start in
another namespace. */ -}}
{{- define "infercrane-agent.engineIngress" -}}
{{- if .Values.executor.engineIngress -}}
{{- .Values.executor.engineIngress -}}
{{- else -}}
{{- printf "namespace:kubernetes.io/metadata.name=%s;pods:app.kubernetes.io/name=%s+app.kubernetes.io/instance=%s" .Release.Namespace (include "infercrane-agent.name" .) .Release.Name -}}
{{- end -}}
{{- end -}}
