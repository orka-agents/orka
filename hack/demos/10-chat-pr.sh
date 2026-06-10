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
# Demo 10's Anthropic-compatible endpoint has no query-string namespace when
# driven through the Claude CLI, so mint the bearer token from the demo namespace
# where the Provider lives unless the caller deliberately chose another token
# namespace.
if [[ "${ORKA_TOKEN_NAMESPACE:-default}" == "default" && "${DEMO_NAMESPACE}" != "default" ]]; then
  export ORKA_TOKEN_NAMESPACE="${DEMO_NAMESPACE}"
fi
prepare_api_env

render_chat_request_file  > "${DEMO_WORKDIR}/chat-request.txt"
render_chat_story_file    > "${DEMO_WORKDIR}/chat-story.txt"

demo_scenario "Chat → GitHub PR — Kubernetes is the AI runtime" \
  "One chat turn → real GitHub pull request. The coordinator spins up Agents and Tasks as Kubernetes Pods. No CI plugins, no glue scripts."

demo_event "🧹" "Clearing any prior chat session, Agents, and Tasks so this run starts clean…"

# The chat turn carries no agent specs; the server-side coordinator system
# prompt instructs the model to create_agent itself. Whatever Agents linger
# from earlier runs are cleaned by name patterns (delete_chat_session_tasks
# also removes proxy-* Tasks created by this session).
delete_chat_session_tasks
delete_demo_chat_agents_if_present
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

# Pick the best Opus model the cluster will accept. Caller can pin a
# specific model via DEMO_CHAT_MODEL (full <provider>/<model> form) to
# skip discovery. Otherwise we read the provider catalog from
# `/anthropic/v1/models` and select the first preferred Opus the cluster
# actually offers. This keeps the demo self-tuning to whatever upstream
# catalog the cluster's Provider sees.
#
# IMPORTANT: do NOT probe by POSTing to `/anthropic/v1/messages`. That
# endpoint is not a passthrough — it runs Orka's full agentic coordinator
# (large system prompt + multi-iteration tool loop), so a single "ping"
# takes 60-120s to return. A short curl timeout there always aborts with
# HTTP 000 and mis-reports "no Opus model accepted" even though the model
# is healthy. The models catalog is a cheap GET that returns instantly.
pick_chat_opus_model() {
  if [[ -n "${DEMO_CHAT_MODEL:-}" ]]; then
    printf '%s\n' "${DEMO_CHAT_MODEL}"
    return 0
  fi
  if [[ -n "${DEMO_CLAUDE_MODEL:-}" ]]; then
    printf '%s\n' "${DEMO_CLAUDE_MODEL}"
    return 0
  fi
  local provider="${DEMO_PROVIDER_REF}"
  local token catalog candidate model_id fallback_model
  token="$(get_orka_token)"
  # Fetch the offered model IDs once (provider-qualified and bare forms).
  catalog="$(curl -sS -m 15 \
    -H "Authorization: Bearer ${token}" \
    "${ORKA_API_BASE%/}/anthropic/v1/models" 2>/dev/null \
    | jq -r '.data[].id' 2>/dev/null || true)"
  for candidate in claude-opus-4.8 claude-opus-4.7 claude-opus-4.6; do
    model_id="${provider}/${candidate}"
    # Accept either the provider-qualified id or the bare candidate.
    if grep -qx "${model_id}" <<<"${catalog}" 2>/dev/null; then
      printf '%s\n' "${model_id}"
      return 0
    fi
    if grep -qx "${candidate}" <<<"${catalog}" 2>/dev/null; then
      printf '%s\n' "${model_id}"
      return 0
    fi
  done
  fallback_model="$(demo_anthropic_model)"
  if [[ -n "${fallback_model}" ]]; then
    printf '%s\n' "${fallback_model}"
    return 0
  fi
  return 1
}

# The model discovery below reads the live Orka API catalog, so the API
# must be reachable first. prepare_api_env only mints the token; the
# port-forward is established by require_orka_api_reachable. Without this
# the catalog GET fires against a not-yet-bound :8080 and finds no models.
require_orka_api_reachable

DEMO_CHAT_OPUS_MODEL="$(pick_chat_opus_model || true)"
if [[ -z "${DEMO_CHAT_OPUS_MODEL}" ]]; then
  die "no chat model configured and no preferred Opus model offered by ${ORKA_API_BASE}/anthropic/v1/models — looked for claude-opus-4.8, claude-opus-4.7, claude-opus-4.6. Set DEMO_CHAT_MODEL or DEMO_CLAUDE_MODEL to override."
fi
# Make the chosen model the one demo_anthropic_model emits.
export DEMO_CLAUDE_MODEL="${DEMO_CHAT_OPUS_MODEL}"

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=6

