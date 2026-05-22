#!/usr/bin/env bash
# Demo 60 — Agent Sandbox (session reuse across turns)
#
# Three Tasks share a single SandboxClaim through sessionRef. Turn 1 is the
# scout (read-only). Turn 2 is the builder (file write + git push + gh PR).
# Turn 3 is a CI fixup that reattaches the same workspace. The payoff card
# hard-asserts that all three turns landed on the SAME claim name.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log()).
# ---------------------------------------------------------------------------
require_demo_base
require_pr_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

scout_agent="${DEMO_SANDBOX_SCOUT_AGENT}"
builder_agent="${DEMO_SANDBOX_BUILDER_AGENT}"
session="${DEMO_SANDBOX_SESSION}"
t1="${DEMO_SANDBOX_TURN1_TASK}"
t2="${DEMO_SANDBOX_TURN2_TASK}"
t3="${DEMO_SANDBOX_TURN3_TASK}"

prompts_dir="${script_dir}/prompts"

sandbox_session_claim_name() {
  local session_name="$1"
  local claim_namespace="${DEMO_SANDBOX_CLAIM_NAMESPACE:-${DEMO_NAMESPACE}}"
  local task_namespace="${DEMO_NAMESPACE}"
  local template_namespace="${DEMO_SANDBOX_TEMPLATE_NAMESPACE:-${DEMO_NAMESPACE}}"
  local digest

  if command -v shasum >/dev/null 2>&1; then
    digest="$(
      printf '%s\0%s\0%s\0%s\0%s' \
        "${claim_namespace}" \
        "${task_namespace}" \
        "${template_namespace}" \
        "${DEMO_SANDBOX_TEMPLATE_REF}" \
        "${session_name}" \
        | shasum -a 256 | awk '{print $1}'
    )"
  elif command -v sha256sum >/dev/null 2>&1; then
    digest="$(
      printf '%s\0%s\0%s\0%s\0%s' \
        "${claim_namespace}" \
        "${task_namespace}" \
        "${template_namespace}" \
        "${DEMO_SANDBOX_TEMPLATE_REF}" \
        "${session_name}" \
        | sha256sum | awk '{print $1}'
    )"
  else
    die "shasum or sha256sum is required to compute the demo SandboxClaim name"
  fi

  printf 'orka-session-%.32s\n' "${digest}"
}

render_sandbox_scout_agent      > "${DEMO_WORKDIR}/sandbox-scout-agent.yaml"
render_sandbox_builder_agent    > "${DEMO_WORKDIR}/sandbox-builder-agent.yaml"
render_sandbox_turn_task "${t1}" "${scout_agent}"   "${prompts_dir}/sandbox-turn-1-scout.txt"   --create-session > "${DEMO_WORKDIR}/sandbox-turn-1.yaml"
render_sandbox_turn_task "${t2}" "${builder_agent}" "${prompts_dir}/sandbox-turn-2-builder.txt"                  > "${DEMO_WORKDIR}/sandbox-turn-2.yaml"
render_sandbox_turn_task "${t3}" "${builder_agent}" "${prompts_dir}/sandbox-turn-3-fixup.txt"                    > "${DEMO_WORKDIR}/sandbox-turn-3.yaml"

log "Resetting any prior sandbox session for ${session}"
delete_task_if_exists "${t1}"
delete_task_if_exists "${t2}"
delete_task_if_exists "${t3}"
sandbox_claim_namespace="${DEMO_SANDBOX_CLAIM_NAMESPACE:-${DEMO_NAMESPACE}}"
kubectl delete sandboxclaims -n "${sandbox_claim_namespace}" \
  -l "orka.ai/session=${session}" --ignore-not-found >/dev/null 2>&1 || true
# Session claims are named orka-session-<sha256> by the worker and do NOT
# carry the orka.ai/session label, so the label-selector delete above misses
# it. Delete only the deterministic claim for this demo session; other
# session claims may belong to active workspaces.
stale_claim="$(sandbox_session_claim_name "${session}")"
kubectl delete sandboxclaim -n "${sandbox_claim_namespace}" "${stale_claim}" \
  --ignore-not-found >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7
clear
banner "Agent Sandbox — session reuse"

# Chapter 1 ------------------------------------------------------------------
narrate "Two agents with different toolsets share a workspace across turns."
chapter "Apply the scout + builder Agents" "🤝"
log_info "Session: ${session}  ·  Sandbox template: ${DEMO_SANDBOX_TEMPLATE_REF}"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-scout-agent.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-builder-agent.yaml"

# Chapter 2 ------------------------------------------------------------------
narrate "Turn 1 creates the session — sessionRef.create=true."
chapter "Turn 1: scout the repo (read-only)" "🔎"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-1.yaml"
log_info "Waiting for scout to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t1}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t1}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 1 succeeded; SandboxClaim created"
demo_pe "kubectl get sandboxclaims -n ${sandbox_claim_namespace} -l orka.ai/session=${session}"

# Chapter 3 ------------------------------------------------------------------
narrate "Turn 2 reattaches the existing claim — sessionRef.create=false."
chapter "Turn 2: builder implements + opens PR" "🛠️"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-2.yaml"
log_info "Waiting for builder to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t2}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t2}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 2 succeeded; PR opened"

# Chapter 4 ------------------------------------------------------------------
narrate "Turn 3 reuses the same workspace — branch is still checked out."
chapter "Turn 3: CI fixup on the same branch" "🩹"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-3.yaml"
log_info "Waiting for fixup turn to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t3}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t3}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 3 succeeded"

# Chapter 5 ------------------------------------------------------------------
narrate "All three turns landed on Succeeded — Orka stitched the workspace."
chapter "Session Tasks at a glance" "🪵"
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/session=${session} -L orka.ai/session"

# Chapter 6 ------------------------------------------------------------------
narrate "PR URL from turn 2's result; verify it's a real GitHub pull request."
chapter "The pull request" "🚢"
assert_real_pr_result "${t2}"

# Chapter 7 ------------------------------------------------------------------
narrate "Hard assertion: same claim across all three turns or the demo fails."
chapter "Session reuse summary" "📦"
payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${t2}" sandbox-builder
fi
