#!/usr/bin/env bash

# Sourced by live-agent-sandbox-e2e.sh after its isolated cluster, provider,
# fixture and admission setup. This uses only the normal Task/class lifecycle.

native_runtime_observation() {
  local task_name="$1" pool_name pod_name task_json pool_json instance session_uid generation auth_binding
  wait_for_jsonpath task "${acp_task_namespace}" "${task_name}" '{.status.phase}' Running 600
  task_json="$(kubectl -n "${acp_task_namespace}" get task "${task_name}" -o json)"
  pool_name="$(jq -r '.status.execution.runtimePoolName' <<<"${task_json}")"
  instance="$(jq -r '.status.execution.runtimeInstanceID' <<<"${task_json}")"
  session_uid="$(jq -r '.status.execution.runtimeSessionUID' <<<"${task_json}")"
  generation="$(jq -r '.status.execution.runtimeSessionGeneration' <<<"${task_json}")"
  pool_json="$(kubectl -n "${acp_task_namespace}" get runtimepool "${pool_name}" -o json)"
  auth_binding="$(jq -r '.metadata.annotations["orka.ai/private-auth-secret-e" + (.status.controllerEpoch | tostring)]' <<<"${pool_json}")"
  [[ "${auth_binding}" == */* ]] || die "native runtime has no bound authentication Secret"
  pod_name="$(kubectl -n "${acp_runtime_namespace}" get pods -l "orka.ai/runtime-pool-name=${pool_name}" -o json |
    jq -r --arg instance "${instance}" '.items[] | select(.metadata.uid == ($instance | split(".")[0])) | .metadata.name')"
  [[ -n "${pod_name}" && "${pod_name}" != null ]] || die "native Task has no runtime Pod"
  kubectl -n "${acp_runtime_namespace}" exec -i "${pod_name}" -- \
    sh -c 'umask 077; cat > /tmp/native-session-observer; chmod 700 /tmp/native-session-observer' <"${native_observer_binary}"
  # Provider Pods receive credentials by bootstrap, without Secret mounts.
  # Pipe only this instance's bound authentication Secret to the strict status
  # client. Credentials never enter command arguments, files or test output.
  kubectl -n "${acp_runtime_namespace}" get secret "${auth_binding%/*}" -o json |
    kubectl -n "${acp_runtime_namespace}" exec -i "${pod_name}" -- \
      /tmp/native-session-observer "${session_uid}" "${generation}" "${instance}" "${auth_binding#*/}"
}

native_fixture_observation() {
  kubectl get --raw '/api/v1/namespaces/vekil-system/services/http:vekil:1337/proxy/fixture/native-session'
}

create_native_session_task() {
  local task_name="$1" marker="$2"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${task_name}
  namespace: ${acp_task_namespace}
spec:
  type: agent
  agentRef:
    name: orka-native-session-agent
  agentRuntime:
    maxTurns: 8
  timeout: 10m
  sessionRef:
    name: orka-native-session
    create: true
    append: true
  execution:
    workspace:
      classRef:
        name: ${acp_suspend_class_name}
      reusePolicy: session
  prompt: "ORKA_HOLD_20S then reply ${marker}"
YAML
}

run_workspace_native_session() {
  log "Running OpenCode native conversation suspend/cold-resume conformance"
  local native_observer_arch native_observer_binary
  native_observer_arch="$(docker version --format '{{.Server.Arch}}')"
  mkdir -p "${repo_root}/bin"
  native_observer_binary="${repo_root}/bin/native-session-observer"
  CGO_ENABLED=0 GOOS=linux GOARCH="${native_observer_arch}" \
    go build -trimpath -ldflags='-s -w' -o "${native_observer_binary}" ./scripts/fixtures/native-session-observer
  prepare_workspace_suspend_class
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: orka-native-session-agent
  namespace: ${acp_task_namespace}
spec:
  runtime:
    type: opencode
    contractVersion: orka.harness.v2
    defaultMaxTurns: 8
    defaultAllowedTools:
      - read
  model:
    name: openai/gpt-4.1
    contextWindow: 32768
    maxTokens: 4096
YAML
  local first second workspace_name pool_name first_pod first_count second_count
  create_native_session_task orka-native-first ORKA_NATIVE_FIRST_OK
  first="$(native_runtime_observation orka-native-first)"
  wait_for_jsonpath task "${acp_task_namespace}" orka-native-first '{.status.phase}' Succeeded 600
  assert_task_result_contains "${acp_task_namespace}" orka-native-first ORKA_NATIVE_FIRST_OK
  workspace_name="$(kubectl -n "${acp_task_namespace}" get task orka-native-first -o jsonpath='{.status.executionWorkspace.workspaceRef.name}')"
  pool_name="$(kubectl -n "${acp_task_namespace}" get task orka-native-first -o jsonpath='{.status.execution.runtimePoolName}')"
  wait_for_jsonpath executionworkspace "${acp_task_namespace}" "${workspace_name}" '{.status.state}' Suspended 600
  wait_for_jsonpath runtimepool "${acp_task_namespace}" "${pool_name}" '{.status.lifecycle}' Stopped 240
  first_pod="$(jq -r '.runtimeInstanceID | split(".")[0]' <<<"${first}")"
  if kubectl -n "${acp_runtime_namespace}" get pods -o json | jq -e --arg uid "${first_pod}" 'any(.items[]; .metadata.uid == $uid)' >/dev/null; then
    die "native Session suspension left its original runtime Pod alive"
  fi
  first_count="$(native_fixture_observation)"
  jq -e '.requests == 2 and .toolCalls == 1 and (.earlierResultSeen | not) and (.canonicalBootstrap | not)' <<<"${first_count}" >/dev/null ||
    die "first native exchange did not execute exactly one diagnostic tool"

  create_native_session_task orka-native-second ORKA_NATIVE_SECOND_OK
  second="$(native_runtime_observation orka-native-second)"
  jq -en --argjson first "${first}" --argjson second "${second}" '
    $first.providerSessionID == $second.providerSessionID and
    $first.runtimeInstanceID != $second.runtimeInstanceID and
    $first.credentialDigest != $second.credentialDigest and
    $second.generation > $first.generation and $second.method == "session/resume"' >/dev/null ||
    die "cold resume did not restore the same native conversation under fresh runtime identity and credentials"
  wait_for_jsonpath task "${acp_task_namespace}" orka-native-second '{.status.phase}' Succeeded 600
  assert_task_result_contains "${acp_task_namespace}" orka-native-second ORKA_NATIVE_SECOND_OK
  wait_for_jsonpath task "${acp_task_namespace}" orka-native-second \
    '{.status.conditions[?(@.type=="NativeSessionContinuity")].reason}' Restored 30
  second_count="$(native_fixture_observation)"
  jq -e '.requests == 3 and .toolCalls == 1 and .earlierResultSeen and (.canonicalBootstrap | not)' <<<"${second_count}" >/dev/null ||
    die "native follow-up lost, reconstructed, or repeated the earlier diagnostic"
  wait_for_jsonpath executionworkspace "${acp_task_namespace}" "${workspace_name}" '{.status.state}' Suspended 600
  log "OpenCode 1.18.9 retained its native session ID and diagnostic through a fresh Pod, process and credentials; no tool was repeated"
  delete_session_if_present "${acp_task_namespace}" orka-native-session
  kubectl -n "${acp_task_namespace}" delete task orka-native-first orka-native-second --wait=true --timeout=2m
  kubectl -n "${acp_task_namespace}" delete agent orka-native-session-agent --wait=true --timeout=1m
}
