#!/usr/bin/env bash
set -Eeuo pipefail

readonly backup_marker_format="orka-backup-verification-v1"
readonly empty_list_json='{"apiVersion":"v1","kind":"List","items":[]}'

usage() {
  cat <<'USAGE'
Usage:
  scripts/upgrade-orka-crds.sh \
    --context CONTEXT \
    --sqlite-backup /absolute/path/to/sqlite-or-pvc-backup \
    --sqlite-backup-marker /absolute/path/to/sqlite-or-pvc-backup.verified \
    --cr-backup /absolute/path/to/orka-crs.json \
    --cr-backup-marker /absolute/path/to/orka-crs.json.verified \
    [--gateway-workload GATEWAY_NAMESPACE/GATEWAY_NAME=KIND/WORKLOAD_NAMESPACE/WORKLOAD_NAME]... \
    [--delete-legacy-wrapper]

The script performs a fail-closed ACP v2 CRD hard cutover. It will not apply
CRDs while any of the following remain:
  * an orka.harness.v1 AgentRuntime;
  * an Agent that references a live or backed-up v1 AgentRuntime;
  * any live Agent that uses built-in OpenCode and must be recreated after the
    new Agent CRD is applied;
  * a GatewayBinding that references an affected Agent;
  * a Task that still uses spec.workspace.gitSecretRef or
    spec.agentRuntime.workspace.gitSecretRef;
  * a legacy harness-wrapper Deployment, Service, or Secret.

Every Gateway reached by an affected backed-up or live GatewayBinding must have
at least one explicit --gateway-workload mapping. KIND must be deployment or
statefulset. Each mapped workload must exist in the Gateway namespace, have
spec.replicas=0, report zero replicas, and have observed its current generation.

Both backup markers must contain exactly one of each required field:
  format=orka-backup-verification-v1
  kind=sqlite-pvc        # use kind=orka-crs for the CR backup marker
  context=CONTEXT
  cluster_uid=<kube-system namespace UID>
  api_server_identity_sha256=<sha256 of canonical API server/TLS identity>
  sha256=<lowercase sha256 of the backup file>
  verified=true
  verified_at=<UTC RFC3339 timestamp, for example 2026-07-25T08:00:00Z>

Writing a marker is an operator attestation that the referenced backup was
independently inspected or restore-tested. The CR backup must be a Kubernetes
JSON List containing the pre-cutover AgentRuntime, Agent, and GatewayBinding
inventory. Store backup artifacts and markers outside the repository.

--delete-legacy-wrapper is explicit opt-in. Without it, the script prints exact
kubectl delete commands but never deletes legacy resources. Even with the flag,
other v1 objects must be migrated or deleted by the operator before CRDs apply.
USAGE
}

die() {
  echo "error: $*" >&2
  exit 1
}

require_value() {
  local option="$1"
  local value="${2:-}"
  [[ -n "${value}" ]] || die "${option} requires a value"
  [[ "${value}" != --* ]] || die "${option} requires a value"
}

context=""
cluster_uid=""
api_server_identity_sha256=""
sqlite_backup=""
sqlite_backup_marker=""
cr_backup=""
cr_backup_marker=""
delete_legacy_wrapper=false
gateway_workload_args=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context)
      require_value "$1" "${2:-}"
      context="$2"
      shift 2
      ;;
    --sqlite-backup)
      require_value "$1" "${2:-}"
      sqlite_backup="$2"
      shift 2
      ;;
    --sqlite-backup-marker)
      require_value "$1" "${2:-}"
      sqlite_backup_marker="$2"
      shift 2
      ;;
    --cr-backup)
      require_value "$1" "${2:-}"
      cr_backup="$2"
      shift 2
      ;;
    --cr-backup-marker)
      require_value "$1" "${2:-}"
      cr_backup_marker="$2"
      shift 2
      ;;
    --gateway-workload)
      require_value "$1" "${2:-}"
      gateway_workload_args+=("$2")
      shift 2
      ;;
    --delete-legacy-wrapper)
      delete_legacy_wrapper=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "${context}" ]] || die "--context is required"
