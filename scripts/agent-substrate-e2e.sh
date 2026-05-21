#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
KIND_REGISTRY_NAME="${KIND_REGISTRY_NAME:-kind-registry}"
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-https://github.com/agent-substrate/substrate.git}"
SUBSTRATE_REF="${SUBSTRATE_REF:-b80031d260959b1fc5c6f61e3099fe2a6d368af1}"
IMAGE_TAG="${IMAGE_TAG:-agent-substrate-ci}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
TASK_TIMEOUT_SECONDS="${TASK_TIMEOUT_SECONDS:-900}"
SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED:-0}"

SUBSTRATE_DIR=""
TMP_ROOT=""
DOCKER_CONFIG_DIR=""
PORT_FORWARD_PID=""

log() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"
}

redact() {
  sed -E \
    -e 's/(Authorization:[[:space:]]*Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/(Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/([Tt]xn-[Tt]oken:[[:space:]]*)[^[:space:]]+/\1[REDACTED]/g' \
    -e 's/([Tt]oken["'\'']?[=:][[:space:]]*["'\'']?)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/g' \
    -e 's/(ORKA_WORKSPACE_HANDOFF_TOKEN=)[^[:space:]]+/\1[REDACTED]/g'
}

run_redacted() {
  set +e
  "$@" 2>&1 | redact
  local rc=${PIPESTATUS[0]}
  set -e
  return "${rc}"
}

kubectl_ate() {
  "${TMP_ROOT}/kubectl-ate" --context "kind-${KIND_CLUSTER}" "$@"
}

