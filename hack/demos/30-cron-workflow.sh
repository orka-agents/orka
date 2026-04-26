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

render_cron_agent_manifest > "${DEMO_WORKDIR}/cron-agent.yaml"
render_cron_task_manifest > "${DEMO_WORKDIR}/cron-task.yaml"

delete_task_if_exists "${DEMO_CRON_TASK_NAME}"
kubectl delete tasks -n "${DEMO_NAMESPACE}" -l "orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true" --ignore-not-found >/dev/null 2>&1 || true
kubectl apply -f "${DEMO_WORKDIR}/cron-agent.yaml" >/dev/null
kubectl apply -f "${DEMO_WORKDIR}/cron-task.yaml" >/dev/null
DEMO_CRON_CHILD_TASK="$(wait_for_first_scheduled_child "${DEMO_CRON_READY_TIMEOUT:-240}")" || die "timed out waiting for the first scheduled child task"
wait_for_task_terminal "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_TASK_TIMEOUT:-1200}" >/dev/null || die "scheduled child task did not reach a terminal state"
wait_for_task_result_available "${DEMO_CRON_CHILD_TASK}" "${DEMO_CRON_RESULT_TIMEOUT:-120}" || die "scheduled child task result was not available in time"

clear

p "# Scheduled workflow"
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-agent.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-task.yaml"
pe "kubectl get task ${DEMO_CRON_TASK_NAME} -n ${DEMO_NAMESPACE}"
pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/tasks/${DEMO_CRON_CHILD_TASK}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"
