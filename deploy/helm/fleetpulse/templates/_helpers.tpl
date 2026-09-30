{{/* Common labels. */}}
{{- define "fp.labels" -}}
app.kubernetes.io/part-of: fleetpulse
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{/* Selector labels; call with (dict "root" $ "component" "fleet-api"). */}}
{{- define "fp.selector" -}}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{- define "fp.componentLabels" -}}
{{ include "fp.labels" .root }}
app.kubernetes.io/component: {{ .component }}
{{- end }}

{{/* Image reference; call with (dict "root" $ "image" "pipeline"). */}}
{{- define "fp.image" -}}
{{ printf "%s/fleetpulse-%s:%s" .root.Values.global.imageRegistry .image (toString .root.Values.global.imageTag) }}
{{- end }}

{{/* Secret-backed env var; call with (dict "root" $ "name" "ENV" "key" "SECRET_KEY" "optional" false). */}}
{{- define "fp.secretEnv" -}}
- name: {{ .name }}
  valueFrom:
    secretKeyRef:
      name: {{ .root.Values.secrets.name }}
      key: {{ default .name .key }}
      {{- if .optional }}
      optional: true
      {{- end }}
{{- end }}

{{/* PostgreSQL DSN using Kubernetes $(VAR) expansion of a previously defined
     password env var; call with (dict "root" $ "user" "fleetpulse_api" "pwVar" "DB_API_PASSWORD"). */}}
{{- define "fp.pgUrl" -}}
{{- $pg := .root.Values.endpoints.postgres -}}
{{- $q := printf "sslmode=%s" $pg.sslmode -}}
{{- if $pg.caConfigMap }}{{ $q = printf "%s&sslrootcert=/etc/fleetpulse/db-ca/ca.pem" $q }}{{ end -}}
{{ printf "postgresql://%s:$(%s)@%s:%v/%s?%s" .user .pwVar $pg.host $pg.port $pg.database $q }}
{{- end }}

{{/* Redis URL for a logical database; call with (dict "root" $ "db" 0). */}}
{{- define "fp.redisUrl" -}}
{{- $r := .root.Values.endpoints.redis -}}
{{ printf "%s://:$(REDIS_PASSWORD)@%s:%v/%v" (ternary "rediss" "redis" $r.tls) $r.host $r.port .db }}
{{- end }}

{{/* Pod-level defaults shared by every workload. */}}
{{- define "fp.podSpecCommon" -}}
serviceAccountName: fleetpulse
automountServiceAccountToken: false
securityContext:
  {{- toYaml .Values.podSecurityContext | nindent 2 }}
{{- end }}

{{/* Environment for the Go pipeline services (Kafka, Postgres writer, Redis). */}}
{{- define "fp.pipelineEnv" -}}
{{ include "fp.secretEnv" (dict "root" . "name" "DB_WRITER_PASSWORD") }}
{{ include "fp.secretEnv" (dict "root" . "name" "REDIS_PASSWORD") }}
{{- if .Values.endpoints.kafka.saslMechanism }}
{{ include "fp.secretEnv" (dict "root" . "name" "KAFKA_SASL_USERNAME") }}
{{ include "fp.secretEnv" (dict "root" . "name" "KAFKA_SASL_PASSWORD") }}
{{- end }}
- name: DATABASE_URL
  value: {{ include "fp.pgUrl" (dict "root" . "user" "fleetpulse_writer" "pwVar" "DB_WRITER_PASSWORD") | quote }}
- name: REDIS_URL
  value: {{ include "fp.redisUrl" (dict "root" . "db" 0) | quote }}
{{- end }}

{{/* Writable scratch space: every container runs with a read-only root filesystem. */}}
{{- define "fp.tmpVolume" -}}
- name: tmp
  emptyDir: {sizeLimit: 256Mi}
{{- end }}

{{- define "fp.dbCaVolume" -}}
{{- if .Values.endpoints.postgres.caConfigMap }}
- name: db-ca
  configMap: {name: {{ .Values.endpoints.postgres.caConfigMap }}}
{{- end }}
{{- end }}

{{- define "fp.dbCaMount" -}}
{{- if .Values.endpoints.postgres.caConfigMap }}
- {name: db-ca, mountPath: /etc/fleetpulse/db-ca, readOnly: true}
{{- end }}
{{- end }}

{{/* Standard HTTP probes on the ops port. */}}
{{- define "fp.opsProbes" -}}
livenessProbe:
  httpGet: {path: /healthz, port: {{ .port }}}
  periodSeconds: 10
  failureThreshold: 6
readinessProbe:
  httpGet: {path: /readyz, port: {{ .port }}}
  periodSeconds: 5
  failureThreshold: 3
{{- end }}

{{/* HPA; call with (dict "root" $ "component" "x" "cfg" .Values.x.autoscaling). */}}
{{- define "fp.hpa" -}}
{{- if .cfg.enabled }}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ .component }}
  labels:
    {{- include "fp.componentLabels" . | nindent 4 }}
spec:
  scaleTargetRef: {apiVersion: apps/v1, kind: Deployment, name: {{ .component }}}
  minReplicas: {{ .cfg.minReplicas }}
  maxReplicas: {{ .cfg.maxReplicas }}
  metrics:
    - type: Resource
      resource: {name: cpu, target: {type: Utilization, averageUtilization: {{ .cfg.targetCPU }}}}
  behavior:
    scaleUp: {stabilizationWindowSeconds: 0, policies: [{type: Percent, value: 100, periodSeconds: 30}]}
    scaleDown: {stabilizationWindowSeconds: 300, policies: [{type: Percent, value: 25, periodSeconds: 60}]}
{{- end }}
{{- end }}

{{/* PodDisruptionBudget; call with (dict "root" $ "component" "x"). */}}
{{- define "fp.pdb" -}}
{{- if .root.Values.pdb.enabled }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ .component }}
  labels:
    {{- include "fp.componentLabels" . | nindent 4 }}
spec:
  maxUnavailable: 1
  selector:
    matchLabels:
      {{- include "fp.selector" . | nindent 6 }}
{{- end }}
{{- end }}

{{/* Spread replicas across zones and nodes. */}}
{{- define "fp.spread" -}}
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "fp.selector" . | nindent 8 }}
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        {{- include "fp.selector" . | nindent 8 }}
{{- end }}
