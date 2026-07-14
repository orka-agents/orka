#!/usr/bin/env bash

set -Eeuo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
script_path="${script_dir}/$(basename "${BASH_SOURCE[0]}")"
e2e_kind_helper="${script_dir}/e2e-kind-cluster.sh"
e2e_kind_scoped_arg="--e2e-kind-scoped"

kind_cluster="${KIND_CLUSTER:-orka-live-github-label-trigger-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18082}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-github-label-trigger-e2e}"
target_repo_url="${GITHUB_LABEL_TRIGGER_TARGET_REPO_URL:-https://github.com/orka-agents/orka}"
target_number="${GITHUB_LABEL_TRIGGER_TARGET_NUMBER:-1}"
agent_name="${GITHUB_LABEL_TRIGGER_AGENT_NAME:-github-label-ci-agent}"
label_name="agent:implement"
webhook_secret=""
api_pf_pid=""
task_name=""
work_dir=""
api_pf_log=""
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup=""

initialize_work_dir() {
  work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-github-label-trigger-e2e.XXXXXX")" ||
    die "failed to create temporary work directory"
  api_pf_log="${work_dir}/api-port-forward.log"
  manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"
}

hash_text() {
  local value="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "${value}" | sha256sum | awk '{print $1}'
    return
  fi
  if command -v shasum >/dev/null 2>&1; then
    printf '%s' "${value}" | shasum -a 256 | awk '{print $1}'
    return
  fi
  if command -v openssl >/dev/null 2>&1; then
    printf '%s' "${value}" | openssl dgst -sha256 | awk '{print $NF}'
    return
  fi

  die "no SHA-256 tool available (need sha256sum, shasum, or openssl)"
}

validate_scoped_target() {
  local expected_context="kind-${kind_cluster}"
  local state_dir="${E2E_CLUSTER_STATE_DIR:-}"
  local lock_root="${E2E_CLUSTER_LOCK_ROOT:-}"
  local operation_token="${E2E_KIND_OPERATION_TOKEN:-}"
  local lock_dir
  local current_context
  local current_cluster
  local identity_material
  local server
  local certificate_authority_data
  local fingerprint
  local state_fingerprint

  [[ "${E2E_KIND_TARGET_READY:-}" == "1" ]] ||
    die "refusing to run outside e2e-kind-cluster.sh run: target is not ready"
  [[ -n "${KUBECONFIG:-}" && "${KUBECONFIG}" != *:* ]] ||
    die "refusing to run without one isolated KUBECONFIG"
  [[ -n "${state_dir}" && -n "${lock_root}" && -n "${operation_token}" ]] ||
    die "refusing to run without helper state and operation lock metadata"
  [[ -d "${state_dir}" && ! -L "${state_dir}" ]] ||
    die "helper state directory is missing or unsafe"
  [[ "${E2E_KIND_EXPECTED_CONTEXT:-}" == "${expected_context}" ]] ||
    die "helper expected context does not match ${expected_context}"
  [[ "${E2E_KIND_EXPECTED_CLUSTER:-}" == "${expected_context}" ]] ||
    die "helper expected cluster does not match ${expected_context}"
  [[ "${KUBECONFIG}" == "${state_dir}/target.kubeconfig" ]] ||
    die "KUBECONFIG is not the helper-owned target kubeconfig"
  [[ -f "${KUBECONFIG}" && ! -L "${KUBECONFIG}" ]] ||
    die "helper-owned target kubeconfig is missing or unsafe"

  local state_file
  for state_file in cluster context kubeconfig fingerprint status; do
    [[ -f "${state_dir}/${state_file}" && ! -L "${state_dir}/${state_file}" ]] ||
      die "helper state is missing safe ${state_file} metadata"
  done
  [[ "$(<"${state_dir}/cluster")" == "${kind_cluster}" ]] ||
    die "helper state cluster does not match ${kind_cluster}"
  [[ "$(<"${state_dir}/context")" == "${expected_context}" ]] ||
    die "helper state context does not match ${expected_context}"
  [[ "$(<"${state_dir}/kubeconfig")" == "${KUBECONFIG}" ]] ||
    die "helper state kubeconfig does not match KUBECONFIG"
  [[ "$(<"${state_dir}/status")" == "ready" ]] ||
    die "helper target state is not ready"

  lock_dir="${lock_root}/kind-${kind_cluster}.operation"
  [[ -d "${lock_dir}" && ! -L "${lock_dir}" ]] ||
    die "helper operation lock is not held"
  [[ -f "${lock_dir}/token" && ! -L "${lock_dir}/token" ]] ||
    die "helper operation lock token is missing or unsafe"
  [[ "$(<"${lock_dir}/token")" == "${operation_token}" ]] ||
    die "helper operation lock token does not match"
  [[ -f "${lock_dir}/state_dir" && ! -L "${lock_dir}/state_dir" ]] ||
    die "helper operation lock state metadata is missing or unsafe"
  [[ "$(<"${lock_dir}/state_dir")" == "${state_dir}" ]] ||
    die "helper operation lock targets different state"

  current_context="$(kubectl --kubeconfig "${KUBECONFIG}" config current-context)" ||
    die "failed to read scoped Kubernetes context"
  current_cluster="$(kubectl --kubeconfig "${KUBECONFIG}" config view --minify -o 'jsonpath={.contexts[0].context.cluster}')" ||
    die "failed to read scoped Kubernetes cluster"
  [[ "${current_context}" == "${expected_context}" ]] ||
    die "scoped Kubernetes context ${current_context} does not match ${expected_context}"
  [[ "${current_cluster}" == "${expected_context}" ]] ||
    die "scoped Kubernetes cluster ${current_cluster} does not match ${expected_context}"

  identity_material="$(kubectl --kubeconfig "${KUBECONFIG}" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.server}{"\n"}{.clusters[0].cluster.certificate-authority-data}')" ||
    die "failed to read scoped Kubernetes target identity"
  server="${identity_material%%$'\n'*}"
  [[ "${identity_material}" == *$'\n'* ]] ||
    die "scoped Kubernetes target identity is incomplete"
  certificate_authority_data="${identity_material#*$'\n'}"
  [[ -n "${server}" && -n "${certificate_authority_data}" ]] ||
    die "scoped Kubernetes target identity is incomplete"
  fingerprint="$(hash_text "${identity_material}")"
  state_fingerprint="$(<"${state_dir}/fingerprint")"
  [[ "${fingerprint}" == "${state_fingerprint}" ]] ||
    die "scoped Kubernetes target identity does not match helper state"
}