dump_diagnostics() {
  local rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    return 0
  fi

  log "Failure diagnostics"
  run_redacted kubectl get pods -A -o wide || true
  run_redacted kubectl -n orka-system get deployment,pods -o wide || true
  run_redacted kubectl -n orka-system get events --sort-by=.metadata.creationTimestamp || true
  run_redacted kubectl -n default get agents,tasks,jobs,pods -o wide || true
  run_redacted kubectl -n default get tasks -o yaml || true
  run_redacted kubectl -n orka-system logs deployment/orka-controller-manager --all-containers --tail=-1 || true

  for job in $(kubectl -n default get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
    log "Logs for job/${job}"
    run_redacted kubectl -n default logs "job/${job}" --all-containers --tail=-1 || true
  done

  run_redacted kubectl -n ate-system get pods,svc,deploy,daemonset,statefulset -o wide || true
  run_redacted kubectl -n ate-system logs deployment/ate-api-server-deployment --all-containers --tail=400 || true
  run_redacted kubectl -n ate-system logs deployment/ate-controller --all-containers --tail=400 || true
  run_redacted kubectl -n ate-system logs deployment/atenet-router --all-containers --tail=400 || true
  run_redacted kubectl -n ate-system logs daemonset/atelet --all-containers --tail=400 || true

  if [[ -x "${TMP_ROOT}/kubectl-ate" ]]; then
    run_redacted kubectl_ate get actors -o table || true
    run_redacted kubectl_ate get workers -o table || true
  fi

  return "${rc}"
}

cleanup() {
  if [[ -n "${PORT_FORWARD_PID}" ]]; then
    kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
  fi
  if [[ "${KEEP_CLUSTER}" != "1" ]]; then
    kind delete cluster --name "${KIND_CLUSTER}" >/dev/null 2>&1 || true
  else
    log "KEEP_CLUSTER=1, leaving kind cluster ${KIND_CLUSTER}"
  fi
  if [[ -n "${DOCKER_CONFIG_DIR}" ]]; then
    rm -rf "${DOCKER_CONFIG_DIR}"
  fi
  if [[ -n "${TMP_ROOT}" && "${KEEP_CLUSTER}" != "1" ]]; then
    rm -rf "${TMP_ROOT}"
  fi
}

trap dump_diagnostics ERR
trap cleanup EXIT

require_command() {
  local command="$1"
  command -v "${command}" >/dev/null 2>&1 || {
    echo "missing required command: ${command}" >&2
    exit 1
  }
}

wait_for_rollouts() {
  log "Waiting for Substrate control plane"
  kubectl -n ate-system rollout status deployment/ate-api-server-deployment --timeout=10m
  kubectl -n ate-system rollout status deployment/ate-controller --timeout=10m
  kubectl -n ate-system rollout status deployment/atenet-router --timeout=10m
  kubectl -n ate-system rollout status daemonset/atelet --timeout=10m
  kubectl -n ate-system rollout status statefulset/valkey-cluster --timeout=10m
  if kubectl -n ate-system get deployment/rustfs >/dev/null 2>&1; then
    kubectl -n ate-system rollout status deployment/rustfs --timeout=10m
  fi
}

ensure_snapshot_bucket() {
  log "Ensuring local Substrate snapshot bucket"
  kubectl -n ate-system delete pod/rustfs-bucket-init --ignore-not-found --wait=true >/dev/null
  kubectl -n ate-system run rustfs-bucket-init \
    --image=amazon/aws-cli:2.32.3 \
    --restart=Never \
    --env=AWS_ACCESS_KEY_ID=rustfsadmin \
    --env=AWS_SECRET_ACCESS_KEY=rustfsadmin \
    --env=AWS_DEFAULT_REGION=us-east-1 \
    --command -- /bin/sh -c \
    'aws --endpoint-url http://rustfs.ate-system.svc:9000 s3api head-bucket --bucket ate-snapshots >/dev/null 2>&1 || aws --endpoint-url http://rustfs.ate-system.svc:9000 s3api create-bucket --bucket ate-snapshots >/dev/null'
  kubectl -n ate-system wait --for=jsonpath='{.status.phase}'=Succeeded pod/rustfs-bucket-init --timeout=2m
  run_redacted kubectl -n ate-system logs pod/rustfs-bucket-init --tail=-1 || true
  kubectl -n ate-system delete pod/rustfs-bucket-init --ignore-not-found --wait=true >/dev/null
}

wait_jsonpath_equals() {
  local description="$1"
  local command="$2"
  local expected="$3"
  local timeout_seconds="$4"
  local started now value
  started="$(date +%s)"

  while true; do
    set +e
    value="$(eval "${command}" 2>/dev/null)"
    local rc=$?
    set -e
    if [[ "${rc}" -eq 0 && "${value}" == "${expected}" ]]; then
      log "${description}: ${expected}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for ${description}; expected ${expected}, got ${value:-<empty>}" >&2
      return 1
    fi
    sleep 5
  done
}

wait_actor_status() {
  local actor_name="$1"
  local expected="$2"
  local timeout_seconds="$3"
  local started now status
  started="$(date +%s)"

  while true; do
    status="$(kubectl_ate get actor "${actor_name}" -o json 2>/dev/null | jq -r '.actors[0].status // empty')"
    if [[ "${status}" == "${expected}" ]]; then
      log "actor/${actor_name}: ${expected}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for actor/${actor_name}; expected ${expected}, got ${status:-<empty>}" >&2
      return 1
    fi
    sleep 5
  done
}

wait_task_phase() {
  local task="$1"
  local expected="$2"
  local timeout_seconds="${3:-${TASK_TIMEOUT_SECONDS}}"
  local started now phase
  started="$(date +%s)"

  log "Waiting for task/${task} to become ${expected}"
  while true; do
    phase="$(kubectl -n default get task "${task}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "${phase}" in
      "${expected}")
        log "task/${task}: ${phase}"
        return 0
        ;;
      Succeeded|Failed)
        echo "task/${task} finished as ${phase}, expected ${expected}" >&2
        return 1
        ;;
    esac
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for task/${task}; current phase ${phase:-<empty>}" >&2
      return 1
    fi
    sleep 10
  done
}

assert_task_jsonpath() {
  local task="$1"
  local path="$2"
  local expected="$3"
  local actual
  actual="$(kubectl -n default get task "${task}" -o "jsonpath=${path}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "task/${task} expected ${path}=${expected}, got ${actual:-<empty>}" >&2
    exit 1
  fi
}

patch_substrate_kind_registry_script() {
  local script="${SUBSTRATE_DIR}/hack/create-kind-cluster.sh"
  sed -i.bak \
    -e 's|reg_name="kind-registry"|reg_name="${KIND_REGISTRY_NAME:-kind-registry}"|' \
    -e 's|reg_port="5001"|reg_port="${KIND_REGISTRY_PORT:-5001}"|' \
    "${script}"
  rm -f "${script}.bak"
  if ! grep -q "KIND_REGISTRY_PORT" "${script}"; then
    echo "failed to patch Substrate kind registry script for registry override" >&2
    exit 1
  fi
}

