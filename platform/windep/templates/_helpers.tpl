{{/*
Helpers for the WinDep chart.

IMPORTANT: the workload selector label is the bare `app: windep-<component>` used by
the original manifests. Cilium (LB pool serviceSelector, BGP advertisement selector),
the NetworkPolicy podSelector, and the Services all match on it — so it must be emitted
verbatim and must NOT change across upgrades. `windep.selectorLabels` returns exactly
that single label; do not add to it.
*/}}

{{- define "windep.namespace" -}}
{{- .Values.namespace.name | default "windep" -}}
{{- end -}}

{{/* Immutable selector label for a component. Arg: dict "ctx" $ "component" "api" */}}
{{- define "windep.selectorLabels" -}}
app: windep-{{ .component }}
{{- end -}}

{{/*
Full metadata labels for a component: the selector label + standard k8s recommended
labels + any commonLabels. Arg: dict "ctx" $ "component" "api"
*/}}
{{- define "windep.labels" -}}
app: windep-{{ .component }}
app.kubernetes.io/name: windep-{{ .component }}
app.kubernetes.io/component: {{ .component }}
app.kubernetes.io/part-of: windep
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/managed-by: {{ .ctx.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .ctx.Chart.Name .ctx.Chart.Version | replace "+" "_" }}
{{- with .ctx.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* imagePullSecrets block (list of {name:}). Renders nothing when empty. */}}
{{- define "windep.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
lbIPs renders the io.cilium/lb-ipam-ips value: the IPv4 VIP alone, or "v4,v6" when a v6
VIP is set (Cilium accepts a comma-separated list for dual-stack). Arg: dict "vip" .. "vip6" ..
*/}}
{{- define "windep.lbIPs" -}}
{{- if .vip6 -}}{{ printf "%s,%s" .vip .vip6 }}{{- else -}}{{ .vip }}{{- end -}}
{{- end -}}
