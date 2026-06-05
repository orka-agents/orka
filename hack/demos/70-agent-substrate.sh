#!/usr/bin/env bash
# Demo 70 — Agent Substrate (gVisor execution workspaces)
#
# Orka's workspace executor is provider-neutral. Demo 60 backed agent Tasks
# with agent-sandbox; this demo backs the SAME Task API with Agent Substrate —
# a second provider that runs each workspace as a gVisor-isolated Actor drawn
# from a pre-warmed WorkerPool, and snapshots/suspends it between turns.
#
# Three beats:
#   1. Lifecycle  — cleanupPolicy: delete; fresh Actor claimed, run, deleted.
#   2. Retained   — cleanupPolicy: retain + reusePolicy: session; workspace
#                   stays warm (phase Retained).
#   3. Reuse      — a second Task with the same sessionRef reattaches the warm
#                   workspace (status.executionWorkspace.reused == true).
# The payoff card asserts provider=substrate on all three and reused=true on
# the reuse beat.
#
# The agent runtime CLI is stubbed to /bin/true (CODEX_CLI_PATH), so this demo
# is model-free and deterministic — it exercises the WORKSPACE lifecycle, not
# model output. The Substrate control plane + WorkerPool + ActorTemplate must
# already be installed (hack/demos/cluster/install-substrate.sh).
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Pin the demo namespace BEFORE sourcing the libs, and make it AUTHORITATIVE.
# Demo 70 runs on the dedicated Substrate kind cluster, where
# install-substrate.sh placed the Agent/Tasks (and the bootstrap-token secret)
# in DEMO_SUBSTRATE_NAMESPACE (default "default"). The shared wait_for_task_*
# helpers key off DEMO_NAMESPACE while the render_substrate_* renderers key off
# DEMO_SUBSTRATE_NAMESPACE — if those diverge, tasks are created in one
# namespace and polled in another, so the demo hangs until timeout. We
# therefore FORCE DEMO_NAMESPACE to the substrate namespace, overriding any
# value inherited from the shell (e.g. "demo-magic" left over from demos
# 50/60, which doesn't exist on the Substrate cluster). Override the namespace
# via DEMO_SUBSTRATE_NAMESPACE, not DEMO_NAMESPACE.
export DEMO_SUBSTRATE_NAMESPACE="${DEMO_SUBSTRATE_NAMESPACE:-default}"
export DEMO_NAMESPACE="${DEMO_SUBSTRATE_NAMESPACE}"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log()).
# ---------------------------------------------------------------------------
require_demo_base
require_substrate_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

agent="${DEMO_SUBSTRATE_AGENT}"
substrate_ns="${DEMO_NAMESPACE}"
lifecycle_task="${DEMO_SUBSTRATE_LIFECYCLE_TASK}"
retain_task="${DEMO_SUBSTRATE_RETAIN_TASK}"
reuse_task="${DEMO_SUBSTRATE_REUSE_TASK}"
template_ns="${DEMO_SUBSTRATE_TEMPLATE_NAMESPACE}"
template_name="${DEMO_SUBSTRATE_TEMPLATE_NAME}"

render_substrate_agent                                          > "${DEMO_WORKDIR}/substrate-agent.yaml"
render_substrate_task "${lifecycle_task}" delete none           > "${DEMO_WORKDIR}/substrate-lifecycle.yaml"
render_substrate_task "${retain_task}"    retain  session true  > "${DEMO_WORKDIR}/substrate-retain.yaml"
render_substrate_task "${reuse_task}"     delete  session false > "${DEMO_WORKDIR}/substrate-reuse.yaml"
render_substrate_story_file                                    > "${DEMO_WORKDIR}/substrate-story.txt"

demo_scenario "Agent Substrate — gVisor workspaces, one Task API" \
  "The same Orka Task that ran on agent-sandbox in Demo 60 now runs on Agent Substrate: a gVisor-isolated Actor from a warm WorkerPool. Switch one field — provider: substrate — and keep the entire workflow."

demo_event "🧹" "Clearing any prior demo Tasks so this run starts clean…"
delete_task_if_exists "${lifecycle_task}"
delete_task_if_exists "${retain_task}"
delete_task_if_exists "${reuse_task}"

