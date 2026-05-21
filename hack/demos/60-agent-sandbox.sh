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

render_sandbox_scout_agent      > "${DEMO_WORKDIR}/sandbox-scout-agent.yaml"
render_sandbox_builder_agent    > "${DEMO_WORKDIR}/sandbox-builder-agent.yaml"
render_sandbox_turn_task "${t1}" "${scout_agent}"   "${prompts_dir}/sandbox-turn-1-scout.txt"   --create-session > "${DEMO_WORKDIR}/sandbox-turn-1.yaml"
render_sandbox_turn_task "${t2}" "${builder_agent}" "${prompts_dir}/sandbox-turn-2-builder.txt"                  > "${DEMO_WORKDIR}/sandbox-turn-2.yaml"
render_sandbox_turn_task "${t3}" "${builder_agent}" "${prompts_dir}/sandbox-turn-3-fixup.txt"                    > "${DEMO_WORKDIR}/sandbox-turn-3.yaml"

log "Resetting any prior sandbox session for ${session}"
delete_task_if_exists "${t1}"
delete_task_if_exists "${t2}"
delete_task_if_exists "${t3}"
kubectl delete sandboxclaims -n "${DEMO_NAMESPACE}" \
  -l "orka.ai/session=${session}" --ignore-not-found >/dev/null 2>&1 || true
# Session claims are named orka-session-<sha256> by the worker (see
# workers/common/agent_runtime.go:625) and do NOT carry the orka.ai/session
# label, so the label-selector delete above misses them. Sweep any leftover
# session claims here so turn 1 starts with a clean workspace.
for stale_claim in $(kubectl get sandboxclaims -n "${DEMO_NAMESPACE}" \
    -o jsonpath='{range .items[?(@.metadata.name)]}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | grep -E '^orka-session-' || true); do
  kubectl delete sandboxclaim -n "${DEMO_NAMESPACE}" "${stale_claim}" \
    --ignore-not-found >/dev/null 2>&1 || true
done

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7
clear
banner "Agent Sandbox — session reuse"

# Chapter 1 ------------------------------------------------------------------
narrate "Two agents with different toolsets share a workspace across turns."
chapter "Apply the scout + builder Agents" "🤝"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-scout-agent.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-builder-agent.yaml"
demo_pe "kubectl get agents -n ${DEMO_NAMESPACE} ${scout_agent} ${builder_agent}"

# Chapter 2 ------------------------------------------------------------------
narrate "Turn 1 creates the session — sessionRef.create=true."
chapter "Turn 1: scout the repo" "🔎"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-1.yaml"
log_info "Waiting for scout to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t1}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t1}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 1 succeeded"
demo_pe "kubectl get sandboxclaims -n ${DEMO_NAMESPACE} -l orka.ai/session=${session}"

# Chapter 3 ------------------------------------------------------------------
narrate "Turn 2 reattaches the existing claim — sessionRef.create=false."
chapter "Turn 2: builder implements + opens PR" "🛠️"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-2.yaml"
log_info "Waiting for builder to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t2}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t2}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 2 succeeded"

# Chapter 4 ------------------------------------------------------------------
narrate "Turn 3 reuses the same workspace — branch is still checked out."
chapter "Turn 3: CI fixup on the same branch" "🩹"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-3.yaml"
log_info "Waiting for fixup turn to finish (timeout ${DEMO_SANDBOX_TURN_TIMEOUT:-1800}s)..."
wait_for_task_succeeded         "${t3}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t3}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
log_success "turn 3 succeeded"

# Chapter 5 ------------------------------------------------------------------
narrate "Worker logs print the claim name — same string for all three turns."
chapter "Worker log evidence" "🪵"
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/session=${session} -o jsonpath='{range .items[*]}{.metadata.name}{\"\\t\"}{.status.phase}{\"\\n\"}{end}'"

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
