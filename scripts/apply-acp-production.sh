#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  echo "Usage: $0 OVERLAY_DIR KUSTOMIZE KUBECTL" >&2
}

[[ $# -eq 3 ]] || {
  usage
  exit 2
}

overlay_dir="$1"
kustomize="$2"
kubectl="$3"

for command in "${kustomize}" "${kubectl}" base64 dd jq sleep tr wc; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done
[[ -d "${overlay_dir}" && ! -L "${overlay_dir}" ]] || {
  echo "ACP production overlay must be a real directory: ${overlay_dir}" >&2
  exit 1
}
admission_webhooks_dir="${overlay_dir}/../orka-admission-webhooks"
[[ -d "${admission_webhooks_dir}" && ! -L "${admission_webhooks_dir}" ]] || {
  echo "admission webhook base must be a real sibling directory: ${admission_webhooks_dir}" >&2
  exit 1
}

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/apply-acp-production.XXXXXX")"
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

manifest="${work_dir}/manifest.yaml"
rendered_json="${work_dir}/rendered.json"
namespace_resource="${work_dir}/namespace.json"
runtime_config="${work_dir}/runtime-images-configmap.json"
execution_control="${work_dir}/agent-execution-control.json"
existing_control="${work_dir}/existing-agent-execution-control.json"
workload_manifest="${work_dir}/workload-manifest.json"
snapshot_secret="${work_dir}/agent-execution-snapshot-key.json"
snapshot_key="${work_dir}/snapshot-key"
snapshot_key_data="${work_dir}/snapshot-key-data"
snapshot_key_encoded="${work_dir}/snapshot-key-encoded"
snapshot_key_decoded="${work_dir}/snapshot-key-decoded"
admission_tls_secret="${work_dir}/orka-admission-tls.json"
admission_tls_cert="${work_dir}/orka-admission-tls.crt"
admission_tls_key="${work_dir}/orka-admission-tls.key"
admission_ca_cert="${work_dir}/orka-admission-ca.crt"
admission_endpoints="${work_dir}/orka-admission-endpoints.json"
admission_control="${work_dir}/orka-admission-control.json"
admission_webhooks_source="${work_dir}/orka-admission-webhooks.yaml"
admission_webhooks_rendered="${work_dir}/orka-admission-webhooks-rendered.json"
admission_webhooks_manifest="${work_dir}/orka-admission-webhooks.json"
admission_smoke_request="${work_dir}/orka-admission-smoke-request.json"
admission_smoke_response="${work_dir}/orka-admission-smoke-response.json"
"${kustomize}" build "${overlay_dir}" >"${manifest}"
"${kubectl}" create --dry-run=client --validate=false -f "${manifest}" -o json >"${rendered_json}"
"${kustomize}" build "${admission_webhooks_dir}" >"${admission_webhooks_source}"
"${kubectl}" create --dry-run=client --validate=false -f "${admission_webhooks_source}" -o json >"${admission_webhooks_rendered}"

jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "Namespace" and .metadata.name == "orka-system"))
  | if length == 1 then .[0] else error("expected exactly one orka-system Namespace") end
' "${rendered_json}" >"${namespace_resource}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true"))
  | if length == 1 then .[0] else error("expected exactly one generated ACP runtime image ConfigMap") end
' "${rendered_json}" >"${runtime_config}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.apiVersion == "core.orka.ai/v1alpha1" and .kind == "AgentExecutionControl"))
  | if length == 1 and .[0].metadata.namespace == "orka-system" and .[0].metadata.name == "cluster"
    then .[0]
    else error("expected exactly one orka-system/cluster AgentExecutionControl")
    end
' "${rendered_json}" >"${execution_control}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | {
      apiVersion: "v1",
      kind: "List",
      items: map(select(.apiVersion != "core.orka.ai/v1alpha1" or .kind != "AgentExecutionControl"))
    }
