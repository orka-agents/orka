#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

require_demo_base
require_pr_demo_env
require_chat_client
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

clear

p "# Demo 10: Chat-to-PR"
p "Brief: one chat request becomes coordinator, coder, reviewers, validation, review, CI, PR."

p "Setup: render the operational prompt and the four demo Agents."
pe "render_pr_agents_manifest > ${DEMO_WORKDIR}/pr-agents.yaml"
pe "render_chat_request_file > ${DEMO_WORKDIR}/chat-request.txt"
pe "render_chat_story_file   > ${DEMO_WORKDIR}/chat-story.txt"

p "Setup: clean prior session tasks and Agents so the run is reproducible."
pe "demo_chat_reset"

p "Show: where the chat client points and which model it speaks."
pe "export ANTHROPIC_BASE_URL=$(demo_anthropic_base_url) ANTHROPIC_MODEL=$(demo_anthropic_model)"
pe "print_anthropic_target"

p "Show: the chat request will create the four Agents via Orka's create_agent tool path."
pe "chat_request_key_lines"

p "Show: human-readable brief of the request."
pe "cat ${DEMO_WORKDIR}/chat-story.txt"

if [[ "${DEMO_SHOW_FULL_PROMPT}" == "1" ]]; then
  p "Show: the full operational prompt (set DEMO_SHOW_FULL_PROMPT=0 to skip)."
  pe "cat ${DEMO_WORKDIR}/chat-request.txt"
fi

p "Run: send the request through Orka's Anthropic-compatible endpoint."
pe "require_orka_api_reachable"
pe "demo_chat_send_request"

p "Follow: find the coordinator Task Orka created from this chat session."
pe "demo_chat_discover_parent_task"

p "Follow: wait for the coordinator Task to reach Succeeded."
pe "wait_for_task_succeeded \$DEMO_CHAT_PARENT_TASK ${DEMO_CHAT_TASK_TIMEOUT:-10800}"
pe "wait_for_task_result_available \$DEMO_CHAT_PARENT_TASK ${DEMO_CHAT_RESULT_TIMEOUT:-120}"

p "Inspect: the resources Orka persisted for this chat run."
pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/source=anthropic-proxy"
pe "list_chat_agents"

p "Inspect: parent Task — session, agent, phase, backing Job."
pe "task_overview \$DEMO_CHAT_PARENT_TASK"

p "Inspect: child Tasks — implementation and review delegation."
pe "task_children_summary \$DEMO_CHAT_PARENT_TASK"

p "Show: the PR handoff."
pe "task_result_summary \$DEMO_CHAT_PARENT_TASK"

p "Verify: a real PR URL with no unrecovered child failures."
pe "assert_real_pr_result \$DEMO_CHAT_PARENT_TASK"

p "Summary."
pe "summarize_task_run \$DEMO_CHAT_PARENT_TASK chat-to-pr"
