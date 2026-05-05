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

render_pr_agents_manifest > "${DEMO_WORKDIR}/pr-agents.yaml"
render_chat_request_file > "${DEMO_WORKDIR}/chat-request.txt"
render_chat_story_file > "${DEMO_WORKDIR}/chat-story.txt"

delete_chat_session_tasks
delete_agent_if_exists "${DEMO_PR_COORDINATOR_NAME}"
delete_agent_if_exists "${DEMO_CODER_AGENT_NAME}"
delete_agent_if_exists "${DEMO_SECURITY_REVIEWER_NAME}"
delete_agent_if_exists "${DEMO_QUALITY_REVIEWER_NAME}"
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

clear

p "# Demo 10: Chat-to-PR"

p "Brief: a maintainer asks for a repo change, and Orka turns one chat request into implementation, validation, review, CI, and a PR."
export ANTHROPIC_BASE_URL="$(demo_anthropic_base_url)"
export ANTHROPIC_MODEL="$(demo_anthropic_model)"
pe "printf 'client=%s\\nendpoint=%s\\nmodel=%s\\norchestration=%s\\n' \"\$DEMO_CLAUDE_BIN\" \"\$ANTHROPIC_BASE_URL\" \"\$ANTHROPIC_MODEL\" 'server-side Orka task workflow'"

p "Show: the chat request will create the coordinator and specialist Agents through Orka's create_agent tool path."
pe "grep -E '^(Start exactly one|Create the Agents|After all four|Do not use create_agent initialPrompt)' ${DEMO_WORKDIR}/chat-request.txt"

p "Show: the visible brief includes the exact live request and repository details."
pe "cat ${DEMO_WORKDIR}/chat-story.txt"
if [[ "${DEMO_SHOW_FULL_PROMPT}" == "1" ]]; then
  p "Show: transparency mode prints the full operational chat prompt before it is sent. Set DEMO_SHOW_FULL_PROMPT=0 to collapse this during rehearsals."
  pe "cat ${DEMO_WORKDIR}/chat-request.txt"
else
  p "Tell: the full operational prompt is rendered for audit but collapsed for this run."
  pe "printf 'Full chat prompt: %s\n' ${DEMO_WORKDIR}/chat-request.txt"
fi

p "Run: send the request through Orka's Anthropic-compatible endpoint."
pe "require_orka_api_reachable"
DEMO_CHAT_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
pe 'run_demo_chat_request_file "${DEMO_WORKDIR}/chat-request.txt" "${DEMO_WORKDIR}/chat-client-result.json"'

p "Follow: find the coordinator Task that Orka created for this chat session."
DEMO_CHAT_PARENT_TASK="$(wait_for_chat_parent_task "${DEMO_CHAT_PARENT_TIMEOUT:-120}" "${DEMO_CHAT_STARTED_AT}")" || die "failed to discover the Anthropic-proxy-created coordinator task"
pe "printf 'coordinator task: %s\\n' ${DEMO_CHAT_PARENT_TASK}"

p "Follow: wait until the coordinator Task succeeds."
pe "wait_for_task_succeeded ${DEMO_CHAT_PARENT_TASK} ${DEMO_CHAT_TASK_TIMEOUT:-10800}"

p "Follow: wait until the workflow has a stored result."
pe "wait_for_task_result_available ${DEMO_CHAT_PARENT_TASK} ${DEMO_CHAT_RESULT_TIMEOUT:-120}"

p "Inspect: Orka persisted the chat request as Kubernetes resources."
pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/source=anthropic-proxy"

p "Inspect: the chat-created Agents are the roles used by the coordinator."
pe "kubectl get agents -n ${DEMO_NAMESPACE} ${DEMO_PR_COORDINATOR_NAME} ${DEMO_CODER_AGENT_NAME} ${DEMO_SECURITY_REVIEWER_NAME} ${DEMO_QUALITY_REVIEWER_NAME}"

p "Inspect: the parent Task records session, agent, phase, and backing Job."
pe "kubectl get task ${DEMO_CHAT_PARENT_TASK} -n ${DEMO_NAMESPACE} -o json | jq '{name: .metadata.name, source: .metadata.labels[\"orka.ai/source\"], session: .spec.sessionRef.name, type: .spec.type, agent: .spec.agentRef.name, phase: .status.phase, jobName: .status.jobName}'"

p "Inspect: child Tasks show implementation and review delegation."
pe "orka_api GET \"/api/v1/tasks/${DEMO_CHAT_PARENT_TASK}/children?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({name: .metadata.name, agent: .spec.agentRef.name, phase: .status.phase})'"

p "Show: the final result is the pull request handoff."
pe "orka_api GET \"/api/v1/tasks/${DEMO_CHAT_PARENT_TASK}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"

p "Verify: fail fast unless the final result contains a real PR URL and no unrecovered child failures."
pe "assert_real_pr_result ${DEMO_CHAT_PARENT_TASK}"

p "Summary: one chat turn created named Agents, then became a coordinator Task, specialist child Tasks, review, and a PR result."
pe "summarize_task_run ${DEMO_CHAT_PARENT_TASK} chat-to-pr"
