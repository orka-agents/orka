#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: switch-backend.sh <agentkit|foundry> <context> <namespace> <new-task-name>

Create one Fibey Task and verify its v2 binding against the selected runtime.
Use the v2 controller's watched namespace and a new Task name for every run.
This command does not patch, delete, retry, or wait for inference completion.
USAGE
}

fail() {
  echo "$*" >&2
  exit 1
}

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  usage
  exit 0
fi
[[ $# == 4 ]] || { usage >&2; exit 2; }
backend="$1"
context="$2"
namespace="$3"
task_name="$4"

case "$backend" in
  agentkit|foundry) ;;
  *) fail "Backend must be agentkit or foundry." ;;
esac
[[ -n "$context" && -n "$namespace" && -n "$task_name" ]] || fail "Context, namespace, and new Task name are required."
for dependency in kubectl jq; do
  command -v "$dependency" >/dev/null || fail "Required command is missing: $dependency"
done

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
kube=(kubectl --context="$context" --namespace="$namespace" --request-timeout=30s)
agent="fibey-remote-$backend"
runtime="fibey-$backend-runtime"

namespace_json="$("${kube[@]}" get namespace "$namespace" -o json)"
jq -e '.metadata.deletionTimestamp == null and .metadata.labels["orka.ai/controller-mode"] == "harness-v2"' \
  <<<"$namespace_json" >/dev/null || fail "Namespace must belong to an existing harness-v2 controller."

agent_json="$("${kube[@]}" get agent "$agent" -o json)"
jq -e --arg runtime "$runtime" '
  .metadata.deletionTimestamp == null and .spec.runtime.runtimeRef.name == $runtime
' <<<"$agent_json" >/dev/null || fail "Agent does not reference the selected runtime."

runtime_json="$("${kube[@]}" get agentruntime "$runtime" -o json)"
jq -e --arg backend "$backend" '
  .spec.capabilities as $cap |
  .metadata.deletionTimestamp == null and
  .spec.contractVersion == "orka.harness.v2" and
  .status.ready == true and
  .status.observedGeneration == .metadata.generation and
  .status.observedCapabilities.runtimeInstanceID == $cap.runtimeInstanceID and
  .status.observedCapabilities.runtimeProfileDigest == $cap.profile.digest and
  $cap.profile.providerKind == $backend and
  $cap.profile.adapterName == ($backend + "-serve-acp") and
  $cap.profile.workspaceIntent == "read" and
  ($cap.mcpPolicy.allowedTools // []) == [] and
  ($cap.mcpPolicy.allowBash // false) == false and
  ($cap.mcpPolicy.approvalRequiredTools // []) == [] and
  $cap.workspaceGovernance.mode == "strict-governed" and
  ($cap.workspaceGovernance.trusted // false) == false and
  $cap.workspaceGovernance.orkaOwnedWorkspaceDeltas == true and
  $cap.workspaceGovernance.promptScopedBrokerAuthorization == true and
  $cap.workspaceGovernance.noDirectSCMPublication == true and
  $cap.workspaceGovernance.orkaOwnedCleanRoomPublication == true and
  $cap.workspaceGovernance.exactInstanceFencing == true and
  $cap.workspaceGovernance.duplicateSafeMutations == true and
  $cap.workspaceGovernance.cancellationSettlement == true
' <<<"$runtime_json" >/dev/null || fail "Runtime must be current, Ready, strict-governed, and configured for this no-tools read demo."

template="$("${kube[@]}" create --dry-run=client --validate=false -f "$script_dir/task.yaml" -o json)"
request="$(jq --arg name "$task_name" --arg namespace "$namespace" --arg agent "$agent" '
  .metadata.name = $name | .metadata.namespace = $namespace | .spec.agentRef.name = $agent
' <<<"$template")"
if ! created="$("${kube[@]}" create -f - -o json <<<"$request")"; then
  fail "CREATE did not return success. Inspect Task $task_name in $context/$namespace before another submission."
fi

if ! "${kube[@]}" wait "task/$task_name" --for=jsonpath='{.status.agentExecutionBinding}' --timeout=120s >/dev/null; then
  fail "Task $task_name was submitted, but its binding is not confirmed. Inspect that Task; do not resubmit it automatically."
fi
if ! bound="$("${kube[@]}" get task "$task_name" -o json)"; then
  fail "Task $task_name was submitted, but its assigned service could not be checked. Inspect that Task; do not resubmit it automatically."
fi
jq -e --argjson created "$created" --argjson agent "$agent_json" \
  --argjson runtime "$runtime_json" --argjson namespace "$namespace_json" '
  .status.agentExecutionBinding as $binding |
  .metadata.uid == $created.metadata.uid and
  $binding.task.uid == $created.metadata.uid and
  $binding.task.boundSpecGeneration == $created.metadata.generation and
  $binding.task.namespaceUID == $namespace.metadata.uid and
  $binding.contractVersion == "orka.harness.v2" and
  $binding.backend == "external-endpoint" and
  $binding.agent.name == $agent.metadata.name and
  $binding.agent.namespace == $created.metadata.namespace and
  $binding.agent.uid == $agent.metadata.uid and
  $binding.agent.generation == $agent.metadata.generation and
  $binding.runtimeRef.name == $runtime.metadata.name and
  $binding.runtimeRef.uid == $runtime.metadata.uid and
  $binding.runtimeRef.generation == $runtime.metadata.generation and
  $binding.runtimeProfileDigest == $runtime.spec.capabilities.profile.digest
' <<<"$bound" >/dev/null || fail "Task $task_name has a different binding than the preflight selection. Inspect it without resubmitting."

echo "task/$task_name"
echo "Confirmed harness v2 binding to $runtime in $context/$namespace. Inference may still be running." >&2
