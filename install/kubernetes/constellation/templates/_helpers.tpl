{{/* Common labels applied to every Constellation object. */}}
{{- define "constellation.labels" -}}
app.kubernetes.io/part-of: constellation
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/* Fail fast if the API server address wasn't provided in any form (kube-proxy
     is replaced, so the agent/operator have no kube-proxy path to it). k8sServiceHost
     is only required when k8sAPIServerURLs is also empty - with the latter set,
     Cilium's own --k8s-api-server-urls handles the actual connection. This value
     still has to render as something non-empty in that case though: createConfig's
     k8sAPIServerURLs path calls rest.InClusterConfig() first (for the pod's
     token/CA) before overriding .Host, and InClusterConfig() bails immediately
     on an empty KUBERNETES_SERVICE_HOST - so a placeholder is required, not
     just permitted, even though the real connection host comes from
     k8sAPIServerURLs. */}}
{{- define "constellation.k8sServiceHost" -}}
{{- if .Values.k8sServiceHost -}}
{{- .Values.k8sServiceHost -}}
{{- else if .Values.k8sAPIServerURLs -}}
kubernetes.default.svc
{{- else -}}
{{- fail "k8sServiceHost is required when k8sAPIServerURLs is not set (the API server VIP), e.g. --set k8sServiceHost=192.168.50.1" -}}
{{- end -}}
{{- end -}}