' "${rendered_json}" >"${workload_manifest}"
jq -esc '
  [.[] | if .kind == "List" then .items[] else . end] as $items
  | ($items | map(select(.apiVersion == "apps/v1" and .kind == "Deployment" and
      .metadata.namespace == "orka-system" and .metadata.name == "orka-admission"))) as $deployments
  | ($items | map(select(.apiVersion == "v1" and .kind == "Service" and
      .metadata.namespace == "orka-system" and .metadata.name == "orka-admission"))) as $services
  | ($deployments | length) == 1 and
    ($services | length) == 1 and
    ($deployments[0].spec.replicas >= 2) and
    ($deployments[0].spec.strategy.type == "RollingUpdate") and
    ($deployments[0].spec.strategy.rollingUpdate.maxUnavailable == 0) and
    ($deployments[0].spec.template.spec.containers | any(
      .name == "admission" and
      (.image | test("@sha256:[a-f0-9]{64}$"))
    ))
' "${rendered_json}" >/dev/null || {
  echo "production overlay must contain one digest-pinned, zero-unavailable, replicated orka-admission runtime and Service" >&2
  exit 1
}

validate_snapshot_secret() {
  if ! jq -er '.data["snapshot-key"] | select(type == "string" and length > 0)' "${snapshot_secret}" \
    | base64 -d >"${snapshot_key_data}" 2>/dev/null; then
    echo "agent-execution-snapshot-key Secret must contain snapshot-key" >&2
    return 1
  fi

  local encoded_key key_size
  key_size="$(wc -c <"${snapshot_key_data}" | tr -d '[:space:]')"
  if [[ "${key_size}" == "32" ]]; then
    return 0
  fi

  # Match the controller parser: raw input is accepted only at exactly 32
  # bytes. Otherwise trim surrounding whitespace from base64 text, while
  # retaining Go's allowance for embedded CR/LF line wrapping.
  encoded_key="$(<"${snapshot_key_data}")"
  while [[ -n "${encoded_key}" && "${encoded_key}" == [[:space:]]* ]]; do
    encoded_key="${encoded_key:1}"
  done
  while [[ -n "${encoded_key}" && "${encoded_key}" == *[[:space:]] ]]; do
    encoded_key="${encoded_key:0:${#encoded_key}-1}"
  done
  encoded_key="${encoded_key//$'\r'/}"
  encoded_key="${encoded_key//$'\n'/}"
  if [[ "${encoded_key}" =~ ^[A-Za-z0-9+/]{43}=$ ]]; then
    printf '%s' "${encoded_key}" >"${snapshot_key_encoded}"
  else
    : >"${snapshot_key_encoded}"
  fi
  if [[ "$(wc -c <"${snapshot_key_encoded}" | tr -d '[:space:]')" == "44" ]] \
    && base64 -d <"${snapshot_key_encoded}" >"${snapshot_key_decoded}" 2>/dev/null \
    && [[ "$(wc -c <"${snapshot_key_decoded}" | tr -d '[:space:]')" == "32" ]]; then
    return 0
  fi

  echo "agent-execution-snapshot-key/snapshot-key must contain exactly 32 raw bytes or their base64 encoding" >&2
  return 1
}

ensure_execution_control() {
  if ! "${kubectl}" -n orka-system get agentexecutioncontrol.core.orka.ai cluster \
    --ignore-not-found -o json >"${existing_control}"; then
    echo "unable to inspect orka-system/cluster AgentExecutionControl" >&2
    return 1
  fi
  if [[ ! -s "${existing_control}" ]]; then
    "${kubectl}" create -f "${execution_control}" >/dev/null
  fi
}

ensure_snapshot_secret() {
  if ! "${kubectl}" -n orka-system get secret agent-execution-snapshot-key --ignore-not-found -o json >"${snapshot_secret}"; then
    echo "unable to inspect orka-system/agent-execution-snapshot-key" >&2
    return 1
  fi
  if [[ ! -s "${snapshot_secret}" ]]; then
    umask 077
    dd if=/dev/urandom bs=32 count=1 2>/dev/null | base64 | tr -d '\r\n' >"${snapshot_key}"
    "${kubectl}" -n orka-system create secret generic agent-execution-snapshot-key \
      --from-file="snapshot-key=${snapshot_key}" >/dev/null
    "${kubectl}" -n orka-system get secret agent-execution-snapshot-key -o json >"${snapshot_secret}"
  fi
  validate_snapshot_secret
}

