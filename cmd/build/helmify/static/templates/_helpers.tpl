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
Require an explicit tag or SHA256 digest for configured ACP runtime images.
The controller resolves tags before admitting runtime workloads.
An empty provider image leaves that provider unavailable; Tasks still fail closed
because the ACP runtime remains enabled and has no legacy fallback.
*/}}
{{- define "orka.validateACPRuntimeImage" -}}
{{- $name := .name -}}
{{- $ref := default "" .ref -}}
{{- if $ref -}}
{{/* Match the canonical names accepted by the controller's distribution/reference parser. */}}
{{- $domainComponent := `[a-zA-Z0-9](?:[a-zA-Z0-9-]*[a-zA-Z0-9])?` -}}
{{- $domain := printf `(?:%s(?:\.%s)*|\[[a-fA-F0-9:]+\])(?::[0-9]+)?` $domainComponent $domainComponent -}}
{{- $pathComponent := `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*` -}}
{{- $path := printf `%s(?:/%s)*` $pathComponent $pathComponent -}}
{{- $tag := `[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}` -}}
{{- $suffix := printf `(?::%s(?:@sha256:[0-9a-f]{64})?|@sha256:[0-9a-f]{64})` $tag -}}
{{- $error := printf "%s must use a full registry/repository name with an explicit tag or SHA256 digest" $name -}}
{{- if not (regexMatch (printf `^(?:%s/)?%s%s$` $domain $path $suffix) $ref) -}}
{{- fail $error -}}
{{- end -}}
{{- $imageName := regexReplaceAll (printf `%s$` $suffix) $ref "" -}}
{{- $parts := splitList "/" $imageName -}}
{{- if lt (len $parts) 2 -}}
{{- fail $error -}}
{{- end -}}
{{- $registry := first $parts -}}
{{- $repository := join "/" (rest $parts) -}}
{{/* The parser treats a prefix outside its domain grammar as part of the repository path. */}}
{{- if not (regexMatch (printf `^%s$` $domain) $registry) -}}
{{- $repository = $imageName -}}
{{- end -}}
{{- $qualified := or (eq $registry "localhost") (contains "." $registry) (contains ":" $registry) (ne (lower $registry) $registry) -}}
{{- if or (not $qualified) (eq $registry "index.docker.io") (and (eq $registry "docker.io") (not (contains "/" $repository))) (gt (len $repository) 255) -}}
{{- fail $error -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
The chart-managed provider proxy stays in the release namespace. Operators
choose its upstream gateway and explicitly allow the required network access.
*/}}
{{- define "orka.validateProviderProxyConfig" -}}
{{- if .Values.providerProxy.enabled -}}
{{- $configuredNamespace := trim (default "" .Values.controller.acpRuntime.providerProxyNamespace) -}}
{{- if and $configuredNamespace (ne $configuredNamespace .Release.Namespace) -}}
{{- fail (printf "controller.acpRuntime.providerProxyNamespace must be empty or match the Helm release namespace %q when providerProxy.enabled=true" .Release.Namespace) -}}
{{- end -}}
{{- $upstream := trimSuffix "/" (trim (default "" .Values.providerProxy.upstreamBaseURL)) -}}
{{- if not (regexMatch `^https?://(\[[0-9A-Fa-f:.]*:[0-9A-Fa-f:.]*\]|[A-Za-z0-9][^/?#@\[\]:[:space:]]*)(:[0-9]+)?(/[^?#[:space:]]*)?$` $upstream) -}}
{{- fail "providerProxy.upstreamBaseURL must be an HTTP(S) URL without credentials, a query, or a fragment when providerProxy.enabled=true" -}}
{{- end -}}
{{- $upstreamURL := urlParse $upstream -}}
{{- $port := trimPrefix ":" (regexFind `:[0-9]+$` $upstreamURL.host) -}}
{{- if and $port (or (lt (atoi $port) 1) (gt (atoi $port) 65535)) -}}
{{- fail "providerProxy.upstreamBaseURL port must be between 1 and 65535" -}}
{{- end -}}
{{/* Match the proxy's url.Parse followed by HasUnsafePathSegment, including its second path decode. */}}
{{- $path := $upstreamURL.path -}}
{{- range $pass := until 2 -}}
{{- if regexMatch `(^|/)\.{1,2}(/|$)|\\|\x00` $path -}}
{{- fail "providerProxy.upstreamBaseURL path must not contain dot segments, backslashes, or null bytes, including encoded forms" -}}
{{- end -}}
{{- if eq $pass 0 -}}
{{/* Escape delimiters and controls, but retain percent escapes for one more path decode. */}}
{{- $decodeURL := printf "/%s" ($path | urlquery | replace "+" "%20" | replace "%25" "%") -}}
{{- $path = (urlParse $decodeURL).path -}}
{{- end -}}
{{- end -}}
{{- if not (kindIs "slice" .Values.providerProxy.egress) -}}
{{- fail "providerProxy.egress must be a list of NetworkPolicy egress rules" -}}
{{- end -}}
{{- if empty .Values.providerProxy.egress -}}
{{- $service := include "orka.providerProxyUpstreamService" . | fromJson -}}
{{- if not $service.name -}}
{{- fail "providerProxy.egress is required unless providerProxy.upstreamBaseURL names an in-cluster Service as http(s)://<service>.<namespace>.svc[:port]; the chart then derives the egress rule from that Service" -}}
{{- end -}}
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
Default the tenant namespace to the Helm release namespace.
*/}}
{{- define "orka.watchNamespace" -}}
{{- default .Release.Namespace .Values.controller.watchNamespace -}}
{{- end }}

