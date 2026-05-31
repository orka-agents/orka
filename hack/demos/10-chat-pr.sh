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

render_chat_request_file  > "${DEMO_WORKDIR}/chat-request.txt"
render_chat_story_file    > "${DEMO_WORKDIR}/chat-story.txt"

# The chat turn carries no agent specs; the server-side coordinator system
# prompt instructs the model to create_agent itself. Whatever Agents linger
# from earlier runs are cleaned by name patterns (delete_chat_session_tasks
# also removes proxy-* Tasks created by this session).
delete_chat_session_tasks
delete_demo_chat_agents_if_present
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

# Pick the best Opus model the cluster will accept. Caller can pin a
# specific model via DEMO_CHAT_MODEL (full <provider>/<model> form) to
# skip discovery. Otherwise we ping `/anthropic/v1/messages` with each
# candidate in preferred order — first 2xx wins. This keeps the demo
# self-tuning to whatever upstream catalog the cluster's Provider sees.
pick_chat_opus_model() {
  if [[ -n "${DEMO_CHAT_MODEL:-}" ]]; then
    printf '%s\n' "${DEMO_CHAT_MODEL}"
    return 0
  fi
  local provider="${DEMO_PROVIDER_REF}"
  local token candidate model_id http_code body_file
  token="$(get_orka_token)"
  body_file="$(mktemp)"
  trap 'rm -f "${body_file}"' RETURN
  for candidate in claude-opus-4.7 claude-opus-4.6; do
    model_id="${provider}/${candidate}"
    http_code="$(curl -sS -m 15 -o "${body_file}" -w '%{http_code}' \
      -X POST "${ORKA_API_BASE%/}/anthropic/v1/messages" \
      -H "Authorization: Bearer ${token}" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"${model_id}\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}]}" \
      2>/dev/null || printf '000')"
    if [[ "${http_code}" =~ ^2 ]]; then
      printf '%s\n' "${model_id}"
      return 0
    fi
  done
  return 1
}

DEMO_CHAT_OPUS_MODEL="$(pick_chat_opus_model || true)"
if [[ -z "${DEMO_CHAT_OPUS_MODEL}" ]]; then
  die "no Opus model accepted by ${ORKA_API_BASE}/anthropic — tried claude-opus-4.7, claude-opus-4.6. Set DEMO_CHAT_MODEL=<provider>/<model> to override."
fi
# Make the chosen model the one demo_anthropic_model emits.
export DEMO_CLAUDE_MODEL="${DEMO_CHAT_OPUS_MODEL}"

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=6
clear
banner "Chat to PR"

# Chapter 1 ------------------------------------------------------------------
narrate "One chat turn becomes a coordinator, specialists, review, CI, PR."
chapter "A maintainer asks for one repo change" "🧑"
log_info "Connecting to $(demo_anthropic_base_url)"
log_info "Client: ${DEMO_CLAUDE_BIN} (${DEMO_CHAT_CLIENT})"
log_info "Request preset: ${DEMO_REQUEST_PRESET}"
demo_show "${DEMO_WORKDIR}/chat-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "Discover available models, pick an Opus, then send the request as Claude."
chapter "Send the request through Orka's Anthropic API" "📨"
export ANTHROPIC_BASE_URL="$(demo_anthropic_base_url)"
export ANTHROPIC_MODEL="$(demo_anthropic_model)"
require_orka_api_reachable
log_success "Orka Anthropic API reachable at ${ANTHROPIC_BASE_URL}"
log_info "Provider-default models exposed by Orka (/anthropic/v1/models):"
demo_pe "curl -sS -H \"Authorization: Bearer \$(get_orka_token)\" ${ANTHROPIC_BASE_URL}/v1/models | jq -r '.data[].id'"
log_info "Selected Opus model: ${DEMO_CHAT_OPUS_MODEL} (Orka passes the model name through to ${DEMO_PROVIDER_REF})"
DEMO_CHAT_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Show the prompt that `claude -p` will receive on stdin, then the exact
# command itself. demo_show handles profile-correct verbosity (full body
# in presenter, head -20 in docs, head -8 in social, path-only in hero)
# so audit-mode viewers see the real ask while social cuts stay short.
log_info "Prompt sent to claude -p (from ${DEMO_WORKDIR}/chat-request.txt):"
demo_show "${DEMO_WORKDIR}/chat-request.txt"
# Show the exact `claude -p` command the viewer could run themselves. We
# render it WITHOUT executing (demo_show_cmd) — the real invocation runs
# via run_demo_chat_request_file just below with a sidecar --settings
# file so Claude Code's settings.env doesn't override ANTHROPIC_BASE_URL
# with the user's local proxy. Token is fetched from the shell — never
# inlined into the visible command.
demo_show_cmd "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL} ANTHROPIC_API_KEY=\$(get_orka_token) ${DEMO_CLAUDE_BIN} -p --model ${DEMO_CHAT_OPUS_MODEL} < ${DEMO_WORKDIR}/chat-request.txt"
log_info "Running the actual chat turn (output captured to ${DEMO_WORKDIR}/chat-client-result.json)..."
# Background heartbeat so viewers see something during the model's quiet
# multi-turn tool dance. Ticks every 10s, only when stderr is a tty so
# log scrapers stay clean. We tear it down whether the call succeeds or
# not — `trap` covers the SIGTERM/exit path.
__demo_chat_heartbeat() {
  local started="${SECONDS}"
  while sleep 10; do
    if [[ -t 2 ]]; then
      printf '\r\033[2K%b[%s] ⏳  chat turn in flight (tool round-trips)... elapsed=%ss%b' \
        "${DIM}" "$(__demo_log_ts)" "$((SECONDS - started))" "${COLOR_RESET}" >&2
    fi
  done
}
__demo_chat_heartbeat &
__DEMO_CHAT_HB_PID=$!
trap 'kill "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true; [[ -t 2 ]] && printf "\r\033[2K" >&2 || true' EXIT
run_demo_chat_request_file "${DEMO_WORKDIR}/chat-request.txt" "${DEMO_WORKDIR}/chat-client-result.json"
kill "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true
wait "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true
trap - EXIT
[[ -t 2 ]] && printf '\r\033[2K' >&2 || true
log_success "Chat request accepted; coordinator Task will appear shortly"

# Chapter 3 ------------------------------------------------------------------
narrate "The chat turn creates a real coordinator Task in Kubernetes."
chapter "Orka spawns the coordinator" "🎬"
log_info "Watching for the coordinator task to appear..."
DEMO_CHAT_PARENT_TASK="$(wait_for_chat_parent_task "${DEMO_CHAT_PARENT_TIMEOUT:-120}" "${DEMO_CHAT_STARTED_AT}")" \
  || die "failed to discover the Anthropic-proxy-created coordinator task"
log_success "coordinator task: ${DEMO_CHAT_PARENT_TASK}"

# Chapter 4 ------------------------------------------------------------------
narrate "The coordinator invents its own Agents via create_agent. Names vary per run."
chapter "Watch the coordinator delegate" "🪄"
# The chat-driven coordinator lives in the chat session itself (no single
# Kubernetes Task represents it). Show the child Tasks the chat created
# during this run, plus the Agents it spun up.
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/source=anthropic-proxy --sort-by=.metadata.creationTimestamp"
# Whatever Agents the coordinator created in this run carry the chat label.
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} -l orka.ai/created-by=chat"

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
