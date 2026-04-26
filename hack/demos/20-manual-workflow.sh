#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

require_demo_base
require_pr_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

render_pr_agents_manifest > "${DEMO_WORKDIR}/pr-agents.yaml"
render_manual_task_manifest > "${DEMO_WORKDIR}/manual-task.yaml"

delete_task_if_exists "${DEMO_MANUAL_TASK_NAME}"

clear

p "# Manual coordinator workflow"
pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"
pe "sed -n '1,220p' ${DEMO_WORKDIR}/manual-task.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/manual-task.yaml"
pe "follow_task_logs_when_ready ${DEMO_MANUAL_TASK_NAME}"
pe "wait_for_task_result_available ${DEMO_MANUAL_TASK_NAME}"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/tasks/${DEMO_MANUAL_TASK_NAME}/children?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({name: .metadata.name, agent: .spec.agentRef.name, phase: .status.phase})'"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/tasks/${DEMO_MANUAL_TASK_NAME}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"