# Coordinator status hook — replaces the generic "phase=X children=N"
# heartbeat with a chat-aware breakdown of child Tasks by phase, plus
# persistent demo_event announcements for major milestones.
_chat_coordinator_status() {
  local parent="$1"
  local elapsed="$2"
  # Everything here must go to stderr. The wait helpers capture the task phase
  # via `phase="$(wait_for_task_terminal ...)"`, and this hook runs inside that
  # same command substitution. demo_announce_once routes through demo_event,
  # which prints to STDOUT — so if an announce fires on the same tick the task
  # reaches a terminal phase, its line is captured into the phase string and
  # corrupts the "== Succeeded" check, failing an otherwise-green run. Force
  # stderr (mirrors the _sandbox_turn_status fix in 60-agent-sandbox.sh).
  {
    local phase counts children_count latest_child latest_phase
    phase="$(kubectl get task "${parent}" -n "${DEMO_NAMESPACE}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    # Child Task phase histogram via one kubectl call.
    counts="$(kubectl --request-timeout=3s get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/source=anthropic-proxy" --no-headers 2>/dev/null \
      | awk '{p=$3; if(p=="")p="Pending"; c[p]++}
             END { out=""; for(p in c){if(out!="")out=out" "; out=out p"="c[p]}
                   if(out=="")out="(none yet)"; print out }')"
    children_count="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/source=anthropic-proxy" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    latest_child="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/source=anthropic-proxy" --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
    latest_phase="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/source=anthropic-proxy" --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1:].status.phase}' 2>/dev/null || true)"

    (( children_count >= 1 )) && demo_announce_once "chat-first-child" \
      "👶" "Coordinator created its first specialist child Task — agentic fan-out has started"
    (( children_count >= 3 )) && demo_announce_once "chat-fanout" \
      "🌳" "Coordinator has now spawned ${children_count}+ specialist Tasks (implement, test, review, CI…)"

    __demo_heartbeat 'coordinator/%s phase=%s children=%s [%s] latest=%s/%s elapsed=%ss' \
      "${parent}" "${phase:-Pending}" "${children_count}" "${counts}" \
      "${latest_child:-—}" "${latest_phase:-—}" "${elapsed}"
  } 1>&2
}

# Chapter 1 ------------------------------------------------------------------
narrate "Orka speaks the Anthropic Messages protocol. One chat turn from any Claude-compatible client drives a full agentic SDLC — coordinator, specialists, review, CI, real PR."
chapter "A maintainer asks for one repo change" "🧑"
log_info "Connecting to $(demo_anthropic_base_url)"
log_info "Client: ${DEMO_CLAUDE_BIN} (${DEMO_CHAT_CLIENT})"
log_info "Request preset: ${DEMO_REQUEST_PRESET}"
demo_show_full "${DEMO_WORKDIR}/chat-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "Discover available models, pick an Opus, then send the request as Claude."
chapter "Send the request through Orka's Anthropic API" "📨"
demo_event "🛰️ " "Same /v1/messages endpoint Claude clients already use. Orka is API-compatible — drop-in for any Anthropic tool."
export ANTHROPIC_BASE_URL="$(demo_anthropic_base_url)"
export ANTHROPIC_MODEL="$(demo_anthropic_model)"
require_orka_api_reachable
log_success "Orka Anthropic API reachable at ${ANTHROPIC_BASE_URL}"
log_info "Provider-default models exposed by Orka (/anthropic/v1/models):"
demo_pe "curl -sS -H \"Authorization: Bearer \$(get_orka_token)\" ${ANTHROPIC_BASE_URL}/v1/models | jq -r '.data[].id'"
log_info "Selected chat model: ${DEMO_CHAT_OPUS_MODEL} (Orka passes the model name through to ${DEMO_PROVIDER_REF})"
DEMO_CHAT_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
log_info "Prompt sent to claude -p:"
demo_show "${DEMO_WORKDIR}/chat-request.txt"
demo_show_cmd "ANTHROPIC_BASE_URL=${ANTHROPIC_BASE_URL} ANTHROPIC_API_KEY=\$(get_orka_token) ${DEMO_CLAUDE_BIN} -p --model ${DEMO_CHAT_OPUS_MODEL} < ${DEMO_WORKDIR}/chat-request.txt"
demo_event "▶️ " "Running the chat turn — claude-code tool-calls through Orka, which turns those calls into Kubernetes Task objects."
# Background heartbeat so viewers see something during the model's quiet
# multi-turn tool dance. Ticks the elapsed spinner every 10s (only when
# stderr is a tty so log scrapers stay clean) and, every ~60s, prints
# a richer one-line snapshot of the child tasks the coordinator has
# scheduled so far — visible even when stderr is redirected, so audit
# logs preserve actual progress milestones.
__demo_chat_heartbeat() {
  local started="${SECONDS}"
  local last_snapshot=0
  local tick=0
  local elapsed
  while sleep 10; do
    tick=$((tick + 1))
    elapsed=$((SECONDS - started))
    if [[ -t 2 ]]; then
      printf '\r\033[2K%b[%s] ⏳  chat turn in flight (tool round-trips)... elapsed=%ds%b' \
        "${DIM}" "$(__demo_log_ts)" "${elapsed}" "${COLOR_RESET}" >&2
    fi
    # Every ~60s, append a milestone line showing child-task progress.
    # Use a short kubectl timeout so a slow API server doesn't stall the
    # heartbeat. Output via a real newline so prior milestones stay on
    # screen, then the spinner resumes on the next line.
    if (( elapsed - last_snapshot >= 60 )); then
      last_snapshot=${elapsed}
      local counts
      counts="$(kubectl --request-timeout=3s get tasks -n "${DEMO_NAMESPACE}" \
        -l orka.ai/source=anthropic-proxy --no-headers 2>/dev/null \
        | awk '{phase=$3; if(phase=="")phase="Pending"; c[phase]++}
               END { out=""; for(p in c){if(out!="")out=out" "; out=out p"="c[p]}
                     if(out=="")out="(no child tasks yet)"; print out }' )"
      [[ -t 2 ]] && printf '\r\033[2K' >&2
      printf '%b[%s] 🪄  coordinator progress: %s%b\n' \
        "${DIM}" "$(__demo_log_ts)" "${counts}" "${COLOR_RESET}" >&2
    fi
  done
}
__demo_chat_heartbeat &
__DEMO_CHAT_HB_PID=$!
trap 'demo_run_exit_cleanups; kill "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true; [[ -t 2 ]] && printf "\r\033[2K" >&2 || true' EXIT
run_demo_chat_request_file "${DEMO_WORKDIR}/chat-request.txt" "${DEMO_WORKDIR}/chat-client-result.json"
kill "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true
wait "${__DEMO_CHAT_HB_PID}" 2>/dev/null || true
trap - EXIT
[[ -t 2 ]] && printf '\r\033[2K' >&2 || true
demo_event "📬" "Chat HTTP turn returned. The coordinator Task is already running on the cluster."

