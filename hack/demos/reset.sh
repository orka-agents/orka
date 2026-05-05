#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"

require_cmd kubectl
require_cmd jq

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

prepare_api_env >/dev/null 2>&1 || true
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

printf '%s\n' "Demo resource cleanup finished."
printf '%s\n' "Security scan history may remain in SQLite; use a fresh DEMO_SECURITY_SCAN_NAME if you need a clean history view."
