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

{{- define "orka.harnessV1AuthSecretName" -}}
{{- printf "%s-harness-wrapper-auth" (include "orka.fullname" . | trunc 42 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1LedgerName" -}}
{{- printf "%s-harness-v1-ledger" (include "orka.fullname" . | trunc 45 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.harnessV1PolicyNamespace" -}}
{{- if .Values.harnessV1.policy.namespace -}}
{{- .Values.harnessV1.policy.namespace -}}
{{- else if .Values.controller.watchNamespace -}}
{{- .Values.controller.watchNamespace -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end }}

{{- define "orka.controllerName" -}}
{{- printf "%s-controller" (include "orka.fullname" . | trunc 52 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.admissionName" -}}
{{- printf "%s-admission" (include "orka.fullname" . | trunc 53 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.agentExecutionBindingAdmissionPolicyName" -}}
{{- printf "%s-agent-execution-binding" (include "orka.fullname" . | trunc 39 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.agentExecutionClassificationAdmissionPolicyName" -}}
{{- printf "%s-agent-execution-classification" (include "orka.fullname" . | trunc 34 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.agentExecutionResolutionAdmissionPolicyName" -}}
{{- printf "%s-agent-execution-resolution" (include "orka.fullname" . | trunc 38 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.admissionControllerUsername" -}}
{{- default (printf "system:serviceaccount:%s:%s" .Release.Namespace (include "orka.serviceAccountName" .)) .Values.admission.identity.controllerUsername -}}
{{- end }}

{{- define "orka.admissionAdjudicationControllerUsername" -}}
{{- default (include "orka.admissionControllerUsername" .) .Values.admission.identity.adjudicationControllerUsername -}}
{{- end }}

{{- define "orka.publisherName" -}}
{{- printf "%s-workspace-publisher" (include "orka.fullname" . | trunc 43 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "orka.publisherAuthSecretName" -}}
{{- printf "%s-workspace-publisher-auth" (include "orka.fullname" . | trunc 38 | trimSuffix "-") | trunc 63 | trimSuffix "-" }}
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
Agent execution snapshots contain sensitive resolved inputs. When either
agent protocol is enabled, require an operator-managed Secret for their
encryption key rather than generating or storing the key in Helm values.
*/}}
{{- define "orka.validateAgentExecutionSnapshot" -}}
{{- if or .Values.controller.acpRuntime.enabled .Values.harnessV1.enabled -}}
{{- if not (trim (default "" .Values.controller.agentExecutionSnapshot.existingSecret)) -}}
{{- fail "controller.agentExecutionSnapshot.existingSecret is required when agent execution is enabled" -}}
{{- end -}}
{{- if not (trim (default "" .Values.controller.agentExecutionSnapshot.key)) -}}
{{- fail "controller.agentExecutionSnapshot.key is required when agent execution is enabled" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "orka.validateAgentExecutionControl" -}}
{{- if .Values.controller.agentExecutionControl.create -}}
{{- if not (has .Values.controller.agentExecutionControl.v1Mode (list "enabled" "drain-only" "disabled")) -}}
{{- fail "controller.agentExecutionControl.v1Mode must be enabled, drain-only, or disabled" -}}
{{- end -}}
{{- if not (has .Values.controller.agentExecutionControl.v2Mode (list "enabled" "drain-only" "disabled")) -}}
{{- fail "controller.agentExecutionControl.v2Mode must be enabled, drain-only, or disabled" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The admission plane is stateless and independent from the singleton controller.
Its certificate and CA trust are always operator-managed, and fail-closed
webhooks are a separate activation step after the replicas are ready.
*/}}
{{- define "orka.validateAdmission" -}}
{{- if .Values.admission.enabled -}}
{{- if lt (int .Values.admission.replicas) 2 -}}
{{- fail "admission.replicas must be at least 2 when admission.enabled=true" -}}
{{- end -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" (.Values.controller.image.digest | default "")) -}}
{{- fail "controller.image.digest must be a sha256 digest when admission.enabled=true" -}}
{{- end -}}
{{- if not (trim (default "" .Values.admission.tls.existingSecret)) -}}
{{- fail "admission.tls.existingSecret is required when admission.enabled=true" -}}
{{- end -}}
{{- if not (trim (default "" .Values.admission.tls.certKey)) -}}
{{- fail "admission.tls.certKey is required when admission.enabled=true" -}}
{{- end -}}
{{- if not (trim (default "" .Values.admission.tls.privateKeyKey)) -}}
{{- fail "admission.tls.privateKeyKey is required when admission.enabled=true" -}}
{{- end -}}
{{- if empty .Values.admission.identity.adminGroups -}}
{{- fail "admission.identity.adminGroups must contain at least one group when admission.enabled=true" -}}
{{- end -}}
{{- range .Values.admission.identity.adminGroups -}}
{{- if not (trim .) -}}
{{- fail "admission.identity.adminGroups entries must be nonempty" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if .Values.admission.webhooks.enabled -}}
{{- if not .Values.admission.enabled -}}
{{- fail "admission.enabled must be true when admission.webhooks.enabled=true" -}}
{{- end -}}
{{- if and (not (trim (default "" .Values.admission.webhooks.caBundle))) (empty .Values.admission.webhooks.caInjectionAnnotations) -}}
{{- fail "admission.webhooks requires a nonempty caBundle or caInjectionAnnotations when enabled" -}}
{{- end -}}
{{- if or (lt (int .Values.admission.webhooks.timeoutSeconds) 1) (gt (int .Values.admission.webhooks.timeoutSeconds) 30) -}}
{{- fail "admission.webhooks.timeoutSeconds must be between 1 and 30" -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Harness v1 is an explicitly enabled compatibility data plane. Its image must
be immutable, its admission ledger durable, and its bearer credential must be
unambiguous. Inline tokens remain optional because the chart can generate a
release-local Secret, but operator-supplied tokens must have adequate entropy.
*/}}
{{- define "orka.validateHarnessV1" -}}
{{- if .Values.harnessV1.enabled -}}
{{- if not (trim (default "" .Values.harnessV1.image.repository)) -}}
{{- fail "harnessV1.image.repository is required when harnessV1.enabled=true" -}}
{{- end -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" (.Values.harnessV1.image.digest | default "")) -}}
{{- fail "harnessV1.image.digest must be a sha256 digest when harnessV1.enabled=true" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.auth.tokenKey)) -}}
{{- fail "harnessV1.auth.tokenKey is required when harnessV1.enabled=true" -}}
{{- end -}}
{{- if and .Values.harnessV1.auth.existingSecret .Values.harnessV1.auth.token -}}
{{- fail "harnessV1.auth.existingSecret and harnessV1.auth.token are mutually exclusive" -}}
{{- end -}}
{{- if and .Values.harnessV1.auth.token (lt (len .Values.harnessV1.auth.token) 32) -}}
{{- fail "harnessV1.auth.token must be at least 32 characters when set" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.ledger.size)) -}}
{{- fail "harnessV1.ledger.size is required when harnessV1.enabled=true" -}}
{{- end -}}
{{- if not .Values.store.persistence.enabled -}}
{{- fail "store.persistence.enabled must be true when harnessV1.enabled=true" -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.policy.name)) -}}
{{- fail "harnessV1.policy.name is required when harnessV1.enabled=true" -}}
{{- end -}}
{{- if .Values.harnessV1.policy.create -}}
{{- range .Values.harnessV1.policy.allowedBuiltInRuntimeTypes -}}
{{- if not (has . (list "codex" "claude" "copilot")) -}}
{{- fail "harnessV1.policy.allowedBuiltInRuntimeTypes may contain only codex, claude, or copilot" -}}
{{- end -}}
{{- end -}}
{{- if not (has .Values.harnessV1.policy.retryEligibility (list "none" "duplicate-safe-only")) -}}
{{- fail "harnessV1.policy.retryEligibility must be none or duplicate-safe-only" -}}
{{- end -}}
{{- if not (has .Values.harnessV1.policy.networkIsolationProfile (list "default-deny" "per-trust-domain")) -}}
{{- fail "harnessV1.policy.networkIsolationProfile must be default-deny or per-trust-domain" -}}
{{- end -}}
{{- end -}}
{{- if not (trim (default "" .Values.harnessV1.dispatch.interval)) -}}
{{- fail "harnessV1.dispatch.interval is required when harnessV1.enabled=true" -}}
{{- end -}}
{{- if lt (int .Values.harnessV1.dispatch.workers) 1 -}}
{{- fail "harnessV1.dispatch.workers must be positive when harnessV1.enabled=true" -}}
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
Create the namespace for the chart-managed client ServiceAccount.
When namespace isolation is enforced and the controller watches one namespace,
place the default client in that namespace so its token remains usable.
*/}}
{{- define "orka.clientNamespace" -}}
{{- if .Values.client.namespace }}
{{- .Values.client.namespace }}
{{- else if and .Values.controller.enforceNamespaceIsolation .Values.controller.watchNamespace }}
{{- .Values.controller.watchNamespace }}
{{- else }}
{{- .Release.Namespace }}
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
Create release-scoped static worker ClusterRoleBinding names.
*/}}
{{- define "orka.aiWorkerClusterRoleBindingName" -}}
{{- printf "%s-ai-worker-rolebinding" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.vendorWorkerClusterRoleBindingName" -}}
{{- printf "%s-vendor-worker-rolebinding" (include "orka.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}

{{- define "orka.containerWorkerClusterRoleBindingName" -}}
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
