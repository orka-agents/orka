#!/usr/bin/env bash
# MCP tool conformance against the unmodified official Substrate v1 pin.
set -Eeuo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ORKA_NAMESPACE=orka-system
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
TMP_ROOT="${SUBSTRATE_E2E_RUN_DIR:-${ROOT_DIR}/bin/substrate-e2e-${KIND_CLUSTER}}"
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
  if (( rc )) && [[ "${CLUSTER_PREPARED:-0}" == 1 ]]; then
    runtime_diagnostics
    kubectl get pods -A 2>/dev/null || true
    job_diagnostics native-mcp-client
    workload_logs ate-system -l app=ate-api-server
    workload_logs ate-system -l app=atenet-egress
    workload_logs ate-demo -l ate.dev/worker-pool=orka-native
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
  # Select diagnostic status fields; omit specs and native identity records.
  kubectl -n orka-system --request-timeout=15s get tools,substrateactorpools -o json 2>/dev/null |
    jq '[.items[] | {kind, name: .metadata.name, phase: .status.phase, state: .status.state,
      lifecycle: .status.lifecycle, message: .status.message,
      execution: (.status.execution | if . == null then null else {state, outcome, reason} end),
      conditions: [.status.conditions[]? | {type, status, reason, message}]}]' | redact >&2 || true
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
publish_worker_image() {
  (cd "${SUBSTRATE_DIR}" && KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}" ko build --platform="linux/$(go env GOARCH)" ./cmd/ateworker-gvisor)
}
build_image() {
  local name="$1" dockerfile="$2" ref
  ref="localhost:${KIND_REGISTRY_PORT}/orka/${name}:native-conformance"
  docker build -t "${ref}" -f "${ROOT_DIR}/${dockerfile}" "${ROOT_DIR}" >&2
  docker push "${ref}" >&2
  docker inspect --format '{{index .RepoDigests 0}}' "${ref}"
}
# One template-sized Actor: worker Pod limits leave room for exactly one
# (gVisor reserves 64Mi of the worker's memory limit).
NATIVE_ACTOR_CPU_MILLICORES=1000
NATIVE_ACTOR_MEMORY_BYTES=1073741824
native_template_manifest() {
  local name="$1" image="$2"
  jq -n --arg name "${name}" --arg image "${image}" \
    --argjson cpu "${NATIVE_ACTOR_CPU_MILLICORES}" --arg memory "${NATIVE_ACTOR_MEMORY_BYTES}" '
    {metadata:{atespace:"orka-system",name:$name},workerSelector:{matchLabels:{"orka.ai/native-pool":"conformance"}},
     containers:[{name:"server",image:$image,
       env:[{name:"ORKA_WORKSPACE_AGENT_LISTEN_ADDR",value:":80"}],
       wakeupProbe:{httpGet:{path:"/healthz",port:80}},
       securityContext:{capabilities:{drop:["ALL"],add:["NET_BIND_SERVICE","SETUID","SETGID","CHOWN","KILL"]}}}],
     resources:{limits:{cpuMillicores:$cpu,memoryBytes:$memory}},
     snapshotConfig:{preferredFidelity:"SNAPSHOT_FIDELITY_VOLUMES",goldenSnapshotConfig:{mode:"GOLDEN_SNAPSHOT_MODE_DISABLED"}}}
  '
}
# Orka's controller authenticates with its pod certificate. Grant it editor on
# its Atespace and global viewer for worker placement reads; it is never a
# bootstrap owner. "get access-policy" exits 0 with no output when a scope has
# no policy, and "create" refuses an existing one, so read the policy as JSON
# and either create it or merge the binding into it under its uid/version
# preconditions.
ensure_access_binding() {
  local role="$1" principal="$2" manifest="$3"
  shift 3
  local current
  current="$(kubectl_ate get access-policy "$@" -o json)"
  if [[ -z "${current}" ]]; then
    jq -n --arg role "${role}" --arg principal "${principal}" \
      '{metadata:{name:"default"},bindings:[{role:$role,members:[$principal]}]}' >"${manifest}"
    kubectl_ate create access-policy "$@" -f "${manifest}" >/dev/null
    return
  fi
  if jq -e --arg role "${role}" --arg principal "${principal}" \
    'any(.bindings[]?; .role == $role and any(.members[]?; . == $principal))' <<<"${current}" >/dev/null; then
    return
  fi
  jq --arg role "${role}" --arg principal "${principal}" '
    .bindings = ((.bindings // []) as $bindings |
      if any($bindings[]; .role == $role)
      then $bindings | map(if .role == $role then .members = ((.members // []) + [$principal]) else . end)
      else $bindings + [{role:$role,members:[$principal]}] end)' <<<"${current}" >"${manifest}"
  kubectl_ate update access-policy "$@" -f "${manifest}" >/dev/null
}
grant_orka_native_access() {
  local principal="user:spiffe://cluster.local/ns/${ORKA_NAMESPACE}/sa/orka-controller-manager"
  ensure_access_binding editor "${principal}" "${TMP_ROOT}/orka-atespace-policy.json" -a orka-system
  ensure_access_binding viewer "${principal}" "${TMP_ROOT}/orka-global-policy.json" --global
}
create_native_resources() {
  local worker_image="$1" mcp_image="$2"
  kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
  bash "${ROOT_DIR}/scripts/lib/ensure-static-mode-namespace.sh" kubectl "${ORKA_NAMESPACE}" harness-v2
  # Substrate's system NetworkPolicy admits API clients only from labeled namespaces.
  kubectl label namespace "${ORKA_NAMESPACE}" ate.dev/api-access=true --overwrite
  kubectl_ate get atespace orka-system >/dev/null 2>&1 || kubectl_ate create atespace orka-system
  grant_orka_native_access
  kubectl -n ate-demo apply -f - <<YAML
apiVersion: ate.dev/v1
kind: WorkerPool
metadata:
  name: orka-native
  labels: {orka.ai/native-pool: conformance}
spec:
  replicas: 3
  workerImage: ${worker_image}
  sandboxClasses: [{name: gvisor, configRef: {name: gvisor-default}}]
  template:
    resources:
      requests: {cpu: 250m, memory: 512Mi}
      limits: {cpu: "1", memory: 1088Mi}
YAML
  native_template_manifest orka-mcp "${mcp_image}" >"${TMP_ROOT}/orka-mcp.json"
  kubectl_ate create actor-template -f "${TMP_ROOT}/orka-mcp.json"
  kubectl -n ate-demo wait --for=jsonpath='{.status.readyReplicas}'=3 workerpool/orka-native --timeout=5m
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
        # Substrate MCP Tools are reached only through the authenticated orka-api
        # proxy; this client's ServiceAccount never talks to the router.
        - {name: ORKA_CONTROLLER_URL, value: http://orka-api.orka-system.svc:8080}
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
  local controller mcp client worker registry_ip
  controller="$(build_image controller Dockerfile)"
  mcp="$(build_image mcp-e2e-server cmd/orka-mcp-e2e-server/Dockerfile)"
  client="$(build_image tool-e2e-client cmd/orka-tool-e2e-client/Dockerfile)"
  registry_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' kind-registry)"
  [[ -n "${registry_ip}" ]] || { echo 'shared registry is not reachable from kind' >&2; return 1; }
  # Actor image pulls happen inside gVisor and use the registry's kind-network address.
  mcp="${mcp/localhost:${KIND_REGISTRY_PORT}/${registry_ip}:5000}"
  openssl rand -hex 32 >"${TMP_ROOT}/bootstrap-token"
  chmod 600 "${TMP_ROOT}/bootstrap-token"
  worker="$(publish_worker_image)"
  create_native_resources "${worker}" "${mcp}"
  kubectl -n orka-system create secret generic orka-substrate-bootstrap --from-file="token=${TMP_ROOT}/bootstrap-token" --dry-run=client -o yaml | kubectl apply -f -
  deploy_orka "${controller}"
  exercise_mcp "${client}"
  substrate_require_clean_upstream "${SUBSTRATE_DIR}"
  log 'Unmodified upstream Substrate MCP tool conformance passed'
}
if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then main "$@"; fi
