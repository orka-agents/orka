#!/usr/bin/env bash
# Conformance against the unmodified official Substrate pin.
set -Eeuo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ORKA_NAMESPACE=orka-system
ACP_RUNTIME_NAMESPACE=orka-runtimes
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
TMP_ROOT="${SUBSTRATE_E2E_RUN_DIR:-${ROOT_DIR}/bin/substrate-e2e-${KIND_CLUSTER}}"
SUBSTRATE_E2E_ACP_TASK_SMOKE=1
SUBSTRATE_E2E_SUSPEND_RESUME=1
SUBSTRATE_E2E_LIFECYCLE=1
LIFECYCLE_AMBIGUITY_MARKER=ORKA_E2E_WS_LC_AMBIGUOUS_OK
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME=orka-substrate-bootstrap
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY=token
PORT_FORWARD_PIDS=()
source "${ROOT_DIR}/scripts/lib/substrate-upstream.sh"
source "${ROOT_DIR}/scripts/lib/substrate-orka-local.sh"
source "${ROOT_DIR}/scripts/lib/e2e-admission-tls.sh"
source "${ROOT_DIR}/scripts/lib/redact.sh"

log() { printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"; }
kubectl_ate() { "${TMP_ROOT}/kubectl-ate" --context "kind-${KIND_CLUSTER}" "$@"; }
cleanup() {
  local rc=$?
  for pid in "${PORT_FORWARD_PIDS[@]}"; do kill "${pid}" 2>/dev/null || true; done
  rm -f "${TMP_ROOT}/native-history-token" "${TMP_ROOT}/native-history-header"
  if (( rc )) && [[ "${CLUSTER_PREPARED:-0}" == 1 ]]; then
    runtime_diagnostics
    kubectl get pods -A 2>/dev/null || true
    job_diagnostics substrate-direct-conformance
    job_diagnostics native-mcp-client
    checkpoint_diagnostics
    workload_logs ate-system -l app=ate-api-server
    workload_logs ate-system -l app=atenet-egress
    workload_logs ate-demo -l ate.dev/worker-pool=orka-native
    workload_logs vekil-system -l app.kubernetes.io/component=responses-fixture
    workload_logs orka-system -l orka.ai/network-role=provider-auth-proxy
    workload_logs orka-system -l control-plane=controller-manager
  fi
  if [[ "${KEEP_CLUSTER}" != 1 && "${CLUSTER_PREPARED:-0}" == 1 ]]; then kind delete cluster --name "${KIND_CLUSTER}"; fi
  exit "${rc}"
}
runtime_diagnostics() {
  local bootstrap_secret=""
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret)
  if [[ -f "${TMP_ROOT}/bootstrap-token" ]]; then bootstrap_secret="$(<"${TMP_ROOT}/bootstrap-token")"; fi
  # Capture provisioning failures while pools still exist. Task cleanup can
  # remove them before the outer success deadline expires. Omit all specs,
  # Task results, transcripts, and provider identity records.
  kubectl -n orka-system --request-timeout=15s get tasks,runtimepools,executionworkspaces,executionworkspacecheckpoints -o json 2>/dev/null |
    jq '[.items[] | {kind, name: .metadata.name, phase: .status.phase, state: .status.state,
      lifecycle: .status.lifecycle, message: .status.message,
      execution: (.status.execution | if . == null then null else {state, outcome, reason} end),
      conditions: [.status.conditions[]? | {type, status, reason, message}]}]' | redact >&2 || true
}
checkpoint_diagnostics() {
  local bootstrap_secret=""
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret)
  if [[ -f "${TMP_ROOT}/bootstrap-token" ]]; then bootstrap_secret="$(<"${TMP_ROOT}/bootstrap-token")"; fi
  # ateom reports data capture before atelet uploads it and releases the
  # workspace. Keep the node-side failure even when polling outlives it.
  # Select diagnostic fields before output; RPC requests can contain secrets.
  kubectl -n ate-system --request-timeout=15s logs -l app=atelet --all-containers=true \
    --tail=2000 --since=15m --pod-running-timeout=5s 2>/dev/null |
    jq -Rrc 'fromjson? | select(
      .level == "ERROR" or .level == "WARN" or
      ((.method // "") | test("/(Checkpoint|UploadPausedCheckpoint)$")) or
      ((.msg // "") | test("checkpoint|snapshot"; "i"))) |
      {time, level, msg, method, err, error, message, phase, "elapsed-time"} |
      with_entries(select(.value != null))' | tail -n 80 | redact >&2 || true
}
workload_logs() {
  local namespace="$1" bootstrap_secret=""
  shift
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret)
  if [[ -f "${TMP_ROOT}/bootstrap-token" ]]; then bootstrap_secret="$(<"${TMP_ROOT}/bootstrap-token")"; fi
  kubectl -n "${namespace}" --request-timeout=15s logs "$@" --all-containers=true \
    --prefix=true --tail=200 --pod-running-timeout=5s 2>&1 | redact >&2 || true
}
job_diagnostics() {
  local name="$1" bootstrap_secret=""
  local ORKA_REDACT_SECRET_VARS=(bootstrap_secret)
  if [[ -f "${TMP_ROOT}/bootstrap-token" ]]; then bootstrap_secret="$(<"${TMP_ROOT}/bootstrap-token")"; fi
  log "Conformance diagnostics for job/${name}"
  # Restrict metadata to status; Pod specs can contain projected credentials.
  kubectl -n orka-system --request-timeout=15s get pods -l "job-name=${name}" -o json 2>/dev/null |
    jq '[.items[] | {name: .metadata.name, phase: .status.phase, conditions: .status.conditions,
      containers: [.status.containerStatuses[]? | {name, state}]}]' | redact >&2 || true
  workload_logs orka-system "job/${name}"
}
wait_job() {
  local name="$1" seconds="$2" start status
  start=$(date +%s)
  while true; do
    status="$(kubectl -n orka-system --request-timeout=15s get job "${name}" -o json)" || return 1
    if jq -e 'any(.status.conditions[]?; (.type == "Failed" or .type == "FailureTarget") and .status == "True")' <<<"${status}" >/dev/null; then
      printf 'Conformance job/%s failed\n' "${name}" >&2
      return 1
    fi
    if jq -e 'any(.status.conditions[]?; .type == "Complete" and .status == "True")' <<<"${status}" >/dev/null; then return 0; fi
    if (( $(date +%s) - start >= seconds )); then
      printf 'Timed out waiting for conformance job/%s\n' "${name}" >&2
      return 1
    fi
    sleep 2
  done
}
wait_field() {
  local resource="$1" name="$2" expression="$3" expected="$4" seconds="${5:-600}" start now next_diagnostics object
  start=$(date +%s)
  next_diagnostics=$((start + 30))
  while true; do
    object="$(kubectl -n orka-system --request-timeout=15s get "${resource}" "${name}" -o json 2>/dev/null)" || return 1
    if [[ "$(jq -r "${expression}" <<<"${object}")" == "${expected}" ]]; then return 0; fi
    if [[ "${resource}" == task ]] && jq -e '.status.phase | . == "Failed" or . == "Succeeded" or . == "Cancelled"' <<<"${object}" >/dev/null; then
      printf 'Task/%s settled before %s = %s\n' "${name}" "${expression}" "${expected}" >&2
      runtime_diagnostics
      return 1
    fi
    if [[ "${resource}" == executionworkspace ]] && jq -e '.status.state == "Failed"' <<<"${object}" >/dev/null; then
      printf 'ExecutionWorkspace/%s failed before %s = %s\n' "${name}" "${expression}" "${expected}" >&2
      runtime_diagnostics
      return 1
    fi
    now=$(date +%s)
    if (( now - start >= seconds )); then
      printf 'Timed out waiting for %s/%s %s = %s\n' "${resource}" "${name}" "${expression}" "${expected}" >&2
      runtime_diagnostics
      return 1
    fi
    if (( now >= next_diagnostics )); then
      runtime_diagnostics
      next_diagnostics=$((now + 30))
    fi
    sleep 2
  done
}
wait_absent() {
  local resource="$1" name="$2"
  local present
  present="$(kubectl -n orka-system get "${resource}" "${name}" --ignore-not-found -o name)" || return 1
  if [[ -n "${present}" ]]; then
    kubectl -n orka-system wait --for=delete "${resource}/${name}" --timeout=600s
  fi
}
mount_substrate_identity() {
  local resource="$1" name="$2" container="$3"
  kubectl -n orka-system patch "${resource}" "${name}" --type=strategic -p "$(jq -cn --arg container "${container}" '{spec:{template:{spec:{securityContext:{fsGroup:65532},containers:[{name:$container,volumeMounts:[{name:"substrate-client",mountPath:"/run/substrate-client",readOnly:true},{name:"substrate-server",mountPath:"/run/substrate-server",readOnly:true}]}],volumes:[{name:"substrate-client",projected:{sources:[{podCertificate:{signerName:"podidentity.podcert.ate.dev/identity",keyType:"ECDSAP256",credentialBundlePath:"credential-bundle.pem"}}]}},{name:"substrate-server",projected:{sources:[{clusterTrustBundle:{signerName:"servicedns.podcert.ate.dev/identity",labelSelector:{matchLabels:{"podcert.ate.dev/canarying":"live"}},path:"trust-bundle.pem"}}]}}]}}}}')"
}
publish_ateom_image() {
  (cd "${SUBSTRATE_DIR}" && KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}" ko build --platform="linux/$(go env GOARCH)" ./cmd/ateom-gvisor)
}
build_image() {
  local name="$1" dockerfile="$2" ref
  ref="localhost:${KIND_REGISTRY_PORT}/orka/${name}:native-conformance"
  docker build -t "${ref}" -f "${ROOT_DIR}/${dockerfile}" "${ROOT_DIR}" >&2
  docker push "${ref}" >&2
  docker inspect --format '{{index .RepoDigests 0}}' "${ref}"
}
native_template_manifest() {
  local name="$1" image="$2" public_key="$3"
  # Native overlay roots and DurableDir mounts start as 0700. Restore root
  # traversal and workspace writes before commands drop to UID 1000.
  jq -n --arg name "${name}" --arg image "${image}" --arg publicKey "${public_key}" '
    {metadata:{atespace:"orka-system",name:$name},workerSelector:{matchLabels:{"orka.ai/native-pool":"conformance"}},
     containers:[{name:"server",image:$image,env:([{name:"ORKA_WORKSPACE_AGENT_LISTEN_ADDR",value:":80"}] + (if $name == "orka-direct" then [{name:"ORKA_WORKSPACE_BOOTSTRAP_PUBLIC_KEY",value:$publicKey}] else [] end)),
       command:(if $name == "orka-direct" then ["/bin/sh","-ec"] else [] end),
       args:(if $name == "orka-direct" then ["stat -c native-root-mode=%a /; chmod 0755 /; chmod 1777 /workspace; exec /orka-workspace-agent"] else [] end),
       readyz:{httpGet:{path:(if $name == "orka-direct" then "/v1/health" else "/healthz" end),port:80}},
       securityContext:{capabilities:{drop:["ALL"],add:["NET_BIND_SERVICE","SETUID","SETGID","CHOWN","KILL"]}},
       volumeMounts:(if $name == "orka-direct" then [{name:"identity",mountPath:"/run/orka-substrate-identity"},{name:"workspace",mountPath:"/workspace"}] else [] end)}],
     volumes:(if $name == "orka-direct" then [{name:"workspace",durableDir:{}},{name:"identity",systemInfo:{dataSources:[{actorMetadata:{items:[{field:"ACTOR_METADATA_FIELD_ATESPACE",path:"atespace"},{field:"ACTOR_METADATA_FIELD_NAME",path:"name"},{field:"ACTOR_METADATA_FIELD_UID",path:"uid"}]}}]}}] else [] end),
     resources:{limits:[{name:"cpu",quantity:"1"},{name:"memory",quantity:"1Gi"}]},
     snapshotsConfig:{storageLocation:"s3://ate-snapshots/orka-conformance/",onPause:"SNAPSHOT_CONTENT_SCOPE_DATA",onCommit:"SNAPSHOT_CONTENT_SCOPE_DATA",onResume:{fromData:"RESUME_SOURCE_COLD_BOOT"}},
     sandboxConfig:{sandboxClass:"SANDBOX_CLASS_GVISOR",configName:"gvisor-default"}}
  '
}
create_native_resources() {
  local worker_image="$1" direct_image="$2" mcp_image="$3" public_key="$4"
  kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
  bash "${ROOT_DIR}/scripts/lib/ensure-static-mode-namespace.sh" kubectl "${ORKA_NAMESPACE}" harness-v2
  kubectl_ate get atespace orka-system >/dev/null 2>&1 || kubectl_ate create atespace orka-system
  kubectl -n ate-demo apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-native
  labels: {orka.ai/native-pool: conformance}
spec:
  replicas: 3
  workerImage: ${worker_image}
  template:
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: "2", memory: 2Gi}
YAML
  local name image
  for name in orka-direct orka-mcp orka-acp-infra; do
    image="${mcp_image}"
    [[ "${name}" != orka-direct ]] || image="${direct_image}"
    native_template_manifest "${name}" "${image}" "${public_key}" >"${TMP_ROOT}/${name}.json"
    kubectl_ate create actor-template -f "${TMP_ROOT}/${name}.json"
  done
  kubectl -n ate-system patch deployment atenet-router --type=json -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--route-timeout=30m"}]'
  kubectl -n ate-system patch deployment atenet-router --type=strategic -p '{"spec":{"template":{"spec":{"containers":[{"name":"envoy","command":["/usr/local/bin/envoy","-c","/etc/envoy/envoy.yaml","--component-log-level","upstream:info,router:info,ext_proc:info"]}]}}}}'
  kubectl -n ate-system rollout status deployment/atenet-router --timeout=3m
  kubectl -n ate-demo wait --for=jsonpath='{.status.readyReplicas}'=3 workerpool/orka-native --timeout=5m
}
create_workspace_class() {
  kubectl -n orka-system apply -f - <<'YAML'
apiVersion: acp.workspace.orka.ai/v1alpha1
kind: RuntimeProviderConfig
metadata: {name: native-substrate}
spec: {backend: substrate}
---
apiVersion: workspace.orka.ai/v1alpha1
kind: ExecutionWorkspaceProvider
metadata: {name: native-substrate}
spec:
  controllerName: acp.workspace.orka.ai/runtime-pool
  parametersRef: {group: acp.workspace.orka.ai, kind: RuntimeProviderConfig, name: native-substrate}
  lifecycleState: Active
  requiredContracts: [workspace.orka.ai/v1]
---
apiVersion: acp.workspace.orka.ai/v1alpha1
kind: RuntimeWorkspaceProfile
metadata: {name: native-substrate}
spec:
  substrate:
    templateRef: {namespace: orka-system, name: orka-acp-infra}
    suspend: {mode: DataOnly}
---
apiVersion: workspace.orka.ai/v1alpha1
kind: ExecutionWorkspaceClass
metadata: {name: native-substrate}
spec:
  providerRef: {name: native-substrate}
  parametersRef: {group: acp.workspace.orka.ai, kind: RuntimeWorkspaceProfile, name: native-substrate}
  mode: Interactive
  allowedReuseScopes: [None, Session]
  lifecycle:
    defaultOnDetach: Suspend
    allowedOnDetach: [Suspend, Delete]
    detachTimeout: 5m
    maxLifetime: 2h
    deletionPolicy: {providerResources: Delete, persistentVolumes: Delete, checkpoints: Delete}
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata: {name: native-substrate}
spec:
  runtime: {type: codex, contractVersion: orka.harness.v2, defaultMaxTurns: 1}
  model: {name: gpt-5.5}
YAML
  wait_field executionworkspaceclass native-substrate '.status.conditions[]? | select(.type=="Ready") | .status' True
}
submit_task() {
  local name="$1" session="$2" prompt="$3" timeout="${4:-15m}" max_turns="${5:-1}" class="${6:-native-substrate}"
  jq -n --arg name "${name}" --arg session "${session}" --arg prompt "${prompt}" --arg timeout "${timeout}" --argjson maxTurns "${max_turns}" --arg class "${class}" '{apiVersion:"core.orka.ai/v1alpha1",kind:"Task",metadata:{name:$name,namespace:"orka-system"},spec:{type:"agent",agentRef:{name:"native-substrate"},agentRuntime:{maxTurns:$maxTurns},timeout:$timeout,sessionRef:{name:$session,create:true},execution:{workspace:{classRef:{name:$class},reusePolicy:"session"}},prompt:$prompt}}' | kubectl create -f -
}
workspace_for_task() { kubectl -n orka-system get task "$1" -o json | jq -er '.metadata.labels["acp.workspace.orka.ai/execution-workspace"]'; }
pool_for_task() { kubectl -n orka-system get task "$1" -o json | jq -er '.status.execution.runtimePoolName'; }
journal_for_pool() {
  local uid
  uid="$(kubectl -n orka-system get runtimepool "$1" -o jsonpath='{.metadata.uid}')"
  kubectl -n orka-system get configmap -l "orka.ai/runtime-pool-uid=${uid}" -o json | jq -er '.items[] | select(.data["runtime.json"]) | .data["runtime.json"] | fromjson'
}
service_request() {
  local response_mode="$1" method="$2" request_timeout=3
  shift 2
  local namespace="$1" service="$2" target_port="$3" path="$4" port
  local request_headers=()
  local response_args=(--fail)
  if [[ "${response_mode}" == status ]]; then
    response_args=(--output /dev/null --write-out '%{http_code}')
  fi
  if [[ "${method}" == DELETE ]]; then request_timeout=30; fi
  if [[ -n "${5:-}" ]]; then
    [[ -s "$5" ]] || return 1
    request_headers=(--header "@$5")
  fi
  port="$(python3 - <<'PORT'
import socket
with socket.socket() as sock:
    sock.bind(('127.0.0.1', 0))
    print(sock.getsockname()[1])
PORT
)"
  kubectl -n "${namespace}" port-forward "service/${service}" "${port}:${target_port}" >"${TMP_ROOT}/${service}-port-forward.log" 2>&1 &
  local pid=$! result start
  PORT_FORWARD_PIDS+=("${pid}")
  start=$(date +%s)
  until result="$(curl -sS --connect-timeout 1 --max-time "${request_timeout}" --request "${method}" "${response_args[@]}" "${request_headers[@]}" "http://127.0.0.1:${port}${path}" 2>/dev/null)"; do
    if (( $(date +%s) - start > 20 )); then kill "${pid}" 2>/dev/null || true; return 1; fi
    sleep 1
  done
  kill "${pid}" 2>/dev/null || true
  printf '%s\n' "${result}"
}
service_read() { service_request body GET "$@"; }
service_status() {
  local method="$1"
  shift
  service_request status "${method}" "$@"
}
create_history_api_identity() {
  jq -n '{apiVersion:"v1",kind:"List",items:[
    {apiVersion:"v1",kind:"ServiceAccount",metadata:{name:"native-history-client",namespace:"orka-system"}},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"Role",metadata:{name:"native-history-client",namespace:"orka-system"},
      rules:[{apiGroups:["core.orka.ai"],resources:["sessions"],resourceNames:["native-session"],verbs:["get"]}]},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",metadata:{name:"native-history-client",namespace:"orka-system"},
      roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"native-history-client"},
      subjects:[{kind:"ServiceAccount",name:"native-history-client",namespace:"orka-system"}]}
  ]}' | kubectl -n orka-system apply -f - || return 1
  (
    umask 077
    rm -f "${TMP_ROOT}/native-history-token" "${TMP_ROOT}/native-history-header"
    kubectl -n orka-system create token native-history-client --duration=15m >"${TMP_ROOT}/native-history-token" || exit 1
    [[ -s "${TMP_ROOT}/native-history-token" ]] || exit 1
    {
      printf 'Authorization: Bearer '
      cat "${TMP_ROOT}/native-history-token" || exit 1
      printf '\n'
    } >"${TMP_ROOT}/native-history-header"
  ) || return 1
}
create_cleanup_api_identity() {
  local session="$1"
  jq -n --arg session "${session}" '{apiVersion:"v1",kind:"List",items:[
    {apiVersion:"v1",kind:"ServiceAccount",metadata:{name:"native-cleanup-client",namespace:"orka-system"}},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"Role",metadata:{name:"native-cleanup-client",namespace:"orka-system"},
      rules:[{apiGroups:["core.orka.ai"],resources:["sessions"],resourceNames:[$session],verbs:["get","delete"]}]},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",metadata:{name:"native-cleanup-client",namespace:"orka-system"},
      roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"native-cleanup-client"},
      subjects:[{kind:"ServiceAccount",name:"native-cleanup-client",namespace:"orka-system"}]}
  ]}' | kubectl -n orka-system apply -f - || return 1
  (
    umask 077
    rm -f "${TMP_ROOT}/native-cleanup-token" "${TMP_ROOT}/native-cleanup-header"
    kubectl -n orka-system create token native-cleanup-client --duration=10m >"${TMP_ROOT}/native-cleanup-token" || exit 1
    [[ -s "${TMP_ROOT}/native-cleanup-token" ]] || exit 1
    {
      printf 'Authorization: Bearer '
      cat "${TMP_ROOT}/native-cleanup-token" || exit 1
      printf '\n'
    } >"${TMP_ROOT}/native-cleanup-header"
  ) || return 1
}
delete_native_session() {
  local session="$1" status start
  create_cleanup_api_identity "${session}" || return 1
  start=$(date +%s)
  while true; do
    status="$(service_status DELETE orka-system orka-api 8080 "/api/v1/sessions/${session}?namespace=orka-system" "${TMP_ROOT}/native-cleanup-header")" || return 1
    case "${status}" in
      200|202|204|404) break ;;
      409)
        if (( $(date +%s) - start >= 120 )); then
          printf 'Session/%s remained unsettled during cleanup\n' "${session}" >&2
          return 1
        fi
        sleep 2
        ;;
      *) printf 'Session/%s deletion failed with HTTP %s\n' "${session}" "${status}" >&2; return 1 ;;
    esac
  done
  start=$(date +%s)
  while true; do
    status="$(service_status GET orka-system orka-api 8080 "/api/v1/sessions/${session}?namespace=orka-system" "${TMP_ROOT}/native-cleanup-header")" || return 1
    case "${status}" in
      404)
        kubectl -n orka-system delete serviceaccount,role,rolebinding native-cleanup-client || return 1
        rm -f "${TMP_ROOT}/native-cleanup-token" "${TMP_ROOT}/native-cleanup-header"
        return 0
        ;;
      200)
        if (( $(date +%s) - start >= 60 )); then
          printf 'Session/%s remained readable after cleanup\n' "${session}" >&2
          return 1
        fi
        sleep 2
        ;;
      *) printf 'Session/%s deletion verification failed with HTTP %s\n' "${session}" "${status}" >&2; return 1 ;;
    esac
  done
}
fixture_read() { service_read vekil-system vekil 1337 "$1"; }
fixture_key() { printf '%s' "$1" | shasum -a 256 | awk '{print substr($1, 1, 16)}'; }
assert_fixture_count() {
  local key
  key="$(fixture_key "$1")"
  [[ "$(fixture_read /fixture/marker-counts | jq -r --arg key "${key}" '.[$key] // 0')" == "$2" ]]
}
assert_fixture_canary() {
  local key
  key="$(fixture_key "$1")"
  # One request emits the shell call; the second must carry its verified output.
  assert_fixture_count "$1" 2
  fixture_read /fixture/marker-observations | jq -e --arg key "${key}" \
    '.[$key].workspaceCanaryVerified == true' >/dev/null
}
wait_fixture_request() {
  local task_name="$1" marker="$2" seconds="${3:-180}" key count start object
  key="$(fixture_key "${marker}")"
  start=$(date +%s)
  while true; do
    count="$(fixture_read /fixture/marker-counts | jq -er --arg key "${key}" '.[$key] // 0')" || return 1
    if [[ ! "${count}" =~ ^[0-9]+$ ]] || (( count > 1 )); then
      printf 'Task/%s did not reach the fixture exactly once\n' "${task_name}" >&2
      return 1
    fi
    if [[ "${count}" == 1 ]]; then return 0; fi
    object="$(kubectl -n orka-system --request-timeout=15s get task "${task_name}" -o json)" || return 1
    if jq -e '.status.phase | . == "Failed" or . == "Succeeded" or . == "Cancelled"' <<<"${object}" >/dev/null; then
      printf 'Task/%s settled before reaching the provider fixture\n' "${task_name}" >&2
      runtime_diagnostics
      return 1
    fi
    if (( $(date +%s) - start >= seconds )); then
      printf 'Task/%s never reached the provider fixture\n' "${task_name}" >&2
      runtime_diagnostics
      return 1
    fi
    sleep 2
  done
}
wait_fixture_disconnect() {
  local marker="$1" seconds="${2:-60}" key count start
  key="$(fixture_key "${marker}")"
  start=$(date +%s)
  while true; do
    count="$(fixture_read /fixture/marker-observations | jq -er --arg key "${key}" '.[$key].disconnects // 0')" || return 1
    if [[ ! "${count}" =~ ^[0-9]+$ ]] || (( count > 1 )); then
      printf 'Invalid provider disconnect count\n' >&2
      return 1
    fi
    if [[ "${count}" == 1 ]]; then
      assert_fixture_count "${marker}" 1
      return $?
    fi
    if (( $(date +%s) - start >= seconds )); then
      printf 'Cancellation did not close the held provider stream\n' >&2
      return 1
    fi
    sleep 2
  done
}
exercise_acp_lifecycle() {
  create_history_api_identity || return 1
  log "Running cold execution, long streaming, suspend and checkpoint export"
  submit_task native-first native-session 'ORKA_HOLD_60S Reply exactly: ORKA_NATIVE_FIRST_OK'
  wait_field task native-first '.status.phase' Running
  wait_field task native-first '.status.execution.promptID != null and .status.execution.promptID != ""' true
  # A prompt ID is allocated before inference starts. Observe the held model
  # request before restarting, so this checks recovery of an in-flight turn.
  wait_fixture_request native-first ORKA_NATIVE_FIRST_OK
  local pool first_actor workspace
  pool="$(pool_for_task native-first)"
  first_actor="$(journal_for_pool "${pool}" | jq -er '.attempt.uid')"
  # A manager restart must recover demand without submitting the prompt twice.
  kubectl -n orka-system rollout restart deployment/orka-controller-manager
  kubectl -n orka-system rollout status deployment/orka-controller-manager --timeout=5m
  wait_field task native-first '.status.phase' Succeeded
  assert_fixture_count ORKA_NATIVE_FIRST_OK 1
  workspace="$(workspace_for_task native-first)"
  wait_field executionworkspace "${workspace}" '.status.state' Suspended
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  service_read orka-system orka-api 8080 '/api/v1/sessions/native-session?namespace=orka-system' "${TMP_ROOT}/native-history-header" |
    jq -e '.messageCount > 0 and (.transcript | contains("ORKA_NATIVE_FIRST_OK"))' >/dev/null
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  local workspace_uid
  workspace_uid="$(kubectl -n orka-system get executionworkspace "${workspace}" -o jsonpath='{.metadata.uid}')"
  jq -n --arg name "${workspace}" --arg uid "${workspace_uid}" '{apiVersion:"workspace.orka.ai/v1alpha1",kind:"ExecutionWorkspaceCheckpoint",metadata:{name:"native-save",namespace:"orka-system"},spec:{workspaceRef:{name:$name,uid:$uid}}}' | kubectl create -f -
  wait_field executionworkspacecheckpoint native-save '.status.phase' Ready
  submit_task native-continue native-session 'Reply exactly: ORKA_NATIVE_CONTINUE_OK'
  wait_field task native-continue '.status.phase' Succeeded
  assert_fixture_count ORKA_NATIVE_CONTINUE_OK 1
  local continued_key first_key
  continued_key="$(fixture_key ORKA_NATIVE_CONTINUE_OK)"
  first_key="$(fixture_key ORKA_NATIVE_FIRST_OK)"
  fixture_read /fixture/marker-observations | jq -e --arg key "${continued_key}" --arg first "${first_key}" \
    '.[$key].sawHistory and (.[$key].historyMarkers | index($first) != null)' >/dev/null
  wait_field executionworkspace "${workspace}" '.status.state' Suspended
  [[ "$(journal_for_pool "${pool}" | jq -r '.checkpoint.sourceUID')" != "${first_actor}" ]] || { echo 'continuation reused its prior Actor' >&2; return 1; }
  exercise_acp_failed_recovery "${workspace}" "${pool}"
  local checkpoint_uid digest
  checkpoint_uid="$(kubectl -n orka-system get executionworkspacecheckpoint native-recovery-save -o jsonpath='{.metadata.uid}')"
  digest="$(kubectl -n orka-system get executionworkspacecheckpoint native-recovery-save -o jsonpath='{.status.digest}')"
  delete_native_session native-session
  kubectl -n orka-system delete executionworkspace "${workspace}" --wait=false
  wait_absent executionworkspace "${workspace}"
  jq -n --arg uid "${checkpoint_uid}" --arg digest "${digest}" '{apiVersion:"core.orka.ai/v1alpha1",kind:"Task",metadata:{name:"native-fork",namespace:"orka-system"},spec:{type:"agent",agentRef:{name:"native-substrate"},timeout:"15m",execution:{workspace:{classRef:{name:"native-substrate"},onDetach:"Delete",restoreFrom:{name:"native-recovery-save",uid:$uid,digest:$digest}}},prompt:"Reply exactly: ORKA_NATIVE_FORK_OK"}}' | kubectl create -f -
  wait_field task native-fork '.status.phase' Succeeded
  kubectl -n orka-system delete executionworkspacecheckpoint native-save native-recovery-save
  wait_absent executionworkspace "$(workspace_for_task native-fork)"
  log "Checking cancellation and timeout do not retain active compute"
  submit_task native-timeout timeout-session 'ORKA_HOLD_120S Reply exactly: ORKA_NATIVE_TIMEOUT_OK' 30s
  wait_fixture_request native-timeout ORKA_NATIVE_TIMEOUT_OK
  wait_field task native-timeout '
    .status.phase == "Cancelled" and .status.execution.state == "Cancelled" and
    .status.execution.outcome == "Cancelled" and .status.execution.reason == "TaskTimeout" and
    .status.execution.attempt == 1' true
  wait_fixture_disconnect ORKA_NATIVE_TIMEOUT_OK
  wait_field executionworkspace "$(workspace_for_task native-timeout)" '.status.state' Suspended
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  delete_native_session timeout-session
  submit_task native-cancel cancel-session 'ORKA_HOLD_120S Reply exactly: ORKA_NATIVE_CANCEL_OK'
  wait_field task native-cancel '.status.phase' Running
  wait_fixture_request native-cancel ORKA_NATIVE_CANCEL_OK
  kubectl -n orka-system delete task native-cancel --wait=false
  wait_field task native-cancel '
    .status.phase == "Cancelled" and .status.execution.state == "Cancelled" and
    .status.execution.outcome == "Cancelled" and .status.execution.attempt == 1' true
  wait_fixture_disconnect ORKA_NATIVE_CANCEL_OK
  wait_field executionworkspace "$(workspace_for_task native-cancel)" '.status.state' Suspended
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  delete_native_session cancel-session
  wait_absent task native-cancel
  cleanup_acp_workspaces
  assert_fixture_count ORKA_NATIVE_TIMEOUT_OK 1
  assert_fixture_count ORKA_NATIVE_CANCEL_OK 1
  kubectl -n orka-system delete serviceaccount,role,rolebinding native-history-client
}
exercise_acp_failed_recovery() {
  log "Checking Actor loss settles once and retains explicit recovery data"
  local workspace="$1" pool="$2" digest actor actor_uid workspace_uid start restart_epoch
  digest="$(journal_for_pool "${pool}" | jq -er '.checkpoint.digest')"
  submit_task native-lost native-session 'ORKA_HOLD_120S Reply exactly: ORKA_NATIVE_LOST_OK'
  wait_fixture_request native-lost ORKA_NATIVE_LOST_OK
  actor="$(journal_for_pool "${pool}" | jq -er '.attempt.name')"
  actor_uid="$(journal_for_pool "${pool}" | jq -er '.attempt.uid')"
  kubectl_ate get actors --atespace orka-system -o json |
    jq -e --arg name "${actor}" --arg uid "${actor_uid}" \
      '.actors | length == 1 and .[0].metadata.name == $name and .[0].metadata.uid == $uid' >/dev/null
  # Use the official API to terminate exactly the admitted Actor. Upstream
  # can report filesystem cleanup failure after terminating its workload;
  # the observed Task, journal, and compute state below determine success.
  if ! kubectl_ate delete actor "${actor}" --atespace orka-system --any-state >/dev/null 2>&1; then
    log "Provider deletion returned an error; checking the observed runtime loss"
  fi
  wait_field task native-lost '
    .status.phase == "Failed" and .status.execution.state == "OutcomeUnknown" and
    .status.execution.outcome == "OutcomeUnknown" and .status.execution.attempt == 1' true
  kubectl -n orka-system wait task/native-lost \
    --for=jsonpath='{.metadata.annotations.acp\.workspace\.orka\.ai/workspace-settled}'=true --timeout=5m
  wait_field executionworkspace "${workspace}" '.status.state' Failed
  start=$(date +%s)
  until journal_for_pool "${pool}" | jq -e '.failure != "" and .phase == "Failed" and .attempt == null' >/dev/null; do
    (( $(date +%s) - start < 300 )) || { echo 'failed native attempt did not release compute' >&2; return 1; }
    sleep 2
  done
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  [[ "$(journal_for_pool "${pool}" | jq -er '.checkpoint.digest')" == "${digest}" ]]
  workspace_uid="$(kubectl -n orka-system get executionworkspace "${workspace}" -o jsonpath='{.metadata.uid}')"
  jq -n --arg name "${workspace}" --arg uid "${workspace_uid}" '{apiVersion:"workspace.orka.ai/v1alpha1",kind:"ExecutionWorkspaceCheckpoint",metadata:{name:"native-recovery-unconfirmed",namespace:"orka-system"},spec:{workspaceRef:{name:$name,uid:$uid}}}' | kubectl create -f -
  wait_field executionworkspacecheckpoint native-recovery-unconfirmed '
    .status.phase == "Pending" and (.status.digest // "") == "" and
    any(.status.conditions[]?; .reason == "AwaitingSuspension")' true
  restart_epoch="$(kubectl -n orka-system get runtimepool "${pool}" -o jsonpath='{.status.controllerEpoch}')"
  kubectl -n orka-system rollout restart deployment/orka-controller-manager
  kubectl -n orka-system rollout status deployment/orka-controller-manager --timeout=5m
  kubectl -n orka-system get leases -o json | jq -e --argjson epoch "${restart_epoch}" '
    any(.items[];
      (.metadata.annotations["core.orka.ai/acp-upgrade-drain-marker"] // "{}" | fromjson) |
      .controllerEpoch == $epoch and .state == "Completed")' >/dev/null
  assert_fixture_count ORKA_NATIVE_LOST_OK 1
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  jq -n --arg name "${workspace}" --arg uid "${workspace_uid}" '{apiVersion:"workspace.orka.ai/v1alpha1",kind:"ExecutionWorkspaceCheckpoint",metadata:{name:"native-recovery-save",namespace:"orka-system"},spec:{workspaceRef:{name:$name,uid:$uid},recoverLastCheckpoint:true}}' | kubectl create -f -
  wait_field executionworkspacecheckpoint native-recovery-save '.status.phase' Ready
  [[ "$(kubectl -n orka-system get executionworkspacecheckpoint native-recovery-save -o jsonpath='{.status.digest}')" == "${digest}" ]]
  kubectl -n orka-system delete executionworkspacecheckpoint native-recovery-unconfirmed
}
create_lifetime_workspace_class() {
  kubectl -n orka-system get executionworkspaceclass native-substrate -o json |
    jq '{apiVersion, kind, metadata: {name: "native-lifetime", namespace: .metadata.namespace}, spec} |
      .spec.lifecycle.maxLifetime = "120s"' | kubectl -n orka-system create -f - || return 1
  wait_field executionworkspaceclass native-lifetime '.status.conditions[]? | select(.type=="Ready") | .status' True
}
create_lifetime_fixture_access() {
  # Normal worker egress permits the authenticated model proxy. This test's
  # shell command also needs one direct connection to the local fixture.
  jq -n '{apiVersion:"networking.k8s.io/v1",kind:"NetworkPolicy",
    metadata:{name:"native-lifetime-fixture",namespace:"ate-demo"},
    spec:{podSelector:{matchLabels:{"ate.dev/worker-pool":"orka-native"}},policyTypes:["Egress"],
      egress:[{to:[{namespaceSelector:{matchLabels:{"kubernetes.io/metadata.name":"vekil-system"}},
        podSelector:{matchLabels:{"app.kubernetes.io/name":"vekil","app.kubernetes.io/component":"responses-fixture"}}}],
        ports:[{protocol:"TCP",port:1337}]}]}}' | kubectl -n ate-demo create -f - || return 1
}
exercise_acp_lifetime() {
  log "Checking workspace maxLifetime stops an active shell command"
  # Keep the Task timeout and the command's 300-second hold longer than the
  # workspace's lifetime. Only natural workspace expiry may stop this Task.
  create_lifetime_workspace_class
  create_lifetime_fixture_access
  submit_task native-lifetime lifetime-session 'ORKA_HOLD_300S Run the held shell command. Reply exactly: ORKA_NATIVE_LIFETIME_OK' 15m 2 native-lifetime
  wait_fixture_request native-lifetime ORKA_NATIVE_LIFETIME_TOOL_OK

  local task workspace pool worker deadline remaining pod_uid original_prompt original_uid
  task="$(kubectl -n orka-system get task native-lifetime -o json)"
  jq -e '.status.phase == "Running" and .status.execution.attempt == 1' <<<"${task}" >/dev/null
  original_prompt="$(jq -er '.status.execution.promptID | select(length > 0)' <<<"${task}")"
  original_uid="$(jq -er '.metadata.uid' <<<"${task}")"
  workspace="$(workspace_for_task native-lifetime)"
  pool="$(pool_for_task native-lifetime)"
  worker="$(journal_for_pool "${pool}" | jq -ec '.attempt.worker |
    select([.namespace, .pod, .podUID] | all(.[]; type == "string" and length > 0))')"
  pod_uid="$(kubectl -n "$(jq -r '.namespace' <<<"${worker}")" get pod "$(jq -r '.pod' <<<"${worker}")" -o jsonpath='{.metadata.uid}')"
  [[ "${pod_uid}" == "$(jq -r '.podUID' <<<"${worker}")" ]]
  deadline="$(kubectl -n orka-system get executionworkspace "${workspace}" -o json |
    jq -er '(.metadata.creationTimestamp | fromdateiso8601) + 120')"
  # Prove the command started before expiry. Bound cancellation and worker
  # removal to 45 seconds after expiry, well before the command can finish.
  (( $(date +%s) < deadline ))
  remaining=$((deadline + 45 - $(date +%s)))
  wait_field task native-lifetime '
    .status.phase == "Cancelled" and .status.execution.state == "Cancelled" and
    .status.execution.outcome == "Cancelled" and .status.execution.reason == "TaskTimeout" and
    .status.execution.attempt == 1' true "${remaining}"
  (( $(date +%s) >= deadline )) || { echo 'Task cancelled before its workspace lifetime expired' >&2; return 1; }
  remaining=$((deadline + 45 - $(date +%s)))
  (( remaining > 0 ))
  wait_fixture_disconnect ORKA_NATIVE_LIFETIME_TOOL_OK "${remaining}"
  fixture_read /fixture/marker-observations |
    jq -e --arg key "$(fixture_key ORKA_NATIVE_LIFETIME_TOOL_OK)" --argjson deadline "${deadline}" '
      .[$key].disconnectedAtUnixMilli >= ($deadline * 1000)' >/dev/null
  while true; do
    (( $(date +%s) <= deadline + 45 )) || { echo 'workspace lifetime left its worker running' >&2; return 1; }
    pod_uid="$(kubectl -n "$(jq -r '.namespace' <<<"${worker}")" --request-timeout=5s get pod "$(jq -r '.pod' <<<"${worker}")" --ignore-not-found -o jsonpath='{.metadata.uid}')" || return 1
    [[ -n "${pod_uid}" ]] || break
    [[ "${pod_uid}" == "$(jq -r '.podUID' <<<"${worker}")" ]] || { echo 'workspace worker identity changed during expiry' >&2; return 1; }
    sleep 2
  done
  (( $(date +%s) <= deadline + 45 )) || { echo 'workspace worker cleanup exceeded the lifetime allowance' >&2; return 1; }
  kubectl -n orka-system get task native-lifetime -o json |
    jq -e --arg uid "${original_uid}" --arg prompt "${original_prompt}" '
      .metadata.uid == $uid and .status.execution.promptID == $prompt and
      .status.phase == "Cancelled" and .status.execution.attempt == 1' >/dev/null
  local requests
  requests="$(fixture_read /fixture/marker-counts | jq -er --arg key "$(fixture_key ORKA_NATIVE_LIFETIME_OK)" '.[$key]')"
  # shell_command stays blocked; exec_command can return a running session
  # and make one more held model request. Neither may start the command again.
  [[ "${requests}" == 1 || "${requests}" == 2 ]]
  assert_fixture_count ORKA_NATIVE_LIFETIME_TOOL_OK 1
  log "Verified workspace maxLifetime cancelled the original prompt and removed its worker before the shell command could finish"
  delete_native_session lifetime-session
  wait_absent executionworkspace "${workspace}"
  wait_absent runtimepool "${pool}"
  cleanup_acp_workspaces
  kubectl -n ate-demo delete networkpolicy native-lifetime-fixture
  kubectl -n orka-system delete executionworkspaceclass native-lifetime
}
cleanup_acp_workspaces() {
  [[ "${KUBECONFIG}" == "${TMP_ROOT}/kubeconfig" && "${KIND_CLUSTER}" == "${KIND_CLUSTER_NAME}" ]]
  kubectl -n orka-system delete executionworkspaces --all --wait=false
  kubectl -n orka-system wait --for=delete executionworkspaces --all --timeout=600s
  kubectl -n orka-system wait --for=delete runtimepools --all --timeout=600s
  local start
  start=$(date +%s)
  while (( $(kubectl -n orka-system get configmaps -l orka.ai/substrate-checkpoint-catalog=true -o json | jq '.items|length') != 0 )); do
    (( $(date +%s) - start < 180 )) || { echo 'checkpoint cleanup did not settle' >&2; return 1; }
    sleep 2
  done
  [[ "$(kubectl_ate get actors --atespace orka-system -o json | jq '.actors|length')" == 0 ]]
  [[ "$(kubectl_ate get tags --atespace orka-system -o json | jq '.tags|length')" == 0 ]]
}
wait_acp_canary_task() {
  # These credential-free Tasks have read intent. Codex must execute the file
  # operation, while delivery must reject its unpublished changes. Checkpoint
  # recovery preserves that failed work without bypassing workspace governance.
  wait_field task "$1" '
    .status.phase == "Failed" and .status.execution.state == "Succeeded" and
    .status.execution.attempt == 1 and .status.delivery.state == "ReadOnlyWorkspaceModified"' true
  assert_fixture_canary "$2"
}
exercise_acp_checkpoint_data() {
  log "Checking ACP checkpoint file recovery and read-only delivery enforcement"
  local workspace workspace_uid checkpoint_uid digest
  # Each file operation needs one model request for the shell call and one
  # for the fixture to verify its output. Keep the lifecycle Tasks at one.
  submit_task native-data-write native-data-session 'Write the checkpoint canary file using the shell. Reply exactly: ORKA_NATIVE_DATA_WRITE_OK' 15m 2
  wait_acp_canary_task native-data-write ORKA_NATIVE_DATA_WRITE_OK
  workspace="$(workspace_for_task native-data-write)"
  wait_field executionworkspace "${workspace}" '.status.state' Suspended
  workspace_uid="$(kubectl -n orka-system get executionworkspace "${workspace}" -o jsonpath='{.metadata.uid}')"
  jq -n --arg name "${workspace}" --arg uid "${workspace_uid}" '{apiVersion:"workspace.orka.ai/v1alpha1",kind:"ExecutionWorkspaceCheckpoint",metadata:{name:"native-data-save",namespace:"orka-system"},spec:{workspaceRef:{name:$name,uid:$uid}}}' | kubectl create -f -
  wait_field executionworkspacecheckpoint native-data-save '.status.phase' Ready
  submit_task native-data-change native-data-session 'Read the checkpoint canary file, then change it using the shell. Reply exactly: ORKA_NATIVE_DATA_CHANGE_OK' 15m 2
  wait_acp_canary_task native-data-change ORKA_NATIVE_DATA_CHANGE_OK
  wait_field executionworkspace "${workspace}" '.status.state' Suspended
  checkpoint_uid="$(kubectl -n orka-system get executionworkspacecheckpoint native-data-save -o jsonpath='{.metadata.uid}')"
  digest="$(kubectl -n orka-system get executionworkspacecheckpoint native-data-save -o jsonpath='{.status.digest}')"
  delete_native_session native-data-session
  kubectl -n orka-system delete executionworkspace "${workspace}" --wait=false
  wait_absent executionworkspace "${workspace}"
  jq -n --arg uid "${checkpoint_uid}" --arg digest "${digest}" '{apiVersion:"core.orka.ai/v1alpha1",kind:"Task",metadata:{name:"native-data-restore",namespace:"orka-system"},spec:{type:"agent",agentRef:{name:"native-substrate"},agentRuntime:{maxTurns:2},timeout:"15m",execution:{workspace:{classRef:{name:"native-substrate"},onDetach:"Delete",restoreFrom:{name:"native-data-save",uid:$uid,digest:$digest}}},prompt:"Read the checkpoint canary file using the shell. Reply exactly: ORKA_NATIVE_DATA_READ_OK"}}' | kubectl create -f -
  wait_acp_canary_task native-data-restore ORKA_NATIVE_DATA_READ_OK
  log "Verified ACP checkpoint restores saved file bytes after source deletion"
  kubectl -n orka-system delete executionworkspacecheckpoint native-data-save
  cleanup_acp_workspaces
}
exercise_direct() {
  local image="$1"
  kubectl -n orka-system apply -f - <<YAML
apiVersion: batch/v1
kind: Job
metadata: {name: substrate-direct-conformance}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 600
  template:
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext: {fsGroup: 65532}
      containers:
      - name: conformance
        image: ${image}
        volumeMounts:
        - {name: bootstrap, mountPath: /run/orka-bootstrap, readOnly: true}
        - {name: substrate-client, mountPath: /run/substrate-client, readOnly: true}
        - {name: substrate-server, mountPath: /run/substrate-server, readOnly: true}
      volumes:
      - name: bootstrap
        secret: {secretName: orka-substrate-bootstrap}
      - name: substrate-client
        projected:
          sources:
          - podCertificate:
              signerName: podidentity.podcert.ate.dev/identity
              keyType: ECDSAP256
              credentialBundlePath: credential-bundle.pem
      - name: substrate-server
        projected:
          sources:
          - clusterTrustBundle:
              signerName: servicedns.podcert.ate.dev/identity
              labelSelector: {matchLabels: {podcert.ate.dev/canarying: live}}
              path: trust-bundle.pem
YAML
  wait_job substrate-direct-conformance 600
  kubectl -n orka-system logs job/substrate-direct-conformance | grep -Fxq 'native direct workspace conformance passed'
  kubectl -n orka-system delete job substrate-direct-conformance
}
exercise_mcp() {
  local image="$1"
  kubectl -n orka-system apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: SubstrateActorPool
metadata: {name: native-mcp}
spec:
  templateRef: {name: orka-mcp, namespace: orka-system}
  targetActors: 1
  precreateActors: true
---
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata: {name: native-mcp}
spec:
  description: Native Substrate MCP conformance
  parameters: {type: object, properties: {message: {type: string}}, required: [message]}
  mcp:
    path: /mcp
    substrateActor:
      templateRef: {name: orka-mcp, namespace: orka-system}
      poolRef: {name: native-mcp}
      boot: true
YAML
  wait_field tool native-mcp '.status.available' true
  kubectl -n orka-system apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata: {name: native-mcp-client}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: native-mcp-client}