# Chapter 3 ------------------------------------------------------------------
narrate "The chat turn creates a real coordinator Task in Kubernetes."
chapter "Orka spawns the coordinator" "🎬"
demo_event "🔭" "Looking up the Task that the chat session minted (via orka.ai/source=anthropic-proxy label + creation timestamp)."
DEMO_CHAT_PARENT_TASK="$(wait_for_chat_parent_task "${DEMO_CHAT_PARENT_TIMEOUT:-120}" "${DEMO_CHAT_STARTED_AT}")" \
  || die "failed to discover the Anthropic-proxy-created coordinator task"
demo_event "✅" "Coordinator Task discovered: ${DEMO_CHAT_PARENT_TASK} — the K8s representation of the chat session's parent agent."

# Chapter 4 ------------------------------------------------------------------
narrate "The coordinator invents its own Agents via create_agent. Names vary per run."
chapter "Watch the coordinator delegate" "🪄"
demo_event "🧩" "The coordinator uses create_agent + create_task to fan out work. Specialist Agents are minted on demand — no static workflow YAML."
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/source=anthropic-proxy --sort-by=.metadata.creationTimestamp"
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} -l orka.ai/created-by=chat"

# Chapter 5 ------------------------------------------------------------------
narrate "Implementation, validation, parallel review, CI — silently, in the background."
chapter "Coordinator runs to completion" "⏳"
demo_event "⏱️ " "Waiting for the coordinator to drive all specialist Tasks to Succeeded."
DEMO_WAIT_STATUS_HOOK=_chat_coordinator_status \
  wait_for_task_succeeded            "${DEMO_CHAT_PARENT_TASK}" "${DEMO_CHAT_TASK_TIMEOUT:-10800}" >/dev/null
wait_for_task_result_available     "${DEMO_CHAT_PARENT_TASK}" "${DEMO_CHAT_RESULT_TIMEOUT:-120}"  >/dev/null
demo_event "🏁" "Coordinator succeeded — all specialist Tasks finished, PR is in the result payload."

# Chapter 6 ------------------------------------------------------------------
narrate "Real PR. Real CI. Real review. Reproducible from one chat turn."
chapter "The pull request" "🚢"
demo_event "🔗" "PR URL extracted from the coordinator's structured result. assert_real_pr_result validates it's a real GitHub PR."
assert_real_pr_result "${DEMO_CHAT_PARENT_TASK}"
payoff_card_pr        "${DEMO_CHAT_PARENT_TASK}"

# Presenter only: keep the structured JSON for the audit-trail audience.
if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${DEMO_CHAT_PARENT_TASK}" chat-to-pr
fi
