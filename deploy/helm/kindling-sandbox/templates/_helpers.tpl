{{/* Nombre base del chart. */}}
{{- define "kindling-sandbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Nombre completo: release + chart, sin duplicar. */}}
{{- define "kindling-sandbox.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $n := default .Chart.Name .Values.nameOverride -}}
{{- if contains $n .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $n | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "kindling-sandbox.operatorName" -}}
{{- printf "%s-operator" (include "kindling-sandbox.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kindling-sandbox.gatewayName" -}}
{{- printf "%s-gateway" (include "kindling-sandbox.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Etiquetas comunes. */}}
{{- define "kindling-sandbox.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "kindling-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{- define "kindling-sandbox.operatorSelector" -}}
app.kubernetes.io/name: {{ include "kindling-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: operator
{{- end -}}

{{- define "kindling-sandbox.gatewaySelector" -}}
app.kubernetes.io/name: {{ include "kindling-sandbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: gateway
{{- end -}}

{{/* Imagen del operador: repositorio:tag (tag por defecto = appVersion). */}}
{{- define "kindling-sandbox.operatorImage" -}}
{{- printf "%s:%s" .Values.operator.image.repository (default .Chart.AppVersion .Values.operator.image.tag) -}}
{{- end -}}

{{/* Imagen del gateway; no hay imagen oficial, así que el repositorio es obligatorio. */}}
{{- define "kindling-sandbox.gatewayImage" -}}
{{- $repo := required "gateway.image.repository es obligatorio con gateway.enabled=true (no hay imagen oficial del gateway)" .Values.gateway.image.repository -}}
{{- printf "%s:%s" $repo (default .Chart.AppVersion .Values.gateway.image.tag) -}}
{{- end -}}

{{/* ¿El gateway sirve HTTPS? */}}
{{- define "kindling-sandbox.gatewayTLS" -}}
{{- if .Values.gateway.tls.existingSecret -}}true{{- end -}}
{{- end -}}

{{/* URL del frontal que usa el operador: la explícita o el Service del gateway. */}}
{{- define "kindling-sandbox.frontalURL" -}}
{{- if .Values.operator.frontal.url -}}
{{- .Values.operator.frontal.url -}}
{{- else if .Values.gateway.enabled -}}
{{- printf "%s://%s.%s.svc:%d" (ternary "https" "http" (not (empty (include "kindling-sandbox.gatewayTLS" .)))) (include "kindling-sandbox.gatewayName" .) .Release.Namespace (int .Values.gateway.service.port) -}}
{{- else -}}
{{- required "operator.frontal.url es obligatorio si gateway.enabled=false" .Values.operator.frontal.url -}}
{{- end -}}
{{- end -}}

{{/* ServiceAccount del operador. */}}
{{- define "kindling-sandbox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kindling-sandbox.operatorName" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}