[[ -n "${sqlite_backup}" ]] || die "--sqlite-backup is required"
[[ -n "${sqlite_backup_marker}" ]] || die "--sqlite-backup-marker is required"
[[ -n "${cr_backup}" ]] || die "--cr-backup is required"
[[ -n "${cr_backup_marker}" ]] || die "--cr-backup-marker is required"

command -v kubectl >/dev/null 2>&1 || die "kubectl is required"
command -v jq >/dev/null 2>&1 || die "jq is required"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
crd_dir="${root}/charts/orka/crds"
[[ -d "${crd_dir}" ]] || die "CRD directory does not exist: ${crd_dir}"
[[ -n "$(find "${crd_dir}" -type f -name '*.yaml' -print -quit)" ]] || die "CRD directory contains no YAML files: ${crd_dir}"

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-crd-upgrade.XXXXXX")"
cleanup() {
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

kube() {
  kubectl --context "${context}" "$@"
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${path}" | awk '{print tolower($1)}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${path}" | awk '{print tolower($1)}'
    return
  fi
  if command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 "${path}" | awk '{print tolower($NF)}'
    return
  fi
  die "sha256sum, shasum, or openssl is required"
}

validate_backup_file() {
  local label="$1"
  local path="$2"
  [[ "${path}" == /* ]] || die "${label} path must be absolute: ${path}"
  [[ ! -L "${path}" ]] || die "${label} path must not be a symbolic link: ${path}"
  [[ -f "${path}" ]] || die "${label} file does not exist: ${path}"
  [[ -s "${path}" ]] || die "${label} file is empty: ${path}"
}

marker_field() {
  local marker="$1"
  local key="$2"
  local count
  count="$(awk -F= -v key="${key}" '$1 == key { count++ } END { print count + 0 }' "${marker}")"
  [[ "${count}" == "1" ]] || die "backup marker ${marker} must contain exactly one ${key}= field"
  awk -F= -v key="${key}" '$1 == key { print substr($0, length(key) + 2) }' "${marker}"
}

validate_backup_marker() {
  local backup_label="$1"
  local backup_path="$2"
  local marker_path="$3"
  local expected_kind="$4"
  local marker_format marker_kind marker_context marker_cluster_uid marker_api_server_identity marker_digest marker_verified marker_verified_at actual_digest

  validate_backup_file "${backup_label}" "${backup_path}"
  validate_backup_file "${backup_label} verification marker" "${marker_path}"

  marker_format="$(marker_field "${marker_path}" format)"
  marker_kind="$(marker_field "${marker_path}" kind)"
  marker_context="$(marker_field "${marker_path}" context)"
  marker_cluster_uid="$(marker_field "${marker_path}" cluster_uid)"
  marker_api_server_identity="$(marker_field "${marker_path}" api_server_identity_sha256)"
  marker_digest="$(marker_field "${marker_path}" sha256)"
  marker_verified="$(marker_field "${marker_path}" verified)"
  marker_verified_at="$(marker_field "${marker_path}" verified_at)"

  [[ "${marker_format}" == "${backup_marker_format}" ]] || die "${backup_label} marker has unsupported format"
  [[ "${marker_kind}" == "${expected_kind}" ]] || die "${backup_label} marker kind must be ${expected_kind}"
  [[ "${marker_context}" == "${context}" ]] || die "${backup_label} marker context does not match --context"
  [[ "${marker_cluster_uid}" == "${cluster_uid}" ]] || die "${backup_label} marker cluster_uid does not match the target cluster"
  [[ "${marker_api_server_identity}" == "${api_server_identity_sha256}" ]] || die "${backup_label} marker api_server_identity_sha256 does not match the target cluster"
  [[ "${marker_verified}" == "true" ]] || die "${backup_label} marker must attest verified=true"
  [[ "${marker_digest}" =~ ^[0-9a-f]{64}$ ]] || die "${backup_label} marker sha256 must be 64 lowercase hexadecimal characters"
  [[ "${marker_verified_at}" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || die "${backup_label} marker verified_at must be a UTC RFC3339 timestamp"

  actual_digest="$(sha256_file "${backup_path}")"
  [[ "${actual_digest}" == "${marker_digest}" ]] || die "${backup_label} digest does not match its verification marker"
}

validate_cr_backup_shape() {
  jq -e '
    type == "object" and
    .kind == "List" and
    (.items | type == "array") and
    ([.items[]
      | select(.kind == "AgentRuntime" or .kind == "Agent" or .kind == "GatewayBinding")
      | (((.metadata.namespace // "") | length) > 0 and ((.metadata.name // "") | length) > 0)
    ] | all)
  ' "${cr_backup}" >/dev/null || die "CR backup must be a Kubernetes JSON List with namespaced AgentRuntime, Agent, and GatewayBinding entries"
}

load_cluster_identity() {
  local namespace_json="${tmp_dir}/kube-system-namespace.json"
  local kubeconfig_json="${tmp_dir}/target-kubeconfig.json"
  local api_identity_json="${tmp_dir}/api-server-identity.json"

  kube get namespace kube-system -o json >"${namespace_json}" || die "failed to read kube-system namespace identity"
  cluster_uid="$(jq -r '.metadata.uid // ""' "${namespace_json}")"
  [[ -n "${cluster_uid}" && "${cluster_uid}" != "null" ]] || die "kube-system namespace has no UID"
  [[ "${#cluster_uid}" -le 128 ]] || die "kube-system namespace UID is unexpectedly long"
  [[ "${cluster_uid}" =~ ^[A-Za-z0-9][A-Za-z0-9._:-]*$ ]] || die "kube-system namespace UID has an unexpected format"

  kubectl --context "${context}" config view --minify --flatten -o json >"${kubeconfig_json}" || die "failed to read target API server identity"
  jq -c '
    .clusters
    | if length == 1 then
        .[0].cluster
        | {
            server: (.server // null),
            certificateAuthorityData: (.["certificate-authority-data"] // null),
            insecureSkipTLSVerify: (.["insecure-skip-tls-verify"] // null),
            tlsServerName: (.["tls-server-name"] // null),
            proxyURL: (.["proxy-url"] // null),
            disableCompression: (.["disable-compression"] // null)
          }
      else
        error("expected exactly one target cluster")
      end
  ' "${kubeconfig_json}" >"${api_identity_json}" || die "target kubeconfig has no unique API server identity"
  api_server_identity_sha256="$(sha256_file "${api_identity_json}")"
}

validate_backups() {
  load_cluster_identity
  validate_backup_marker "SQLite/PVC backup" "${sqlite_backup}" "${sqlite_backup_marker}" "sqlite-pvc"
  validate_backup_marker "CR backup" "${cr_backup}" "${cr_backup_marker}" "orka-crs"
  validate_cr_backup_shape
}

write_empty_list() {
  local output="$1"
  printf '%s\n' "${empty_list_json}" >"${output}"
}

fetch_resource_list() {
  local resource="$1"
  local discovery_file="$2"
  local output="$3"
  if grep -Fqx -- "${resource}" "${discovery_file}"; then
    kube get "${resource}" -A -o json >"${output}" || die "failed to list ${resource}"
    jq -e '.kind == "List" and (.items | type == "array")' "${output}" >/dev/null || die "${resource} returned an invalid list"
    return
  fi
  write_empty_list "${output}"
}

collect_cluster_state() {
  kube get --raw=/version >/dev/null || die "cannot reach Kubernetes context ${context}"
  kube api-resources --api-group=core.orka.ai -o name >"${tmp_dir}/core-api-resources" || die "failed to discover core.orka.ai resources"
  kube api-resources --api-group=gateway.orka.ai -o name >"${tmp_dir}/gateway-api-resources" || die "failed to discover gateway.orka.ai resources"

  fetch_resource_list "agentruntimes.core.orka.ai" "${tmp_dir}/core-api-resources" "${tmp_dir}/live-agentruntimes.json"
  fetch_resource_list "agents.core.orka.ai" "${tmp_dir}/core-api-resources" "${tmp_dir}/live-agents.json"
  fetch_resource_list "tasks.core.orka.ai" "${tmp_dir}/core-api-resources" "${tmp_dir}/live-tasks.json"
  fetch_resource_list "gatewaybindings.gateway.orka.ai" "${tmp_dir}/gateway-api-resources" "${tmp_dir}/live-gatewaybindings.json"
}

build_legacy_builtin_agent_inventory() {
  jq -r '
    .items[]
    | select((.spec.runtime.type // "") == "opencode")
    | [(.metadata.namespace // ""), (.metadata.name // "")] | @tsv
  ' "${tmp_dir}/live-agents.json" >"${tmp_dir}/live-legacy-builtin-agents.tsv"
  sort_unique_file "${tmp_dir}/live-legacy-builtin-agents.tsv"
}

build_legacy_task_inventory() {
  jq -r '
    .items[]
    | . as $task
    | [
        {path: "spec.workspace.gitSecretRef", value: ($task.spec.workspace.gitSecretRef // null)},
        {path: "spec.agentRuntime.workspace.gitSecretRef", value: ($task.spec.agentRuntime.workspace.gitSecretRef // null)}
      ][]
    | select(.value != null)
    | [($task.metadata.namespace // ""), ($task.metadata.name // ""), .path] | @tsv
  ' "${tmp_dir}/live-tasks.json" >"${tmp_dir}/live-legacy-task-workspaces.tsv"
  sort_unique_file "${tmp_dir}/live-legacy-task-workspaces.tsv"
}

sort_unique_file() {
  local path="$1"
  LC_ALL=C sort -u "${path}" -o "${path}"
}

append_legacy_runtime_keys() {
  local source="$1"
  local kind_filter="$2"
  local output="$3"
  jq -r --arg kind_filter "${kind_filter}" '
    .items[]
    | select(($kind_filter == "" or .kind == $kind_filter))
    | select((.spec.contractVersion // .spec.contract.version // "") == "orka.harness.v1")
    | [(.metadata.namespace // ""), (.metadata.name // "")] | @tsv
  ' "${source}" >>"${output}"
}

append_agent_records() {
  local source="$1"
  local kind_filter="$2"
  local output="$3"
  jq -r --arg kind_filter "${kind_filter}" '
    .items[]
    | select(($kind_filter == "" or .kind == $kind_filter))
    | select(((.spec.runtime.runtimeRef.name // "") | length) > 0)
    | [(.metadata.namespace // ""), (.metadata.name // ""), .spec.runtime.runtimeRef.name] | @tsv
  ' "${source}" >>"${output}"
}

append_binding_records() {
  local source="$1"
  local kind_filter="$2"
  local output="$3"
  jq -r --arg kind_filter "${kind_filter}" '
    .items[]
    | select(($kind_filter == "" or .kind == $kind_filter))
    | select(((.spec.agentRef.name // "") | length) > 0 and ((.spec.gatewayRef.name // "") | length) > 0)
    | [(.metadata.namespace // ""), (.metadata.name // ""), .spec.agentRef.name, .spec.gatewayRef.name] | @tsv
  ' "${source}" >>"${output}"
}

key_exists() {
  local namespace="$1"
  local name="$2"
  local path="$3"
  local key
  key="${namespace}"$'\t'"${name}"
  grep -Fqx -- "${key}" "${path}"
}

build_reference_inventory() {
  local namespace name reference binding gateway
  local all_agents="${tmp_dir}/all-agents.tsv"
  local all_bindings="${tmp_dir}/all-bindings.tsv"

  touch "${tmp_dir}/legacy-runtime-keys.tsv"
  touch "${tmp_dir}/affected-agent-keys.tsv"
  touch "${tmp_dir}/affected-gateways.tsv"
  : >"${tmp_dir}/live-v1-runtimes.tsv"
  : >"${tmp_dir}/live-affected-agents.tsv"
  : >"${tmp_dir}/live-affected-bindings.tsv"
  : >"${tmp_dir}/live-agent-records.tsv"
  : >"${tmp_dir}/live-binding-records.tsv"
  : >"${all_agents}"
  : >"${all_bindings}"

  append_legacy_runtime_keys "${cr_backup}" "AgentRuntime" "${tmp_dir}/legacy-runtime-keys.tsv"
  append_legacy_runtime_keys "${tmp_dir}/live-agentruntimes.json" "" "${tmp_dir}/legacy-runtime-keys.tsv"
  append_legacy_runtime_keys "${tmp_dir}/live-agentruntimes.json" "" "${tmp_dir}/live-v1-runtimes.tsv"
  sort_unique_file "${tmp_dir}/legacy-runtime-keys.tsv"
  sort_unique_file "${tmp_dir}/live-v1-runtimes.tsv"

  append_agent_records "${cr_backup}" "Agent" "${all_agents}"
  append_agent_records "${tmp_dir}/live-agents.json" "" "${all_agents}"
  while IFS=$'\t' read -r namespace name reference; do
    [[ -n "${namespace}" && -n "${name}" && -n "${reference}" ]] || continue
    if key_exists "${namespace}" "${reference}" "${tmp_dir}/legacy-runtime-keys.tsv"; then
      printf '%s\t%s\n' "${namespace}" "${name}" >>"${tmp_dir}/affected-agent-keys.tsv"
    fi
  done <"${all_agents}"
  sort_unique_file "${tmp_dir}/affected-agent-keys.tsv"

  append_agent_records "${tmp_dir}/live-agents.json" "" "${tmp_dir}/live-agent-records.tsv"
  while IFS=$'\t' read -r namespace name reference; do
    [[ -n "${namespace}" && -n "${name}" && -n "${reference}" ]] || continue
    if key_exists "${namespace}" "${name}" "${tmp_dir}/affected-agent-keys.tsv"; then
      printf '%s\t%s\t%s\n' "${namespace}" "${name}" "${reference}" >>"${tmp_dir}/live-affected-agents.tsv"
    fi
  done <"${tmp_dir}/live-agent-records.tsv"
  sort_unique_file "${tmp_dir}/live-affected-agents.tsv"

  append_binding_records "${cr_backup}" "GatewayBinding" "${all_bindings}"
  append_binding_records "${tmp_dir}/live-gatewaybindings.json" "" "${all_bindings}"
  while IFS=$'\t' read -r namespace binding reference gateway; do
    [[ -n "${namespace}" && -n "${binding}" && -n "${reference}" && -n "${gateway}" ]] || continue
    if key_exists "${namespace}" "${reference}" "${tmp_dir}/affected-agent-keys.tsv"; then
      printf '%s\t%s\n' "${namespace}" "${gateway}" >>"${tmp_dir}/affected-gateways.tsv"
    fi
  done <"${all_bindings}"
  sort_unique_file "${tmp_dir}/affected-gateways.tsv"

  append_binding_records "${tmp_dir}/live-gatewaybindings.json" "" "${tmp_dir}/live-binding-records.tsv"
  while IFS=$'\t' read -r namespace binding reference gateway; do
    [[ -n "${namespace}" && -n "${binding}" && -n "${reference}" && -n "${gateway}" ]] || continue
    if key_exists "${namespace}" "${reference}" "${tmp_dir}/affected-agent-keys.tsv"; then
      printf '%s\t%s\t%s\t%s\n' "${namespace}" "${binding}" "${reference}" "${gateway}" >>"${tmp_dir}/live-affected-bindings.tsv"
    fi
  done <"${tmp_dir}/live-binding-records.tsv"
  sort_unique_file "${tmp_dir}/live-affected-bindings.tsv"
}

parse_gateway_workload_mappings() {
  local mapping gateway_part workload_part gateway_namespace gateway_name gateway_extra
  local workload_kind workload_namespace workload_name workload_extra resource
  : >"${tmp_dir}/gateway-workloads.tsv"

  if [[ "${#gateway_workload_args[@]}" -eq 0 ]]; then
    return 0
  fi

  for mapping in "${gateway_workload_args[@]}"; do
    [[ "${mapping}" == *=* ]] || die "invalid --gateway-workload mapping: ${mapping}"
    gateway_part="${mapping%%=*}"
    workload_part="${mapping#*=}"
    IFS=/ read -r gateway_namespace gateway_name gateway_extra <<<"${gateway_part}"
    IFS=/ read -r workload_kind workload_namespace workload_name workload_extra <<<"${workload_part}"
    [[ -n "${gateway_namespace}" && -n "${gateway_name}" && -z "${gateway_extra:-}" ]] || die "gateway mapping key must be NAMESPACE/GATEWAY_NAME: ${mapping}"
    [[ -n "${workload_kind}" && -n "${workload_namespace}" && -n "${workload_name}" && -z "${workload_extra:-}" ]] || die "gateway workload must be KIND/NAMESPACE/NAME: ${mapping}"
    [[ "${workload_namespace}" == "${gateway_namespace}" ]] || die "gateway workload must be in Gateway namespace ${gateway_namespace}: ${mapping}"

    case "${workload_kind}" in
      deployment|deployments|deployment.apps|deployments.apps)
        resource="deployment.apps"
        ;;
      statefulset|statefulsets|statefulset.apps|statefulsets.apps)
        resource="statefulset.apps"
        ;;
      *)
        die "gateway workload kind must be deployment or statefulset: ${mapping}"
        ;;
    esac

    printf '%s\t%s\t%s\t%s\n' "${gateway_namespace}" "${gateway_name}" "${resource}" "${workload_name}" >>"${tmp_dir}/gateway-workloads.tsv"
  done
  sort_unique_file "${tmp_dir}/gateway-workloads.tsv"
}

verify_gateway_workload() {
  local gateway_namespace="$1"
  local gateway_name="$2"
  local resource="$3"
  local workload_name="$4"
  local output safe_name
  local generation observed_generation spec_replicas status_replicas ready_replicas available_replicas updated_replicas current_replicas terminating_replicas

  safe_name="$(printf '%s-%s-%s-%s' "${gateway_namespace}" "${gateway_name}" "${resource}" "${workload_name}" | tr -c 'A-Za-z0-9_.-' '_')"
  output="${tmp_dir}/workload-${safe_name}.json"
  kube get "${resource}" -n "${gateway_namespace}" "${workload_name}" -o json >"${output}" || die "failed to read gateway workload ${resource}/${gateway_namespace}/${workload_name}"
  jq -e --arg namespace "${gateway_namespace}" --arg name "${workload_name}" '
    .metadata.namespace == $namespace and .metadata.name == $name
  ' "${output}" >/dev/null || die "gateway workload identity mismatch for ${resource}/${gateway_namespace}/${workload_name}"

  generation="$(jq -r '.metadata.generation // 0' "${output}")"
  observed_generation="$(jq -r '.status.observedGeneration // 0' "${output}")"
  spec_replicas="$(jq -r '.spec.replicas // 1' "${output}")"
  status_replicas="$(jq -r '.status.replicas // 0' "${output}")"
  ready_replicas="$(jq -r '.status.readyReplicas // 0' "${output}")"
  available_replicas="$(jq -r '.status.availableReplicas // 0' "${output}")"
  updated_replicas="$(jq -r '.status.updatedReplicas // 0' "${output}")"
  current_replicas="$(jq -r '.status.currentReplicas // 0' "${output}")"
  terminating_replicas="$(jq -r '.status.terminatingReplicas // 0' "${output}")"

  if [[ "${spec_replicas}" != "0" || "${status_replicas}" != "0" || "${ready_replicas}" != "0" || "${available_replicas}" != "0" || "${updated_replicas}" != "0" || "${current_replicas}" != "0" || "${terminating_replicas}" != "0" || "${observed_generation}" -lt "${generation}" ]]; then
    echo "blocked: Gateway ${gateway_namespace}/${gateway_name} workload ${resource}/${workload_name} is not fully scaled to zero" >&2
    echo "  generation=${generation} observedGeneration=${observed_generation} spec.replicas=${spec_replicas} status.replicas=${status_replicas} ready=${ready_replicas} available=${available_replicas} updated=${updated_replicas} current=${current_replicas} terminating=${terminating_replicas}" >&2
    return 1
  fi
  return 0
}

verify_gateway_quiescence() {
  local namespace gateway mapped resource workload_name
  local failed=false

  if [[ ! -s "${tmp_dir}/affected-gateways.tsv" ]]; then
    if [[ -s "${tmp_dir}/gateway-workloads.tsv" ]]; then
      die "--gateway-workload was provided, but the verified CR backup and live state contain no affected Gateway"
    fi
    return 0
  fi

  while IFS=$'\t' read -r namespace gateway; do
    [[ -n "${namespace}" && -n "${gateway}" ]] || continue
    mapped=false
    while IFS=$'\t' read -r mapped_namespace mapped_gateway resource workload_name; do
      if [[ "${mapped_namespace}" == "${namespace}" && "${mapped_gateway}" == "${gateway}" ]]; then
        mapped=true
        verify_gateway_workload "${namespace}" "${gateway}" "${resource}" "${workload_name}" || failed=true
      fi
    done <"${tmp_dir}/gateway-workloads.tsv"
    if [[ "${mapped}" != "true" ]]; then
      echo "blocked: affected Gateway ${namespace}/${gateway} has no --gateway-workload mapping" >&2
      failed=true
    fi
  done <"${tmp_dir}/affected-gateways.tsv"

  while IFS=$'\t' read -r namespace gateway resource workload_name; do
    [[ -n "${namespace}" && -n "${gateway}" ]] || continue
    if ! key_exists "${namespace}" "${gateway}" "${tmp_dir}/affected-gateways.tsv"; then
      echo "blocked: --gateway-workload maps unknown affected Gateway ${namespace}/${gateway} (${resource}/${workload_name})" >&2
      failed=true
    fi
  done <"${tmp_dir}/gateway-workloads.tsv"

  [[ "${failed}" == "false" ]]
}

discover_legacy_wrapper_resources() {
  local resource display raw namespace name component
  : >"${tmp_dir}/legacy-wrapper-resources.tsv"

  for entry in "deployment.apps:Deployment" "service:Service" "secret:Secret"; do
    resource="${entry%%:*}"
    display="${entry#*:}"
    raw="${tmp_dir}/wrapper-${resource//./-}.txt"
    kube get "${resource}" -A -o 'custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,COMPONENT:.metadata.labels.app\.kubernetes\.io/component' --no-headers >"${raw}" || die "failed to list ${display} metadata"
    while read -r namespace name component _; do
      [[ -n "${namespace}" && -n "${name}" ]] || continue
      case "${resource}:${name}:${component:-}" in
        deployment.apps:agent-harness-wrapper:*|deployment.apps:*-agent-harness-wrapper:*|service:agent-harness-wrapper:*|service:*-agent-harness-wrapper:*|secret:harness-wrapper-auth:*|secret:*-harness-wrapper-auth:*|*:*:agent-harness-wrapper)
          printf '%s\t%s\t%s\t%s\n' "${resource}" "${display}" "${namespace}" "${name}" >>"${tmp_dir}/legacy-wrapper-resources.tsv"
          ;;
      esac
    done <"${raw}"
  done
  sort_unique_file "${tmp_dir}/legacy-wrapper-resources.tsv"
}

delete_legacy_wrapper_resources() {
  local resource display namespace name
  [[ -s "${tmp_dir}/legacy-wrapper-resources.tsv" ]] || return 0
  echo "Deleting legacy harness-wrapper resources by explicit operator request:" >&2
  while IFS=$'\t' read -r resource display namespace name; do
    echo "  ${display} ${namespace}/${name}" >&2
    kube delete "${resource}" -n "${namespace}" "${name}" --wait=true || die "failed to delete ${display} ${namespace}/${name}"
  done <"${tmp_dir}/legacy-wrapper-resources.tsv"
  discover_legacy_wrapper_resources
}

report_reference_blockers() {
  local namespace name reference gateway
  local clean=true

  if [[ -s "${tmp_dir}/live-v1-runtimes.tsv" ]]; then
    clean=false
    while IFS=$'\t' read -r namespace name; do
      echo "blocked: AgentRuntime ${namespace}/${name} still declares orka.harness.v1" >&2
    done <"${tmp_dir}/live-v1-runtimes.tsv"
  fi

  if [[ -s "${tmp_dir}/live-affected-agents.tsv" ]]; then
    clean=false
    while IFS=$'\t' read -r namespace name reference; do
      echo "blocked: Agent ${namespace}/${name} still references affected AgentRuntime ${reference}" >&2
    done <"${tmp_dir}/live-affected-agents.tsv"
  fi

  if [[ -s "${tmp_dir}/live-affected-bindings.tsv" ]]; then
    clean=false
    while IFS=$'\t' read -r namespace name reference gateway; do
      echo "blocked: GatewayBinding ${namespace}/${name} still references affected Agent ${reference} and Gateway ${gateway}" >&2
    done <"${tmp_dir}/live-affected-bindings.tsv"
  fi

  [[ "${clean}" == "true" ]]
}

report_legacy_builtin_agent_blockers() {
  local namespace name
  [[ -s "${tmp_dir}/live-legacy-builtin-agents.tsv" ]] || return 0
  while IFS=$'\t' read -r namespace name; do
    echo "blocked: Agent ${namespace}/${name} must be removed before the OpenCode CRD cutover" >&2
    echo "  migrate: export and delete the Agent before cutover, then recreate it after the new CRD is applied with a provider-qualified model.name, reviewed positive contextWindow/maxTokens, and no providerRef, provider Secret, or Agent systemPrompt; alternatively migrate it to claude/codex/copilot or runtimeRef before retrying" >&2
  done <"${tmp_dir}/live-legacy-builtin-agents.tsv"
  return 1
}

report_legacy_task_blockers() {
  local namespace name field_path
  [[ -s "${tmp_dir}/live-legacy-task-workspaces.tsv" ]] || return 0
  while IFS=$'\t' read -r namespace name field_path; do
    echo "blocked: Task ${namespace}/${name} still uses legacy ${field_path}" >&2
    echo "  migrate: export and delete the Task before cutover, then recreate it with spec.workspace.readCredentialRef after the new CRD is applied" >&2
  done <"${tmp_dir}/live-legacy-task-workspaces.tsv"
  return 1
}

report_wrapper_blockers() {
  local resource display namespace name
  [[ -s "${tmp_dir}/legacy-wrapper-resources.tsv" ]] || return 0
  while IFS=$'\t' read -r resource display namespace name; do
    echo "blocked: legacy harness-wrapper ${display} ${namespace}/${name} still exists" >&2
    echo "  cleanup: kubectl --context ${context} -n ${namespace} delete ${resource} ${name} --wait=true" >&2
  done <"${tmp_dir}/legacy-wrapper-resources.tsv"
  return 1
}

run_cluster_preflight() {
  local allow_cleanup="$1"
  local clean=true

  collect_cluster_state
  build_reference_inventory
  build_legacy_builtin_agent_inventory
  build_legacy_task_inventory
  verify_gateway_quiescence || clean=false
  discover_legacy_wrapper_resources

  if [[ "${allow_cleanup}" == "true" && "${delete_legacy_wrapper}" == "true" && "${clean}" == "true" ]]; then
    delete_legacy_wrapper_resources
  fi

  report_reference_blockers || clean=false
  report_legacy_builtin_agent_blockers || clean=false
  report_legacy_task_blockers || clean=false
  report_wrapper_blockers || clean=false
  [[ "${clean}" == "true" ]]
}

validate_backups
parse_gateway_workload_mappings

if ! run_cluster_preflight true; then
  die "hard-cutover preflight failed; no CRDs were applied"
fi

# Server-side validation catches schema/RBAC admission failures without mutation.
kube apply --server-side --force-conflicts --dry-run=server -f "${crd_dir}" >/dev/null || die "server-side CRD dry-run failed"

# Revalidate backup identity and live blockers immediately before the mutating apply.
validate_backups
if ! run_cluster_preflight false; then
  die "hard-cutover state changed after dry-run; no CRDs were applied"
fi
validate_backups

kube apply --server-side --force-conflicts -f "${crd_dir}"
echo "ACP v2 CRD hard cutover applied to context ${context}." >&2
