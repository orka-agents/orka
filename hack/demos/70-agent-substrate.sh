#!/usr/bin/env bash
# Demo 70 — Agent Substrate (a real agent in a gVisor workspace)
#
# Orka's workspace executor is provider-neutral. Demo 60 backed agent Tasks
# with agent-sandbox; this demo backs the SAME Task API with Agent Substrate —
# a second provider that runs each workspace as a gVisor-isolated Actor drawn
# from a pre-warmed WorkerPool, and keeps it warm between turns.
#
# Two beats, both with a REAL model-driven codex agent:
#   COLD — fresh gVisor workspace: the agent clones the repo, makes a change,
#          stops. Orka pushes the branch; the demo opens a real pull request.
#   WARM — a second Task with the same sessionRef reattaches the RETAINED
#          workspace (reused=true) for a follow-up change — no cold start. The
#          PR is updated with the second commit.
#
# Clean-exit contract: the agent EDITS FILES ONLY and stops; Orka's pushBranch
# pushes the branch; THIS SCRIPT opens/updates the PR via gh. (If the agent ran
# post-edit commands itself, a nonzero one could make codex exit 1 even though
# the work succeeded.) The Task sets ORKA_CODEX_DISABLE_SANDBOX=true because
# gVisor is the sandbox — codex's inner bubblewrap cannot nest under runsc.
#
# Prerequisites (hack/demos/cluster/install-substrate.sh): the Substrate control
# plane, a WorkerPool + ActorTemplate on a codex-capable image, an in-cluster
# model proxy (vekil), and the model + git Secrets.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Pin the demo namespace BEFORE sourcing the libs, and make it AUTHORITATIVE.
# Demo 70 runs on the dedicated Substrate kind cluster. The shared
# wait_for_task_* helpers key off DEMO_NAMESPACE while the render_substrate_*
# renderers key off DEMO_SUBSTRATE_NAMESPACE — if those diverge, tasks are
# created in one namespace and polled in another, so the demo hangs. We force
# DEMO_NAMESPACE to the substrate namespace, overriding any value inherited
# from the shell (e.g. "demo-magic" left over from demos 50/60). Override the
# namespace via DEMO_SUBSTRATE_NAMESPACE, not DEMO_NAMESPACE.
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
# The PR step uses gh; fail early if it is missing rather than mid-demo.
require_cmd gh

agent="${DEMO_SUBSTRATE_AGENT}"
substrate_ns="${DEMO_NAMESPACE}"
cold_task="${DEMO_SUBSTRATE_COLD_TASK}"
warm_task="${DEMO_SUBSTRATE_WARM_TASK}"
template_ns="${DEMO_SUBSTRATE_TEMPLATE_NAMESPACE}"
template_name="${DEMO_SUBSTRATE_TEMPLATE_NAME}"
# Substrate retains the session's gVisor Actor (named deterministically from
# the session coordinates) after a run, and Actors are gRPC-only — there is no
# kubectl resource to delete them in cleanup. If two runs share a session name,
# the SECOND run's "cold" beat would reattach the first run's retained Actor and
# report reused=true — breaking the cold/warm story. So unless the caller pins
# DEMO_SUBSTRATE_SESSION explicitly, give each run a unique session id. The
# session only needs to be stable WITHIN a run (cold creates, warm reattaches).
if [[ -z "${DEMO_SUBSTRATE_SESSION_PINNED:-}" ]]; then
  export DEMO_SUBSTRATE_SESSION="${DEMO_SUBSTRATE_SESSION}-$(date +%s)"
fi
session="${DEMO_SUBSTRATE_SESSION}"
pr_repo="${DEMO_SUBSTRATE_PR_REPO}"
push_branch="${DEMO_SUBSTRATE_PUSH_BRANCH}"
base_branch="${DEMO_SUBSTRATE_GIT_BASE_BRANCH}"
model="${DEMO_SUBSTRATE_RUNTIME_MODEL}"

# The cold beat creates a fresh README marker; the warm beat appends a second
# line. The agent edits only — Orka pushes, this script PRs.
cold_prompt="    Append exactly this line to the end of README.md and make no other change:
    \"<!-- orka substrate demo: cold gVisor workspace -->\"
    Then stop. Do not run git, do not open a PR — Orka handles the push."
warm_prompt="    The repo is already checked out from the previous turn (warm workspace).
    Append exactly this line to the end of README.md and make no other change:
    \"<!-- orka substrate demo: warm reuse, no cold start -->\"
    Then stop. Do not run git, do not open a PR — Orka handles the push."

render_substrate_agent                                       > "${DEMO_WORKDIR}/substrate-agent.yaml"
render_substrate_task "${cold_task}" session true  "${cold_prompt}" > "${DEMO_WORKDIR}/substrate-cold.yaml"
render_substrate_task "${warm_task}" session false "${warm_prompt}" > "${DEMO_WORKDIR}/substrate-warm.yaml"
render_substrate_story_file                                  > "${DEMO_WORKDIR}/substrate-story.txt"

