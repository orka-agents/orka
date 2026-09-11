#!/usr/bin/env bash
# Validates the harness v2 CRDs against a live cluster: runtime registration,
# unsupported protocol rejection, and immutable Task execution bindings. The CRDs
# from config/crd/bases must already be applied and Established by their one
# designated platform owner.
#
# Usage: KUBECTL="kubectl" scripts/static-mode-crd-validate.sh
set -euo pipefail

KUBECTL=${KUBECTL:-kubectl}
NAMESPACE=${NAMESPACE:-static-mode-crd-validate}
ZERO_DIGEST="sha256:0000000000000000000000000000000000000000000000000000000000000000"
FAILURES=0

k() {
  # shellcheck disable=SC2086
  ${KUBECTL} "$@"
}

pass() { printf 'ok      %s\n' "$1"; }
fail() {
  printf 'FAILED  %s\n' "$1" >&2
  FAILURES=$((FAILURES + 1))
}

# expect_accept <description> — applies stdin, expecting success.
expect_accept() {
  local description="$1"
  if k apply -f - >/dev/null 2>&1; then
    pass "${description}"
  else
    fail "${description} (expected acceptance)"
  fi
}

# expect_reject <description> <message-fragment> — applies stdin, expecting a
# rejection whose error mentions the fragment.
expect_reject() {
  local description="$1" fragment="$2" output
  if output=$(k apply -f - 2>&1); then
    fail "${description} (expected rejection, got acceptance)"
    return
  fi
  if [[ "${output}" != *"${fragment}"* ]]; then
    fail "${description} (rejected for the wrong reason: ${output})"
    return
  fi
  pass "${description}"
}

# expect_status_patch <accept|reject> <description> <kind/name> <merge-patch> [fragment]
expect_status_patch() {
  local mode="$1" description="$2" target="$3" patch="$4" fragment="${5:-}" output
  if output=$(k -n "${NAMESPACE}" patch "${target}" --subresource=status --type=merge -p "${patch}" 2>&1); then
    if [[ "${mode}" == accept ]]; then pass "${description}"; else fail "${description} (expected rejection, got acceptance)"; fi
    return
  fi
  if [[ "${mode}" == reject ]]; then
    if [[ -n "${fragment}" && "${output}" != *"${fragment}"* ]]; then
      fail "${description} (rejected for the wrong reason: ${output})"
    else
      pass "${description}"
    fi
  else
    fail "${description} (expected acceptance: ${output})"
  fi
}

for removed_crd in \
  agentexecutioncontrols.core.orka.ai \
  agentexecutionpolicies.core.orka.ai \
  agentexecutionadjudications.core.orka.ai; do
  if k get crd "${removed_crd}" >/dev/null 2>&1; then
    fail "retired CRD is absent: ${removed_crd}"
  else
    pass "retired CRD is absent: ${removed_crd}"
  fi
done

k create namespace "${NAMESPACE}" --dry-run=client -o yaml | k apply -f - >/dev/null
cleanup() { k delete namespace "${NAMESPACE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== AgentRuntime discriminated union =="

expect_accept "v2 AgentRuntime shape is accepted" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata: {name: v2-runtime, namespace: ${NAMESPACE}}
spec:
  contractVersion: orka.harness.v2
  deployment: {mode: external-endpoint, endpoint: "https://runtime.example.com"}
  clientAuth:
    controllerBearerTokenSecretRef: {name: auth, key: controller}
    operationCapabilitySecretRef: {name: auth, key: capability}
  capabilities:
    runtimeInstanceID: instance-1
    profile:
      digest: ${ZERO_DIGEST}
      digestSchemaVersion: 1
      acpProfile: acp.v1
      adapterName: adapter
      adapterDigest: ${ZERO_DIGEST}
      providerKind: codex
      model: gpt-5.2-codex
      agentConfigurationDigest: ${ZERO_DIGEST}
      toolPolicyDigest: ${ZERO_DIGEST}
      approvalPolicyDigest: ${ZERO_DIGEST}
      mcpConfigurationDigest: ${ZERO_DIGEST}
      workspaceIntent: read
      proxyCredentialRole: provider-inference
      proxyCredentialScope: "model:gpt-5.2-codex"
      resourceClass: standard
    mcpPolicy:
      allowedTools: []
      disallowedTools: []
      allowBash: false
      approvalRequiredTools: []
    limits:
      maxResidentSessions: 10
      maxConcurrentPrompts: 4
      maxRequestBytes: 1048576
      maxEventLineBytes: 1048576
      maxTerminalResultBytes: 1048576
      maxBufferedEvents: 256
      maxUpdateEventsPerSecond: 100
      minPromptLeaseMillis: 5000
      maxPromptLeaseMillis: 120000
      maxPendingPermissions: 32
      maxWorkspaceDeltaBytes: 536870912
    supportsDrain: true
    workspaceGovernance:
      mode: strict-governed
      trusted: false
      orkaOwnedWorkspaceDeltas: true
      promptScopedBrokerAuthorization: true
      noDirectSCMPublication: true
      orkaOwnedCleanRoomPublication: true
      exactInstanceFencing: true
      duplicateSafeMutations: true
      cancellationSettlement: true
EOF

RUNTIME_JSON="$(k -n "${NAMESPACE}" get agentruntime v2-runtime -o json)"
expect_reject "harness v1 registration is rejected" "Unsupported value" <<<"$(
  jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .status)
      | .metadata.name = "v1-runtime" | .spec.contractVersion = "orka.harness.v1"' <<<"${RUNTIME_JSON}"
)"
expect_reject "runtime contract is required" "contractVersion" <<<"$(
  jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .status)
      | .metadata.name = "unclassified-runtime" | del(.spec.contractVersion)' <<<"${RUNTIME_JSON}"
)"
expect_reject "runtime requires pinned capabilities" "pinned instance" <<<"$(
  jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.managedFields, .status)
      | .metadata.name = "v2-no-caps" | .spec.capabilities = {}' <<<"${RUNTIME_JSON}"
)"

