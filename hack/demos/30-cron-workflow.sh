#!/usr/bin/env bash
# Demo 30 — Scheduled Workflow
#
# A cron-scheduled parent Task spawns recurring child runs. The demo prepares
# at least one completed child off-camera so you can show the end state
# without waiting for the next tick, then narrates the visible pieces.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log() so the pre-warm phase doesn't bleed
# into the cast).
# ---------------------------------------------------------------------------
require_demo_base
require_cron_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

render_cron_agent_manifest > "${DEMO_WORKDIR}/cron-agent.yaml"
render_cron_task_manifest  > "${DEMO_WORKDIR}/cron-task.yaml"
render_cron_story_file     > "${DEMO_WORKDIR}/cron-story.txt"

demo_scenario "Scheduled AI workflow — cron in Kubernetes" \
  "Add a 'schedule:' field to a Task and Orka turns an AI agent into a recurring Kubernetes job. Each tick spawns a fresh child Task owned by the parent — just like CronJob spawns Jobs."

demo_event "📥" "Pre-warming one scheduled child so the presenter view starts with a real result…"
delete_task_if_exists "${DEMO_CRON_TASK_NAME}"
kubectl delete tasks -n "${DEMO_NAMESPACE}" \
  -l "orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true" \
  --ignore-not-found >/dev/null 2>&1 || true
kubectl apply -f "${DEMO_WORKDIR}/cron-agent.yaml" >/dev/null
kubectl apply -f "${DEMO_WORKDIR}/cron-task.yaml"  >/dev/null

DEMO_CRON_CHILD_TASK="$(wait_for_first_scheduled_child "${DEMO_CRON_READY_TIMEOUT:-240}")" \
  || die "timed out waiting for the first scheduled child task"
demo_event "⏰" "First scheduled child landed: ${DEMO_CRON_CHILD_TASK}"
wait_for_task_succeeded        "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_TASK_TIMEOUT:-1200}" \
  || die "scheduled child task did not succeed"
wait_for_task_result_available "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_RESULT_TIMEOUT:-120}" \
  || die "scheduled child task result was not available in time"
demo_event "✅" "Scheduled child ${DEMO_CRON_CHILD_TASK} finished and stored its result"

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=5

# Chapter 1 ------------------------------------------------------------------
narrate "Add a 'schedule:' field to a Task and Orka turns an AI agent into a recurring K8s job — stale-PR triage, every tick."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/cron-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "Same Agent + Task primitive as demos 10 and 20. Adding 'schedule:' turns it from one-shot into recurring — Orka handles the cron mechanics."
chapter "Apply the triage Agent + cron Task" "📅"
demo_event "📥" "Two Kubernetes objects: an Agent and a Task with a cron 'schedule:'. No new infrastructure."
log_info "Schedule: ${DEMO_CRON_SCHEDULE}  (production: use ${DEMO_CRON_PRODUCTION_HINT:-*/30 * * * * or 0 */4 * * *})"
log_info "Peek at the cron Task — note 'schedule:', 'concurrencyPolicy:', and the GH_TOKEN env binding:"
demo_pe "head -25 ${DEMO_WORKDIR}/cron-task.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/cron-agent.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/cron-task.yaml"
demo_event "⏰" "Parent Task is now phase=Scheduled — it stays that way; each tick instantiates a fresh child Task it owns."

# Chapter 3 ------------------------------------------------------------------
narrate "The parent Task stays Scheduled forever — each tick instantiates a fresh child Task. Same model as Kubernetes CronJob spawning Jobs."
chapter "Watch the schedule tick" "👶"
demo_event "🔗" "Parent → child via orka.ai/parent-task label. Children are independent Tasks — same semantics as Jobs under a CronJob."
log_success "first child already completed off-camera: ${DEMO_CRON_CHILD_TASK}"
demo_pe "kubectl get task ${DEMO_CRON_TASK_NAME} -n ${DEMO_NAMESPACE}"
demo_pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true"
demo_event "📊" "Each child carries its own status, result, and audit trail — identical surface to any other Task in the demos."

# Chapter 4 ------------------------------------------------------------------
narrate "Each tick writes a structured result via the same API your interactive demos use — here, a stale-PR triage report ready to drop into Slack."
chapter "Read the triage report" "📋"
demo_event "📄" "Same /result API used by every Task — no special cron-result endpoint. Plumb into Slack / email / a dashboard like any other."
log_info "Markdown report from ${DEMO_CRON_CHILD_TASK}:"
demo_pe "orka_api GET \"/api/v1/tasks/${DEMO_CRON_CHILD_TASK}/result?namespace=${DEMO_NAMESPACE}\" | jq -r '.result | fromjson | .summary'"
demo_event "✅" "Triage report is just a structured payload — JSON the agent emits via the standard result API."

# Chapter 5 ------------------------------------------------------------------
narrate "Anywhere you'd reach for a CronJob today — release notes drafts, CVE scans, weekly digests — you can now schedule an AI agent the same way. Same RBAC, same audit log, same result API."
chapter "Schedule overview" "🚦"
DEMO_CRON_CHILD_TASK="${DEMO_CRON_CHILD_TASK}" payoff_card_cron "${DEMO_CRON_CHILD_TASK}"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_task_run "${DEMO_CRON_CHILD_TASK}" scheduled-pr-triage
fi