demo_scenario "Agent Substrate — a real agent in a gVisor workspace" \
  "A live ${model} agent clones ${pr_repo}, makes a change inside a gVisor-isolated Actor, and opens a real PR. Then a second task reuses the warm workspace — no cold start. Same Orka Task API as Demo 60; only the provider changed."

demo_event "🧹" "Clearing any prior demo Tasks + stale demo branch so this run starts clean…"
delete_task_if_exists "${cold_task}"
delete_task_if_exists "${warm_task}"
# Close any previous demo PR and delete the branch so the cold beat opens fresh.
__prev_pr="$(gh pr list --repo "${pr_repo}" --head "${push_branch}" --state open \
  --json number --jq '.[0].number' 2>/dev/null || true)"
if [[ -n "${__prev_pr}" ]]; then
  gh pr close "${__prev_pr}" --repo "${pr_repo}" --delete-branch >/dev/null 2>&1 || true
else
  git ls-remote --exit-code --heads "https://github.com/${pr_repo}.git" "${push_branch}" >/dev/null 2>&1 \
    && gh api -X DELETE "repos/${pr_repo}/git/refs/heads/${push_branch}" >/dev/null 2>&1 || true
fi

# ---------------------------------------------------------------------------
# Per-task status hook — narrates Task/workspace transitions during waits.
#
# IMPORTANT: everything here goes to stderr. The wait helpers capture the task
# phase via `phase="$(wait_for_task_terminal ...)"`, and this hook runs inside
# that same command substitution. demo_announce_once routes through demo_event
# (STDOUT), so without the stderr wrap an announce firing on the terminal tick
# would corrupt the captured phase. Force the whole hook body to stderr.
# ---------------------------------------------------------------------------
_substrate_task_status() {
  local task_name="$1"
  local elapsed="$2"
  {
    local phase ws_phase ws_provider ws_reused
    phase="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    ws_phase="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.executionWorkspace.phase}' 2>/dev/null || true)"
    ws_provider="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.executionWorkspace.provider}' 2>/dev/null || true)"
    ws_reused="$(kubectl get task "${task_name}" -n "${substrate_ns}" \
      -o jsonpath='{.status.executionWorkspace.reused}' 2>/dev/null || true)"

    [[ "${ws_provider}" == "substrate" ]] && demo_announce_once "ws-${task_name}-claim" \
      "📦" "Substrate workspace claimed — a gVisor Actor is now this agent's sandbox (reaching the model proxy + github from inside runsc)"
    [[ "${ws_reused}" == "true" ]] && demo_announce_once "ws-${task_name}-reused" \
      "⚡" "Warm workspace reattached (reused=true) — repo already cloned, no cold start"

    __demo_heartbeat 'task/%s phase=%s workspace=%s elapsed=%ss (live model run)' \
      "${task_name}" "${phase:-Pending}" "${ws_phase:-Pending}" "${elapsed}"
  } 1>&2
}

# _substrate_recap "<what just happened>" "<why it matters>" — reflective beat.
_substrate_recap() {
  local what="$1"
  local why="$2"
  demo_phase "🧩" "What just happened" "${what}"
  demo_event "💡" "Why it matters: ${why}"
  if demo_profile_is presenter; then
    sleep "${DEMO_SUBSTRATE_BEAT_PAUSE:-3}"
  fi
}

# open_or_update_demo_pr — open the PR after the cold beat (branch already
# pushed by Orka), or report the existing PR after the warm beat. Prints the URL.
open_or_update_demo_pr() {
  local existing url
  existing="$(gh pr list --repo "${pr_repo}" --head "${push_branch}" --state open \
    --json url --jq '.[0].url' 2>/dev/null || true)"
  if [[ -n "${existing}" ]]; then
    printf '%s\n' "${existing}"
    return 0
  fi
  url="$(gh pr create --repo "${pr_repo}" --head "${push_branch}" --base "${base_branch}" \
    --title "Orka Agent Substrate demo" \
    --body "Opened from the Orka Agent Substrate demo. A ${model} codex agent made this change inside a gVisor-isolated Substrate workspace; Orka pushed the branch; the demo opened this PR." \
    2>/dev/null || true)"
  printf '%s\n' "${url}"
}

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7

# Chapter 1 ------------------------------------------------------------------
narrate "Orka's workspace executor is provider-neutral. The agent Task you saw on agent-sandbox runs unchanged on Substrate — a gVisor Actor from a warm pool — by switching one field."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/substrate-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "One real codex Agent. Its model endpoint is an in-cluster proxy; the agent edits files inside the gVisor workspace."
chapter "Apply the Agent" "🤖"
demo_event "📥" "A codex-runtime Agent (model ${model}). secretRef carries the in-cluster model endpoint — the agent runs a real model from inside the sandbox."
log_info "Template: ${template_ns}/${template_name}  (gVisor ActorTemplate)   Repo: ${pr_repo}"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-agent.yaml"