rules:
- apiGroups: [core.orka.ai]
  resources: [tools]
  resourceNames: [native-mcp]
  verbs: [get]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: native-mcp-client}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: native-mcp-client}
subjects: [{kind: ServiceAccount, name: native-mcp-client, namespace: orka-system}]
---
apiVersion: batch/v1
kind: Job
metadata: {name: native-mcp-client}
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 180
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: native-mcp-client
      containers:
      - name: tool-client
        image: ${image}
        env:
        - {name: ORKA_TOOL_NAMESPACE, value: orka-system}
        - {name: ORKA_TOOL_NAME, value: native-mcp}
        - {name: ORKA_TOOL_ARGS, value: '{"message":"native"}'}
        - {name: ORKA_TOOL_EXPECT_RESULT, value: 'mcp-e2e-ok:native-mcp:native'}
YAML
  wait_job native-mcp-client 180
  kubectl -n orka-system delete tool native-mcp --wait=false
  wait_absent tool native-mcp
  kubectl -n orka-system delete substrateactorpool native-mcp --wait=false
  wait_absent substrateactorpool native-mcp
  kubectl -n orka-system delete job,role,rolebinding,serviceaccount native-mcp-client
}
main() {
  for command in docker git go jq kind ko kubectl openssl python3 curl; do command -v "${command}" >/dev/null || { echo "${command} is required" >&2; return 1; }; done
  mkdir -p "${TMP_ROOT}/docker-config"
  chmod 700 "${TMP_ROOT}"
  export DOCKER_CONFIG="${TMP_ROOT}/docker-config"
  printf '{"auths":{}}\n' >"${DOCKER_CONFIG}/config.json"
  trap cleanup EXIT
  substrate_prepare_upstream "${ROOT_DIR}" "${TMP_ROOT}" "${KIND_CLUSTER}"
  CLUSTER_PREPARED=1
  (cd "${SUBSTRATE_DIR}" && go build -o "${TMP_ROOT}/kubectl-ate" ./cmd/kubectl-ate)
  local controller direct mcp client fixture conformance acp worker public_key registry_ip
  controller="$(build_image controller Dockerfile)"
  direct="$(build_image workspace-agent cmd/orka-workspace-agent/Dockerfile)"
  mcp="$(build_image mcp-e2e-server cmd/orka-mcp-e2e-server/Dockerfile)"
  client="$(build_image tool-e2e-client cmd/orka-tool-e2e-client/Dockerfile)"
  fixture="$(build_image responses-fixture scripts/fixtures/openai-responses/Dockerfile)"
  conformance="$(build_image substrate-conformance scripts/fixtures/substrate-conformance/Dockerfile)"
  acp="localhost:${KIND_REGISTRY_PORT}/orka/acp-codex-runtime:native-conformance"
  make -C "${ROOT_DIR}" docker-build-acp-codex-runtime ACP_CODEX_RUNTIME_IMG="${acp}"
  docker push "${acp}"
  acp="$(docker inspect --format '{{index .RepoDigests 0}}' "${acp}")"
  registry_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kind-registry)"
  [[ -n "${registry_ip}" ]] || { echo 'shared registry is not reachable from kind' >&2; return 1; }
  # Actor image pulls happen inside gVisor and use the registry's kind-network address.
  direct="${direct/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  mcp="${mcp/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  acp="${acp/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  openssl rand -hex 32 >"${TMP_ROOT}/bootstrap-token"
  chmod 600 "${TMP_ROOT}/bootstrap-token"
  public_key="$(cd "${ROOT_DIR}" && go run ./cmd/orka-substrate-doctor --public-key-from="${TMP_ROOT}/bootstrap-token")"
  worker="$(publish_ateom_image)"
  create_native_resources "${worker}" "${direct}" "${mcp}" "${public_key}"
  kubectl -n orka-system create secret generic orka-substrate-bootstrap --from-file="token=${TMP_ROOT}/bootstrap-token" --dry-run=client -o yaml | kubectl apply -f -
  deploy_responses_fixture "${fixture}"
  deploy_orka "${controller}" "${acp}"
  exercise_direct "${conformance}"
  exercise_mcp "${client}"
  create_workspace_class
  exercise_acp_lifecycle
  exercise_acp_checkpoint_data
  exercise_acp_lifetime
  substrate_require_clean_upstream "${SUBSTRATE_DIR}"
  log 'Unmodified upstream Substrate conformance passed'
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