run_under_e2e_kind_helper() {
  [[ -x "${e2e_kind_helper}" ]] || die "missing executable E2E Kind helper: ${e2e_kind_helper}"
  log "Running live GitHub label trigger e2e through isolated Kind helper"
  exec env KIND_CLUSTER="${kind_cluster}" \
    "${e2e_kind_helper}" run --create --cleanup -- \
    "${script_path}" "${e2e_kind_scoped_arg}" "$@"
}

preflight() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq
  require_cmd python3

  if [[ ! "${target_number}" =~ ^[0-9]+$ || "${target_number}" -le 0 ]]; then
    die "GITHUB_LABEL_TRIGGER_TARGET_NUMBER must be a positive integer"
  fi
  normalize_repo_url "${target_repo_url}" >/dev/null
  [[ -f "${manager_kustomization}" ]] || die "missing ${manager_kustomization}"
}

redact() {
  local text
  text="$(cat)"
  if [[ -n "${webhook_secret}" ]]; then
    text="${text//${webhook_secret}/[REDACTED_WEBHOOK_SECRET]}"
  fi
  printf '%s' "${text}" | sed -E \
    -e 's/(X-Hub-Signature-256: *sha256=)[A-Fa-f0-9]+/\1[REDACTED_SIGNATURE]/g' \
    -e 's/(ORKA_GITHUB_WEBHOOK_SECRET=)[^[:space:]]+/\1[REDACTED_WEBHOOK_SECRET]/g'
}

cleanup_port_forward() {
  if [[ -n "${api_pf_pid}" ]]; then
    if kill -0 "${api_pf_pid}" 2>/dev/null; then
      kill "${api_pf_pid}" 2>/dev/null || true
    fi
    wait "${api_pf_pid}" 2>/dev/null || true
    api_pf_pid=""
  fi
}

