#!/usr/bin/env bash
set -Eeuo pipefail

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
require_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"; }
redact() {
  sed -E \
    -e 's#(Authorization:[[:space:]]*Bearer[[:space:]]+)[^[:space:]"}]+#\1[REDACTED]#g' \
    -e 's#(ACTIONS_ID_TOKEN_REQUEST_TOKEN=)[^[:space:]]+#\1[REDACTED]#g' \
    -e 's#eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}#[REDACTED-JWT]#g'
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cluster="${KIND_CLUSTER:-orka-live-github-oidc-e2e}"
namespace="${ORKA_NAMESPACE:-orka-system}"
deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-github-oidc-e2e}"
audience="${ORKA_GITHUB_OIDC_AUDIENCE:-orka-live-github-oidc-e2e}"
issuer="${ORKA_GITHUB_OIDC_ISSUER:-https://token.actions.githubusercontent.com}"
repository="${GITHUB_REPOSITORY:-orka-agents/orka}"
allowed_subjects="${ORKA_GITHUB_OIDC_ALLOWED_SUBJECTS:-repo:${repository}:*}"
oidc_namespace="${ORKA_GITHUB_OIDC_NAMESPACE:-default}"
token="${ORKA_GITHUB_OIDC_TOKEN:-}"
port="${ORKA_API_LOCAL_PORT:-18080}"
workdir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/orka-oidc-e2e.XXXXXX")"
kustomization="${repo_root}/config/manager/kustomization.yaml"
backup="${workdir}/kustomization.yaml"
api_pf_pid=""
api_pf_log="${workdir}/api-port-forward.log"

cleanup() {
  status=$?
  if [[ -n "${api_pf_pid}" ]]; then
    kill "${api_pf_pid}" >/dev/null 2>&1 || true
    wait "${api_pf_pid}" 2>/dev/null || true
  fi
  if [[ -f "${backup}" ]]; then cp "${backup}" "${kustomization}"; fi
  if [[ ${status} -ne 0 ]]; then
    {
      kubectl -n "${namespace}" get pods 2>/dev/null || true
      kubectl -n "${namespace}" logs deployment/"${deployment}" --tail=200 2>/dev/null || true
      if [[ -f "${api_pf_log}" ]]; then
        printf '%s\n' '--- API port-forward log ---'
        cat "${api_pf_log}"
      fi
    } | redact >&2
  fi
  kind delete cluster --name "${cluster}" >/dev/null 2>&1 || true
  rm -rf "${workdir}"
  exit "${status}"
}
trap cleanup EXIT

fetch_token() {
  if [[ -n "${token}" ]]; then return; fi
  [[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" && -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ]] || die "GitHub OIDC token source is unavailable"
  response="${workdir}/oidc.json"
  curl -fsS -H "Authorization: Bearer ${ACTIONS_ID_TOKEN_REQUEST_TOKEN}" \
    "${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=$(printf '%s' "${audience}" | jq -sRr @uri)" >"${response}"
  token="$(jq -er '.value' "${response}")"
  [[ -n "${token}" ]] || die "GitHub OIDC endpoint returned an empty token"
}

request() {
  method="$1"; url="$2"; output="$3"; shift 3
  curl -sS -o "${output}" -w '%{http_code}' -X "${method}" "${url}" "$@"
}

start_api_port_forward() {
  kubectl -n "${namespace}" port-forward service/orka-api "${port}:8080" >>"${api_pf_log}" 2>&1 &
  api_pf_pid=$!
}

wait_for_api() {
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if curl -fsS --connect-timeout 5 --max-time 10 "http://127.0.0.1:${port}/readyz" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${api_pf_pid}" ]] && ! kill -0 "${api_pf_pid}" 2>/dev/null; then
      wait "${api_pf_pid}" 2>/dev/null || true
      api_pf_pid=""
      start_api_port_forward
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "Orka API did not become ready"
}

for cmd in make go docker kind kubectl curl jq; do require_cmd "${cmd}"; done
cd "${repo_root}"
cp "${kustomization}" "${backup}"
fetch_token
log "Creating kind cluster ${cluster}"
make setup-test-e2e KIND_CLUSTER="${cluster}"
kubectl config use-context "kind-${cluster}" >/dev/null
log "Building and loading controller image"
make docker-build IMG="${manager_image}"
kind load docker-image "${manager_image}" --name "${cluster}"
make deploy IMG="${manager_image}"
kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s
kubectl -n "${namespace}" patch deployment "${deployment}" --type=strategic \
  -p '{"spec":{"template":{"spec":{"containers":[{"name":"manager","imagePullPolicy":"IfNotPresent"}]}}}}'
kubectl -n "${namespace}" set env deployment/"${deployment}" \
  ORKA_OIDC_ISSUER="${issuer}" \
  ORKA_OIDC_AUDIENCE="${audience}" \
  ORKA_OIDC_ALLOWED_SUBJECTS="${allowed_subjects}" \
  ORKA_OIDC_NAMESPACE="${oidc_namespace}" \
  ORKA_OIDC_JWKS_URL- \
  ORKA_CONTEXT_TOKEN_PROFILE- \
  ORKA_CONTEXT_TOKEN_ISSUER- \
  ORKA_CONTEXT_TOKEN_AUDIENCE-
kubectl -n "${namespace}" rollout status deployment/"${deployment}" --timeout=5m
start_api_port_forward
wait_for_api

payload="${workdir}/task.json"
response="${workdir}/response.json"
task="github-oidc-$(date +%s)-${RANDOM}"
jq -n --arg name "${task}" '{name:$name,namespace:"default",type:"container",image:"busybox:1.36",command:["/bin/sh","-c"],args:["echo github-oidc"]}' >"${payload}"
status="$(request POST "http://127.0.0.1:${port}/api/v1/tasks" "${response}" -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' --data @"${payload}")"
[[ "${status}" == 201 ]] || { cat "${response}" | redact >&2; die "OIDC task creation returned HTTP ${status}"; }
jq -e --arg issuer "${issuer}" '.spec.requestedBy.issuer == $issuer and ((.spec.requestedBy.subject // "") != "")' "${response}" >/dev/null

for shape in top nested; do
  tampered="${workdir}/tampered-${shape}.json"
  if [[ "${shape}" == top ]]; then
    jq '. + {requestedBy:{issuer:"evil",subject:"evil"}}' "${payload}" >"${tampered}"
  else
    jq '. + {spec:{requestedBy:{issuer:"evil",subject:"evil"}}}' "${payload}" >"${tampered}"
  fi
  status="$(request POST "http://127.0.0.1:${port}/api/v1/tasks" "${workdir}/${shape}.json" -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' --data @"${tampered}")"
  [[ "${status}" == 400 ]] || die "${shape} requestedBy tampering returned HTTP ${status}"
done

if kubectl -n "${namespace}" logs deployment/"${deployment}" --all-containers=true 2>/dev/null | grep -Fq "${token}"; then
  die "GitHub OIDC token appeared in controller logs"
fi
log "Live GitHub OIDC E2E passed"