publish_ateom_image() {
  local published
  published="$(
    cd "${SUBSTRATE_DIR}"
    export DOCKER_CONFIG="${DOCKER_CONFIG_DIR}"
    export KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}"
    ko publish ./cmd/servers/ateom-gvisor
  )"
  published="$(printf '%s\n' "${published}" | tail -n1)"
  if [[ -z "${published}" ]]; then
    echo "ko did not return an ateom-gvisor image reference" >&2
    exit 1
  fi
  printf '%s' "${published}"
}

create_substrate_resources() {
  local ateom_image="$1"
  local workspace_actor_image="$2"

  log "Creating Substrate WorkerPool and ActorTemplate"
  kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
  kubectl apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-workers
  namespace: ate-demo
spec:
  replicas: 1
  ateomImage: ${ateom_image}
---
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: orka-codex-ci
  namespace: ate-demo
  labels:
    orka.ai/execution-workspace: "true"
    orka.ai/workspace-provider: substrate
  annotations:
    orka.ai/agent-runtimes: codex
    orka.ai/workspace-daemon-port: "80"
    orka.ai/workspace-protocol: http-json-v1
    orka.ai/workspace-staging-root: /app
spec:
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  containers:
  - name: workspace
    image: ${workspace_actor_image}
    command:
      - /orka-workspace-agent
    env:
      - name: ORKA_WORKSPACE_AGENT_LISTEN_ADDR
        value: ":80"
      - name: ORKA_WORKSPACE_HANDOFF_TOKEN_FILE
        value: /app/orka-workspace-handoff-token
    ports:
      - containerPort: 80
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  snapshotsConfig:
    location: gs://ate-snapshots/orka-codex-ci/
  runsc:
    amd64:
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
    arm64:
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
YAML

  wait_jsonpath_equals \
    "actortemplate/orka-codex-ci readiness" \
    "kubectl -n ate-demo get actortemplate orka-codex-ci -o jsonpath='{.status.phase}'" \
    "Ready" \
    900
}

deploy_orka() {
  local controller_image="$1"
  local codex_image="$2"
  local tmp_config
  tmp_config="$(mktemp -d "${TMP_ROOT}/orka-config.XXXXXX")"

  log "Regenerating manifests and installing Orka CRDs"
  make -C "${ROOT_DIR}" manifests generate
  make -C "${ROOT_DIR}" install
  make -C "${ROOT_DIR}" kustomize

  cp -R "${ROOT_DIR}/config" "${tmp_config}/config"
  (cd "${tmp_config}/config/manager" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  "${ROOT_DIR}/bin/kustomize" build "${tmp_config}/config/default" | kubectl apply -f -

  local patch
  patch="$(jq -cn \
    --arg codex_image "${codex_image}" \
    '{
      spec: {
        template: {
          spec: {
            containers: [
              {
                name: "manager",
                imagePullPolicy: "IfNotPresent",
                resources: {
                  requests: { cpu: "250m", memory: "256Mi" },
                  limits: { cpu: "2", memory: "1Gi" }
                },
                livenessProbe: {
                  httpGet: { path: "/healthz", port: 8081 },
                  initialDelaySeconds: 30,
                  periodSeconds: 20,
                  timeoutSeconds: 5,
                  failureThreshold: 6
                },
                readinessProbe: {
                  httpGet: { path: "/readyz", port: 8081 },
                  initialDelaySeconds: 10,
                  periodSeconds: 10,
                  timeoutSeconds: 5,
                  failureThreshold: 6
                },
                args: [
                  "--leader-elect",
                  "--health-probe-bind-address=:8081",
                  "--controller-url=http://orka-api.orka-system.svc:8080",
                  "--codex-worker-image=" + $codex_image,
                  "--execution-workspace-default-provider=substrate",
                  "--agent-sandbox-enabled=false",
                  "--substrate-enabled=true",
                  "--substrate-api-endpoint=api.ate-system.svc:443",
                  "--substrate-api-insecure-skip-verify=true",
                  "--substrate-router-url=http://atenet-router.ate-system.svc",
                  "--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev",
                  "--substrate-default-template=orka-codex-ci",
                  "--substrate-default-template-namespace=ate-demo",
                  "--substrate-claim-timeout=2m",
                  "--substrate-command-timeout=10m",
                  "--substrate-cleanup-policy=delete"
                ]
              }
            ]
          }
        }
      }
    }')"
  kubectl -n orka-system patch deployment orka-controller-manager --type=strategic -p "${patch}"
  kubectl -n orka-system rollout status deployment/orka-controller-manager --timeout=5m
}

