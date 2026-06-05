#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"

require_cmd kubectl
require_cmd jq

sandbox_session_claim_name() {
  local session_name="$1"
  local claim_namespace="${DEMO_SANDBOX_CLAIM_NAMESPACE:-${DEMO_NAMESPACE}}"
  local task_namespace="${DEMO_NAMESPACE}"
  local template_namespace="${DEMO_SANDBOX_TEMPLATE_NAMESPACE:-${DEMO_NAMESPACE}}"
  local template_ref="${DEMO_SANDBOX_TEMPLATE_REF:-orka-live-template}"
  local digest

  if command -v shasum >/dev/null 2>&1; then
    digest="$(
      printf '%s\0%s\0%s\0%s\0%s' \
        "${claim_namespace}" \
        "${task_namespace}" \
        "${template_namespace}" \
        "${template_ref}" \
        "${session_name}" \
        | shasum -a 256 | awk '{print $1}'
    )"
  elif command -v sha256sum >/dev/null 2>&1; then
    digest="$(
      printf '%s\0%s\0%s\0%s\0%s' \
        "${claim_namespace}" \
        "${task_namespace}" \
        "${template_namespace}" \
        "${template_ref}" \
        "${session_name}" \
        | sha256sum | awk '{print $1}'
    )"
  else
    die "shasum or sha256sum is required to compute the demo SandboxClaim name"
  fi

  printf 'orka-session-%.32s\n' "${digest}"
}

log "Cleaning up demo tasks"
delete_tasks_by_selector "$(demo_label_selector)"
delete_tasks_by_name_prefix "chat-${DEMO_CHAT_SESSION_PREFIX}"
delete_tasks_by_session_ref_prefix "${DEMO_CHAT_SESSION_PREFIX}"
delete_tasks_by_name_prefix "${DEMO_MANUAL_TASK_NAME}"
delete_tasks_by_name_prefix "${DEMO_CRON_TASK_NAME}"
delete_tasks_by_name_prefix "${DEMO_SECURITY_SCAN_PREFIX}"

log "Cleaning up demo agents"
kubectl delete agents -n "${DEMO_NAMESPACE}" -l "$(demo_label_selector)" --ignore-not-found >/dev/null 2>&1 || true
delete_agent_if_exists "${DEMO_PR_COORDINATOR_NAME}"

log "Cleaning up demo security resources"
kubectl delete repositoryscans -n "${DEMO_NAMESPACE}" -l "$(demo_label_selector)" --ignore-not-found >/dev/null 2>&1 || true
delete_repository_scans_by_name_prefix "${DEMO_SECURITY_SCAN_PREFIX}"

log "Cleaning up kontxt demo resources"
kubectl delete sa,jobs -n "${DEMO_KONTXT_NAMESPACE:-default}" \
  -l 'orka.ai/demo in (kontxt)' --ignore-not-found >/dev/null 2>&1 || true
kubectl delete tasks   -n "${DEMO_NAMESPACE}" \
  -l 'orka.ai/demo in (kontxt)' --ignore-not-found >/dev/null 2>&1 || true

log "Cleaning up agent-sandbox demo resources"
kubectl delete tasks,sandboxclaims -n "${DEMO_NAMESPACE}" \
  -l 'orka.ai/demo in (sandbox)' --ignore-not-found >/dev/null 2>&1 || true
kubectl delete agents -n "${DEMO_NAMESPACE}" \
  -l 'orka.ai/demo in (sandbox)' --ignore-not-found >/dev/null 2>&1 || true
# Session claims are named orka-session-<sha256> by the worker and don't
# carry the orka.ai/demo label. Delete only the deterministic claim for
# this demo session; other session claims may belong to active workspaces.
sandbox_claim_namespace="${DEMO_SANDBOX_CLAIM_NAMESPACE:-${DEMO_NAMESPACE}}"
sandbox_session="${DEMO_SANDBOX_SESSION:-vekil-metrics-77}"
stale_claim="$(sandbox_session_claim_name "${sandbox_session}")"
kubectl delete sandboxclaim -n "${sandbox_claim_namespace}" "${stale_claim}" \
  --ignore-not-found >/dev/null 2>&1 || true

# Demo 70 (Agent Substrate) normally runs on its OWN kind cluster, torn down
# via `make demo-substrate-down`. This selector-scoped delete only matters if
# the demo was pointed at a shared cluster; Substrate Actors are reaped by the
# controller when the owning Tasks are removed.
log "Cleaning up Agent Substrate demo resources"
substrate_namespace="${DEMO_SUBSTRATE_NAMESPACE:-default}"
kubectl delete tasks -n "${substrate_namespace}" \
  -l 'orka.ai/demo in (substrate)' --ignore-not-found >/dev/null 2>&1 || true
kubectl delete agents -n "${substrate_namespace}" \
  -l 'orka.ai/demo in (substrate)' --ignore-not-found >/dev/null 2>&1 || true

prepare_api_env >/dev/null 2>&1 || true
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

printf '%s\n' "Demo resource cleanup finished."
printf '%s\n' "Security scan history may remain in SQLite; use a fresh DEMO_SECURITY_SCAN_NAME if you need a clean history view."