restore_manager_kustomization() {
  if [[ -f "${manager_kustomization_backup}" ]]; then
    cp "${manager_kustomization_backup}" "${manager_kustomization}" || true
  fi
}

dump_diagnostics() {
  log "Collecting redacted diagnostics"

  {
    echo "=== Current Kubernetes Context ==="
    kubectl config current-context 2>/dev/null || true
    echo
    echo "=== Orka Namespace Resources ==="
    kubectl get pods,svc,deploy,tasks -n "${orka_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Default Namespace Resources ==="
    kubectl get pods,svc,deploy,agents,tasks -n default -o wide 2>/dev/null || true
    echo
    echo "=== Orka Namespace Events ==="
    kubectl get events -n "${orka_namespace}" --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Default Namespace Events ==="
    kubectl get events -n default --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    local controller_pod
    controller_pod="$(kubectl get pods -l control-plane=controller-manager -n "${orka_namespace}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${controller_pod}" ]]; then
      kubectl logs "${controller_pod}" -n "${orka_namespace}" -c manager --tail=200 2>/dev/null || true
    else
      kubectl logs deployment/"${orka_controller_deployment}" -n "${orka_namespace}" -c manager --tail=200 2>/dev/null || true
    fi
    echo
    echo "=== API Port-forward Log ==="
    if [[ -f "${api_pf_log}" ]]; then
      cat "${api_pf_log}" 2>/dev/null || true
    fi
  } | redact >&2
}

on_exit() {
  local status="$1"
  set +e

  if [[ "${status}" -ne 0 ]]; then
    dump_diagnostics
  fi

  cleanup_port_forward

  if [[ -n "${task_name}" ]]; then
    kubectl delete task "${task_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true
  fi
  kubectl delete agent "${agent_name}" -n default --ignore-not-found=true >/dev/null 2>&1 || true

  restore_manager_kustomization
  if [[ -n "${work_dir}" ]]; then
    rm -rf "${work_dir}" >/dev/null 2>&1 || true
  fi

  if [[ "${status}" -ne 0 ]]; then
    log "Live GitHub label trigger e2e failed"
  fi
}

run() {
  printf '+ ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
  "$@"
}

wait_for_http() {
  local url="$1"
  local description="$2"
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${api_pf_pid}" ]] && ! kill -0 "${api_pf_pid}" 2>/dev/null; then
      warn "API port-forward exited while waiting for ${description}; restarting"
      wait "${api_pf_pid}" 2>/dev/null || true
      api_pf_pid="$(start_api_port_forward)"
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "${description} never became available at ${url}"
}

start_port_forward() {
  local namespace_arg="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local logfile="$5"

  kubectl -n "${namespace_arg}" port-forward "${resource}" "${local_port}:${remote_port}" >>"${logfile}" 2>&1 &
  echo $!
}

start_api_port_forward() {
  start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}"
}

api_request() {
  local method="$1"
  local url="$2"
  local output_file="$3"
  shift 3

  local output_basename
  local err_file
  local status

  output_basename="$(basename "${output_file}")"
  err_file="${work_dir}/curl-${method}-${output_basename}.err"
  if ! status="$(curl -sS --connect-timeout 10 --max-time 60 \
    -o "${output_file}" \
    -w '%{http_code}' \
    -X "${method}" \
    "$@" \
    "${url}" 2>"${err_file}")"; then
    {
      echo "curl ${method} ${url} failed"
      cat "${err_file}" 2>/dev/null || true
      cat "${output_file}" 2>/dev/null || true
    } | redact >&2
    return 1
  fi

  printf '%s' "${status}"
}

expect_http_status() {
  local actual="$1"
  local expected="$2"
  local response_file="$3"
  local description="$4"

  if [[ "${actual}" != "${expected}" ]]; then
    {
      echo "${description} returned HTTP ${actual}, expected ${expected}"
      echo
      cat "${response_file}" 2>/dev/null || true
    } | redact >&2
    return 1
  fi
}

generate_secret() {
  python3 - <<'PY'
import secrets
print(secrets.token_hex(32))
PY
}

