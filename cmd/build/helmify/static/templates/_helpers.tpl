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

{{/* Read the live controller's exact protocol identity. */}}
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
{{- $mode -}}
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
{{- if .Values.providerProxy.enabled -}}
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
Every release owns the harness-v2 execution contract and one tenant namespace.
*/}}
{{- define "orka.validateControllerMode" -}}
{{- if ne .Values.controller.mode "harness-v2" -}}
{{- fail "controller.mode must be harness-v2; other harness protocols are unsupported" -}}
{{- end -}}
{{- if hasKey .Values "harnessV1" -}}
{{- fail "harnessV1 settings are unsupported; Orka only supports harness-v2" -}}
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
{{- if ne $existingMode "harness-v2" -}}
{{- fail "existing controller must explicitly use harness-v2; install unsupported or unclassified controllers as a new release and namespace" -}}
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
{{- $clientNamespace := trim (default "" .Values.client.namespace) -}}
{{- if and $clientNamespace (ne $clientNamespace .Values.controller.watchNamespace) -}}
{{- fail "client.namespace must be empty or match controller.watchNamespace" -}}
{{- end -}}

{{- if not (trim (default "" .Values.controller.acpRuntime.namespace)) -}}
{{- fail "controller.acpRuntime.namespace is required when controller.mode=harness-v2" -}}
{{- end -}}
{{- if eq .Values.controller.acpRuntime.namespace .Release.Namespace -}}
{{- fail "controller.acpRuntime.namespace must differ from the release namespace" -}}
{{- end -}}

{{- end }}

{{/*
Agent execution snapshots contain sensitive resolved inputs. Require an
operator-managed Secret for their encryption key.
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
