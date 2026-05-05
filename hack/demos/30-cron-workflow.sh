#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

require_demo_base
require_cron_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

render_cron_agent_manifest > "${DEMO_WORKDIR}/cron-agent.yaml"
render_cron_task_manifest > "${DEMO_WORKDIR}/cron-task.yaml"

log "Preparing one scheduled child run before the presenter view; this may take a few minutes"
delete_task_if_exists "${DEMO_CRON_TASK_NAME}"
kubectl delete tasks -n "${DEMO_NAMESPACE}" -l "orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true" --ignore-not-found >/dev/null 2>&1 || true
kubectl apply -f "${DEMO_WORKDIR}/cron-agent.yaml" >/dev/null
kubectl apply -f "${DEMO_WORKDIR}/cron-task.yaml" >/dev/null
DEMO_CRON_CHILD_TASK="$(wait_for_first_scheduled_child "${DEMO_CRON_READY_TIMEOUT:-240}")" || die "timed out waiting for the first scheduled child task"
wait_for_task_succeeded "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_TASK_TIMEOUT:-1200}" || die "scheduled child task did not succeed"
wait_for_task_result_available "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_RESULT_TIMEOUT:-120}" || die "scheduled child task result was not available in time"
log "Prepared scheduled child task ${DEMO_CRON_CHILD_TASK}"

clear

p "# Demo 30: Scheduled Workflow"

p "Brief: create a scheduled repository heartbeat and follow one completed child run to its result."

p "Tell: accelerated demo pacing intentionally prepared one scheduled child run before presenting, so the demo can show the end state without waiting for the next cron tick."
pe "printf 'completed child task: %s\\n' ${DEMO_CRON_CHILD_TASK}"

p "Show: apply the reporter Agent and the scheduled parent Task."
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-agent.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-task.yaml"

p "Inspect: the parent Task is the schedule definition."
pe "kubectl get task ${DEMO_CRON_TASK_NAME} -n ${DEMO_NAMESPACE}"

p "Inspect: scheduled child Tasks are the actual runs."
pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true"

p "Show: the latest completed child result is the heartbeat report."
pe "orka_api GET \"/api/v1/tasks/${DEMO_CRON_CHILD_TASK}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"

p "Summary: scheduled work uses the same Task status, history, and result APIs as interactive runs."
pe "summarize_task_run ${DEMO_CRON_CHILD_TASK} scheduled-heartbeat"