normalize_repo_url() {
  local input="$1"
  local stripped
  local full_name
  stripped="${input%.git}"
  stripped="${stripped%/}"

  case "${stripped}" in
    https://github.com/*)
      full_name="${stripped#https://github.com/}"
      ;;
    http://github.com/*)
      full_name="${stripped#http://github.com/}"
      ;;
    git@github.com:*)
      full_name="${stripped#git@github.com:}"
      ;;
    *)
      die "target repo URL must be a github.com repository URL, got ${input}"
      ;;
  esac

  full_name="${full_name#/}"
  full_name="${full_name%/}"

  local owner repo extra
  IFS=/ read -r owner repo extra <<<"${full_name}"
  if [[ -z "${owner}" || -z "${repo}" || -n "${extra:-}" ]]; then
    die "target repo URL must identify exactly one GitHub repository, got ${input}"
  fi

  printf '%s\t%s\t%s\n' "${owner}/${repo}" "https://github.com/${owner}/${repo}" "https://github.com/${owner}/${repo}.git"
}

write_agent_manifest() {
  cat <<EOF_AGENT | kubectl apply -f -
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${agent_name}
  namespace: default
spec:
  runtime:
    type: codex
    defaultMaxTurns: 5
    defaultAllowBash: false
  systemPrompt:
    inline: |
      You are a CI smoke-test agent. Do not execute any work.
EOF_AGENT
}

write_payload() {
  local payload_file="$1"
  local repo_full="$2"
  local repo_html="$3"
  local repo_clone="$4"
  local delivery="$5"

  jq -n \
    --arg label "${label_name}" \
    --arg repo_full "${repo_full}" \
    --arg repo_html "${repo_html}" \
    --arg repo_clone "${repo_clone}" \
    --arg sender "github-actions[bot]" \
    --arg body "Synthetic workflow_dispatch payload for Orka GitHub label trigger e2e. Delivery: ${delivery}" \
    --argjson number "${target_number}" \
    '{
      action: "labeled",
      label: {name: $label},
      repository: {
        full_name: $repo_full,
        html_url: $repo_html,
        clone_url: $repo_clone,
        default_branch: "main"
      },
      issue: {
        number: $number,
        title: "Orka GitHub label trigger smoke test",
        body: $body,
        html_url: ($repo_html + "/issues/" + ($number | tostring))
      },
      sender: {login: $sender}
    }' >"${payload_file}"
}

sign_payload() {
  local payload_file="$1"
  WEBHOOK_SECRET="${webhook_secret}" PAYLOAD_FILE="${payload_file}" python3 - <<'PY'
import hashlib
import hmac
import os
from pathlib import Path

secret = os.environ["WEBHOOK_SECRET"].encode()
body = Path(os.environ["PAYLOAD_FILE"]).read_bytes()
print("sha256=" + hmac.new(secret, body, hashlib.sha256).hexdigest())
PY
}

