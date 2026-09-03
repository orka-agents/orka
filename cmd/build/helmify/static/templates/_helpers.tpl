{{/*
Expand the name of the chart.
*/}}
{{- define "orka.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "orka.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "orka.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "orka.labels" -}}
helm.sh/chart: {{ include "orka.chart" . }}
{{ include "orka.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.labels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "orka.selectorLabels" -}}
app.kubernetes.io/name: {{ include "orka.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "orka.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "orka.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create release-scoped worker ServiceAccount names. Reserve room for each
suffix so long release names cannot collapse all trust tiers to one name.
*/}}
{{- define "orka.aiWorkerServiceAccountName" -}}
{{- printf "%s-ai-worker" (include "orka.fullname" . | trunc 53 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.vendorWorkerServiceAccountName" -}}
{{- printf "%s-vendor-worker" (include "orka.fullname" . | trunc 49 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.containerWorkerServiceAccountName" -}}
{{- printf "%s-container-worker" (include "orka.fullname" . | trunc 46 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create release-scoped harness v1 wrapper names while reserving room for the
longest suffix so names remain valid DNS labels for long Helm release names.
*/}}
{{- define "orka.harnessV1Name" -}}
{{- printf "%s-agent-harness-wrapper" (include "orka.fullname" . | trunc 41 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1LedgerName" -}}
{{- printf "%s-harness-v1-ledger" (include "orka.fullname" . | trunc 45 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1DrainName" -}}
{{- printf "%s-drain" (include "orka.harnessV1Name" . | trunc 57 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1DrainEgressName" -}}
{{- printf "%s-egress" (include "orka.harnessV1DrainName" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1AbortName" -}}
{{- printf "%s-abort" (include "orka.harnessV1Name" . | trunc 57 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1AbortEgressName" -}}
{{- printf "%s-egress" (include "orka.harnessV1AbortName" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1DeleteDrainName" -}}
{{- printf "%s-delete" (include "orka.harnessV1Name" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1DeleteDrainEgressName" -}}
{{- printf "%s-egress" (include "orka.harnessV1DeleteDrainName" . | trunc 56 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Render the complete harness v1 Pod template from one canonical helper. The
ledger generation hashes this structure with a fixed sentinel in place of the
generation itself, so only a real Pod-template change advances the generation.
*/}}
{{- define "orka.harnessV1PodTemplate" -}}
{{- $root := .root -}}
{{- $generation := .generation -}}
metadata:
  labels:
    {{- include "orka.labels" $root | nindent 4 }}
    app.kubernetes.io/component: agent-harness-wrapper
    orka.ai/network-role: harness-v1
  {{- with $root.Values.harnessV1.tls.rolloutNonce }}
  annotations:
    orka.ai/harness-v1-tls-rollout-nonce: {{ . | quote }}
  {{- end }}
spec:
  serviceAccountName: {{ include "orka.harnessV1Name" $root }}
  automountServiceAccountToken: false
  securityContext:
    runAsUser: 0
    runAsGroup: 0
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: wrapper
      image: {{ include "orka.imageRef" $root.Values.harnessV1.image | quote }}
      imagePullPolicy: {{ $root.Values.harnessV1.image.pullPolicy }}
      ports:
        - name: https
          containerPort: 8080
          protocol: TCP
      env:
        - name: ORKA_HARNESS_WRAPPER_RUNTIME
          value: multi
        - name: ORKA_HARNESS_WRAPPER_LISTEN_ADDR
          value: :8080
        - name: ORKA_CONTROLLER_URL
          value: http://{{ include "orka.fullname" $root }}.{{ $root.Release.Namespace }}.svc:{{ $root.Values.service.port }}
        - name: ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE
          value: /var/run/orka/harness-wrapper-auth/token
        - name: ORKA_HARNESS_WRAPPER_TLS_CERT_FILE
          value: /var/run/orka/harness-wrapper-tls/tls.crt
        - name: ORKA_HARNESS_WRAPPER_TLS_KEY_FILE
          value: /var/run/orka/harness-wrapper-tls/tls.key
        - name: ORKA_HARNESS_WRAPPER_ADMISSION_LEDGER_PATH
          value: /var/lib/orka/harness-v1/admission-ledger.db
        - name: ORKA_HARNESS_WRAPPER_LEDGER_GENERATION
          value: {{ $generation | quote }}
        - name: ORKA_HARNESS_WRAPPER_LEDGER_RETENTION
          value: {{ $root.Values.harnessV1.ledger.retention | quote }}
        - name: ORKA_ALLOW_BASH
          value: "true"
        - name: ORKA_HARNESS_WRAPPER_CHILD_UID
          value: "1000"
        - name: ORKA_HARNESS_WRAPPER_CHILD_GID
          value: "1000"
        - name: ORKA_CODEX_SANDBOX_MODE
          value: {{ $root.Values.harnessV1.codexSandboxMode | quote }}
      volumeMounts:
        - name: auth
          mountPath: /var/run/orka/harness-wrapper-auth
          readOnly: true
        - name: tls
          mountPath: /var/run/orka/harness-wrapper-tls
          readOnly: true
        - name: controller-api-token
          mountPath: /var/run/secrets/kubernetes.io/serviceaccount
          readOnly: true
        - name: ledger
          mountPath: /var/lib/orka/harness-v1
        - name: tmp
          mountPath: /tmp
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        runAsUser: 0
        runAsGroup: 0
        capabilities:
          drop:
            - ALL
          add:
            - SETUID
            - SETGID
            - CHOWN
            - KILL
            - FOWNER
      livenessProbe:
        httpGet:
          path: /v1/health
          port: https
          scheme: HTTPS
        initialDelaySeconds: 10
        periodSeconds: 20
      readinessProbe:
        httpGet:
          path: /v1/ready
          port: https
          scheme: HTTPS
        initialDelaySeconds: 5
        periodSeconds: 10
      {{- with $root.Values.harnessV1.resources }}
      resources:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  volumes:
    - name: auth
      secret:
        secretName: {{ $root.Values.harnessV1.auth.existingSecret | quote }}
        defaultMode: 0400
        items:
          - key: {{ $root.Values.harnessV1.auth.tokenKey | quote }}
            path: token
    - name: tls
      secret:
        secretName: {{ $root.Values.harnessV1.tls.existingSecret | quote }}
        defaultMode: 0400
        items:
          - key: tls.crt
            path: tls.crt
          - key: tls.key
            path: tls.key
          - key: ca.crt
            path: ca.crt
    - name: controller-api-token
      projected:
        defaultMode: 0400
        sources:
          - serviceAccountToken:
              path: token
              expirationSeconds: 3600
    - name: ledger
      persistentVolumeClaim:
        claimName: {{ include "orka.harnessV1LedgerName" $root }}
    - name: tmp
      emptyDir: {}
{{- end }}

{{- define "orka.harnessV1PodTemplateGeneration" -}}
{{- $template := include "orka.harnessV1PodTemplate" (dict "root" . "generation" "ORKA_HARNESS_V1_TEMPLATE_GENERATION") | fromYaml -}}
{{- toJson $template | sha256sum -}}
{{- end }}

{{/* Read the live wrapper inputs used by rollover hooks. */}}
{{- define "orka.harnessV1ExistingImage" -}}
{{- $image := "" -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "wrapper" -}}
{{- $image = default "" .image -}}
{{- end -}}
{{- end -}}
{{- required "existing harness v1 wrapper Deployment is missing the wrapper image" $image -}}
{{- end }}

{{- define "orka.harnessV1ExistingImagePullPolicy" -}}
{{- $pullPolicy := "IfNotPresent" -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "wrapper" -}}
{{- $pullPolicy = default "IfNotPresent" .imagePullPolicy -}}
{{- end -}}
{{- end -}}
{{- $pullPolicy -}}
{{- end }}

{{- define "orka.harnessV1ExistingGeneration" -}}
{{- $generation := "" -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "wrapper" -}}
{{- range (default (list) .env) -}}
{{- if eq (default "" .name) "ORKA_HARNESS_WRAPPER_LEDGER_GENERATION" -}}
{{- $generation = default "" .value -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $generation -}}
{{- end }}

{{- define "orka.harnessV1ExistingAuthSecretName" -}}
{{- $secretName := "" -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) .) -}}
{{- if eq (default "" .name) "auth" -}}
{{- $secretName = dig "secret" "secretName" "" . -}}
{{- end -}}
{{- end -}}
{{- required "existing harness v1 wrapper Deployment is missing the auth Secret name" $secretName -}}
{{- end }}

{{- define "orka.harnessV1ExistingAuthSecretKey" -}}
{{- $secretKey := "" -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) .) -}}
{{- if eq (default "" .name) "auth" -}}
{{- range (dig "secret" "items" (list) .) -}}
{{- if eq (default "" .path) "token" -}}
{{- $secretKey = default "" .key -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- required "existing harness v1 wrapper Deployment is missing the auth Secret token key" $secretKey -}}
{{- end }}

{{- define "orka.harnessV1ExistingTLSSecretName" -}}
{{- $secretName := "" -}}
{{- $legacyAuthSecretName := "" -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) .) -}}
{{- if eq (default "" .name) "tls" -}}
{{- $secretName = dig "secret" "secretName" "" . -}}
{{- else if eq (default "" .name) "auth" -}}
{{- $legacyAuthSecretName = dig "secret" "secretName" "" . -}}
{{- end -}}
{{- end -}}
{{- if not $secretName -}}
{{- $secretName = $legacyAuthSecretName -}}
{{- end -}}
{{- required "existing harness v1 wrapper Deployment is missing the TLS Secret name" $secretName -}}
{{- end }}

{{/* Read the live controller's exact namespace watch scope. */}}
{{- define "orka.existingControllerWatchNamespace" -}}
{{- $watchNamespaces := list -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix "--watch-namespace=" $arg -}}
{{- $watchNamespaces = append $watchNamespaces (trimPrefix "--watch-namespace=" $arg) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq (len $watchNamespaces) 1 -}}
{{- index $watchNamespaces 0 -}}
{{- end -}}
{{- end }}

{{/* Read the exact chart fullname from the live controller's in-cluster URL. */}}
{{- define "orka.existingControllerFullname" -}}
{{- $fullnames := list -}}
{{- $namespaceSuffix := printf ".%s.svc" .namespace -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .controller) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix "--controller-url=http://" $arg -}}
{{- $endpoint := trimPrefix "--controller-url=http://" $arg -}}
{{- $hostPort := first (splitList "/" $endpoint) -}}
{{- $host := first (splitList ":" $hostPort) -}}
{{- if hasSuffix $namespaceSuffix $host -}}
{{- $fullname := trimSuffix $namespaceSuffix $host -}}
{{- if and $fullname (not (contains "." $fullname)) -}}
{{- $fullnames = append $fullnames $fullname -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq (len $fullnames) 1 -}}
{{- index $fullnames 0 -}}
{{- end -}}
{{- end }}

{{/* Read the live controller's exact ACP runtime namespace. */}}
{{- define "orka.existingControllerACPRuntimeNamespace" -}}
{{- $runtimeNamespaces := list -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix "--acp-runtime-namespace=" $arg -}}
{{- $runtimeNamespaces = append $runtimeNamespaces (trimPrefix "--acp-runtime-namespace=" $arg) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq (len $runtimeNamespaces) 1 -}}
{{- index $runtimeNamespaces 0 -}}
{{- end -}}
{{- end }}

{{/* Read the live controller's exact static mode. Legacy controllers return empty. */}}
{{- define "orka.existingControllerMode" -}}
{{- $modes := list -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix "--controller-mode=" $arg -}}
{{- $modes = append $modes (trimPrefix "--controller-mode=" $arg) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq (len $modes) 1 -}}
{{- $mode := index $modes 0 -}}
{{- if has $mode (list "harness-v1" "harness-v2") -}}
{{- $mode -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/* Read the live controller's exact agent-execution snapshot Secret name. */}}
{{- define "orka.existingControllerAgentExecutionSnapshotSecretName" -}}
{{- $volumes := list -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) .) -}}
{{- if eq (default "" .name) "agent-execution-snapshot-key" -}}
{{- $volumes = append $volumes . -}}
{{- end -}}
{{- end -}}
{{- if eq (len $volumes) 1 -}}
{{- dig "secret" "secretName" "" (index $volumes 0) -}}
{{- end -}}
{{- end }}

{{/* Read the live controller's exact snapshot Secret item mounted as key. */}}
{{- define "orka.existingControllerAgentExecutionSnapshotSecretKey" -}}
{{- $volumes := list -}}
{{- range (dig "spec" "template" "spec" "volumes" (list) .) -}}
{{- if eq (default "" .name) "agent-execution-snapshot-key" -}}
{{- $volumes = append $volumes . -}}
{{- end -}}
{{- end -}}
{{- if eq (len $volumes) 1 -}}
{{- $keys := list -}}
{{- range (dig "secret" "items" (list) (index $volumes 0)) -}}
{{- if eq (default "" .path) "key" -}}
{{- $keys = append $keys (default "" .key) -}}
{{- end -}}
{{- end -}}
{{- if eq (len $keys) 1 -}}
{{- index $keys 0 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/* Read the live controller inputs used when the wrapper Deployment is absent. */}}
{{- define "orka.harnessV1ExistingControllerState" -}}
{{- $state := "" -}}
{{- $mode := "" -}}
{{- $harnessMarker := false -}}
{{- $harnessEnabled := false -}}
{{- $harnessDisabled := false -}}
{{- $acpEnabled := false -}}
{{- $acpDisabled := false -}}
{{- $dualMarker := false -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix "--controller-mode=" $arg -}}
{{- $mode = trimPrefix "--controller-mode=" $arg -}}
{{- end -}}
{{- if hasPrefix "--harness-v1-enabled=" $arg -}}
{{- $harnessMarker = true -}}
{{- end -}}
{{- if eq $arg "--harness-v1-enabled=true" -}}
{{- $harnessEnabled = true -}}
{{- else if eq $arg "--harness-v1-enabled=false" -}}
{{- $harnessDisabled = true -}}
{{- else if eq $arg "--acp-runtime-enabled=true" -}}
{{- $acpEnabled = true -}}
{{- else if eq $arg "--acp-runtime-enabled=false" -}}
{{- $acpDisabled = true -}}
{{- else if hasPrefix "--agent-execution-" $arg -}}
{{- $dualMarker = true -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if eq $mode "harness-v1" -}}
{{- $state = "enabled" -}}
{{- else if eq $mode "harness-v2" -}}
{{- $state = "disabled" -}}
{{- else if and $harnessEnabled (not $harnessDisabled) -}}
{{- $state = "enabled" -}}
{{- else if and $harnessDisabled (not $harnessEnabled) -}}
{{- $state = "disabled" -}}
{{- else if and (not $harnessMarker) (not $dualMarker) (ne $acpEnabled $acpDisabled) -}}
{{- $state = "legacy-v2-disabled" -}}
{{- end -}}
{{- $state -}}
{{- end }}

{{- define "orka.harnessV1ExistingControllerAuthSecretName" -}}
{{- $secretName := "" -}}
{{- $prefix := "--harness-v1-auth-secret-name=" -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix $prefix $arg -}}
{{- $secretName = trimPrefix $prefix $arg -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- required "existing harness v1 controller Deployment is missing the auth Secret name" $secretName -}}
{{- end }}

{{- define "orka.harnessV1ExistingControllerAuthSecretKey" -}}
{{- $secretKey := "" -}}
{{- $prefix := "--harness-v1-auth-secret-key=" -}}
{{- range (dig "spec" "template" "spec" "containers" (list) .) -}}
{{- if eq (default "" .name) "controller" -}}
{{- range (default (list) .args) -}}
{{- $arg := toString . -}}
{{- if hasPrefix $prefix $arg -}}
{{- $secretKey = trimPrefix $prefix $arg -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- required "existing harness v1 controller Deployment is missing the auth Secret token key" $secretKey -}}
{{- end }}

{{- define "orka.controllerName" -}}
{{- printf "%s-controller" (include "orka.fullname" . | trunc 52 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.controllerWebhookServiceName" -}}
{{- printf "%s-webhook" (include "orka.fullname" . | trunc 55 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Keep legacy short names, but hash any identity controllerName truncates. */}}
{{- define "orka.controllerWebhookName" -}}
{{- $fullname := include "orka.fullname" . -}}
{{- if le (len $fullname) 52 -}}
{{- include "orka.controllerName" . -}}
{{- else -}}
{{- $identity := printf "%s/%s/%s/%s/%s" .Release.Namespace .Release.Name (default "" .Values.fullnameOverride) (default "" .Values.nameOverride) .Chart.Name -}}
{{- printf "%s-controller-%s" ($fullname | trunc 39 | trimSuffix "-") (sha256sum $identity | trunc 12) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "orka.controllerClusterRoleName" -}}
{{- printf "%s-cluster" (include "orka.controllerWebhookName" .) -}}
{{- end }}

{{- define "orka.controllerUsername" -}}
{{- printf "system:serviceaccount:%s:%s" .Release.Namespace (include "orka.serviceAccountName" .) -}}
{{- end }}

{{- define "orka.publisherName" -}}
{{- printf "%s-workspace-publisher" (include "orka.fullname" . | trunc 43 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.publisherAuthSecretName" -}}
{{- printf "%s-workspace-publisher-auth" (include "orka.fullname" . | trunc 38 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The publisher bearer is transmitted on every request; the operation-capability
secret is a signing key that must never transit. Sharing keys or values would
let a bearer holder mint valid operation capabilities.
*/}}
{{- define "orka.validatePublisherAuth" -}}
{{- if .Values.publisher.enabled -}}
{{- if eq (trim .Values.publisher.auth.controllerTokenKey) (trim .Values.publisher.auth.capabilitySecretKey) -}}
{{- fail "publisher.auth.controllerTokenKey and publisher.auth.capabilitySecretKey must reference distinct Secret keys" -}}
{{- end -}}
{{- if and .Values.publisher.auth.controllerToken (eq .Values.publisher.auth.controllerToken .Values.publisher.auth.capabilitySecret) -}}
{{- fail "publisher.auth.controllerToken and publisher.auth.capabilitySecret must be distinct values" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "orka.acpArtifactSecretName" -}}
{{- printf "%s-acp-artifact-capability" (include "orka.fullname" . | trunc 39 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.providerProxyName" -}}
{{- printf "%s-provider-auth-proxy" (include "orka.fullname" . | trunc 43 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.scmEgressProxyName" -}}
{{- printf "%s-scm-egress-proxy" (include "orka.fullname" . | trunc 46 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.scmEgressProxyAuthSecretName" -}}
{{- printf "%s-scm-egress-proxy-auth" (include "orka.fullname" . | trunc 41 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.storeName" -}}
{{- printf "%s-store" (include "orka.fullname" . | trunc 57 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.vekilIngressPolicyName" -}}
{{- printf "%s-vekil-ingress" (include "orka.fullname" . | trunc 49 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the workspace publisher ServiceAccount to use.
*/}}
{{- define "orka.publisherServiceAccountName" -}}
{{- if .Values.publisher.serviceAccount.create }}
{{- default (include "orka.publisherName" .) .Values.publisher.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.publisher.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Reject mutable ACP runtime image references when a provider image is configured.
An empty provider image leaves that provider unavailable; Tasks still fail closed
because the ACP runtime remains enabled and has no legacy fallback.
*/}}
{{- define "orka.validateACPRuntimeImage" -}}
{{- $name := .name -}}
{{- $ref := default "" .ref -}}
{{- if and $ref (not (regexMatch "^.+@sha256:[0-9a-f]{64}$" $ref)) -}}
{{- fail (printf "%s must be an immutable image reference ending in @sha256:<64 lowercase hex characters>; got %q" $name $ref) -}}
{{- end -}}
{{- end }}

{{/*
The chart-managed provider proxy is release-namespaced and its NetworkPolicies
are intentionally pinned to the chart-supported Vekil Service.
*/}}
{{- define "orka.validateProviderProxyConfig" -}}
{{- if and (eq .Values.controller.mode "harness-v2") .Values.providerProxy.enabled -}}
{{- $configuredNamespace := trim (default "" .Values.controller.acpRuntime.providerProxyNamespace) -}}
{{- if and $configuredNamespace (ne $configuredNamespace .Release.Namespace) -}}
{{- fail (printf "controller.acpRuntime.providerProxyNamespace must be empty or match the Helm release namespace %q when providerProxy.enabled=true" .Release.Namespace) -}}
{{- end -}}
{{- $upstream := trimSuffix "/" (trim (default "" .Values.providerProxy.upstreamBaseURL)) -}}
{{- if ne $upstream "http://vekil.vekil-system.svc:1337" -}}
{{- fail "providerProxy.upstreamBaseURL must be http://vekil.vekil-system.svc:1337 (an optional trailing slash is accepted)" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The controller uses a process-local SQLite store, so production deployments
must have exactly one elected writer and must not overlap Pods during rollout.
*/}}
{{- define "orka.validateSQLiteController" -}}
{{- if ne (int .Values.controller.replicas) 1 -}}
{{- fail "controller.replicas must be exactly 1 when using the SQLite store backend" -}}
{{- end -}}
{{- if not .Values.controller.leaderElect -}}
{{- fail "controller.leaderElect must be true when using the SQLite store backend" -}}
{{- end -}}
{{- end }}

{{/*
Every release owns exactly one immutable execution contract and one tenant
namespace. There is no dual, automatic, or drain controller mode.
*/}}
{{- define "orka.validateControllerMode" -}}
{{- if not (has .Values.controller.mode (list "harness-v1" "harness-v2")) -}}
{{- fail "controller.mode must be harness-v1 or harness-v2" -}}
{{- end -}}
{{- if not (trim (default "" .Values.controller.watchNamespace)) -}}
{{- fail "controller.watchNamespace is required for an isolated controller installation" -}}
{{- end -}}
{{- if ne .Values.controller.watchNamespace .Release.Namespace -}}
{{- fail (printf "controller.watchNamespace must equal the Helm release namespace %q" .Release.Namespace) -}}
{{- end -}}
{{- if not .Values.controller.leaderElect -}}
{{- fail "controller.leaderElect must be true for an isolated controller installation" -}}
{{- end -}}
{{- if .Release.IsUpgrade -}}
{{- $existingNamespace := lookup "v1" "Namespace" "" .Release.Namespace -}}
{{- $existingNamespaceMode := "" -}}
{{- if $existingNamespace -}}
{{- $existingNamespaceMode = dig "metadata" "labels" "orka.ai/controller-mode" "" $existingNamespace -}}
{{- end -}}
{{- if ne $existingNamespaceMode .Values.controller.mode -}}
{{- fail (printf "controller mode identity is missing or incompatible; namespace %q must already claim orka.ai/controller-mode=%s before this release can be upgraded" .Release.Namespace .Values.controller.mode) -}}
{{- end -}}
{{- $root := . -}}
{{- $existingControllerList := lookup "apps/v1" "Deployment" .Release.Namespace "" -}}
{{- $existingControllers := list -}}
{{- range (dig "items" (list) (default (dict) $existingControllerList)) -}}
{{- $labels := dig "metadata" "labels" (dict) . -}}
{{- if and (eq (get $labels "app.kubernetes.io/instance") $root.Release.Name) (eq (get $labels "app.kubernetes.io/component") "controller") (eq (get $labels "app.kubernetes.io/managed-by") $root.Release.Service) -}}
{{- $existingControllers = append $existingControllers . -}}
{{- end -}}
{{- end -}}
{{- if gt (len $existingControllers) 1 -}}
{{- fail (printf "multiple controller Deployments are owned by Helm release %q in namespace %q; restore a single controller before upgrading" .Release.Name .Release.Namespace) -}}
{{- end -}}
{{- $existingController := dict -}}
{{- if eq (len $existingControllers) 1 -}}
{{- $existingController = index $existingControllers 0 -}}
{{- end -}}
{{- if $existingController -}}
{{- $existingWatchNamespace := include "orka.existingControllerWatchNamespace" $existingController | trim -}}
{{- if ne $existingWatchNamespace .Values.controller.watchNamespace -}}
{{- fail (printf "controller.watchNamespace is immutable; the existing controller must already watch namespace %q; install cluster-wide or differently scoped controllers as a new release and namespace" .Values.controller.watchNamespace) -}}
{{- end -}}
{{- $existingMode := include "orka.existingControllerMode" $existingController | trim -}}
{{- $existingState := include "orka.harnessV1ExistingControllerState" $existingController | trim -}}
{{- if $existingMode -}}
{{- if ne $existingMode .Values.controller.mode -}}
{{- fail (printf "controller.mode is immutable; install %s as a new release and namespace" .Values.controller.mode) -}}
{{- end -}}
{{- else if eq .Values.controller.mode "harness-v2" -}}
{{- fail "implicit or legacy harness-v2 installations cannot upgrade in place; settle or retire the existing installation and install harness-v2 as a new release and namespace" -}}
{{- else if ne $existingState "enabled" -}}
{{- fail "controller.mode is immutable; install harness-v1 as a new release and namespace" -}}
{{- end -}}
{{- $existingSnapshotSecret := include "orka.existingControllerAgentExecutionSnapshotSecretName" $existingController | trim -}}
{{- if not $existingSnapshotSecret -}}
{{- fail "cannot determine the existing agent execution snapshot Secret name from the live controller; restore its exact agent-execution-snapshot-key volume before upgrading" -}}
{{- end -}}
{{- $desiredSnapshotSecret := trim (default "" .Values.controller.agentExecutionSnapshot.existingSecret) -}}
{{- if ne $existingSnapshotSecret $desiredSnapshotSecret -}}
{{- fail (printf "controller.agentExecutionSnapshot.existingSecret is immutable for in-place upgrades; preserve %q so retained encrypted execution snapshots remain decryptable" $existingSnapshotSecret) -}}
{{- end -}}
{{- $existingSnapshotKey := include "orka.existingControllerAgentExecutionSnapshotSecretKey" $existingController | trim -}}
{{- if not $existingSnapshotKey -}}
{{- fail "cannot determine the existing agent execution snapshot Secret key from the live controller; restore its exact item mounted at path key before upgrading" -}}
{{- end -}}
{{- $desiredSnapshotKey := trim (default "" .Values.controller.agentExecutionSnapshot.key) -}}
{{- if ne $existingSnapshotKey $desiredSnapshotKey -}}
{{- fail (printf "controller.agentExecutionSnapshot.key is immutable for in-place upgrades; preserve %q so retained encrypted execution snapshots remain decryptable" $existingSnapshotKey) -}}
{{- end -}}
{{- if eq .Values.controller.mode "harness-v2" -}}
{{- $existingFullname := include "orka.existingControllerFullname" (dict "controller" $existingController "namespace" .Release.Namespace) | trim -}}
{{- if not $existingFullname -}}
{{- fail "cannot determine the existing harness-v2 chart fullname from the live controller; restore its exact --controller-url argument before upgrading" -}}
{{- end -}}
{{- $desiredFullname := include "orka.fullname" . -}}
{{- if ne $existingFullname $desiredFullname -}}
{{- fail (printf "the effective chart fullname is immutable for harness-v2 upgrades; the existing controller uses %q, but this upgrade would use %q" $existingFullname $desiredFullname) -}}
{{- end -}}
{{- $existingRuntimeNamespace := include "orka.existingControllerACPRuntimeNamespace" $existingController | trim -}}
{{- if not $existingRuntimeNamespace -}}
{{- fail "cannot determine the existing harness-v2 ACP runtime namespace; restore its exact --acp-runtime-namespace argument before upgrading" -}}
{{- end -}}
{{- if ne $existingRuntimeNamespace .Values.controller.acpRuntime.namespace -}}
{{- fail (printf "controller.acpRuntime.namespace is immutable; the existing controller uses namespace %q" $existingRuntimeNamespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- $clientNamespace := trim (default "" .Values.client.namespace) -}}
{{- if and $clientNamespace (ne $clientNamespace .Values.controller.watchNamespace) -}}
{{- fail "client.namespace must be empty or match controller.watchNamespace" -}}
{{- end -}}
{{- if eq .Values.controller.mode "harness-v2" -}}
{{- if not (trim (default "" .Values.controller.acpRuntime.namespace)) -}}
{{- fail "controller.acpRuntime.namespace is required when controller.mode=harness-v2" -}}
{{- end -}}
{{- if eq .Values.controller.acpRuntime.namespace .Release.Namespace -}}
{{- fail "controller.acpRuntime.namespace must differ from the release namespace" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Agent execution snapshots contain sensitive resolved inputs. When either
agent protocol is enabled, require an operator-managed Secret for their
encryption key rather than generating or storing the key in Helm values.
*/}}
{{- define "orka.validateAgentExecutionSnapshot" -}}
{{- if not (trim (default "" .Values.controller.agentExecutionSnapshot.existingSecret)) -}}
{{- fail "controller.agentExecutionSnapshot.existingSecret is required when agent execution is enabled" -}}
{{- end -}}
{{- if not (trim (default "" .Values.controller.agentExecutionSnapshot.key)) -}}
{{- fail "controller.agentExecutionSnapshot.key is required when agent execution is enabled" -}}
{{- end -}}
{{- end }}

{{/*
The release-local controller serves its own fail-closed webhooks. Its
certificate and CA trust are always operator-managed.
*/}}
{{- define "orka.validateWebhooks" -}}
{{- if not (trim (default "" .Values.webhooks.tls.existingSecret)) -}}
{{- fail "webhooks.tls.existingSecret is required" -}}
{{- end -}}
{{- if not (trim (default "" .Values.webhooks.tls.certKey)) -}}
{{- fail "webhooks.tls.certKey is required" -}}
{{- end -}}
{{- if not (trim (default "" .Values.webhooks.tls.privateKeyKey)) -}}
{{- fail "webhooks.tls.privateKeyKey is required" -}}
{{- end -}}
{{- if and (not (trim (default "" .Values.webhooks.caBundle))) (empty .Values.webhooks.caInjectionAnnotations) -}}
{{- fail "webhooks requires a nonempty caBundle or caInjectionAnnotations" -}}
{{- end -}}
{{- if or (lt (int .Values.webhooks.timeoutSeconds) 1) (gt (int .Values.webhooks.timeoutSeconds) 30) -}}
{{- fail "webhooks.timeoutSeconds must be between 1 and 30" -}}
{{- end -}}
{{- end }}

{{/*
Harness v1 is an explicitly selected compatibility data plane. Its image must
be immutable, its admission ledger durable, and its bearer credential must
remain outside rendered Helm manifests.
*/}}
{{- define "orka.validateHarnessV1" -}}
{{- if eq .Values.controller.mode "harness-v1" -}}
{{- if .Values.controller.agentSandbox.enabled -}}
{{- fail "controller.agentSandbox.enabled is unsupported when controller.mode=harness-v1; Agent Sandbox requires harness-v2" -}}
{{- end -}}
{{- if .Values.controller.substrate.enabled -}}
{{- fail "controller.substrate.enabled is unsupported when controller.mode=harness-v1; Substrate requires harness-v2" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.image.repository)) -}}
{{- fail "harnessV1.image.repository is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" (.Values.harnessV1.image.digest | default "")) -}}
{{- fail "harnessV1.image.digest must be a sha256 digest when controller.mode=harness-v1" -}}
{{- end -}}
{{- if trim (default "" .Values.harnessV1.auth.token) -}}
{{- fail "harnessV1.auth.token is unsupported; create a Kubernetes Secret and set harnessV1.auth.existingSecret" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.auth.existingSecret)) -}}
{{- fail "harnessV1.auth.existingSecret is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.auth.tokenKey)) -}}
{{- fail "harnessV1.auth.tokenKey is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.tls.existingSecret)) -}}
{{- fail "harnessV1.tls.existingSecret is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if eq (trim .Values.harnessV1.auth.existingSecret) (trim .Values.harnessV1.tls.existingSecret) -}}
{{- fail "harnessV1.tls.existingSecret must differ from harnessV1.auth.existingSecret" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.ledger.size)) -}}
{{- fail "harnessV1.ledger.size is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.ledger.retention)) -}}
{{- fail "harnessV1.ledger.retention is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (regexMatch "^([1-9][0-9]*(ns|us|µs|ms|s|m|h))+$" (trim (default "" .Values.harnessV1.ledger.retention))) -}}
{{- fail "harnessV1.ledger.retention must be a positive Go duration when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not .Values.store.persistence.enabled -}}
{{- fail "store.persistence.enabled must be true when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.dispatch.interval)) -}}
{{- fail "harnessV1.dispatch.interval is required when controller.mode=harness-v1" -}}
{{- end -}}
{{- if ne (int .Values.harnessV1.dispatch.workers) 1 -}}
{{- fail "harnessV1.dispatch.workers must be exactly 1 when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (regexMatch "^([1-9][0-9]*(ns|us|µs|ms|s|m|h))+$" (trim (default "" .Values.harnessV1.upgradeDrain.timeout))) -}}
{{- fail "harnessV1.upgradeDrain.timeout must be a positive Go duration when controller.mode=harness-v1" -}}
{{- end -}}
{{- if not (regexMatch "^([1-9][0-9]*(ns|us|µs|ms|s|m|h))+$" (trim (default "" .Values.harnessV1.upgradeDrain.pollInterval))) -}}
{{- fail "harnessV1.upgradeDrain.pollInterval must be a positive Go duration when controller.mode=harness-v1" -}}
{{- end -}}
{{- $sandboxMode := trim (default "" .Values.harnessV1.codexSandboxMode) -}}
{{- if and $sandboxMode (not (has $sandboxMode (list "read-only" "workspace-write" "danger-full-access"))) -}}
{{- fail "harnessV1.codexSandboxMode must be read-only, workspace-write, or danger-full-access" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "orka.providerProxyUpstreamBaseURL" -}}
{{- trimSuffix "/" (trim (default "" .Values.providerProxy.upstreamBaseURL)) -}}
{{- end }}


{{/*
Create the namespace for the chart-managed client ServiceAccount. Static
installations always place the client in the watched namespace.
*/}}
{{- define "orka.clientNamespace" -}}
{{- if .Values.client.namespace }}
{{- .Values.client.namespace }}
{{- else }}
{{- .Values.controller.watchNamespace }}
{{- end }}
{{- end }}

{{/*
Create release-scoped worker ClusterRole names.
*/}}
{{- define "orka.aiWorkerClusterRoleName" -}}
{{- printf "%s-ai-worker-role" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.vendorWorkerClusterRoleName" -}}
{{- printf "%s-vendor-worker-role" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.containerWorkerClusterRoleName" -}}
{{- printf "%s-container-worker-role" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/*
Create release-scoped static worker RoleBinding names.
*/}}
{{- define "orka.aiWorkerRoleBindingName" -}}
{{- printf "%s-ai-worker-rolebinding" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.vendorWorkerRoleBindingName" -}}
{{- printf "%s-vendor-worker-rolebinding" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.containerWorkerRoleBindingName" -}}
{{- printf "%s-container-worker-rolebinding" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{/* Render repository@digest when an immutable digest is configured. */}}
{{- define "orka.imageRef" -}}
{{- if .digest -}}
{{ printf "%s@%s" .repository .digest }}
{{- else -}}
{{ printf "%s:%s" .repository .tag }}
{{- end -}}
{{- end }}
