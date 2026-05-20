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

clear

p "# Demo 20: Manual Workflow"
p "Brief: the same repo-change workflow, launched from declarative Kubernetes YAML."

p "Setup: render the coordinator/coder/reviewer Agents and the manual Task manifest."
pe "render_pr_agents_manifest    > ${DEMO_WORKDIR}/pr-agents.yaml"
pe "render_manual_task_manifest  > ${DEMO_WORKDIR}/manual-task.yaml"
pe "render_manual_story_file     > ${DEMO_WORKDIR}/manual-story.txt"

p "Setup: clean prior runs of the named manual Task."
pe "demo_manual_reset"

p "Show: human-readable brief of the request."
pe "cat ${DEMO_WORKDIR}/manual-story.txt"

if [[ "${DEMO_SHOW_FULL_MANIFEST}" == "1" ]]; then
  p "Show: the full Task manifest (set DEMO_SHOW_FULL_MANIFEST=0 to skip)."
  pe "cat ${DEMO_WORKDIR}/manual-task.yaml"
fi

p "Run: apply the Agents and the Task. Orka reconciles them into a Job and Pod."
pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/manual-task.yaml"

p "Inspect: the Task — agent, phase, backing Job."
pe "task_overview ${DEMO_MANUAL_TASK_NAME}"

p "Follow: stream the runtime Pod logs until the coordinator exits."
pe "follow_task_logs_when_ready ${DEMO_MANUAL_TASK_NAME}"

p "Follow: wait for the coordinator Task to reach Succeeded and persist its result."
pe "require_orka_api_reachable"
pe "wait_for_task_succeeded ${DEMO_MANUAL_TASK_NAME} ${DEMO_MANUAL_TASK_TIMEOUT:-10800}"
pe "wait_for_task_result_available ${DEMO_MANUAL_TASK_NAME}"

p "Inspect: child Tasks — implementation and review delegation."
pe "task_children_summary ${DEMO_MANUAL_TASK_NAME}"

p "Show: the PR handoff."
pe "task_result_summary ${DEMO_MANUAL_TASK_NAME}"

p "Verify: a real PR URL with no unrecovered child failures."
pe "assert_real_pr_result ${DEMO_MANUAL_TASK_NAME}"

p "Summary."
pe "summarize_task_run ${DEMO_MANUAL_TASK_NAME} manual-yaml-workflow"