echo "== Agent contract selector =="

expect_accept "v2-classified built-in Agent is accepted" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: v2-agent, namespace: ${NAMESPACE}}
spec:
  runtime: {type: codex, contractVersion: orka.harness.v2}
  model: {name: gpt-5.2-codex}
EOF

expect_reject "Agent rejects a harness v1 selector" "Unsupported value" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: v2-agent, namespace: ${NAMESPACE}}
spec:
  runtime: {type: codex, contractVersion: orka.harness.v1}
  model: {name: gpt-5.2-codex}
EOF

expect_reject "selector with runtimeRef is rejected" "runtimeRef derives the protocol" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: ref-agent, namespace: ${NAMESPACE}}
spec:
  runtime:
    runtimeRef: {name: external}
    contractVersion: orka.harness.v2
EOF

expect_reject "v2 OpenCode Agent with systemPrompt is rejected" "does not support spec.systemPrompt" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: v2-opencode, namespace: ${NAMESPACE}}
spec:
  runtime: {type: opencode, contractVersion: orka.harness.v2}
  model: {name: engine/gpt-5.2, contextWindow: 128000, maxTokens: 8192}
  systemPrompt: {inline: "not allowed"}
EOF

echo "== Task binding invariants =="

expect_accept "plain agent Task is accepted" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata: {name: bound-task, namespace: ${NAMESPACE}}
spec:
  type: agent
  prompt: fix it
  agentRef: {name: v2-agent}
EOF

BINDING=$(cat <<JSON
{"schemaVersion":1,"contractVersion":"orka.harness.v2","backend":"runtime-pool","bindingDigest":"${ZERO_DIGEST}","task":{"namespaceUID":"ns-uid","uid":"task-uid","boundSpecGeneration":1},"snapshot":{"id":"task-uid/${ZERO_DIGEST}","digest":"${ZERO_DIGEST}","schemaVersion":1},"boundAt":"2026-08-05T00:00:00Z"}
JSON
)

expect_status_patch accept "v2 execution binding writes once" \
  tasks.core.orka.ai/bound-task "{\"status\":{\"agentExecutionBinding\":${BINDING}}}"

MUTATED_BINDING=${BINDING/runtime-pool/external-endpoint}
expect_status_patch reject "binding mutation is rejected" \
  tasks.core.orka.ai/bound-task "{\"status\":{\"agentExecutionBinding\":${MUTATED_BINDING}}}" "write-once and immutable"

expect_status_patch reject "binding removal is rejected" \
  tasks.core.orka.ai/bound-task '{"status":{"agentExecutionBinding":null}}' "write-once and immutable"

expect_status_patch accept "a v2-bound Task records execution state" \
  tasks.core.orka.ai/bound-task '{"status":{"execution":{"state":"Queued"}}}'

expect_reject "bound Task spec is immutable" "Task spec is immutable" <<<"$(
  k -n "${NAMESPACE}" get task bound-task -o json | jq '.spec.prompt = "changed"'
)"

expect_accept "second plain agent Task is accepted" <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata: {name: incoherent-binding-task, namespace: ${NAMESPACE}}
spec:
  type: agent
  prompt: fix it
  agentRef: {name: v2-agent}
EOF

INCOHERENT_BINDING=$(cat <<JSON
{"schemaVersion":1,"contractVersion":"orka.harness.v1","backend":"runtime-pool","bindingDigest":"${ZERO_DIGEST}","task":{"namespaceUID":"a","uid":"b","boundSpecGeneration":1},"snapshot":{"id":"b/${ZERO_DIGEST}","digest":"${ZERO_DIGEST}","schemaVersion":1},"boundAt":"2026-08-05T00:00:00Z"}
JSON
)
expect_status_patch reject "binding rejects a harness v1 contract" \
  tasks.core.orka.ai/incoherent-binding-task "{\"status\":{\"agentExecutionBinding\":${INCOHERENT_BINDING}}}" "Unsupported value"

REMOVED_BACKEND_BINDING=${BINDING/runtime-pool/harness-wrapper}
expect_status_patch reject "binding rejects the removed wrapper backend" \
  tasks.core.orka.ai/incoherent-binding-task "{\"status\":{\"agentExecutionBinding\":${REMOVED_BACKEND_BINDING}}}" "Unsupported value"

echo
if [[ ${FAILURES} -gt 0 ]]; then
  echo "static-mode CRD validation FAILED: ${FAILURES} case(s)" >&2
  exit 1
fi
echo "static-mode CRD validation passed"
