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
render_manual_story_file > "${DEMO_WORKDIR}/manual-story.txt"

delete_task_if_exists "${DEMO_MANUAL_TASK_NAME}"

clear

p "# Demo 20: Manual Workflow"

p "Brief: run a repo-change workflow from declarative Kubernetes YAML, using the exact rendered prompt as the source of truth."

p "Show: apply the named coordinator, coder, and reviewer Agents."
pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"

p "Show: the visible brief includes the exact live request and repository details."
pe "cat ${DEMO_WORKDIR}/manual-story.txt"
if [[ "${DEMO_SHOW_FULL_MANIFEST}" == "1" ]]; then
  p "Show: transparency mode prints the full Task manifest, including the prompt that kubectl applies. Set DEMO_SHOW_FULL_MANIFEST=0 to collapse this during rehearsals."
  pe "cat ${DEMO_WORKDIR}/manual-task.yaml"
else
  p "Tell: the full Task manifest is rendered for audit but collapsed for this run."
  pe "sed -n '1,20p' ${DEMO_WORKDIR}/manual-task.yaml"
  pe "printf 'Full Task manifest with prompt: %s\n' ${DEMO_WORKDIR}/manual-task.yaml"
fi

p "Run: create the Task CR. Orka reconciles it into a Job and Pod."
pe "kubectl apply -f ${DEMO_WORKDIR}/manual-task.yaml"
pe "kubectl get task ${DEMO_MANUAL_TASK_NAME} -n ${DEMO_NAMESPACE} -o json | jq '{name: .metadata.name, type: .spec.type, agent: .spec.agentRef.name, phase: .status.phase, jobName: .status.jobName}'"

p "Follow: stream the runtime Pod logs until the coordinator run exits."
pe "follow_task_logs_when_ready ${DEMO_MANUAL_TASK_NAME}"

p "Follow: wait until the coordinator Task succeeds and Orka stores the final result."
pe "require_orka_api_reachable"
pe "wait_for_task_succeeded ${DEMO_MANUAL_TASK_NAME} ${DEMO_MANUAL_TASK_TIMEOUT:-10800}"
pe "wait_for_task_result_available ${DEMO_MANUAL_TASK_NAME}"

p "Inspect: delegated child Tasks show implementation and review work."
pe "orka_api GET \"/api/v1/tasks/${DEMO_MANUAL_TASK_NAME}/children?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({name: .metadata.name, agent: .spec.agentRef.name, phase: .status.phase})'"

p "Show: the final result is the pull request handoff."
pe "orka_api GET \"/api/v1/tasks/${DEMO_MANUAL_TASK_NAME}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"

p "Verify: fail fast unless the final result contains a real PR URL and no unrecovered child failures."
pe "assert_real_pr_result ${DEMO_MANUAL_TASK_NAME}"

p "Summary: YAML launched the same auditable coordinator workflow as chat."
pe "summarize_task_run ${DEMO_MANUAL_TASK_NAME} manual-yaml-workflow"
