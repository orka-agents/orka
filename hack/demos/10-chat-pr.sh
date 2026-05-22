#!/usr/bin/env bash
# Demo 10 — Chat to PR
#
# One chat turn through Orka's Anthropic-compatible endpoint becomes a
# coordinator Task, specialist child Tasks, validation, review, CI, and a
# real GitHub pull request.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).
# Set DEMO_REQUEST_PRESET=quiet-flag|readme-fix|vekil-metrics to pick the
# chat request body (default: quiet-flag — short, real, fits on screen).
#
# Run live:        ./hack/demos/10-chat-pr.sh
# Record (asciinema):
#   asciinema rec --idle-time-limit 1.5 --cols 110 --rows 30 \
#     -c "DEMO_RECORD_PROFILE=docs ./hack/demos/10-chat-pr.sh" /tmp/10.cast

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — these print via the legacy log() to stderr so the cast
# only captures the narrated chapters below).
# ---------------------------------------------------------------------------
require_demo_base
require_pr_demo_env
require_chat_client
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

render_pr_agents_manifest > "${DEMO_WORKDIR}/pr-agents.yaml"
render_chat_request_file  > "${DEMO_WORKDIR}/chat-request.txt"
render_chat_story_file    > "${DEMO_WORKDIR}/chat-story.txt"

delete_chat_session_tasks
delete_agent_if_exists "${DEMO_PR_COORDINATOR_NAME}"
delete_agent_if_exists "${DEMO_CODER_AGENT_NAME}"
delete_agent_if_exists "${DEMO_SECURITY_REVIEWER_NAME}"
delete_agent_if_exists "${DEMO_QUALITY_REVIEWER_NAME}"
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=6
clear
banner "Chat to PR"

# Chapter 1 ------------------------------------------------------------------
narrate "One chat turn becomes a coordinator, specialists, review, CI, PR."
chapter "A maintainer asks for one repo change" "🧑"
log_info "Connecting to $(demo_anthropic_base_url) as $(demo_anthropic_model)"
log_info "Client: ${DEMO_CLAUDE_BIN} (${DEMO_CHAT_CLIENT})"
log_info "Request preset: ${DEMO_REQUEST_PRESET}"
demo_show "${DEMO_WORKDIR}/chat-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "The same wire format your Claude client already speaks."
chapter "Send the request through Orka's Anthropic API" "📨"
export ANTHROPIC_BASE_URL="$(demo_anthropic_base_url)"
export ANTHROPIC_MODEL="$(demo_anthropic_model)"
require_orka_api_reachable
log_success "Orka Anthropic API reachable at ${ANTHROPIC_BASE_URL}"
DEMO_CHAT_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
log_info  "Sending chat request to ${ANTHROPIC_MODEL} via ${DEMO_CHAT_CLIENT}..."
run_demo_chat_request_file "${DEMO_WORKDIR}/chat-request.txt" "${DEMO_WORKDIR}/chat-client-result.json"
log_success "Chat request accepted; coordinator Task will appear shortly"

# Chapter 3 ------------------------------------------------------------------
narrate "The chat turn creates a real coordinator Task in Kubernetes."
chapter "Orka spawns the coordinator" "🎬"
log_info "Watching for the coordinator task to appear..."
DEMO_CHAT_PARENT_TASK="$(wait_for_chat_parent_task "${DEMO_CHAT_PARENT_TIMEOUT:-120}" "${DEMO_CHAT_STARTED_AT}")" \
  || die "failed to discover the Anthropic-proxy-created coordinator task"
log_success "coordinator task: ${DEMO_CHAT_PARENT_TASK}"

# Chapter 4 ------------------------------------------------------------------
narrate "Four named Agents created via create_agent: coder, two reviewers, coordinator."
chapter "Watch the coordinator delegate" "🪄"
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/source=anthropic-proxy"
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} ${DEMO_PR_COORDINATOR_NAME} ${DEMO_CODER_AGENT_NAME} ${DEMO_SECURITY_REVIEWER_NAME} ${DEMO_QUALITY_REVIEWER_NAME}"

# Chapter 5 ------------------------------------------------------------------
narrate "Implementation, validation, parallel review, CI — silently, in the background."
chapter "Coordinator runs to completion" "⏳"
log_info "Waiting for the coordinator to finish (timeout ${DEMO_CHAT_TASK_TIMEOUT:-10800}s)..."
wait_for_task_succeeded            "${DEMO_CHAT_PARENT_TASK}" "${DEMO_CHAT_TASK_TIMEOUT:-10800}" >/dev/null
wait_for_task_result_available     "${DEMO_CHAT_PARENT_TASK}" "${DEMO_CHAT_RESULT_TIMEOUT:-120}"  >/dev/null
log_success "coordinator succeeded"

# Chapter 6 ------------------------------------------------------------------
narrate "Real PR. Real CI. Real review. Reproducible from one chat turn."
chapter "The pull request" "🚢"
assert_real_pr_result "${DEMO_CHAT_PARENT_TASK}"
payoff_card_pr        "${DEMO_CHAT_PARENT_TASK}"

# Presenter only: keep the structured JSON for the audit-trail audience.
if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${DEMO_CHAT_PARENT_TASK}" chat-to-pr
fi