validate_admission_tls_secret() {
  if ! "${kubectl}" -n orka-system get secret orka-admission-tls -o json >"${admission_tls_secret}"; then
    echo "orka-system/orka-admission-tls is required before admission rollout" >&2
    return 1
  fi
  if ! jq -e '
    .type == "kubernetes.io/tls" and
    (.data["tls.crt"] | type == "string" and length > 0) and
    (.data["tls.key"] | type == "string" and length > 0) and
    (.data["ca.crt"] | type == "string" and length > 0)
  ' "${admission_tls_secret}" >/dev/null; then
    echo "orka-system/orka-admission-tls must be a TLS Secret containing tls.crt, tls.key, and ca.crt" >&2
    return 1
  fi
  local key output
  for key in tls.crt tls.key ca.crt; do
    case "${key}" in
      tls.crt) output="${admission_tls_cert}" ;;
      tls.key) output="${admission_tls_key}" ;;
      ca.crt) output="${admission_ca_cert}" ;;
    esac
    if ! jq -er --arg key "${key}" '.data[$key]' "${admission_tls_secret}" \
      | base64 -d >"${output}" 2>/dev/null || [[ ! -s "${output}" ]]; then
      echo "orka-system/orka-admission-tls contains invalid ${key} data" >&2
      return 1
    fi
  done
}

