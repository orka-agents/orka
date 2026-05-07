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

kind_cluster="${KIND_CLUSTER:-orka-live-github-oidc-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18080}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-github-oidc-e2e}"
github_oidc_audience="${ORKA_GITHUB_OIDC_AUDIENCE:-orka-live-github-oidc-e2e}"
github_oidc_issuer="${ORKA_GITHUB_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
github_oidc_token="${ORKA_GITHUB_OIDC_TOKEN:-}"
api_pf_pid=""
task_name=""
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-github-oidc-e2e.XXXXXX")"
api_pf_log="${work_dir}/api-port-forward.log"
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"

redact() {
  local text
  text="$(cat)"
  if [[ -n "${github_oidc_token}" ]]; then
    text="${text//${github_oidc_token}/[REDACTED_GITHUB_OIDC_JWT]}"
  fi
  if [[ -n "${ORKA_GITHUB_OIDC_TOKEN:-}" ]]; then
    text="${text//${ORKA_GITHUB_OIDC_TOKEN}/[REDACTED_GITHUB_OIDC_JWT]}"
  fi
  if [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]]; then
    text="${text//${ACTIONS_ID_TOKEN_REQUEST_TOKEN}/[REDACTED_ACTIONS_ID_TOKEN_REQUEST_TOKEN]}"
  fi
  printf '%s' "${text}" | sed -E \
    -e 's/(Authorization: *([Bb]earer|token) +)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ACTIONS_ID_TOKEN_REQUEST_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/(ORKA_GITHUB_OIDC_TOKEN=)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/[REDACTED_JWT]/g'
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
    kubectl get pods,svc,deploy,tasks -n default -o wide 2>/dev/null || true
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

  restore_manager_kustomization
  make cleanup-test-e2e KIND_CLUSTER="${kind_cluster}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}" >/dev/null 2>&1 || true

  if [[ "${status}" -ne 0 ]]; then
    log "Live GitHub OIDC e2e failed"
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

require_github_oidc_token_source() {
  if [[ -n "${github_oidc_token}" ]]; then
    return 0
  fi
  if [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]]; then
    return 0
  fi

  rm -rf "${work_dir}" >/dev/null 2>&1 || true
  die "GitHub OIDC token source is required: run in GitHub Actions with id-token: write or set ORKA_GITHUB_OIDC_TOKEN"
}