# ---------------------------------------------------------------------------
# Per-task status hook — narrates Task/workspace transitions during waits.
#
# IMPORTANT: everything here must go to stderr. The wait helpers capture the
# task phase via `phase="$(wait_for_task_terminal ...)"`, and this hook runs
# inside that same command substitution. demo_announce_once routes through
# demo_event, which prints to STDOUT — so if an announce fires on the same tick
# the task reaches a terminal phase (common for fast Substrate workspaces that
# claim + complete in ~10s), its line would be captured into the phase string
# and corrupt the "== Succeeded" check, failing the demo after beat 1. We force
# the whole hook body to stderr to keep the narration visible without leaking
# into the phase capture.
# ---------------------------------------------------------------------------
_substrate_task_status() {
  local task_name="$1"
  local elapsed="$2"
  {
    local phase ws_phase ws_provider
    phase="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    ws_phase="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.executionWorkspace.phase}' 2>/dev/null || true)"
    ws_provider="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.executionWorkspace.provider}' 2>/dev/null || true)"

    [[ "${ws_provider}" == "substrate" ]] && demo_announce_once "ws-${task_name}-claim" \
      "📦" "Substrate workspace claimed — a gVisor Actor from the WorkerPool is now this Task's sandbox"
    [[ "${ws_phase}" == "Retained" ]] && demo_announce_once "ws-${task_name}-retained" \
      "🔥" "Workspace retained warm — its snapshot survives for the next sessionRef to reattach"

    __demo_heartbeat 'task/%s phase=%s workspace=%s elapsed=%ss' \
      "${task_name}" "${phase:-Pending}" "${ws_phase:-Pending}" "${elapsed}"
  } 1>&2
}

# _substrate_recap "<what just happened>" "<why it matters>"
# A reflective beat after each fast (~10s) workspace operation, so a viewer
# isn't outrun by how quickly Substrate claims/reuses a workspace. demo_phase
# is profile-aware (suppressed in hero, and in social past chapter 3); the
# short pause only happens in presenter so non-interactive casts stay fast.
_substrate_recap() {
  local what="$1"
  local why="$2"
  demo_phase "🧩" "What just happened" "${what}"
  demo_event "💡" "Why it matters: ${why}"
  if demo_profile_is presenter; then
    sleep "${DEMO_SUBSTRATE_BEAT_PAUSE:-3}"
  fi
}

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7

# Chapter 1 ------------------------------------------------------------------
narrate "Orka's workspace executor is provider-neutral. The Task you saw on agent-sandbox runs unchanged on Substrate — a gVisor Actor from a warm pool — by switching one field."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/substrate-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "One model-free Agent. The runtime CLI is stubbed so the focus is the workspace, not model output."
chapter "Apply the Agent" "🤖"
demo_event "📥" "A single Agent CR. CODEX_CLI_PATH=/bin/true makes the run deterministic — this demo is about the execution substrate."
log_info "Template: ${template_ns}/${template_name}  (Substrate ActorTemplate, gVisor runtime)"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-agent.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "Beat 1 — lifecycle. provider: substrate claims a fresh gVisor Actor; cleanupPolicy: delete tears it down after."
chapter "Beat 1: workspace lifecycle" "♻️"
demo_event "1️⃣ " "Task with execution.workspace.provider=substrate, cleanupPolicy=delete. Claims an Actor, runs, deletes the workspace."
demo_show "${DEMO_WORKDIR}/substrate-lifecycle.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-lifecycle.yaml"
demo_announce_reset "ws-${lifecycle_task}-"
DEMO_WAIT_STATUS_HOOK=_substrate_task_status \
  wait_for_task_succeeded        "${lifecycle_task}" "${DEMO_SUBSTRATE_TASK_TIMEOUT:-600}" >/dev/null
wait_for_task_result_available "${lifecycle_task}" "${DEMO_SUBSTRATE_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "✅" "Beat 1 done. provider=substrate, workspace phase=Deleted — gVisor Actor claimed, ran, cleaned up."
demo_pe "kubectl get task ${lifecycle_task} -n ${substrate_ns} -o jsonpath='{.status.executionWorkspace.provider}{\"  phase=\"}{.status.executionWorkspace.phase}{\"\n\"}'"
_substrate_recap \
  "Orka claimed a gVisor-isolated Actor from the Substrate WorkerPool, ran the task inside it, then deleted the workspace — because cleanupPolicy was 'delete'." \
  "This is the same Orka Task API as Demo 60's agent-sandbox; only the provider field changed. Orka hides the execution substrate behind one contract."

