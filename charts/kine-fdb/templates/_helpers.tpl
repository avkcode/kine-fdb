{{/* Shared names, labels, and the FoundationDB endpoint for the kine-fdb chart. */}}

{{- define "kine-fdb.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "kine-fdb.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Labels shared by every object of the release. */}}
{{- define "kine-fdb.commonLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "kine-fdb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* Labels of kine-fdb objects. */}}
{{- define "kine-fdb.labels" -}}
{{ include "kine-fdb.commonLabels" . }}
app.kubernetes.io/component: server
{{- end -}}

{{/* Selector of kine-fdb pods. The component keeps it from matching the
evaluation FoundationDB pod of the same release. */}}
{{- define "kine-fdb.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kine-fdb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: server
{{- end -}}

{{/* Labels and selector of the evaluation FoundationDB. */}}
{{- define "kine-fdb.fdbLabels" -}}
{{ include "kine-fdb.commonLabels" . }}
app.kubernetes.io/component: foundationdb
{{- end -}}

{{- define "kine-fdb.fdbSelectorLabels" -}}
app.kubernetes.io/name: {{ include "kine-fdb.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: foundationdb
{{- end -}}

{{- define "kine-fdb.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "kine-fdb.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "kine-fdb.devFdbName" -}}
{{- printf "%s-fdb" (include "kine-fdb.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Name of the ConfigMap or Secret that provides the cluster file. */}}
{{- define "kine-fdb.clusterFileSource" -}}
{{- $cf := .Values.fdb.clusterFile -}}
{{- if .Values.fdb.dev.enabled -}}
configMap: {{ include "kine-fdb.devFdbName" . }}
{{- else if $cf.configMap -}}
configMap: {{ $cf.configMap }}
{{- else if $cf.secret -}}
secret: {{ $cf.secret }}
{{- else if $cf.value -}}
configMap: {{ printf "%s-cluster-file" (include "kine-fdb.fullname" .) }}
{{- else -}}
{{- fail "set fdb.clusterFile.configMap, fdb.clusterFile.secret, or fdb.clusterFile.value (or enable fdb.dev for evaluation)" -}}
{{- end -}}
{{- end -}}

{{- define "kine-fdb.clusterFileKey" -}}
{{- if or .Values.fdb.dev.enabled (and .Values.fdb.clusterFile.value (not .Values.fdb.clusterFile.configMap) (not .Values.fdb.clusterFile.secret)) -}}
cluster-file
{{- else -}}
{{- .Values.fdb.clusterFile.key -}}
{{- end -}}
{{- end -}}

{{/* The fdb:// endpoint passed to kine-fdb. */}}
{{- define "kine-fdb.endpoint" -}}
{{- $params := dict "namespace" .Values.fdb.namespace -}}
{{- range $k, $v := .Values.fdb.params -}}
{{- $_ := set $params $k (toString $v) -}}
{{- end -}}
{{- if .Values.fdb.tls.enabled -}}
{{- $_ := set $params "tls_cert_file" "/etc/foundationdb/tls/tls.crt" -}}
{{- $_ := set $params "tls_key_file" "/etc/foundationdb/tls/tls.key" -}}
{{- $_ := set $params "tls_ca_file" "/etc/foundationdb/tls/ca.crt" -}}
{{- with .Values.fdb.tls.verifyPeers -}}
{{- $_ := set $params "tls_verify_peers" . -}}
{{- end -}}
{{- end -}}
{{- $query := list -}}
{{- range $k := keys $params | sortAlpha -}}
{{- $query = append $query (printf "%s=%s" $k (urlquery (get $params $k))) -}}
{{- end -}}
fdb:///etc/foundationdb/fdb.cluster?{{ join "&" $query }}
{{- end -}}