fetch_github_oidc_token() {
  if [[ -n "${github_oidc_token}" ]]; then
    log "Using GitHub OIDC token from ORKA_GITHUB_OIDC_TOKEN"
    return 0
  fi

  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || die "ACTIONS_ID_TOKEN_REQUEST_TOKEN is required unless ORKA_GITHUB_OIDC_TOKEN is set"
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ]] || die "ACTIONS_ID_TOKEN_REQUEST_URL is required unless ORKA_GITHUB_OIDC_TOKEN is set"

  local encoded_audience
  local separator
  local response_file

  encoded_audience="$(jq -rn --arg v "${github_oidc_audience}" '$v|@uri')"
  separator="&"
  if [[ "${ACTIONS_ID_TOKEN_REQUEST_URL}" != *\?* ]]; then
    separator="?"
  fi
  response_file="${work_dir}/github-oidc-token-response.json"

  log "Fetching GitHub Actions OIDC token"
  if ! curl -fsS \
    -H "Authorization: bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
    "${ACTIONS_ID_TOKEN_REQUEST_URL}${separator}audience=${encoded_audience}" \
    >"${response_file}"; then
    {
      echo "GitHub Actions OIDC token request failed"
      cat "${response_file}" 2>/dev/null || true
    } | redact >&2
    die "failed to fetch GitHub Actions OIDC token"
  fi

  github_oidc_token="$(jq -er '.value // empty' "${response_file}")" || die "GitHub Actions OIDC token response did not contain .value"
  [[ -n "${github_oidc_token}" ]] || die "GitHub Actions OIDC token response contained an empty .value"
  rm -f "${response_file}"
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

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq

  require_github_oidc_token_source

  cd "${repo_root}"
  [[ -f "${manager_kustomization}" ]] || die "missing ${manager_kustomization}"
  cp "${manager_kustomization}" "${manager_kustomization_backup}"

  trap 'status=$?; on_exit "${status}"; exit "${status}"' EXIT

  log "Creating or reusing Kind cluster ${kind_cluster}"
  run make setup-test-e2e KIND_CLUSTER="${kind_cluster}"
  run kubectl config use-context "kind-${kind_cluster}"

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"

  log "Loading manager image into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"

  log "Deploying Orka manager"
  run make deploy IMG="${manager_image}"
  run kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s

  log "Configuring local image pull policy and GitHub OIDC auth"
  run kubectl -n "${orka_namespace}" patch deployment "${orka_controller_deployment}" \
    --type=strategic \
    -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"IfNotPresent"}]}}}}'
  run kubectl -n "${orka_namespace}" set env deployment/"${orka_controller_deployment}" \
    ORKA_OIDC_ISSUER="${github_oidc_issuer}" \
    ORKA_OIDC_AUDIENCE="${github_oidc_audience}" \
    ORKA_OIDC_JWKS_URL-
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_api_port_forward)"

  local api_base
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"

  log "Verifying unauthenticated API requests are rejected"
  local unauth_response unauth_status
  unauth_response="${work_dir}/unauth-response.json"
  unauth_status="$(api_request GET "${api_base}/api/v1/tasks?namespace=default" "${unauth_response}")"
  expect_http_status "${unauth_status}" "401" "${unauth_response}" "unauthenticated task list"

  fetch_github_oidc_token

  log "Creating Task with live GitHub OIDC token"
  task_name="github-oidc-$(date +%s)-${RANDOM}"
  local create_payload create_response create_status
  create_payload="${work_dir}/create-task.json"
  create_response="${work_dir}/create-task-response.json"
  jq -n --arg name "${task_name}" '{
    name: $name,
    namespace: "default",
    type: "container",
    image: "busybox:1.36",
    command: ["/bin/sh", "-c"],
    args: ["echo github-oidc"]
  }' >"${create_payload}"

  create_status="$(api_request POST "${api_base}/api/v1/tasks" "${create_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${create_payload}")"
  expect_http_status "${create_status}" "201" "${create_response}" "OIDC task creation"

  jq -e --arg issuer "${github_oidc_issuer}" '
    .spec.requestedBy.issuer == $issuer
    and ((.spec.requestedBy.subject // "") != "")
  ' "${create_response}" >/dev/null || {
    {
      echo "created Task response did not contain expected spec.requestedBy"
      cat "${create_response}"
    } | redact >&2
    die "missing or invalid spec.requestedBy in task creation response"
  }

  log "Verifying created Task persisted requestedBy identity"
  kubectl get task "${task_name}" -n default -o json >"${work_dir}/created-task-kube.json"
  jq -e --arg issuer "${github_oidc_issuer}" '
    .spec.requestedBy.issuer == $issuer
    and ((.spec.requestedBy.subject // "") != "")
  ' "${work_dir}/created-task-kube.json" >/dev/null

  log "Verifying top-level requestedBy tampering is rejected"
  local tamper_top_payload tamper_top_response tamper_top_status
  tamper_top_payload="${work_dir}/tamper-top-requested-by.json"
  tamper_top_response="${work_dir}/tamper-top-requested-by-response.json"
  jq --arg name "${task_name}-top" '.name = $name | . + {requestedBy: {issuer: "evil", subject: "evil"}}' \
    "${create_payload}" >"${tamper_top_payload}"
  tamper_top_status="$(api_request POST "${api_base}/api/v1/tasks" "${tamper_top_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${tamper_top_payload}")"
  expect_http_status "${tamper_top_status}" "400" "${tamper_top_response}" "top-level requestedBy tampering"

  log "Verifying nested spec.requestedBy tampering is rejected"
  local tamper_spec_payload tamper_spec_response tamper_spec_status
  tamper_spec_payload="${work_dir}/tamper-spec-requested-by.json"
  tamper_spec_response="${work_dir}/tamper-spec-requested-by-response.json"
  jq --arg name "${task_name}-spec" '.name = $name | . + {spec: {requestedBy: {issuer: "evil", subject: "evil"}}}' \
    "${create_payload}" >"${tamper_spec_payload}"
  tamper_spec_status="$(api_request POST "${api_base}/api/v1/tasks" "${tamper_spec_response}" \
    -H "Authorization: Bearer ${github_oidc_token}" \
    -H 'Content-Type: application/json' \
    --data @"${tamper_spec_payload}")"
  expect_http_status "${tamper_spec_status}" "400" "${tamper_spec_response}" "nested spec.requestedBy tampering"

  log "Live GitHub OIDC e2e passed"
}

main "$@"