{{/*
Every release owns the harness-v2 execution contract and one tenant namespace.
controller.mode alone selects the protocol. Leftover harnessV1 values are inert:
helm upgrade --reuse-values carries an older chart's defaults, including them.
*/}}
{{- define "orka.validateControllerMode" -}}
{{- $watchNamespace := include "orka.watchNamespace" . -}}
{{- if ne .Values.controller.mode "harness-v2" -}}
{{- fail "controller.mode must be harness-v2; other harness protocols are unsupported" -}}
{{- end -}}
{{- if ne $watchNamespace .Release.Namespace -}}
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
{{- if ne $existingWatchNamespace $watchNamespace -}}
{{- fail (printf "controller.watchNamespace is immutable; the existing controller must already watch namespace %q; install cluster-wide or differently scoped controllers as a new release and namespace" $watchNamespace) -}}
{{- end -}}
{{- $existingMode := include "orka.existingControllerMode" $existingController | trim -}}
{{- if ne $existingMode "harness-v2" -}}
{{- fail "existing controller must explicitly use harness-v2; install unsupported or unclassified controllers as a new release and namespace" -}}
{{- end -}}
{{- $existingSnapshotSecret := include "orka.existingControllerAgentExecutionSnapshotSecretName" $existingController | trim -}}
{{- if not $existingSnapshotSecret -}}
{{- fail "cannot determine the existing agent execution snapshot Secret name from the live controller; restore its exact agent-execution-snapshot-key volume before upgrading" -}}
{{- end -}}
{{- $desiredSnapshotSecret := include "orka.agentExecutionSnapshotSecretName" . -}}
{{- if ne $existingSnapshotSecret $desiredSnapshotSecret -}}
{{- fail (printf "controller.agentExecutionSnapshot.existingSecret is immutable for in-place upgrades; preserve %q so retained encrypted execution snapshots remain decryptable" $existingSnapshotSecret) -}}
{{- end -}}
{{- $existingSnapshotKey := include "orka.existingControllerAgentExecutionSnapshotSecretKey" $existingController | trim -}}
{{- if not $existingSnapshotKey -}}
{{- fail "cannot determine the existing agent execution snapshot Secret key from the live controller; restore its exact item mounted at path key before upgrading" -}}
{{- end -}}
{{- $desiredSnapshotKey := include "orka.agentExecutionSnapshotSecretKey" . -}}
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
{{- if and $clientNamespace (ne $clientNamespace $watchNamespace) -}}
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
Agent execution snapshots contain sensitive resolved inputs and are encrypted
with an AES-256 key mounted from a Secret. Operators may supply that Secret
through controller.agentExecutionSnapshot.existingSecret. When it is empty the
chart renders an empty release-owned Secret and the controller mints the key
into it on first start (see agent-execution-snapshot-secret.yaml). Both paths resolve
through these helpers so the Deployment mount and the upgrade identity guard
agree on one name and one item key.
*/}}
{{- define "orka.agentExecutionSnapshotSecretName" -}}
{{- $existing := trim (default "" .Values.controller.agentExecutionSnapshot.existingSecret) -}}
{{- if $existing -}}
{{- $existing -}}
{{- else -}}
{{- printf "%s-agent-execution-snapshot" (include "orka.fullname" . | trunc 38 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "orka.agentExecutionSnapshotGenerated" -}}
{{- if not (trim (default "" .Values.controller.agentExecutionSnapshot.existingSecret)) -}}true{{- end -}}
{{- end }}

{{- define "orka.agentExecutionSnapshotSecretKey" -}}
{{- default "key" (trim (default "" .Values.controller.agentExecutionSnapshot.key)) -}}
{{- end }}

{{/*
The release-local controller serves its own fail-closed webhooks. Its
certificate and CA trust are always operator-managed.
*/}}
{{- define "orka.validateWebhooks" -}}
{{- if not (trim (default "" .Values.webhooks.tls.certKey)) -}}
{{- fail "webhooks.tls.certKey is required" -}}
{{- end -}}
{{- if not (trim (default "" .Values.webhooks.tls.privateKeyKey)) -}}
{{- fail "webhooks.tls.privateKeyKey is required" -}}
{{- end -}}
{{- if and (not (include "orka.webhookTLSGenerated" .)) (not (trim (default "" .Values.webhooks.caBundle))) (empty .Values.webhooks.caInjectionAnnotations) -}}
{{- fail "webhooks requires a nonempty caBundle or caInjectionAnnotations when webhooks.tls.existingSecret is set" -}}
{{- end -}}
{{- if and (include "orka.webhookTLSGenerated" .) (or (trim (default "" .Values.webhooks.caBundle)) (not (empty .Values.webhooks.caInjectionAnnotations))) -}}
{{- fail "webhooks.caBundle and webhooks.caInjectionAnnotations require webhooks.tls.existingSecret; with controller-managed certificates the controller owns caBundle and an external injector would fight it" -}}
{{- end -}}
{{- if or (lt (int .Values.webhooks.timeoutSeconds) 1) (gt (int .Values.webhooks.timeoutSeconds) 30) -}}
{{- fail "webhooks.timeoutSeconds must be between 1 and 30" -}}
{{- end -}}
{{- end }}

{{/*
Webhook serving TLS. With webhooks.tls.existingSecret the operator owns the
certificate and its CA trust. Without it the chart renders an empty
release-owned Secret and the controller fills it with a self-signed CA and
serving certificate, then injects that CA into the release's
ValidatingWebhookConfiguration (see controller-webhook-tls-secret.yaml).
*/}}
{{- define "orka.webhookTLSGenerated" -}}
{{- if not (trim (default "" .Values.webhooks.tls.existingSecret)) -}}true{{- end -}}
{{- end }}

{{- define "orka.webhookTLSSecretName" -}}
{{- $existing := trim (default "" .Values.webhooks.tls.existingSecret) -}}
{{- if $existing -}}
{{- $existing -}}
{{- else -}}
{{- printf "%s-webhook-tls" (include "orka.fullname" . | trunc 51 | trimSuffix "-") | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end }}

{{- define "orka.providerProxyUpstreamBaseURL" -}}
{{- trimSuffix "/" (trim (default "" .Values.providerProxy.upstreamBaseURL)) -}}
{{- end }}

{{/*
Split providerProxy.upstreamBaseURL into the in-cluster Service it names, as
JSON {name, namespace, port}. Empty name when the host is not of the form
<service>.<namespace>.svc or <service>.<namespace>.svc.cluster.local.
*/}}
{{- define "orka.providerProxyUpstreamService" -}}
{{- $url := urlParse (include "orka.providerProxyUpstreamBaseURL" .) -}}
{{- $hostport := default "" $url.host -}}
{{- $host := regexReplaceAll `:[0-9]+$` $hostport "" -}}
{{- $port := trimPrefix ":" (regexFind `:[0-9]+$` $hostport) -}}
{{- if not $port -}}{{- $port = ternary "443" "80" (eq $url.scheme "https") -}}{{- end -}}
{{- $parts := splitList "." $host -}}
{{- $name := "" -}}
{{- $namespace := "" -}}
{{- if and (ge (len $parts) 3) (eq (index $parts 2) "svc") (or (eq (len $parts) 3) (and (eq (len $parts) 5) (eq (index $parts 3) "cluster") (eq (index $parts 4) "local"))) -}}
{{- $name = index $parts 0 -}}
{{- $namespace = index $parts 1 -}}
{{- end -}}
{{- dict "name" $name "namespace" $namespace "port" $port | toJson -}}
{{- end }}

{{/*
Create the namespace for the chart-managed client ServiceAccount. Static
installations always place the client in the watched namespace.
*/}}
{{- define "orka.clientNamespace" -}}
{{- if .Values.client.namespace }}
{{- .Values.client.namespace }}
{{- else }}
{{- include "orka.watchNamespace" . }}
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

{{/* Require an explicit image tag or SHA256 digest. Release preparation supplies tags. */}}
{{- define "orka.imageRef" -}}
{{- $repository := required "image.repository is required" .repository -}}
{{- if .digest -}}
{{- if not (regexMatch "^sha256:[0-9a-f]{64}$" .digest) -}}
{{- fail "image.digest must be a sha256 digest" -}}
{{- end -}}
{{ printf "%s@%s" $repository .digest }}
{{- else -}}
{{- $tag := required "image.tag is required when image.digest is unset" .tag | toString -}}
{{- if not (regexMatch "^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$" $tag) -}}
{{- fail "image.tag must be a valid container image tag" -}}
{{- end -}}
{{ printf "%s:%s" $repository $tag }}
{{- end -}}
{{- end }}

{{/*
Agent hosts are opt-in. Their Pods, volumes and Secrets
live in a namespace the controller never shares with its watch or ACP runtime
namespaces, and the orka-host-agent sidecar image must be immutable.
*/}}
{{- define "orka.agentHostsEnabled" -}}
{{- if dig "agentHosts" "enabled" false .Values.controller -}}true{{- end -}}
{{- end }}

{{- define "orka.agentHostNamespace" -}}
{{- trim (toString (dig "agentHosts" "namespace" "" .Values.controller)) -}}
{{- end }}

{{/* Match the digest pin required by agenthost.ValidateTemplate. */}}
{{- define "orka.validateAgentHostImage" -}}
{{- if not (regexMatch `^[^@\s]+@sha256:[a-f0-9]{64}$` (toString .ref)) -}}
{{- fail (printf "%s must be pinned by digest: <repository>@sha256:<digest>" .name) -}}
{{- end -}}
{{- end }}

{{- define "orka.agentHostAgentImage" -}}
{{- $image := trim (toString (dig "agentHosts" "agentImage" "" .Values.controller)) -}}
{{- if not $image -}}
{{- if not .Values.controller.image.digest -}}
{{- fail "controller.agentHosts.agentImage must name a digest-pinned image, or controller.image.digest must pin the controller image it defaults to" -}}
{{- end -}}
{{- $image = include "orka.imageRef" .Values.controller.image -}}
{{- end -}}
{{- $_ := include "orka.validateAgentHostImage" (dict "name" "controller.agentHosts.agentImage" "ref" $image) -}}
{{- $image -}}
{{- end }}

{{- define "orka.validateAgentHosts" -}}
{{- if include "orka.agentHostsEnabled" . -}}
{{- $namespace := include "orka.agentHostNamespace" . -}}
{{- if not $namespace -}}
{{- fail "controller.agentHosts.namespace is required when controller.agentHosts.enabled=true" -}}
{{- end -}}
{{- if or (gt (len $namespace) 63) (not (regexMatch `^[a-z0-9]([-a-z0-9]*[a-z0-9])?$` $namespace)) -}}
{{- fail (printf "controller.agentHosts.namespace %q must be a DNS-1123 label, as a namespace name is" $namespace) -}}
{{- end -}}
{{- if or (eq $namespace .Release.Namespace) (eq $namespace .Values.controller.acpRuntime.namespace) -}}
{{- fail (printf "controller.agentHosts.namespace %q must differ from the Helm release namespace and controller.acpRuntime.namespace" $namespace) -}}
{{- end -}}
{{- $_ := include "orka.agentHostAgentImage" . -}}
{{- end -}}
{{- if include "orka.agentHostGitHubEnabled" . -}}
{{- $github := include "orka.agentHostGitHub" . | fromYaml -}}
{{- if not (include "orka.agentHostsEnabled" .) -}}
{{- fail "controller.agentHosts.github.enabled requires controller.agentHosts.enabled=true" -}}
{{- end -}}
{{- if not (dig "connectors" "enabled" false .Values.controller) -}}
{{- fail "controller.agentHosts.github.enabled requires controller.connectors.enabled=true: hosts are fed by people's GitHub links" -}}
{{- end -}}
{{- if not (trim (toString $github.clientID)) -}}
{{- fail "controller.agentHosts.github.clientID is required: the GitHub App's client ID" -}}
{{- end -}}
{{- if eq (empty $github.clientSecret.existingSecret) (empty $github.clientSecret.value) -}}
{{- fail "set exactly one of controller.agentHosts.github.clientSecret.existingSecret and .value" -}}
{{- end -}}
{{- $secret := toString $github.clientSecret.existingSecret -}}
{{- if and $secret (not (regexMatch `^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$` $secret)) -}}
{{- fail "controller.agentHosts.github.clientSecret.existingSecret must be a Secret name" -}}
{{- end -}}
{{- if not (regexMatch `^[-._a-zA-Z0-9]{1,253}$` (toString $github.clientSecret.key)) -}}
{{- fail "controller.agentHosts.github.clientSecret.key must be a Secret data key" -}}
{{- end -}}
{{- if not (regexMatch `^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$` (toString $github.providerName)) -}}
{{- fail "controller.agentHosts.github.providerName must be a lowercase name of at most 63 characters" -}}
{{- end -}}
{{- end -}}
{{- /* Without agent hosts the controller stops reconciling AgentHosts: their
Pods keep running and their deletion never finishes. The CRD is checked
first, so an upgrade from a release without it does not fail on the kind. */ -}}
{{- if and .Release.IsUpgrade (not (include "orka.agentHostsEnabled" .)) -}}
{{- if lookup "apiextensions.k8s.io/v1" "CustomResourceDefinition" "" "agenthosts.core.orka.ai" -}}
{{- $hosts := dig "items" list (lookup "core.orka.ai/v1alpha1" "AgentHost" .Release.Namespace "") -}}
{{- if $hosts -}}
{{- fail (printf "controller.agentHosts.enabled=false would leave %d AgentHost(s) in %s that nothing stops or cleans up; delete them, wait until they are gone, then upgrade" (len $hosts) .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end }}

{{- define "orka.agentHostGitHubEnabled" -}}
{{- if dig "agentHosts" "github" "enabled" false .Values.controller -}}true{{- end -}}
{{- end }}

{{- define "orka.agentHostGitHubSecretName" -}}
{{- $github := include "orka.agentHostGitHub" . | fromYaml -}}
{{- default (printf "%s-github-app" (include "orka.fullname" .)) $github.clientSecret.existingSecret -}}
{{- end }}

{{/*
The agent-host GitHub settings and template with the chart's defaults under
them: an upgrade that reuses an older release's values has none of these
maps, and must render as a fresh install would. The defaults match values.yaml.
*/}}
{{- define "orka.agentHostGitHub" -}}
{{- $defaults := dict "providerName" "github" "displayName" "GitHub" "clientID" "" "clientSecret" (dict "existingSecret" "" "key" "clientSecret" "value" "") -}}
{{- $set := dig "agentHosts" "github" dict .Values.controller -}}
{{- toYaml (mergeOverwrite $defaults (deepCopy $set)) -}}
{{- end }}

{{- define "orka.agentHostTemplate" -}}
{{- $resources := dict "requests" (dict "cpu" "250m" "memory" "1Gi") "limits" (dict "cpu" "4" "memory" "8Gi") -}}
{{- $defaults := dict "name" "copilotd" "storage" "20Gi" "resources" $resources
  "copilotdImage" "ghcr.io/github/copilotd:0.9.6@sha256:0abd0d0bc20ab7c1a6c8d73d4dce7ab8c80d48f78e80d746b485c0415451c72f"
  "hostImage" "mcr.microsoft.com/devcontainers/go:1@sha256:8f1c83128e4bce9d745b1dc0f28f8c027e303d3886e2434966ff135296c41770" -}}
{{- $set := dig "agentHosts" "template" dict .Values.controller -}}
{{- $template := mergeOverwrite $defaults (deepCopy $set) -}}
{{- range $name := list "copilotdImage" "hostImage" -}}
{{- $_ := include "orka.validateAgentHostImage" (dict "name" (printf "controller.agentHosts.template.%s" $name) "ref" (get $template $name)) -}}
{{- end -}}
{{- toYaml $template -}}
{{- end }}