run_e2e() {
  preflight
  validate_scoped_target

  webhook_secret="${ORKA_GITHUB_WEBHOOK_SECRET:-}"
  if [[ -z "${webhook_secret}" ]]; then
    webhook_secret="$(generate_secret)"
  fi

  local repo_full repo_html repo_clone
  IFS=$'\t' read -r repo_full repo_html repo_clone < <(normalize_repo_url "${target_repo_url}")

  cd "${repo_root}"
  initialize_work_dir
  if ! cp "${manager_kustomization}" "${manager_kustomization_backup}"; then
    rm -rf "${work_dir}" >/dev/null 2>&1 || true
    die "failed to back up ${manager_kustomization}"
  fi

  trap 'status=$?; trap - EXIT; on_exit "${status}"; exit "${status}"' EXIT

  log "Using validated Kind cluster ${kind_cluster}"
  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"

  log "Loading manager image into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"

  log "Deploying Orka manager"
  run make deploy IMG="${manager_image}"
  run kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s
  run kubectl wait --for=condition=Established crd/agents.core.orka.ai --timeout=60s

  log "Creating runtime Agent ${agent_name} in default namespace"
  write_agent_manifest

  log "Configuring local image pull policy and GitHub label trigger env"
  run kubectl -n "${orka_namespace}" patch deployment "${orka_controller_deployment}" \
    --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"IfNotPresent"}]}}}}'
  kubectl -n "${orka_namespace}" set env deployment/"${orka_controller_deployment}" \
    ORKA_GITHUB_WEBHOOK_SECRET="${webhook_secret}" \
    ORKA_GITHUB_LABEL_TRIGGER_AGENT="${agent_name}" \
    ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE=default \
    ORKA_GITHUB_LABEL_TRIGGER_TIMEOUT=5m \
    ORKA_GITHUB_LABEL_TRIGGER_MAX_TURNS=5 >/dev/null
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_api_port_forward)"

  local api_base
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"

  local delivery payload_file signature response_file status
  delivery="live-label-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-0}-$(date +%s)-${RANDOM}"
  payload_file="${work_dir}/github-label-payload.json"
  response_file="${work_dir}/github-label-response.json"
  write_payload "${payload_file}" "${repo_full}" "${repo_html}" "${repo_clone}" "${delivery}"
  signature="$(sign_payload "${payload_file}")"

  log "Verifying invalid webhook signatures are rejected"
  local invalid_response invalid_status
  invalid_response="${work_dir}/invalid-signature-response.json"
  invalid_status="$(api_request POST "${api_base}/webhooks/github" "${invalid_response}" \
    -H 'Content-Type: application/json' \
    -H 'X-GitHub-Event: issues' \
    -H "X-GitHub-Delivery: ${delivery}-invalid" \
    -H 'X-Hub-Signature-256: sha256=00' \
    --data-binary @"${payload_file}")"
  expect_http_status "${invalid_status}" "401" "${invalid_response}" "invalid signature webhook"

  log "Sending signed GitHub label webhook for ${repo_full}"
  status="$(api_request POST "${api_base}/webhooks/github" "${response_file}" \
    -H 'Content-Type: application/json' \
    -H 'X-GitHub-Event: issues' \
    -H "X-GitHub-Delivery: ${delivery}" \
    -H "X-Hub-Signature-256: ${signature}" \
    --data-binary @"${payload_file}")"
  expect_http_status "${status}" "201" "${response_file}" "signed label webhook"
  task_name="$(jq -er '.taskName' "${response_file}")"

  log "Verifying created Task ${task_name} targets ${repo_full}"
  local task_file
  task_file="${work_dir}/created-task.json"
  run kubectl get task "${task_name}" -n default -o json >"${task_file}"
  jq -e \
    --arg agent "${agent_name}" \
    --arg delivery "${delivery}" \
    --arg label "${label_name}" \
    --arg repo_full "${repo_full}" \
    --arg repo_clone "${repo_clone}" \
    '.spec.type == "agent"
      and .spec.agentRef.name == $agent
      and .spec.agentRuntime.workspace.gitRepo == $repo_clone
      and .spec.agentRuntime.workspace.branch == "main"
      and ((.spec.agentRuntime.workspace.pushBranch // "") == "")
      and (.spec.agentRuntime.workspace.gitSecretRef == null)
      and .metadata.annotations["orka.ai/github-delivery"] == $delivery
      and .metadata.annotations["orka.ai/github-label"] == $label
      and .metadata.annotations["orka.ai/github-repository"] == $repo_full
      and (.spec.prompt | contains($repo_full))' \
    "${task_file}" >/dev/null

  log "Verifying repeated delivery is idempotent"
  local duplicate_response duplicate_status
  duplicate_response="${work_dir}/duplicate-response.json"
  duplicate_status="$(api_request POST "${api_base}/webhooks/github" "${duplicate_response}" \
    -H 'Content-Type: application/json' \
    -H 'X-GitHub-Event: issues' \
    -H "X-GitHub-Delivery: ${delivery}" \
    -H "X-Hub-Signature-256: ${signature}" \
    --data-binary @"${payload_file}")"
  expect_http_status "${duplicate_status}" "202" "${duplicate_response}" "duplicate label webhook"
  jq -e --arg task "${task_name}" '.status == "duplicate" and .taskName == $task' "${duplicate_response}" >/dev/null

  log "Live GitHub label trigger e2e passed"
}

main() {
  if [[ "${1:-}" != "${e2e_kind_scoped_arg}" ]]; then
    preflight
    run_under_e2e_kind_helper "$@"
  fi
  shift
  run_e2e "$@"
}

main "$@"
