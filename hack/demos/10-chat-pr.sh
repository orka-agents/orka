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
render_chat_request_file > "${DEMO_WORKDIR}/chat-request.txt"

delete_tasks_by_selector "orka.ai/chat-session=${DEMO_CHAT_SESSION}"
orka_api DELETE "/api/v1/chat/${DEMO_CHAT_SESSION}?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true

clear

p "# Chat endpoint -> coordinator -> reviewers -> PR"
pe "kubectl apply -f ${DEMO_WORKDIR}/pr-agents.yaml"

p "I stay at the chat layer here, but I use the CLI front door so we get the streamed chat experience from /api/v1/chat while still driving the prepared coordinator deterministically."
pe "cat ${DEMO_WORKDIR}/chat-request.txt"
pe "cat ${DEMO_WORKDIR}/chat-request.txt | orka_cli run -v --session ${DEMO_CHAT_SESSION} --provider ${DEMO_PROVIDER_REF} --model ${DEMO_AI_MODEL}"

DEMO_CHAT_PARENT_TASK="$(wait_for_chat_parent_task "${DEMO_CHAT_PARENT_TIMEOUT:-120}")" || die "failed to discover the chat-created coordinator task"
wait_for_task_result_available "${DEMO_CHAT_PARENT_TASK}" "${DEMO_CHAT_RESULT_TIMEOUT:-120}" || die "chat workflow result was not available in time"

pe "kubectl get tasks -n ${DEMO_NAMESPACE} -l orka.ai/chat-session=${DEMO_CHAT_SESSION}"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/tasks/${DEMO_CHAT_PARENT_TASK}/children?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({name: .metadata.name, agent: .spec.agentRef.name, phase: .status.phase})'"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/tasks/${DEMO_CHAT_PARENT_TASK}/result?namespace=${DEMO_NAMESPACE}\" | jq '{result: .result}'"
