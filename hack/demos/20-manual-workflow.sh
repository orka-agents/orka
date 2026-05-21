#!/usr/bin/env bash
# Demo 20 — Manual Workflow
#
# A declarative YAML Task drives the same coordinator/coder/reviewer pipeline
# as the chat demo. Same agents, same review gates, same PR — just a kubectl
# apply instead of an Anthropic chat turn.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log() so prep doesn't bleed into the cast).
# ---------------------------------------------------------------------------
require_demo_base
require_pr_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

render_pr_agents_manifest    > "${DEMO_WORKDIR}/pr-agents.yaml"
render_manual_task_manifest  > "${DEMO_WORKDIR}/manual-task.yaml"
render_manual_story_file     > "${DEMO_WORKDIR}/manual-story.txt"

delete_task_if_exists "${DEMO_MANUAL_TASK_NAME}"

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=5
clear
banner "Manual Workflow"

# Chapter 1 ------------------------------------------------------------------
narrate "Declarative YAML drives the same coordinator + specialists as chat."
chapter "Apply the named Agents" "📜"
log_info "Request preset: ${DEMO_REQUEST_PRESET}"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} ${DEMO_PR_COORDINATOR_NAME} ${DEMO_CODER_AGENT_NAME} ${DEMO_SECURITY_REVIEWER_NAME} ${DEMO_QUALITY_REVIEWER_NAME}"

# Chapter 2 ------------------------------------------------------------------
narrate "The Task manifest is the source of truth — same prompt as the chat path."
chapter "Inspect the Task manifest" "📄"
demo_show "${DEMO_WORKDIR}/manual-story.txt"
log_info "Full Task manifest: ${DEMO_WORKDIR}/manual-task.yaml"
demo_show "${DEMO_WORKDIR}/manual-task.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "kubectl apply creates the coordinator Task — Orka reconciles it."
chapter "Create the coordinator Task" "🚀"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/manual-task.yaml"
demo_pe "kubectl get task ${DEMO_MANUAL_TASK_NAME} -n ${DEMO_NAMESPACE}"

# Chapter 4 ------------------------------------------------------------------
narrate "Implementation, validation, parallel review, CI — silently, in the background."
chapter "Coordinator runs to completion" "⏳"
demo_pe "require_orka_api_reachable"
log_info "Waiting for the coordinator to finish (timeout ${DEMO_MANUAL_TASK_TIMEOUT:-10800}s)..."
wait_for_task_succeeded            "${DEMO_MANUAL_TASK_NAME}" "${DEMO_MANUAL_TASK_TIMEOUT:-10800}" >/dev/null
wait_for_task_result_available     "${DEMO_MANUAL_TASK_NAME}" "${DEMO_MANUAL_RESULT_TIMEOUT:-120}"  >/dev/null
log_success "coordinator succeeded"
demo_pe "orka_api GET \"/api/v1/tasks/${DEMO_MANUAL_TASK_NAME}/children?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({name: .metadata.name, agent: .spec.agentRef.name, phase: .status.phase})'"

# Chapter 5 ------------------------------------------------------------------
narrate "Same PR. Same agents. Just YAML."
chapter "The pull request" "🚢"
assert_real_pr_result "${DEMO_MANUAL_TASK_NAME}"
payoff_card_pr        "${DEMO_MANUAL_TASK_NAME}"

# Presenter only: keep the structured JSON for the audit-trail audience.
if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${DEMO_MANUAL_TASK_NAME}" manual-yaml-workflow
fi