create_agent() {
  log "Creating Codex Agent"
  kubectl apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: codex-substrate-ci
  namespace: default
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: gpt-5.4
  systemPrompt:
    inline: |
      You are a CI smoke-test agent. Run the requested command and stop.
YAML
}

apply_task() {
  local name="$1"
  local workspace_yaml="$2"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${name}
  namespace: default
spec:
  type: agent
  agentRef:
    name: codex-substrate-ci
  prompt: "Run the configured CLI and finish."
  timeout: 10m
  agentRuntime:
    maxTurns: 1
    allowBash: true
  env:
    - name: CODEX_CLI_PATH
      value: /bin/true
  execution:
    workspace:
${workspace_yaml}
YAML
}

exercise_orka_tasks() {
  create_agent

  log "Running default Substrate configuration task"
  apply_task "codex-substrate-default-ci" "      enabled: true"
  wait_task_phase "codex-substrate-default-ci" "Succeeded"
  assert_task_jsonpath "codex-substrate-default-ci" "{.status.executionWorkspace.provider}" "substrate"
  assert_task_jsonpath "codex-substrate-default-ci" "{.status.executionWorkspace.templateRef.name}" "orka-codex-ci"
  assert_task_jsonpath "codex-substrate-default-ci" "{.status.executionWorkspace.phase}" "Deleted"
  assert_task_jsonpath "codex-substrate-default-ci" "{.status.resultRef.available}" "true"

  if [[ "${SUBSTRATE_E2E_EXTENDED}" == "1" ]]; then
    log "Running retained-workspace task"
    apply_task "codex-substrate-retain-ci" "      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex-ci
        namespace: ate-demo
      cleanupPolicy: retain"
    wait_task_phase "codex-substrate-retain-ci" "Succeeded"
    assert_task_jsonpath "codex-substrate-retain-ci" "{.status.executionWorkspace.phase}" "Retained"
  fi

  log "Running missing-template negative task"
  apply_task "codex-substrate-missing-template-ci" "      enabled: true
      provider: substrate
      templateRef:
        name: orka-missing-template-ci
        namespace: ate-demo
      cleanupPolicy: delete"
  wait_task_phase "codex-substrate-missing-template-ci" "Failed" 300
  local message
  message="$(kubectl -n default get task codex-substrate-missing-template-ci -o jsonpath='{.status.message}')"
  if [[ "${message}" != *"not found"* ]]; then
    echo "missing-template task failed with unexpected message: ${message}" >&2
    exit 1
  fi
}

wait_http_ok() {
  local url="$1"
  local host_header="$2"
  local auth_header="${3:-}"
  local timeout_seconds="$4"
  local started now
  started="$(date +%s)"

  while true; do
    if [[ -n "${auth_header}" ]]; then
      if curl -fsS -H "Host: ${host_header}" -H "${auth_header}" "${url}" >/dev/null 2>&1; then
        return 0
      fi
    elif curl -fsS -H "Host: ${host_header}" "${url}" >/dev/null 2>&1; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for ${url} via Host ${host_header}" >&2
      return 1
    fi
    sleep 5
  done
}

exercise_direct_substrate() {
  local actor_name="orka-direct-ci"
  local host_header="${actor_name}.actors.resources.substrate.ate.dev"
  local token token_b64 response

  log "Running direct Substrate workspace-agent smoke"
  kubectl_ate create actor "${actor_name}" --template ate-demo/orka-codex-ci
  kubectl_ate resume actor "${actor_name}"

  kubectl -n ate-system port-forward svc/atenet-router 18082:80 >/tmp/orka-atenet-router-port-forward.log 2>&1 &
  PORT_FORWARD_PID="$!"
  sleep 3

  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "" 300
  token="$(printf 'ci-token-%s' "$(date +%s%N)")"
  token_b64="$(printf '%s' "${token}" | base64 | tr -d '\n')"
  curl -fsS \
    -H "Host: ${host_header}" \
    -H "Content-Type: application/json" \
    -X PUT \
    -d "{\"files\":[{\"path\":\"/app/orka-workspace-handoff-token\",\"data\":\"${token_b64}\",\"mode\":384}]}" \
    "http://127.0.0.1:18082/v1/files" >/dev/null

  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "Authorization: Bearer ${token}" 60
  response="$(curl -fsS \
    -H "Host: ${host_header}" \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    -d '{"command":["/bin/sh","-lc","printf direct-ok"]}' \
    "http://127.0.0.1:18082/v1/exec")"
  if [[ "$(jq -r '.exitCode' <<<"${response}")" != "0" || "$(jq -r '.stdout' <<<"${response}")" != "direct-ok" ]]; then
    echo "unexpected direct exec response" >&2
    jq -c '{exitCode,stdout,stderr}' <<<"${response}" | redact >&2
    exit 1
  fi

  kubectl_ate suspend actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_SUSPENDED" 300
  kubectl_ate delete actor "${actor_name}"

  kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
  PORT_FORWARD_PID=""
}

