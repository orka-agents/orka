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

clear

p "# Demo 30: Scheduled Workflow"
p "Brief: a scheduled parent Task creates recurring repository heartbeat child runs."

p "Setup: render the reporter Agent and the scheduled parent Task; clean prior runs."
pe "render_cron_agent_manifest > ${DEMO_WORKDIR}/cron-agent.yaml"
pe "render_cron_task_manifest  > ${DEMO_WORKDIR}/cron-task.yaml"
pe "demo_cron_reset"

p "Run: apply the reporter Agent and the scheduled parent Task."
pe "require_orka_api_reachable"
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-agent.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/cron-task.yaml"

p "Inspect: the parent Task is the schedule definition."
pe "kubectl get task ${DEMO_CRON_TASK_NAME} -n ${DEMO_NAMESPACE}"

p "Follow: wait for the first scheduled child to appear (schedule: ${DEMO_CRON_SCHEDULE})."
pe "demo_cron_discover_first_child"

p "Follow: wait for the child to finish and persist its result."
pe "wait_for_task_succeeded \$DEMO_CRON_CHILD_TASK ${DEMO_CRON_TASK_TIMEOUT:-1200}"
pe "wait_for_task_result_available \$DEMO_CRON_CHILD_TASK ${DEMO_CRON_RESULT_TIMEOUT:-120}"

p "Inspect: scheduled child Tasks."
pe "list_scheduled_children"

p "Show: the heartbeat report."
pe "task_result_summary \$DEMO_CRON_CHILD_TASK"

p "Summary."
pe "summarize_task_run \$DEMO_CRON_CHILD_TASK scheduled-heartbeat"
