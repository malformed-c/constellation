{{/* Common labels applied to every Constellation object. */}}
{{- define "constellation.labels" -}}
app.kubernetes.io/part-of: constellation
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Fail fast if the API server address wasn't provided (kube-proxy is replaced,
     so the agent/operator have no kube-proxy to reach the API server through). */}}
{{- define "constellation.k8sServiceHost" -}}
{{- if not .Values.k8sServiceHost -}}
{{- fail "k8sServiceHost is required (the API server VIP), e.g. --set k8sServiceHost=192.168.50.1" -}}
{{- end -}}
{{- .Values.k8sServiceHost -}}
{{- end -}}
