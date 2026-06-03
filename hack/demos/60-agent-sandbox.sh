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
render_sandbox_story_file       > "${DEMO_WORKDIR}/sandbox-story.txt"

demo_scenario "Agent Sandbox — multi-turn workspace reuse" \
  "Three sequential agent Tasks reattach to ONE workspace via sessionRef. The first turn (a Scout agent) clones the repo, explores it, and writes a plan into the workspace. Turns 2 and 3 (a Builder agent) reattach the same SandboxClaim and pick up the live state — git checkout, dep cache, runtime, planning notes. Heavy setup cost is paid once; subsequent turns start hot."

demo_event "🧹" "Clearing any prior sandbox session ${session} so this run starts clean…"
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
# Per-turn status hook — narrates pod/claim transitions during the long
# (5-10 min) agent run waits. Reset between turns via demo_announce_reset.
# ---------------------------------------------------------------------------
_sandbox_turn_status() {
  local task_name="$1"
  local elapsed="$2"
  local phase job_name pod_phase claim
  phase="$(kubectl get task "${task_name}" -n "${DEMO_NAMESPACE}" \
    -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  job_name="$(kubectl get task "${task_name}" -n "${DEMO_NAMESPACE}" \
    -o jsonpath='{.status.jobName}' 2>/dev/null || true)"
  if [[ -n "${job_name}" ]]; then
    pod_phase="$(kubectl get pods -n "${DEMO_NAMESPACE}" -l "job-name=${job_name}" \
      --sort-by=.metadata.creationTimestamp \
      -o jsonpath='{.items[-1:].status.phase}' 2>/dev/null || true)"
  fi
  claim="$(kubectl get sandboxclaims -n "${sandbox_claim_namespace}" \
    -l "orka.ai/session=${session}" --sort-by=.metadata.creationTimestamp \
    -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"

  [[ -n "${job_name}" ]] && demo_announce_once "turn-${task_name}-job" \
    "🛠️ " "Job created for ${task_name} — pod scheduling on the cluster"
  [[ "${pod_phase}" == "Running" ]] && demo_announce_once "turn-${task_name}-pod" \
    "🏃" "Pod running — agent loop bootstrapping (clone or reattach, then tool calls)"
  [[ -n "${claim}" ]] && demo_announce_once "turn-${task_name}-claim" \
    "📦" "SandboxClaim attached: ${claim} (this is the workspace state — git checkout, deps, runtime)"

  __demo_heartbeat 'turn/%s phase=%s pod=%s elapsed=%ss' \
    "${task_name}" "${phase:-Pending}" "${pod_phase:-Pending}" "${elapsed}"
}

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=8

# Chapter 1 ------------------------------------------------------------------
narrate "Multiple agent Tasks reattach ONE workspace via sessionRef — no cold start per turn. Heavy state (git checkout, dep cache, runtime) stays warm across calls."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/sandbox-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "Two agents with different toolsets share a workspace across turns."
chapter "Apply the scout + builder Agents" "🤝"
demo_event "📥" "Two Agent CRs (different tool allowlists) and a session name. The session is just a label — the workspace it points to is created on demand by the first Task that asks for it."
log_info "Session: ${session}  ·  Sandbox template: ${DEMO_SANDBOX_TEMPLATE_REF}"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-scout-agent.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-builder-agent.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "Turn 1 creates the session — sessionRef.create=true."
chapter "Turn 1: scout the repo (read-only)" "🔎"
demo_event "1️⃣ " "Turn 1 = scout. sessionRef.create=true tells Orka 'mint a new SandboxClaim for this session'. The claim survives the Task that creates it."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-1.yaml"
demo_announce_reset "turn-${t1}-"
DEMO_WAIT_STATUS_HOOK=_sandbox_turn_status \
  wait_for_task_succeeded         "${t1}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t1}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "✅" "Turn 1 succeeded. SandboxClaim is now bound and warm — git checkout, deps cached, runtime booted. Cost: paid once."
demo_pe "kubectl get sandboxclaims -n ${sandbox_claim_namespace} -l orka.ai/session=${session}"

# Chapter 4 ------------------------------------------------------------------
narrate "Turn 2 reattaches the existing claim — sessionRef.create=false."
chapter "Turn 2: builder implements + opens PR" "🛠️"
demo_event "2️⃣ " "Turn 2 = builder. sessionRef.create=false means 'attach to the EXISTING claim'. Same git checkout, same deps, same warm runtime. Cold-start cost: zero."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-2.yaml"
demo_announce_reset "turn-${t2}-"
DEMO_WAIT_STATUS_HOOK=_sandbox_turn_status \
  wait_for_task_succeeded         "${t2}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t2}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "🚀" "Turn 2 succeeded — file written, branch pushed, PR opened. Watch chapter 6 for the PR URL."

# Chapter 5 ------------------------------------------------------------------
narrate "Turn 3 reuses the same workspace — branch is still checked out."
chapter "Turn 3: CI fixup on the same branch" "🩹"
demo_event "3️⃣ " "Turn 3 = CI fixup. The branch opened in turn 2 is STILL CHECKED OUT in the workspace — no re-clone, no re-checkout, no context loss."
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/sandbox-turn-3.yaml"
demo_announce_reset "turn-${t3}-"
DEMO_WAIT_STATUS_HOOK=_sandbox_turn_status \
  wait_for_task_succeeded         "${t3}" "${DEMO_SANDBOX_TURN_TIMEOUT:-1800}" >/dev/null
wait_for_task_result_available  "${t3}" "${DEMO_SANDBOX_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "✅" "Turn 3 succeeded. Three Tasks, one workspace, zero cold starts beyond the first."

# Chapter 6 ------------------------------------------------------------------
narrate "All three turns landed on Succeeded — Orka stitched the workspace."
chapter "Session Tasks at a glance" "🪵"
demo_event "📊" "All three Tasks carry orka.ai/session=${session} — that's how Orka finds the claim. Standard label-selector mechanics."
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/session=${session} -L orka.ai/session"

# Chapter 7 ------------------------------------------------------------------
narrate "PR URL from turn 2's result; verify it's a real GitHub pull request."
chapter "The pull request" "🚢"
demo_event "🔗" "PR URL comes from turn 2's structured result — same /result endpoint as every other Task."
assert_real_pr_result "${t2}"

# Chapter 8 ------------------------------------------------------------------
narrate "Hard assertion: same claim across all three turns or the demo fails."
chapter "Session reuse summary" "📦"
demo_event "🔍" "The payoff card extracts the claim name from each turn's worker logs and ASSERTS they all match. Demo fails if not — proves session reuse end-to-end."
payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${t2}" sandbox-builder
fi