render_admission_webhooks() {
  jq -sc --slurpfile tls "${admission_tls_secret}" '
    ($tls[0].data["ca.crt"]) as $ca
    | [.[] | if .kind == "List" then .items[] else . end]
    | {
        apiVersion: "v1",
        kind: "List",
        items: map(
          if .apiVersion == "admissionregistration.k8s.io/v1" and .kind == "ValidatingWebhookConfiguration"
          then
            .metadata.annotations = ((.metadata.annotations // {}) | del(."cert-manager.io/inject-ca-from-secret"))
            | .webhooks |= map(.clientConfig.caBundle = $ca)
          else . end
        )
      }
  ' "${admission_webhooks_rendered}" >"${admission_webhooks_manifest}"

  jq -e '
    ([.items[] | select(.kind == "ValidatingAdmissionPolicy")] | length) == 3 and
    ([.items[] | select(.kind == "ValidatingAdmissionPolicyBinding")] | length) == 3 and
    ([.items[] | select(.kind == "ValidatingWebhookConfiguration")] | length) == 1 and
    ([.items[] | select(.kind == "ValidatingAdmissionPolicy") | .spec.failurePolicy] | all(. == "Fail")) and
    ([.items[] | select(.kind == "ValidatingAdmissionPolicyBinding") |
      .spec.paramRef.parameterNotFoundAction] | all(. == "Deny")) and
    ([.items[] | select(.kind == "ValidatingWebhookConfiguration") | .webhooks[] |
      (.failurePolicy == "Fail" and (.clientConfig.caBundle | type == "string" and length > 0))] | all)
  ' "${admission_webhooks_manifest}" >/dev/null || {
    echo "admission policy wave must contain three fail-closed parameterized policies and one CA-pinned webhook configuration" >&2
    return 1
  }
}

wait_for_execution_control() {
  local attempt
  for attempt in {1..60}; do
    if "${kubectl}" -n orka-system get agentexecutioncontrol.core.orka.ai cluster -o json \
      >"${admission_control}" 2>/dev/null \
      && jq -e '
        (.metadata.uid | type == "string" and length > 0) and
        (.metadata.generation >= 1) and
        (.status.observedGeneration == .metadata.generation) and
        (.status.backends.v1.modeRevision >= 1) and
        (.status.backends.v2.modeRevision >= 1) and
        ([.status.backends.v1.effectiveMode, .status.backends.v2.effectiveMode] |
          all(. == "enabled" or . == "closing" or . == "drain-only" or . == "disabled"))
      ' "${admission_control}" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  echo "orka-system/cluster AgentExecutionControl did not reach its current observed generation" >&2
  return 1
}

wait_for_admission_endpoints() {
  "${kubectl}" -n orka-system rollout status deployment/orka-admission --timeout=2m >/dev/null
  if ! "${kubectl}" -n orka-system get endpoints orka-admission -o json >"${admission_endpoints}" \
    || ! jq -e '[.subsets[]?.addresses[]?.ip] | unique | length >= 2' "${admission_endpoints}" >/dev/null; then
    echo "orka-admission Service must expose at least two ready endpoints" >&2
    return 1
  fi
}

smoke_admission_handlers() {
  local handler kind resource uid
  local service_proxy="/api/v1/namespaces/orka-system/services/https:orka-admission:443/proxy"
  "${kubectl}" get --raw "${service_proxy}/readyz" >/dev/null
  while IFS='|' read -r handler kind resource; do
    [[ -n "${handler}" ]] || continue
    uid="orka-admission-smoke-${resource}"
    jq -n \
      --arg uid "${uid}" \
      --arg kind "${kind}" \
      --arg resource "${resource}" '
      {
        apiVersion: "admission.k8s.io/v1",
        kind: "AdmissionReview",
        request: {
          uid: $uid,
          kind: {group: "core.orka.ai", version: "v1alpha1", kind: $kind},
          resource: {group: "core.orka.ai", version: "v1alpha1", resource: $resource},
          requestKind: {group: "core.orka.ai", version: "v1alpha1", kind: $kind},
          requestResource: {group: "core.orka.ai", version: "v1alpha1", resource: $resource},
          name: "orka-admission-smoke",
          namespace: "orka-system",
          operation: "CREATE",
          userInfo: {username: "system:admin", groups: ["system:masters"]},
          object: {
            apiVersion: "core.orka.ai/v1alpha1",
            kind: $kind,
            metadata: {name: "orka-admission-smoke", namespace: "orka-system"}
          },
          oldObject: null,
          dryRun: true,
          options: {apiVersion: "meta.k8s.io/v1", kind: "CreateOptions"}
        }
      }
    ' >"${admission_smoke_request}"
    if ! "${kubectl}" create --raw "${service_proxy}${handler}" -f "${admission_smoke_request}" \
      >"${admission_smoke_response}" \
      || ! jq -e --arg uid "${uid}" '
        .apiVersion == "admission.k8s.io/v1" and
        .kind == "AdmissionReview" and
        .response.uid == $uid and
        (.response.allowed | type == "boolean")
      ' "${admission_smoke_response}" >/dev/null; then
      echo "orka-admission handler smoke failed: ${handler}" >&2
      return 1
    fi
  done <<'EOF_ADMISSION_HANDLERS'
/validate-core-orka-ai-v1alpha1-task-provenance|Task|tasks
/validate-core-orka-ai-v1alpha1-task-workspace-class-use|Task|tasks-workspace-class-use
/validate-core-orka-ai-v1alpha1-tool-workspace-class-use|Tool|tools
/validate-core-orka-ai-v1alpha1-agent-contract|Agent|agents
/validate-core-orka-ai-v1alpha1-agentruntime-contract|AgentRuntime|agentruntimes
/validate-core-orka-ai-v1alpha1-task-execution-authority|Task|tasks-execution-authority
/validate-core-orka-ai-v1alpha1-agentexecutionadjudication|AgentExecutionAdjudication|agentexecutionadjudications
/validate-core-orka-ai-v1alpha1-agentexecution-control-policy|AgentExecutionControl|agentexecutioncontrols
/validate-core-orka-ai-v1alpha1-session-resolution|RuntimeSessionControl|runtimesessioncontrols
EOF_ADMISSION_HANDLERS
}

# Establish every workload prerequisite before applying the Deployment that
# references it. Every retry repeats these idempotent phases, so interruption
# after any apply still converges on the desired generation without rotating an
# existing snapshot key or recreating the durable execution-control singleton.
"${kubectl}" apply -f "${namespace_resource}"
ensure_snapshot_secret
validate_admission_tls_secret
"${kubectl}" apply -f "${runtime_config}"
ensure_execution_control
"${kubectl}" apply -f "${workload_manifest}"
wait_for_admission_endpoints
wait_for_execution_control
smoke_admission_handlers
render_admission_webhooks
"${kubectl}" apply -f "${admission_webhooks_manifest}"