main() {
  require_command bash
  require_command curl
  require_command docker
  require_command git
  require_command go
  require_command jq
  require_command kind
  require_command ko
  require_command kubectl

  TMP_ROOT="$(mktemp -d)"
  DOCKER_CONFIG_DIR="$(mktemp -d)"
  printf '{"auths":{}}\n' > "${DOCKER_CONFIG_DIR}/config.json"
  SUBSTRATE_DIR="${TMP_ROOT}/substrate"

  log "Cloning Substrate ${SUBSTRATE_REF}"
  git clone --quiet "${SUBSTRATE_REPO}" "${SUBSTRATE_DIR}"
  git -C "${SUBSTRATE_DIR}" checkout --quiet "${SUBSTRATE_REF}"
  patch_substrate_kind_registry_script

  log "Creating kind cluster and installing Substrate"
  (
    cd "${SUBSTRATE_DIR}"
    export DOCKER_CONFIG="${DOCKER_CONFIG_DIR}"
    export KIND_CLUSTER_NAME="${KIND_CLUSTER}"
    export KIND_REGISTRY_NAME="${KIND_REGISTRY_NAME}"
    export KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT}"
    export KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}"
    hack/create-kind-cluster.sh
    hack/install-ate-kind.sh --deploy-ate-system
  )
  kubectl config use-context "kind-${KIND_CLUSTER}"
  wait_for_rollouts
  ensure_snapshot_bucket

  log "Building kubectl-ate"
  (cd "${SUBSTRATE_DIR}" && go build -o "${TMP_ROOT}/kubectl-ate" ./cmd/kubectl-ate)

  local registry_ip registry_addr controller_image codex_image workspace_push_image workspace_actor_image ateom_image
  registry_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${KIND_REGISTRY_NAME}")"
  if [[ -z "${registry_ip}" ]]; then
    registry_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{println .IPAddress}}{{end}}' "${KIND_REGISTRY_NAME}" | head -n1)"
  fi
  if [[ -z "${registry_ip}" ]]; then
    echo "could not determine registry IP for ${KIND_REGISTRY_NAME}" >&2
    exit 1
  fi
  registry_addr="localhost:${KIND_REGISTRY_PORT}"
  controller_image="${registry_addr}/orka/controller:${IMAGE_TAG}"
  codex_image="${registry_addr}/orka/agent-worker-codex:${IMAGE_TAG}"
  workspace_push_image="${registry_addr}/orka/workspace-agent-root:${IMAGE_TAG}"
  workspace_actor_image="${registry_ip}:5000/orka/workspace-agent-root:${IMAGE_TAG}"

  log "Building and pushing Orka images"
  docker build -t "${controller_image}" -f "${ROOT_DIR}/Dockerfile" "${ROOT_DIR}"
  docker build -t "${codex_image}" -f "${ROOT_DIR}/workers/agent/codex/Dockerfile.substrate-e2e" "${ROOT_DIR}"
  docker build -t "${workspace_push_image}" -f "${ROOT_DIR}/cmd/orka-workspace-agent/Dockerfile" "${ROOT_DIR}"
  docker push "${controller_image}"
  docker push "${codex_image}"
  docker push "${workspace_push_image}"

  log "Publishing Substrate ateom-gvisor image"
  ateom_image="$(publish_ateom_image)"
  create_substrate_resources "${ateom_image}" "${workspace_actor_image}"
  deploy_orka "${controller_image}" "${codex_image}"
  exercise_direct_substrate
  exercise_orka_tasks

  log "Agent Substrate E2E passed"
}

main "$@"