# Chapter 3 ------------------------------------------------------------------
narrate "Beat 1 (COLD) — a fresh gVisor workspace. The agent clones the repo, makes a change, and stops; Orka pushes the branch."
chapter "Beat 1: cold agentic run" "🧊"
demo_event "1️⃣ " "provider=substrate, a fresh Actor. The agent clones ${pr_repo}, edits a file with a live model, then stops. cleanupPolicy=retain keeps the workspace warm."
demo_show "${DEMO_WORKDIR}/substrate-cold.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-cold.yaml"
demo_announce_reset "ws-${cold_task}-"
demo_event "⏳" "Real model run — this takes a couple of minutes (clone, model reasoning, edit, push)."
DEMO_WAIT_STATUS_HOOK=_substrate_task_status \
  wait_for_task_succeeded        "${cold_task}" "${DEMO_SUBSTRATE_TASK_TIMEOUT:-900}" >/dev/null
wait_for_task_result_available "${cold_task}" "${DEMO_SUBSTRATE_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "✅" "Cold beat done — agent edited the file inside gVisor; Orka pushed branch ${push_branch}."

# Chapter 4 ------------------------------------------------------------------
narrate "Orka pushed the branch; the demo opens the pull request."
chapter "Open the pull request" "🚢"
demo_event "🔗" "The agent edited ONLY (clean exit); Orka pushed; now the demo opens the PR via gh."
cold_pr_url="$(open_or_update_demo_pr)"
if [[ -n "${cold_pr_url}" ]]; then
  demo_event "🎉" "Real pull request opened: ${cold_pr_url}"
else
  log_warning "Could not resolve a PR URL for ${push_branch} on ${pr_repo}"
fi
_substrate_recap \
  "A real ${model} agent, running inside a gVisor Actor, cloned ${pr_repo} and made a change. It reached the model proxy AND github.com from inside runsc. Orka pushed the branch; the demo opened the PR." \
  "This is the same Orka agent Task contract as Demo 60 — only execution.workspace.provider changed. The agent gets gVisor isolation for free."

# Chapter 5 ------------------------------------------------------------------
narrate "Beat 2 (WARM) — a second Task with the same sessionRef reattaches the retained workspace. Repo already cloned, no cold start."
chapter "Beat 2: warm reuse" "⚡"
demo_event "2️⃣ " "Same sessionRef=${session}, create=false. Orka reattaches the warm Actor — reused=true. The repo is already there; the agent skips the clone."
demo_show "${DEMO_WORKDIR}/substrate-warm.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/substrate-warm.yaml"
demo_announce_reset "ws-${warm_task}-"
DEMO_WAIT_STATUS_HOOK=_substrate_task_status \
  wait_for_task_succeeded        "${warm_task}" "${DEMO_SUBSTRATE_TASK_TIMEOUT:-900}" >/dev/null
wait_for_task_result_available "${warm_task}" "${DEMO_SUBSTRATE_RESULT_TIMEOUT:-120}" >/dev/null
demo_event "⚡" "Warm beat done — status.executionWorkspace.reused=true. Orka pushed the follow-up commit to the same branch."
warm_pr_url="$(open_or_update_demo_pr)"
[[ -n "${warm_pr_url}" ]] && demo_event "🔗" "PR updated with the warm-reuse commit: ${warm_pr_url}"
_substrate_recap \
  "The second Task named the SAME session with create=false. Orka reattached the workspace the cold beat left warm — reused=true — so the agent skipped cloning and started hot." \
  "Same Task contract, gVisor isolation, AND warm-state reuse across tasks — no cold-start tax. That is the point of a pluggable execution substrate."

# Chapter 6 ------------------------------------------------------------------
narrate "Both Tasks at a glance — same provider, the warm Task carries reused=true."
chapter "Substrate Tasks at a glance" "🪵"
demo_event "📊" "Both Tasks ran on provider=substrate with a real model. The Orka agent Task API is identical to Demo 60 — only the backend changed."
demo_pe "kubectl get tasks -n ${substrate_ns} -l demo.orka.ai/scenario=substrate -o custom-columns=TASK:.metadata.name,PHASE:.status.phase,WS:.status.executionWorkspace.provider,WSPHASE:.status.executionWorkspace.phase,REUSED:.status.executionWorkspace.reused"

# Chapter 7 ------------------------------------------------------------------
narrate "Hard assertion: provider=substrate on both, the warm Task reused the workspace, and a real PR exists."
chapter "Substrate run summary" "📦"
demo_event "🔍" "Payoff card asserts provider=substrate + reused=true and shows the real PR URL. Demo fails if not."
payoff_card_substrate "${cold_task}" "${warm_task}" "${warm_pr_url:-${cold_pr_url:-}}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON (Tasks)%b\n' "${DIM}" "${COLOR_RESET}"
  kubectl get tasks -n "${substrate_ns}" \
    "${cold_task}" "${warm_task}" \
    -o json | jq '.items | map({task: .metadata.name, phase: .status.phase, workspace: .status.executionWorkspace})'
fi
