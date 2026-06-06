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

demo_scenario "Declarative agentic workflow — Tasks as Kubernetes CRDs" \
  "Same end-to-end workflow as demo 10, but as a Kubernetes Task CRD instead of a chat turn. The coordinator fans out to specialist child Tasks. GitOps-shaped, replayable, auditable."

demo_event "🧹" "Clearing any prior Task with the same name so this run starts clean…"
delete_task_if_exists "${DEMO_MANUAL_TASK_NAME}"

# Coordinator status hook — child Task phase breakdown so the long wait
# isn't a black box.
_manual_coordinator_status() {
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
    counts="$(kubectl --request-timeout=3s get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/parent-task=${parent}" --no-headers 2>/dev/null \
      | awk '{p=$3; if(p=="")p="Pending"; c[p]++}
             END { out=""; for(p in c){if(out!="")out=out" "; out=out p"="c[p]}
                   if(out=="")out="(none yet)"; print out }')"
    children_count="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/parent-task=${parent}" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
    latest_child="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/parent-task=${parent}" --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
    latest_phase="$(kubectl get tasks -n "${DEMO_NAMESPACE}" \
      -l "orka.ai/parent-task=${parent}" --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1:].status.phase}' 2>/dev/null || true)"

    (( children_count >= 1 )) && demo_announce_once "manual-first-child" \
      "👶" "Coordinator delegated to its first specialist child Task — agentic fan-out has started"
    (( children_count >= 3 )) && demo_announce_once "manual-fanout" \
      "🌳" "Coordinator has now spawned ${children_count}+ specialist Tasks (implement, test, review, CI…)"

    __demo_heartbeat 'coordinator/%s phase=%s children=%s [%s] latest=%s/%s elapsed=%ss' \
      "${parent}" "${phase:-Pending}" "${children_count}" "${counts}" \
      "${latest_child:-—}" "${latest_phase:-—}" "${elapsed}"
  } 1>&2
}

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=6

# Chapter 1 ------------------------------------------------------------------
narrate "Orka Tasks are Kubernetes CRDs. The same agentic SDLC workflow you saw from chat is here described declaratively — GitOps-shaped, replayable, auditable."
chapter "What this demo is doing" "🧑"
log_info "Request preset: ${DEMO_REQUEST_PRESET}"
demo_show_full "${DEMO_WORKDIR}/manual-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "The coordinator + specialist Agents are pre-baked — applied up front so the Task can reference them by name."
chapter "Apply the named Agents" "📜"
demo_event "📥" "Four named Agent CRs. The Task in chapter 3 references them by name — separates WHO does the work (Agent) from WHAT to do (Task)."
# Pre-clean any prior agents so apply doesn't warn about missing
# last-applied-configuration annotations (they may have been created via
# the chat path earlier and are not annotated).
kubectl delete -n "${DEMO_NAMESPACE}" \
  agents/"${DEMO_PR_COORDINATOR_NAME}" \
  agents/"${DEMO_CODER_AGENT_NAME}" \
  agents/"${DEMO_SECURITY_REVIEWER_NAME}" \
  agents/"${DEMO_QUALITY_REVIEWER_NAME}" \
  --ignore-not-found >/dev/null 2>&1 || true
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} ${DEMO_PR_COORDINATOR_NAME} ${DEMO_CODER_AGENT_NAME} ${DEMO_SECURITY_REVIEWER_NAME} ${DEMO_QUALITY_REVIEWER_NAME}"

# Chapter 3 ------------------------------------------------------------------
narrate "Here's the Task manifest — same prompt the chat demo sent, just as a CR you can commit to git."
chapter "Inspect the Task manifest" "📄"
demo_event "📐" "A real Kubernetes CR — fits any GitOps repo. Same shape as Deployment/CronJob. Argo CD / Flux can drive it."
demo_show "${DEMO_WORKDIR}/manual-task.yaml"

# Chapter 4 ------------------------------------------------------------------
narrate "kubectl apply creates the coordinator Task — Orka reconciles it."
chapter "Create the coordinator Task" "🚀"
demo_event "📤" "kubectl apply — the controller picks it up, schedules a Job, and the agent loop begins."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/manual-task.yaml"
demo_event "🤖" "Coordinator now running. It uses create_task / create_agent to delegate — same primitive as demos 10 + 60."

# Chapter 5 ------------------------------------------------------------------
narrate "Implementation, validation, parallel review, CI — silently, in the background."
chapter "Coordinator runs to completion" "⏳"
require_orka_api_reachable
log_success "Orka API reachable at ${ORKA_API_BASE}"
demo_event "⏱️ " "Waiting for the coordinator to drive all specialist Tasks to Succeeded."
DEMO_WAIT_STATUS_HOOK=_manual_coordinator_status \
  wait_for_task_succeeded            "${DEMO_MANUAL_TASK_NAME}" "${DEMO_MANUAL_TASK_TIMEOUT:-10800}" >/dev/null
wait_for_task_result_available     "${DEMO_MANUAL_TASK_NAME}" "${DEMO_MANUAL_RESULT_TIMEOUT:-120}"  >/dev/null
demo_event "🏁" "Coordinator succeeded — all specialist Tasks finished. The result payload carries the PR URL."
log_info "Child tasks spawned by the coordinator:"
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/parent-task=${DEMO_MANUAL_TASK_NAME}"

# Chapter 6 ------------------------------------------------------------------
narrate "Same PR. Same agents. Just YAML — your existing GitOps stack can now trigger AI workflows."
chapter "The pull request" "🚢"
demo_event "🔗" "PR URL extracted from the coordinator's structured result. assert_real_pr_result validates it's a real GitHub PR."
assert_real_pr_result "${DEMO_MANUAL_TASK_NAME}"
payoff_card_pr        "${DEMO_MANUAL_TASK_NAME}"

# Presenter only: keep the structured JSON for the audit-trail audience.
if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${DEMO_MANUAL_TASK_NAME}" manual-yaml-workflow
fi