# Chapter 4 ------------------------------------------------------------------
narrate "Beat 2 — retain a warm workspace. cleanupPolicy: retain + reusePolicy: session keeps the Actor and snapshots it."
chapter "Beat 2: retain a warm workspace" "🔥"
demo_event "2️⃣ " "cleanupPolicy=retain + reusePolicy=session + sessionRef. The workspace is NOT deleted — it stays warm for reuse."
demo_show "${DEMO_WORKDIR}/substrate-retain.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-retain.yaml"
demo_announce_reset "ws-${retain_task}-"
DEMO_WAIT_STATUS_HOOK=_substrate_task_status \
  wait_for_task_succeeded        "${retain_task}" "${DEMO_SUBSTRATE_TASK_TIMEOUT:-600}" >/dev/null
wait_for_task_result_available "${retain_task}" "${DEMO_SUBSTRATE_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "🔥" "Beat 2 done. workspace phase=Retained — the gVisor Actor + its snapshot survive for the next sessionRef."
_substrate_recap \
  "This task asked for cleanupPolicy=retain + reusePolicy=session and CREATED the session '${DEMO_SUBSTRATE_SESSION}'. The workspace was NOT torn down — it is held warm, snapshotted to the Substrate snapshot store." \
  "Tearing a workspace down and rebuilding it per task wastes the expensive setup (clone, deps, runtime boot). Retaining it warm lets the next task in the session skip all of that."

# Chapter 5 ------------------------------------------------------------------
narrate "Beat 3 — reuse. A second Task with the SAME sessionRef reattaches the warm workspace instead of cold-starting."
chapter "Beat 3: reuse the warm workspace" "⚡"
demo_event "3️⃣ " "Same sessionRef=${DEMO_SUBSTRATE_SESSION}. Orka reattaches the retained Actor — reused=true, no new cold start."
demo_show "${DEMO_WORKDIR}/substrate-reuse.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-reuse.yaml"
demo_announce_reset "ws-${reuse_task}-"
DEMO_WAIT_STATUS_HOOK=_substrate_task_status \
  wait_for_task_succeeded        "${reuse_task}" "${DEMO_SUBSTRATE_TASK_TIMEOUT:-600}" >/dev/null
wait_for_task_result_available "${reuse_task}" "${DEMO_SUBSTRATE_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "⚡" "Beat 3 done. status.executionWorkspace.reused=true — the warm workspace was reattached, not rebuilt."
_substrate_recap \
  "This task named the SAME session '${DEMO_SUBSTRATE_SESSION}' with create=false. Instead of cold-starting a new Actor, Orka reattached the workspace Beat 2 left warm — status.executionWorkspace.reused=true proves it." \
  "Same Orka Task contract, gVisor isolation, AND warm-state reuse across tasks — no cold start tax. That is the whole point of a pluggable execution substrate."

# Chapter 6 ------------------------------------------------------------------
narrate "All three Tasks at a glance — same provider, the reuse Task carries reused=true."
chapter "Substrate Tasks at a glance" "🪵"
demo_event "📊" "Every Task ran on provider=substrate. The Orka Task API is identical to Demo 60 — only the backend changed."
demo_pe "kubectl get tasks -n ${substrate_ns} -l demo.orka.ai/scenario=substrate -o custom-columns=TASK:.metadata.name,PHASE:.status.phase,WS:.status.executionWorkspace.provider,WSPHASE:.status.executionWorkspace.phase,REUSED:.status.executionWorkspace.reused"

# Chapter 7 ------------------------------------------------------------------
narrate "Hard assertion: provider=substrate on all three, and the reuse Task reattached the warm workspace."
chapter "Substrate workspace summary" "📦"
demo_event "🔍" "Payoff card reads each Task's status.executionWorkspace and ASSERTS provider=substrate + reused=true. Demo fails if not."
payoff_card_substrate "${lifecycle_task}" "${retain_task}" "${reuse_task}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON (Tasks)%b\n' "${DIM}" "${COLOR_RESET}"
  kubectl get tasks -n "${substrate_ns}" \
    "${lifecycle_task}" "${retain_task}" "${reuse_task}" \
    -o json | jq '.items | map({task: .metadata.name, phase: .status.phase, workspace: .status.executionWorkspace})'
fi
