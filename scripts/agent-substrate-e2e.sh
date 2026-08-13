#!/usr/bin/env bash
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ORKA_NAMESPACE="${ORKA_NAMESPACE:-orka-system}"
KIND_REGISTRY_NAME="${KIND_REGISTRY_NAME:-kind-registry}"
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
SUBSTRATE_REPO="${SUBSTRATE_REPO:-https://github.com/agent-substrate/substrate.git}"
SUBSTRATE_REF="${SUBSTRATE_REF:-b80031d260959b1fc5c6f61e3099fe2a6d368af1}"
# Git blob IDs for the reviewed source files at the default pin. Every local
# evaluation patch verifies these immutable upstream objects before applying.
SUBSTRATE_ATELET_OCI_BLOB="a2ae14c0a264d8ff2fdc9527f5894901d913c0a4"
SUBSTRATE_ATENET_EXTPROC_IN_BLOB="317511845fef40b7602861383f7664e915215a69"
SUBSTRATE_ATENET_EXTPROC_IN_TEST_BLOB="09bb9a4c4e7d4f5c8185c41535ebcc40fc8ff57b"
SUBSTRATE_ATENET_ENVOY_RUNNER_BLOB="8d38be29f09a7ce23886b71a051586354c8413e5"
SUBSTRATE_ATENET_MANIFEST_BLOB="e309cad0a2e8435d1ed8dfd51ce347ab4f5a7521"
SUBSTRATE_ATEOM_GVISOR_BLOB="7d79dd0a26709599223ed848d1b8f1ea19641cf6"
SUBSTRATE_ATEOM_RUNSC_BLOB="6db499a549f2b6987a867b144e8d6b3828cad9ff"
SUBSTRATE_ATELET_CAPABILITY_PATCH="${ROOT_DIR}/hack/agent-substrate/atelet-root-supervisor-capabilities.patch"
SUBSTRATE_ATENET_REDACTION_PATCH="${ROOT_DIR}/hack/agent-substrate/atenet-router-authorization-redaction.patch"
SUBSTRATE_ATEOM_DELETE_RECOVERY_PATCH="${ROOT_DIR}/hack/agent-substrate/ateom-runsc-delete-recovery.patch"
IMAGE_TAG="${IMAGE_TAG:-agent-substrate-ci}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
TASK_TIMEOUT_SECONDS="${TASK_TIMEOUT_SECONDS:-900}"
SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED:-0}"
MCP_TOOL_EXEC_ATTEMPTS="${MCP_TOOL_EXEC_ATTEMPTS:-3}"
MCP_TOOL_EXEC_RETRY_DELAY_SECONDS="${MCP_TOOL_EXEC_RETRY_DELAY_SECONDS:-15}"
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME="${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME:-orka-substrate-bootstrap}"
SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY="${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY:-token}"
if [[ "${SUBSTRATE_BOOTSTRAP_TOKEN+x}" != "x" || -z "${SUBSTRATE_BOOTSTRAP_TOKEN}" ]]; then
  printf -v SUBSTRATE_BOOTSTRAP_TOKEN 'bootstrap-ci-%s-%s' "$(date +%s%N)" "${RANDOM}"
fi

SUBSTRATE_DIR=""
TMP_ROOT=""
DOCKER_CONFIG_DIR=""
PORT_FORWARD_PID=""
RUNSC_DELETE_INJECTION_NODE=""
RUNSC_DELETE_INJECTION_PATH=""

log() {
  printf '\n[%s] %s\n' "$(date -u +%H:%M:%S)" "$*"
}

# Shared redaction; the bootstrap token literal is substituted at call time.
# shellcheck source=scripts/lib/redact.sh
. "${ROOT_DIR}/scripts/lib/redact.sh"
ORKA_REDACT_SECRET_VARS=(SUBSTRATE_BOOTSTRAP_TOKEN)

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

restore_runsc_delete_injector() {
  local node="${RUNSC_DELETE_INJECTION_NODE:-}"
  local path="${RUNSC_DELETE_INJECTION_PATH:-}"
  if [[ -z "${node}" || -z "${path}" ]]; then
    return 0
  fi

  if ! docker exec "${node}" /bin/sh -ceu '
    path="$1"
    if [ -e "${path}.orka-real" ]; then
      rm -f "${path}"
      mv "${path}.orka-real" "${path}"
    fi
    rm -f "${path}.orka-delete-failure-observed"
  ' sh "${path}"; then
    return 1
  fi
  RUNSC_DELETE_INJECTION_NODE=""
  RUNSC_DELETE_INJECTION_PATH=""
}

dump_diagnostics() {
  local rc=$?
  if [[ "${rc}" -eq 0 ]]; then
    return 0
  fi

  log "Failure diagnostics"
  run_redacted kubectl get pods -A -o wide || true
  run_redacted kubectl -n "${ORKA_NAMESPACE}" get deployment,pods,agents,tasks,jobs -o wide || true
  run_redacted kubectl -n "${ORKA_NAMESPACE}" get events --sort-by=.metadata.creationTimestamp || true
  run_redacted kubectl -n "${ORKA_NAMESPACE}" get tasks -o yaml || true
  run_redacted kubectl -n "${ORKA_NAMESPACE}" logs deployment/orka-controller-manager --all-containers --tail=-1 || true

  for job in $(kubectl -n "${ORKA_NAMESPACE}" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
    log "Logs for job/${job}"
    run_redacted kubectl -n "${ORKA_NAMESPACE}" logs "job/${job}" --all-containers --tail=-1 || true
  done
  run_redacted kubectl -n "${ORKA_NAMESPACE}" get substrateactorpools,tools,leases -o wide || true
  run_redacted kubectl -n "${ORKA_NAMESPACE}" get substrateactorpools,tools,leases -o yaml || true

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
  restore_runsc_delete_injector >/dev/null 2>&1 || true
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

wait_jsonpath_int_at_least() {
  local description="$1"
  local command="$2"
  local minimum="$3"
  local timeout_seconds="$4"
  local started now value
  started="$(date +%s)"

  while true; do
    set +e
    value="$(eval "${command}" 2>/dev/null)"
    local rc=$?
    set -e
    if [[ "${rc}" -eq 0 && "${value}" =~ ^[0-9]+$ && "${value}" -ge "${minimum}" ]]; then
      log "${description}: ${value}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for ${description}; expected >= ${minimum}, got ${value:-<empty>}" >&2
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

wait_actor_absent() {
  local actor_name="$1"
  local timeout_seconds="$2"
  local started now output count rc observation
  started="$(date +%s)"
  observation="not checked"

  while true; do
    if output="$(kubectl_ate get actor "${actor_name}" -o json 2>&1)"; then
      rc=0
    else
      rc=$?
    fi
    if [[ "${rc}" -ne 0 ]] && grep -Fq -- "code = NotFound desc = Actor ${actor_name} not found" <<<"${output}"; then
      log "actor/${actor_name}: absent"
      return 0
    fi
    if [[ "${rc}" -eq 0 ]]; then
      if count="$(jq -er '
        if type != "object" then
          error("actor response is not an object")
        elif has("actors") then
          if (.actors | type) == "array" then (.actors | length) else error("actors is not an array") end
        elif length == 0 then
          0
        else
          error("missing actors array")
        end
      ' <<<"${output}" 2>/dev/null)"; then
        if [[ "${count}" == "0" ]]; then
          log "actor/${actor_name}: absent"
          return 0
        fi
        observation="actor query succeeded with ${count} result(s)"
      else
        observation="actor query succeeded with an invalid response"
      fi
    else
      observation="kubectl-ate failed with exit ${rc} without an actor NotFound response"
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for actor/${actor_name} to be absent; ${observation}" >&2
      return 1
    fi
    sleep 5
  done
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    shasum -a 256 | awk '{print $1}'
  fi
}

substrate_actor_pool_prefix() {
  local namespace="$1"
  local name="$2"
  local hash
  hash="$(printf '%s\0%s' "${namespace}" "${name}" | sha256_hex)"
  printf 'orka-p-%s' "${hash:0:24}"
}

wait_worker_absent() {
  local worker_name="$1"
  local timeout_seconds="$2"
  local started now count
  started="$(date +%s)"

  while true; do
    count="$(kubectl_ate get workers -o json 2>/dev/null | jq --arg worker "${worker_name}" '[.workers[]? | select(.workerPod == $worker)] | length' 2>/dev/null || true)"
    if [[ "${count}" == "0" ]]; then
      log "worker/${worker_name}: absent from Substrate store"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for worker/${worker_name} to leave the Substrate store" >&2
      return 1
    fi
    sleep 2
  done
}

wait_worker_count_at_least() {
  local minimum="$1"
  local timeout_seconds="$2"
  local started now count
  started="$(date +%s)"

  while true; do
    count="$(kubectl_ate get workers -o json 2>/dev/null | jq '[.workers[]? | select(.workerPool == "orka-workers")] | length' 2>/dev/null || true)"
    if [[ "${count}" =~ ^[0-9]+$ && "${count}" -ge "${minimum}" ]]; then
      log "Substrate worker count: ${count}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for at least ${minimum} registered Substrate workers; got ${count:-<empty>}" >&2
      return 1
    fi
    sleep 2
  done
}

assert_no_suspending_actors() {
  local count
  count="$(kubectl_ate get actors -o json | jq '[.actors[]? | select(.status == "STATUS_SUSPENDING")] | length')"
  if [[ "${count}" != "0" ]]; then
    echo "found ${count} Actor(s) stuck in STATUS_SUSPENDING" >&2
    return 1
  fi
  log "No Actors are stuck in STATUS_SUSPENDING"
}

wait_resource_absent() {
  local namespace="$1"
  local resource="$2"
  local name="$3"
  local timeout_seconds="$4"
  local started now
  started="$(date +%s)"

  while true; do
    if ! kubectl -n "${namespace}" get "${resource}" "${name}" >/dev/null 2>&1; then
      log "${resource}/${name}: absent"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for ${resource}/${name} in namespace ${namespace} to be absent" >&2
      return 1
    fi
    sleep 5
  done
}

wait_job_succeeded() {
  local job_name="$1"
  local timeout_seconds="$2"
  local started now succeeded failed
  started="$(date +%s)"

  while true; do
    succeeded="$(kubectl -n "${ORKA_NAMESPACE}" get "job/${job_name}" -o jsonpath='{.status.succeeded}' 2>/dev/null || true)"
    failed="$(kubectl -n "${ORKA_NAMESPACE}" get "job/${job_name}" -o jsonpath='{.status.failed}' 2>/dev/null || true)"
    if [[ "${succeeded}" =~ ^[1-9][0-9]*$ ]]; then
      log "job/${job_name}: Complete"
      return 0
    fi
    if [[ "${failed}" =~ ^[1-9][0-9]*$ ]]; then
      echo "job/${job_name} failed" >&2
      return 1
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for job/${job_name} to complete" >&2
      return 1
    fi
    sleep 5
  done
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

verify_substrate_source_blob() {
  local target="$1"
  local expected_blob="$2"
  local patch_context="$3"
  local actual_blob

  if ! actual_blob="$(git -C "${SUBSTRATE_DIR}" rev-parse "HEAD:${target}" 2>/dev/null)"; then
    echo "Substrate ref ${SUBSTRATE_REF} does not contain expected source ${target}" >&2
    exit 1
  fi
  if [[ "${actual_blob}" != "${expected_blob}" ]]; then
    echo "Substrate ref ${SUBSTRATE_REF} has unreviewed ${patch_context} context in ${target}" >&2
    echo "expected blob ${expected_blob}, got ${actual_blob}; review the provider contract before updating the patch" >&2
    exit 1
  fi
}

verify_patch_paths() {
  local patch_file="$1"
  local expected_paths="$2"
  local patch_paths

  patch_paths="$(git -C "${SUBSTRATE_DIR}" apply --numstat "${patch_file}" | cut -f3- | LC_ALL=C sort)"
  if [[ "${patch_paths}" != "${expected_paths}" ]]; then
    echo "reviewed patch ${patch_file} changes unexpected files: ${patch_paths:-<none>}" >&2
    exit 1
  fi
}

apply_reviewed_substrate_patch() {
  local label="$1"
  local patch_file="$2"
  local expected_paths="$3"
  local -a paths=()
  local path

  if [[ ! -f "${patch_file}" ]]; then
    echo "missing reviewed Substrate patch: ${patch_file}" >&2
    exit 1
  fi
  verify_patch_paths "${patch_file}" "${expected_paths}"
  if ! git -C "${SUBSTRATE_DIR}" apply --check --whitespace=error-all "${patch_file}"; then
    echo "reviewed Substrate ${label} patch no longer applies cleanly" >&2
    exit 1
  fi

  git -C "${SUBSTRATE_DIR}" apply --whitespace=error-all "${patch_file}"

  if ! git -C "${SUBSTRATE_DIR}" apply --reverse --check "${patch_file}"; then
    echo "failed to verify the applied Substrate ${label} patch" >&2
    exit 1
  fi
  while IFS= read -r path; do
    [[ -n "${path}" ]] && paths+=("${path}")
  done <<< "${expected_paths}"
  if ! git -C "${SUBSTRATE_DIR}" diff --check -- "${paths[@]}"; then
    echo "applied Substrate ${label} patch introduced an invalid diff" >&2
    exit 1
  fi
}

apply_substrate_workspace_agent_capability_patch() {
  local target="cmd/servers/atelet/oci.go"
  local changed_files checkout_status

  if [[ ! -f "${SUBSTRATE_ATELET_CAPABILITY_PATCH}" ]]; then
    echo "missing reviewed Substrate compatibility patch: ${SUBSTRATE_ATELET_CAPABILITY_PATCH}" >&2
    exit 1
  fi
  checkout_status="$(git -C "${SUBSTRATE_DIR}" status --porcelain --untracked-files=all)"
  if [[ -n "${checkout_status}" ]]; then
    echo "refusing to patch a dirty Substrate checkout" >&2
    exit 1
  fi
  verify_substrate_source_blob "${target}" "${SUBSTRATE_ATELET_OCI_BLOB}" "OCI capability"
  if ! git -C "${SUBSTRATE_DIR}" apply --check --whitespace=error-all "${SUBSTRATE_ATELET_CAPABILITY_PATCH}"; then
    echo "reviewed Substrate workspace-agent capability patch no longer applies cleanly" >&2
    exit 1
  fi

  git -C "${SUBSTRATE_DIR}" apply --whitespace=error-all "${SUBSTRATE_ATELET_CAPABILITY_PATCH}"

  if ! git -C "${SUBSTRATE_DIR}" apply --reverse --check "${SUBSTRATE_ATELET_CAPABILITY_PATCH}"; then
    echo "failed to verify the applied Substrate workspace-agent capability patch" >&2
    exit 1
  fi
  if ! git -C "${SUBSTRATE_DIR}" diff --check -- "${target}"; then
    echo "applied Substrate workspace-agent capability patch introduced an invalid diff" >&2
    exit 1
  fi
  changed_files="$(git -C "${SUBSTRATE_DIR}" diff --name-only)"
  if [[ "${changed_files}" != "${target}" ]]; then
    echo "Substrate capability patch changed unexpected files: ${changed_files:-<none>}" >&2
    exit 1
  fi
  for capability in CAP_SETGID CAP_SETUID; do
    if [[ "$(grep -Fc "\"${capability}\"" "${SUBSTRATE_DIR}/${target}" || true)" -ne 1 ]]; then
      echo "Substrate capability patch did not scope ${capability} to the workspace-agent supervisor" >&2
      exit 1
    fi
  done
  if [[ "$(grep -Fc 'os.Chmod(rootPath, 0o755)' "${SUBSTRATE_DIR}/${target}" || true)" -ne 1 ]]; then
    echo "Substrate capability patch did not make the workspace-agent rootfs traversable after credential drop" >&2
    exit 1
  fi

  log "Applied reviewed Substrate root-supervisor capability compatibility patch"
}

apply_substrate_atenet_authorization_redaction_patch() {
  local source="cmd/servers/atenet/app/router/extproc_in.go"
  local source_test="cmd/servers/atenet/app/router/extproc_in_test.go"
  local envoy_runner="cmd/servers/atenet/app/router/envoyrunner.go"
  local install_manifest="manifests/ate-install/atenet-router.yaml"
  local expected_paths
  expected_paths="$(printf '%s\n' "${envoy_runner}" "${source}" "${source_test}" "${install_manifest}" | LC_ALL=C sort)"

  verify_substrate_source_blob "${source}" "${SUBSTRATE_ATENET_EXTPROC_IN_BLOB}" "atenet request-metadata"
  verify_substrate_source_blob "${source_test}" "${SUBSTRATE_ATENET_EXTPROC_IN_TEST_BLOB}" "atenet request-metadata test"
  verify_substrate_source_blob "${envoy_runner}" "${SUBSTRATE_ATENET_ENVOY_RUNNER_BLOB}" "atenet Envoy runner logging"
  verify_substrate_source_blob "${install_manifest}" "${SUBSTRATE_ATENET_MANIFEST_BLOB}" "atenet install manifest logging"
  apply_reviewed_substrate_patch "atenet authorization-redaction" "${SUBSTRATE_ATENET_REDACTION_PATCH}" "${expected_paths}"

  if [[ "$(grep -Fc 'case ":method", ":path", ":authority", "host", "x-request-id":' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ]]; then
    echo "Substrate atenet patch did not install the reviewed request-metadata allowlist" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'if !isSafeRequestMetadataHeader(k) {' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ]]; then
    echo "Substrate atenet patch does not reject headers before retaining their values" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'url.ParseRequestURI(value)' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ]]; then
    echo "Substrate atenet patch did not sanitize the request target before logging it" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'if requestURI.Opaque != "" {' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ||
        "$(grep -Fc 'requestURI, err = url.Parse(value)' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ]]; then
    echo "Substrate atenet patch did not preserve absolute-form paths while discarding authority credentials" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'func sanitizeRequestAuthority(value string) string {' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ||
        "$(grep -Fc 'authority.User != nil' "${SUBSTRATE_DIR}/${source}" || true)" -ne 1 ]]; then
    echo "Substrate atenet patch did not reject credential-bearing authority values before logging" >&2
    exit 1
  fi
  if grep -Eq 'redactedHeaderValue|sanitizeRequestHeaderValue|case "authorization"' "${SUBSTRATE_DIR}/${source}"; then
    echo "Substrate atenet patch retained denylist-based request logging" >&2
    exit 1
  fi
  for target in "${envoy_runner}" "${install_manifest}"; do
    if grep -Fq 'upstream:debug,router:debug,ext_proc:debug' "${SUBSTRATE_DIR}/${target}"; then
      echo "Substrate atenet patch retained credential-bearing Envoy debug logging in ${target}" >&2
      exit 1
    fi
    grep -Fq 'upstream:info,router:info,ext_proc:info' "${SUBSTRATE_DIR}/${target}" || {
      echo "Substrate atenet patch did not install bounded Envoy component logging in ${target}" >&2
      exit 1
    }
  done

  log "Applied reviewed Substrate atenet authorization-redaction patch"
}

apply_substrate_ateom_delete_recovery_patch() {
  local service_source="cmd/servers/ateom-gvisor/ateom-gvisor.go"
  local runsc_source="cmd/servers/ateom-gvisor/runsc.go"
  local runsc_test="cmd/servers/ateom-gvisor/runsc_test.go"
  local expected_paths
  expected_paths="$(printf '%s\n' "${service_source}" "${runsc_source}" "${runsc_test}" | LC_ALL=C sort)"

  verify_substrate_source_blob "${service_source}" "${SUBSTRATE_ATEOM_GVISOR_BLOB}" "ateom checkpoint"
  verify_substrate_source_blob "${runsc_source}" "${SUBSTRATE_ATEOM_RUNSC_BLOB}" "runsc delete"
  if git -C "${SUBSTRATE_DIR}" cat-file -e "HEAD:${runsc_test}" 2>/dev/null; then
    echo "Substrate ref ${SUBSTRATE_REF} unexpectedly contains ${runsc_test}; review the local recovery patch" >&2
    exit 1
  fi
  apply_reviewed_substrate_patch "ateom runsc-delete recovery" "${SUBSTRATE_ATEOM_DELETE_RECOVERY_PATCH}" "${expected_paths}"

  if [[ "$(grep -Fc 'runscDeleteAttempts   = 4' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not install bounded runsc delete retries" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'failed closed while verifying container absence' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not retain the fail-closed absence postcondition" >&2
    exit 1
  fi
  local prepare_line checkpoint_line validate_line commit_line restore_prepare_line restore_network_move_line restore_line delete_line
  prepare_line="$(grep -n 'prepareCheckpointRecovery(checkpointPath, recoveryPath, expectedContainers)' "${SUBSTRATE_DIR}/${service_source}" | cut -d: -f1)"
  checkpoint_line="$(grep -n 'rcmd.cmdCheckpoint(ctx, "pause", checkpointPath)' "${SUBSTRATE_DIR}/${service_source}" | cut -d: -f1)"
  validate_line="$(grep -n 'rcmd.cmdValidateCheckpoint(ctx, recoveryPath)' "${SUBSTRATE_DIR}/${service_source}" | tail -n1 | cut -d: -f1)"
  commit_line="$(grep -n 'commitCheckpointRecovery(recoveryPath, expectedContainers)' "${SUBSTRATE_DIR}/${service_source}" | tail -n1 | cut -d: -f1)"
  if [[ -z "${prepare_line}" || -z "${checkpoint_line}" || -z "${validate_line}" || -z "${commit_line}" ||
        "${prepare_line}" -ge "${checkpoint_line}" || "${checkpoint_line}" -ge "${validate_line}" ||
        "${validate_line}" -ge "${commit_line}" ]]; then
    echo "Substrate ateom patch did not enforce prepared -> checkpointed -> validated -> committed recovery ordering" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'checkpointRecoveryCommitName' "${SUBSTRATE_DIR}/${service_source}" || true)" -lt 3 ]]; then
    echo "Substrate ateom patch did not install an explicit checkpoint commit record" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'checkpointRecoveryArtifact' "${SUBSTRATE_DIR}/${service_source}" || true)" -lt 4 ]]; then
    echo "Substrate ateom patch did not inventory checkpoint artifacts" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'func materializeCheckpointTransport(checkpointPath, recoveryPath string) error {' "${SUBSTRATE_DIR}/${service_source}" || true)" -ne 1 ||
        "$(grep -Fc 'compatibilityArtifacts := map[string]string{' "${SUBSTRATE_DIR}/${service_source}" || true)" -ne 1 ||
        "$(grep -Fc 'for artifact, marker := range compatibilityArtifacts {' "${SUBSTRATE_DIR}/${service_source}" || true)" -ne 1 ||
        "$(grep -Fc 'materializeCheckpointTransport(checkpointPath, recoveryPath)' "${SUBSTRATE_DIR}/${service_source}" || true)" -lt 2 ]]; then
    echo "Substrate ateom patch did not preserve the legacy three-object checkpoint transport view" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'func prepareCheckpointRestore(checkpointDir string) error {' "${SUBSTRATE_DIR}/${service_source}" || true)" -ne 1 ||
        "$(grep -Fc 'checkpointPagesCompatibilityMarker' "${SUBSTRATE_DIR}/${service_source}" || true)" -lt 3 ||
        "$(grep -Fc 'checkpointPagesMetadataCompatibilityMarker' "${SUBSTRATE_DIR}/${service_source}" || true)" -lt 3 ]]; then
    echo "Substrate ateom patch did not identify marked transport placeholders before compressed restore" >&2
    exit 1
  fi
  restore_prepare_line="$(grep -n 'prepareCheckpointRestore(checkpointDir)' "${SUBSTRATE_DIR}/${service_source}" | cut -d: -f1)"
  restore_network_move_line="$(grep -n 'netlink.LinkSetNsFd(eth0Link, int(s.interiorNetNS))' "${SUBSTRATE_DIR}/${service_source}" | tail -n1 | cut -d: -f1)"
  if [[ -z "${restore_prepare_line}" || -z "${restore_network_move_line}" ||
        "${restore_prepare_line}" -ge "${restore_network_move_line}" ]]; then
    echo "Substrate ateom patch did not validate restore artifacts before moving worker networking" >&2
    exit 1
  fi
  if [[ "$(grep -Fc '"-compression=flate-best-speed"' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not pin the validated single-file checkpoint format" >&2
    exit 1
  fi
  if grep -Fq '"-leave-running"' "${SUBSTRATE_DIR}/${runsc_source}"; then
    echo "Substrate ateom patch resumed the sandbox before checkpoint commit" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'func (r *runsc) cmdValidateCheckpoint' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not validate the stopped runsc statefile" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'func (r *runsc) containerNamesLocked' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ||
        "$(grep -Fc 'return r.containerNamesLocked(ctx)' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not guard direct runsc list children from the PID 1 reaper" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'filepath.Dir(recoveryPath),' "${SUBSTRATE_DIR}/${service_source}" || true)" -ne 1 ]]; then
    echo "Substrate ateom patch did not stage commit temporaries outside the inventoried recovery directory" >&2
    exit 1
  fi
  if [[ "$(grep -Fc 'json.Unmarshal(stdout.Bytes(), &state)' "${SUBSTRATE_DIR}/${runsc_source}" || true)" -ne 1 ]] ||
     grep -Fq 'cmd.Stderr = &stdout' "${SUBSTRATE_DIR}/${runsc_source}"; then
    echo "Substrate ateom patch did not isolate runsc state JSON from stderr diagnostics" >&2
    exit 1
  fi
  if grep -Fq 'os.Rename(checkpointPath, recoveryPath)' "${SUBSTRATE_DIR}/${service_source}"; then
    echo "Substrate ateom patch retained the post-checkpoint rename crash window" >&2
    exit 1
  fi
  local recovery_test
  local recovery_tests=(
    TestContainerNamesWaitsForReaperReadLock
    TestContainerStatusIgnoresStderrDiagnostics
    TestCmdCheckpointUsesStoppedSingleFileProtocol
    TestCheckpointRecoveryRejectsUnexpectedOrCorruptArtifacts
    TestCheckpointRecoveryReconcilesPreparationBeforeCheckpoint
    TestCheckpointRecoveryReconcilesUncommittedSuccessfulCheckpoint
    TestCheckpointRecoveryReconcilesInterruptedWrite
    TestPrepareCheckpointRestoreRemovesMarkedCompatibilityFiles
    TestPrepareCheckpointRestoreRecoversPartialCompatibilityCleanup
    TestPrepareCheckpointRestorePreservesNativeMultiFileSnapshot
    TestPrepareCheckpointRestoreRejectsPartialNativeState
  )
  for recovery_test in "${recovery_tests[@]}"; do
    if [[ "$(grep -Fc "func ${recovery_test}" "${SUBSTRATE_DIR}/${runsc_test}" || true)" -ne 1 ]]; then
      echo "Substrate ateom patch did not cover ${recovery_test}" >&2
      exit 1
    fi
  done

  restore_line="$(grep -n 'Restore the worker Pod network before fallible runsc cleanup' "${SUBSTRATE_DIR}/${service_source}" | cut -d: -f1)"
  delete_line="$(grep -n 'Delete all application containers' "${SUBSTRATE_DIR}/${service_source}" | cut -d: -f1)"
  if [[ -z "${restore_line}" || -z "${delete_line}" || "${restore_line}" -ge "${delete_line}" ]]; then
    echo "Substrate ateom patch did not restore worker networking before fallible delete cleanup" >&2
    exit 1
  fi

  log "Applied reviewed Substrate ateom runsc-delete recovery patch"
}

verify_reviewed_substrate_patch_set() {
  local expected_files changed_files
  expected_files="$(printf '%s\n' \
    cmd/servers/atelet/oci.go \
    cmd/servers/atenet/app/router/envoyrunner.go \
    cmd/servers/atenet/app/router/extproc_in.go \
    cmd/servers/atenet/app/router/extproc_in_test.go \
    cmd/servers/ateom-gvisor/ateom-gvisor.go \
    cmd/servers/ateom-gvisor/runsc.go \
    cmd/servers/ateom-gvisor/runsc_test.go \
    manifests/ate-install/atenet-router.yaml | LC_ALL=C sort)"
  changed_files="$(git -C "${SUBSTRATE_DIR}" status --short | sed -E 's/^.. //' | LC_ALL=C sort)"
  if [[ "${changed_files}" != "${expected_files}" ]]; then
    echo "reviewed Substrate patch set changed unexpected files: ${changed_files:-<none>}" >&2
    exit 1
  fi

  log "Running focused tests for the reviewed Substrate patches"
  (
    cd "${SUBSTRATE_DIR}"
    go test ./cmd/servers/atelet ./cmd/servers/atenet/app/router -count=1
    if [[ "$(go env GOOS)" == "linux" ]]; then
      go test ./cmd/servers/ateom-gvisor -count=1
    else
      # The pinned netlink dependency does not expose Linux family constants on
      # non-Linux hosts. Cross-compile the package here; Linux CI/live E2E runs
      # the injected delete-recovery tests.
      GOOS=linux GOARCH="$(go env GOARCH)" go test -c \
        -o "${TMP_ROOT}/ateom-gvisor-patch-tests" ./cmd/servers/ateom-gvisor
    fi
  )
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
  local mcp_actor_image="$3"

  log "Creating Substrate WorkerPool and ActorTemplate"
  kubectl create namespace ate-demo --dry-run=client -o yaml | kubectl apply -f -
  bash "${ROOT_DIR}/scripts/lib/ensure-static-mode-namespace.sh" \
    kubectl "${ORKA_NAMESPACE}" harness-v2
  for ns in ate-demo "${ORKA_NAMESPACE}"; do
    kubectl -n "${ns}" create secret generic "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}" \
      "--from-literal=${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY}=${SUBSTRATE_BOOTSTRAP_TOKEN}" \
      --dry-run=client -o yaml | kubectl apply -f -
  done
  kubectl apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: WorkerPool
metadata:
  name: orka-workers
  namespace: ate-demo
spec:
  replicas: 3
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
      - name: ORKA_WORKSPACE_BOOTSTRAP_TOKEN
        value: "${SUBSTRATE_BOOTSTRAP_TOKEN}"
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

  log "Creating Substrate MCP ActorTemplate"
  kubectl apply -f - <<YAML
apiVersion: ate.dev/v1alpha1
kind: ActorTemplate
metadata:
  name: orka-mcp-ci
  namespace: ${ORKA_NAMESPACE}
  labels:
    orka.ai/execution-workspace: "true"
    orka.ai/workspace-provider: substrate
  annotations:
    orka.ai/workspace-daemon-port: "80"
    orka.ai/workspace-protocol: http-json-v1
    orka.ai/workspace-staging-root: /app
spec:
  pauseImage: registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4
  containers:
  - name: workspace
    image: ${mcp_actor_image}
    command:
      - /orka-mcp-e2e-server
    env:
      - name: ORKA_WORKSPACE_AGENT_LISTEN_ADDR
        value: ":80"
      - name: ORKA_WORKSPACE_BOOTSTRAP_TOKEN
        valueFrom:
          secretKeyRef:
            name: ${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}
            key: ${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY}
    ports:
      - containerPort: 80
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  snapshotsConfig:
    location: gs://ate-snapshots/orka-mcp-ci/
  runsc:
    amd64:
      url: gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc
      sha256Hash: a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
    arm64:
      url: gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc
      sha256Hash: 1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9
YAML

  wait_jsonpath_equals \
    "actortemplate/orka-mcp-ci readiness" \
    "kubectl -n ${ORKA_NAMESPACE} get actortemplate orka-mcp-ci -o jsonpath='{.status.phase}'" \
    "Ready" \
    900
}

deploy_orka() {
  local controller_image="$1"
  local tmp_config
  tmp_config="$(mktemp -d "${TMP_ROOT}/orka-config.XXXXXX")"

  log "Regenerating manifests and installing Orka CRDs"
  make -C "${ROOT_DIR}" manifests generate
  make -C "${ROOT_DIR}" install
  make -C "${ROOT_DIR}" kustomize

  cp -R "${ROOT_DIR}/config" "${tmp_config}/config"
  (cd "${tmp_config}/config/manager" && "${ROOT_DIR}/bin/kustomize" edit set image "controller=${controller_image}")
  # Agent Substrate validation exercises the workspace provider directly. The
  # initial ACP release does not dispatch RuntimeSessions to Substrate Actors, so
  # omit the unrelated clean-room publisher workload and provide only the
  # controller's required non-secret image metadata and local capability files.
  (
    cd "${tmp_config}/config/acp-workload"
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../publisher
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../provider-proxy
    "${ROOT_DIR}/bin/kustomize" edit remove resource ../scm-egress-proxy
  )
  local placeholder_digest
  placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
  kubectl -n orka-system create configmap acp-runtime-images \
    --from-literal="ORKA_ACP_CODEX_RUNTIME_IMAGE=example.invalid/orka/acp-codex@${placeholder_digest}" \
    --from-literal="ORKA_ACP_CLAUDE_RUNTIME_IMAGE=example.invalid/orka/acp-claude@${placeholder_digest}" \
    --from-literal="ORKA_ACP_COPILOT_RUNTIME_IMAGE=example.invalid/orka/acp-copilot@${placeholder_digest}" \
    --from-literal="ORKA_ACP_OPENCODE_RUNTIME_IMAGE=example.invalid/orka/acp-opencode@${placeholder_digest}" \
    --dry-run=client -o yaml | kubectl apply -f -

  local capability_dir snapshot_key_field artifact_capability_field publisher_controller_field publisher_operation_field provider_field
  capability_dir="$(mktemp -d "${TMP_ROOT}/acp-capabilities.XXXXXX")"
  snapshot_key_field="snapshot-key"
  artifact_capability_field="capability-secret"
  publisher_controller_field="controller-token"
  publisher_operation_field="operation-capability-secret"
  provider_field="token"
  chmod 0700 "${capability_dir}"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/snapshot-key"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/artifact-capability"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/publisher-token"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/publisher-capability"
  dd if=/dev/urandom bs=32 count=1 2>/dev/null >"${capability_dir}/provider-token"
  chmod 0600 "${capability_dir}"/*
  kubectl -n orka-system create secret generic agent-execution-snapshot-key \
    --from-file="${snapshot_key_field}=${capability_dir}/snapshot-key" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic acp-artifact-capability \
    --from-file="${artifact_capability_field}=${capability_dir}/artifact-capability" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic workspace-publisher-auth \
    --from-file="${publisher_controller_field}=${capability_dir}/publisher-token" \
    --from-file="${publisher_operation_field}=${capability_dir}/publisher-capability" \
    --dry-run=client -o yaml | kubectl apply -f -
  kubectl -n orka-system create secret generic provider-auth-proxy \
    --from-file="${provider_field}=${capability_dir}/provider-token" \
    --dry-run=client -o yaml | kubectl apply -f -
  rm -rf "${capability_dir}"

  "${ROOT_DIR}/bin/kustomize" build "${tmp_config}/config/acp-workload" | kubectl apply -f -

  local patch
  patch="$(jq -cn \
    --arg bootstrap_secret_name "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_NAME}" \
    --arg bootstrap_secret_key "${SUBSTRATE_BOOTSTRAP_TOKEN_SECRET_KEY}" \
    '{
      spec: {
        template: {
          spec: {
            containers: [
              {
                name: "manager",
                # The Substrate-only deployment removes the Publisher workload.
                # Disable the client explicitly so fail-closed capability
                # negotiation does not target a Service that is intentionally
                # absent from this direct provider evaluation cluster.
                env: [
                  {
                    name: "ORKA_WORKSPACE_PUBLISHER_URL",
                    "$patch": "delete"
                  }
                ],
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
                  "--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key",
                  "--controller-url=http://orka-api.orka-system.svc:8080",
                  "--controller-mode=harness-v2",
                  "--watch-namespace=orka-system",
                  "--enforce-namespace-isolation=true",
                  "--execution-mode-controller-usernames=system:serviceaccount:orka-system:orka-controller-manager",
                  "--execution-workspace-default-provider=substrate",
                  "--agent-sandbox-enabled=false",
                  "--substrate-enabled=true",
                  "--substrate-api-endpoint=api.ate-system.svc:443",
                  "--substrate-api-insecure-skip-verify=true",
                  "--substrate-router-url=http://atenet-router.ate-system.svc",
                  "--substrate-actor-dns-suffix=actors.resources.substrate.ate.dev",
                  "--substrate-default-template=orka-codex-ci",
                  "--substrate-default-template-namespace=ate-demo",
                  "--substrate-bootstrap-token-secret-name=" + $bootstrap_secret_name,
                  "--substrate-bootstrap-token-secret-key=" + $bootstrap_secret_key,
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

create_substrate_actor_pools() {
  log "Creating Orka SubstrateActorPools"
  kubectl -n "${ORKA_NAMESPACE}" apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: SubstrateActorPool
metadata:
  name: mcp-substrate-pool-ci
spec:
  templateRef:
    name: orka-mcp-ci
  workerPoolRef:
    name: orka-workers
    namespace: ate-demo
  targetActors: 2
  targetWorkers: 1
  precreateActors: true
YAML

  wait_jsonpath_equals \
    "substrateactorpool/mcp-substrate-pool-ci readiness" \
    "kubectl -n ${ORKA_NAMESPACE} get substrateactorpool mcp-substrate-pool-ci -o jsonpath='{.status.phase}'" \
    "Ready" \
    600
  wait_jsonpath_int_at_least \
    "substrateactorpool/mcp-substrate-pool-ci actor count" \
    "kubectl -n ${ORKA_NAMESPACE} get substrateactorpool mcp-substrate-pool-ci -o jsonpath='{.status.actorCount}'" \
    2 \
    600
}

create_mcp_tool() {
  log "Creating pooled MCP Tool"
  kubectl -n "${ORKA_NAMESPACE}" apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Tool
metadata:
  name: mcp-ci
spec:
  description: E2E MCP tool backed by a durable Substrate actor.
  parameters:
    type: object
    properties:
      message:
        type: string
    required:
      - message
  mcp:
    path: /mcp
    substrateActor:
      templateRef:
        name: orka-mcp-ci
      poolRef:
        name: mcp-substrate-pool-ci
      boot: true
YAML

  wait_jsonpath_equals \
    "tool/mcp-ci availability" \
    "kubectl -n ${ORKA_NAMESPACE} get tool mcp-ci -o jsonpath='{.status.available}'" \
    "true" \
    600
  wait_jsonpath_equals \
    "tool/mcp-ci actor provider" \
    "kubectl -n ${ORKA_NAMESPACE} get tool mcp-ci -o jsonpath='{.status.actor.provider}'" \
    "substrate" \
    60
  wait_jsonpath_equals \
    "tool/mcp-ci poolRef" \
    "kubectl -n ${ORKA_NAMESPACE} get tool mcp-ci -o jsonpath='{.status.actor.poolRef.name}'" \
    "mcp-substrate-pool-ci" \
    60
}

run_mcp_tool_client_job() {
  local tool_client_image="$1"
  local job_name="${2:-mcp-tool-exec-ci}"
  local message="${3:-ci}"
  local expected attempt
  local args_json

  if [[ ! "${MCP_TOOL_EXEC_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]]; then
    echo "MCP_TOOL_EXEC_ATTEMPTS must be a positive integer, got ${MCP_TOOL_EXEC_ATTEMPTS}" >&2
    return 1
  fi
  if [[ ! "${MCP_TOOL_EXEC_RETRY_DELAY_SECONDS}" =~ ^[0-9]+$ ]]; then
    echo "MCP_TOOL_EXEC_RETRY_DELAY_SECONDS must be a non-negative integer, got ${MCP_TOOL_EXEC_RETRY_DELAY_SECONDS}" >&2
    return 1
  fi

  if [[ "$#" -ge 4 ]]; then
    expected="$4"
  else
    expected="mcp-e2e-ok:mcp-ci:${message}"
  fi
  args_json="$(jq -cn --arg message "${message}" '{message: $message}')"

  for ((attempt = 1; attempt <= MCP_TOOL_EXEC_ATTEMPTS; attempt++)); do
    log "Executing MCP Tool through worker ToolExecutor (attempt ${attempt}/${MCP_TOOL_EXEC_ATTEMPTS})"
    kubectl -n "${ORKA_NAMESPACE}" delete "job/${job_name}" --ignore-not-found --wait=true >/dev/null
    kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${job_name}
  namespace: ${ORKA_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${job_name}
  namespace: ${ORKA_NAMESPACE}
rules:
  - apiGroups:
      - core.orka.ai
    resources:
      - tools
    verbs:
      - get
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${job_name}
  namespace: ${ORKA_NAMESPACE}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${job_name}
subjects:
  - kind: ServiceAccount
    name: ${job_name}
    namespace: ${ORKA_NAMESPACE}
---
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job_name}
  namespace: ${ORKA_NAMESPACE}
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: ${job_name}
      restartPolicy: Never
      containers:
        - name: tool-client
          image: ${tool_client_image}
          imagePullPolicy: IfNotPresent
          env:
            - name: ORKA_TOOL_NAMESPACE
              value: ${ORKA_NAMESPACE}
            - name: ORKA_TOOL_NAME
              value: mcp-ci
            - name: ORKA_TOOL_ARGS
              value: '${args_json}'
            - name: ORKA_TOOL_EXPECT_RESULT
              value: '${expected}'
YAML
    if wait_job_succeeded "${job_name}" 300; then
      run_redacted kubectl -n "${ORKA_NAMESPACE}" logs "job/${job_name}" --all-containers --tail=-1
      return 0
    fi

    run_redacted kubectl -n "${ORKA_NAMESPACE}" logs "job/${job_name}" --all-containers --tail=-1 || true
    if (( attempt == MCP_TOOL_EXEC_ATTEMPTS )); then
      echo "job/${job_name} did not complete after ${MCP_TOOL_EXEC_ATTEMPTS} attempts" >&2
      return 1
    fi
    log "Retrying MCP Tool execution after ${MCP_TOOL_EXEC_RETRY_DELAY_SECONDS}s"
    sleep "${MCP_TOOL_EXEC_RETRY_DELAY_SECONDS}"
  done
}

mcp_tool_client_result() {
  local job_name="$1"
  kubectl -n "${ORKA_NAMESPACE}" logs "job/${job_name}" --all-containers --tail=-1 | redact | tail -n1
}

verify_mcp_tool_boots_actor_once() {
  local tool_client_image="$1"
  local actor_id booted_actor_id generation before after before_started before_count after_started after_count

  log "Verifying MCP Tool actor is booted once across forced reconcile"
  actor_id="$(kubectl -n "${ORKA_NAMESPACE}" get tool mcp-ci -o jsonpath='{.status.actor.actorID}')"
  booted_actor_id="$(kubectl -n "${ORKA_NAMESPACE}" get tool mcp-ci -o json | jq -r '.metadata.annotations["orka.ai/substrate-mcp-tool-booted-id"] // ""')"
  if [[ -z "${actor_id}" || "${booted_actor_id}" != "${actor_id}" ]]; then
    echo "tool/mcp-ci booted actor annotation = ${booted_actor_id:-<empty>}, want ${actor_id:-<empty>}" >&2
    exit 1
  fi

  run_mcp_tool_client_job "${tool_client_image}" "mcp-tool-state-before-ci" "boot-state" ""
  before="$(mcp_tool_client_result mcp-tool-state-before-ci)"
  if [[ ! "${before}" =~ ^mcp-e2e-state:mcp-ci:([0-9]+):([0-9]+)$ ]]; then
    echo "unexpected pre-reconcile MCP state response: ${before}" >&2
    exit 1
  fi
  before_started="${BASH_REMATCH[1]}"
  before_count="${BASH_REMATCH[2]}"

  generation="$(
    kubectl -n "${ORKA_NAMESPACE}" patch tool mcp-ci --type=merge \
      -p '{"spec":{"description":"E2E MCP tool backed by a durable Substrate actor after forced reconcile."}}' \
      -o json | jq -r '.metadata.generation'
  )"
  wait_jsonpath_equals \
    "tool/mcp-ci forced reconcile observed generation" \
    "kubectl -n ${ORKA_NAMESPACE} get tool mcp-ci -o json | jq -r '.status.conditions[]? | select(.type == \"Available\") | .observedGeneration'" \
    "${generation}" \
    120

  booted_actor_id="$(kubectl -n "${ORKA_NAMESPACE}" get tool mcp-ci -o json | jq -r '.metadata.annotations["orka.ai/substrate-mcp-tool-booted-id"] // ""')"
  if [[ "${booted_actor_id}" != "${actor_id}" ]]; then
    echo "tool/mcp-ci booted actor annotation after reconcile = ${booted_actor_id:-<empty>}, want ${actor_id}" >&2
    exit 1
  fi

  run_mcp_tool_client_job "${tool_client_image}" "mcp-tool-state-after-ci" "boot-state" ""
  after="$(mcp_tool_client_result mcp-tool-state-after-ci)"
  if [[ ! "${after}" =~ ^mcp-e2e-state:mcp-ci:([0-9]+):([0-9]+)$ ]]; then
    echo "unexpected post-reconcile MCP state response: ${after}" >&2
    exit 1
  fi
  after_started="${BASH_REMATCH[1]}"
  after_count="${BASH_REMATCH[2]}"
  if [[ "${after_started}" != "${before_started}" ]]; then
    echo "tool/mcp-ci actor process restarted across forced reconcile: before=${before}, after=${after}" >&2
    exit 1
  fi
  if (( after_count <= before_count )); then
    echo "tool/mcp-ci actor state did not advance across forced reconcile: before=${before}, after=${after}" >&2
    exit 1
  fi
  log "tool/mcp-ci retained MCP actor state across forced reconcile"
}

verify_mcp_tool_cleanup() {
  local actor_id pool_name generation pool_prefix pool_actor_0 pool_actor_1

  log "Verifying MCP Tool deletion and non-precreating pool scale-down prune actors"
  actor_id="$(kubectl -n "${ORKA_NAMESPACE}" get tool mcp-ci -o jsonpath='{.status.actor.actorID}')"
  pool_name="$(kubectl -n "${ORKA_NAMESPACE}" get tool mcp-ci -o jsonpath='{.status.actor.poolRef.name}')"
  if [[ -z "${actor_id}" ]]; then
    echo "tool/mcp-ci missing status.actor.actorID before cleanup" >&2
    exit 1
  fi
  if [[ -z "${pool_name}" ]]; then
    echo "tool/mcp-ci missing status.actor.poolRef.name before cleanup" >&2
    exit 1
  fi
  pool_prefix="$(substrate_actor_pool_prefix "${ORKA_NAMESPACE}" "${pool_name}")"
  pool_actor_0="${pool_prefix}-00000"
  pool_actor_1="${pool_prefix}-00001"
  kubectl -n "${ORKA_NAMESPACE}" get lease "${actor_id}" >/dev/null

  generation="$(
    kubectl -n "${ORKA_NAMESPACE}" patch substrateactorpool "${pool_name}" --type=merge \
      -p '{"spec":{"targetActors":0,"precreateActors":false}}' \
      -o json | jq -r '.metadata.generation'
  )"
  wait_jsonpath_equals \
    "substrateactorpool/${pool_name} scale-down observed generation" \
    "kubectl -n ${ORKA_NAMESPACE} get substrateactorpool ${pool_name} -o jsonpath='{.status.observedGeneration}'" \
    "${generation}" \
    120

  kubectl -n "${ORKA_NAMESPACE}" delete tool mcp-ci --wait=false
  wait_resource_absent "${ORKA_NAMESPACE}" tool mcp-ci 300
  wait_resource_absent "${ORKA_NAMESPACE}" lease "${actor_id}" 300
  wait_actor_absent "${actor_id}" 300
  wait_jsonpath_equals \
    "substrateactorpool/${pool_name} non-precreate scale-down readiness" \
    "kubectl -n ${ORKA_NAMESPACE} get substrateactorpool ${pool_name} -o jsonpath='{.status.phase}'" \
    "Ready" \
    300
  wait_jsonpath_equals \
    "substrateactorpool/${pool_name} actor count after non-precreate prune" \
    "kubectl -n ${ORKA_NAMESPACE} get substrateactorpool ${pool_name} -o json | jq -r '.status.actorCount // 0'" \
    "0" \
    300
  wait_actor_absent "${pool_actor_0}" 300
  wait_actor_absent "${pool_actor_1}" 300
  log "tool/mcp-ci cleanup removed actor ${actor_id}, its pool lease, and scaled down pool actors"
}

exercise_orka_tasks() {
  local tool_client_image="$1"

  create_substrate_actor_pools

  create_mcp_tool
  run_mcp_tool_client_job "${tool_client_image}"
  verify_mcp_tool_boots_actor_once "${tool_client_image}"
  verify_mcp_tool_cleanup

  log "Skipping workspace-backed ACP Task execution: RuntimeSession-to-Substrate Actor dispatch is deferred; direct Substrate and Orka-brokered MCP workspace paths were validated above"
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

write_workspace_handoff_token() {
  local url="$1"
  local host_header="$2"
  local token_b64="$3"
  local timeout_seconds="$4"
  local started now remaining attempt_timeout
  started="$(date +%s)"

  while true; do
    now="$(date +%s)"
    remaining=$((timeout_seconds - (now - started)))
    if (( remaining <= 0 )); then
      echo "timed out installing workspace handoff token via Host ${host_header}" >&2
      return 1
    fi
    attempt_timeout="${remaining}"
    if (( attempt_timeout > 5 )); then
      attempt_timeout=5
    fi
    # STATUS_RUNNING can precede the router's upstream connection becoming
    # usable after worker replacement or runsc recovery. Keep credentials and
    # response bodies private while retrying that bounded readiness window.
    if curl -fsS \
      --connect-timeout "${attempt_timeout}" \
      --max-time "${attempt_timeout}" \
      -H "Host: ${host_header}" \
      -H "Authorization: Bearer ${SUBSTRATE_BOOTSTRAP_TOKEN}" \
      -H "Content-Type: application/json" \
      -X PUT \
      -d "{\"files\":[{\"path\":\"/app/orka-workspace-handoff-token\",\"data\":\"${token_b64}\",\"mode\":384}]}" \
      "${url}" >/dev/null 2>&1; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout_seconds )); then
      echo "timed out installing workspace handoff token via Host ${host_header}" >&2
      return 1
    fi
    sleep 2
  done
}

run_idempotent_workspace_exec() {
  local url="$1"
  local host_header="$2"
  local handoff_token="$3"
  local request_id="$4"
  local timeout_seconds="$5"
  local started now remaining attempt_timeout response
  started="$(date +%s)"

  while true; do
    now="$(date +%s)"
    remaining=$((timeout_seconds - (now - started)))
    if (( remaining <= 0 )); then
      echo "timed out executing idempotent workspace probe via Host ${host_header}" >&2
      return 1
    fi
    attempt_timeout="${remaining}"
    if (( attempt_timeout > 5 )); then
      attempt_timeout=5
    fi
    # This exact command is deliberately idempotent. Retry only the live
    # routing probe, never arbitrary user commands, while a replacement
    # worker's Envoy upstream converges.
    if response="$(curl -fsS \
      --connect-timeout "${attempt_timeout}" \
      --max-time "${attempt_timeout}" \
      -H "Host: ${host_header}" \
      -H "Authorization: Bearer ${handoff_token}" \
      -H "X-Request-ID: ${request_id}" \
      -H "Content-Type: application/json" \
      -d '{"command":["/bin/sh","-lc","printf direct-ok"]}' \
      "${url}" 2>/dev/null)"; then
      printf '%s\n' "${response}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout_seconds )); then
      echo "timed out executing idempotent workspace probe via Host ${host_header}" >&2
      return 1
    fi
    sleep 2
  done
}

verify_router_request_metadata_allowlist() {
  local handoff_token="$1"
  local request_id="$2"
  local timeout_seconds="${3:-30}"
  local started now raw_log_file
  started="$(date +%s)"
  raw_log_file="${TMP_ROOT}/atenet-router-raw-${request_id}.log"

  while true; do
    # Keep the provider output private and inspect it before any presentation
    # redaction. Otherwise the test could erase the exact token leak it is meant
    # to detect and then falsely pass on the safe request-ID evidence.
    kubectl -n ate-system logs deployment/atenet-router --all-containers --since=2m       >"${raw_log_file}" 2>/dev/null || :
    if grep -Fq -- "${SUBSTRATE_BOOTSTRAP_TOKEN}" "${raw_log_file}" ||
       grep -Fq -- "${handoff_token}" "${raw_log_file}"; then
      echo "atenet-router leaked an Authorization credential in its logs" >&2
      grep -F -- "${request_id}" "${raw_log_file}" | redact >&2 || true
      return 1
    fi
    if grep -Fq -- "${request_id}" "${raw_log_file}"; then
      log "atenet-router logs retain the safe request ID while omitting request credentials"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started > timeout_seconds )); then
      echo "timed out waiting for atenet-router request-metadata allowlist evidence" >&2
      return 1
    fi
    sleep 2
  done
}

run_direct_actor_lifecycle() {
  local actor_name="$1"
  local host_header="${actor_name}.actors.resources.substrate.ate.dev"
  local token token_b64 request_id response

  kubectl_ate create actor "${actor_name}" --template ate-demo/orka-codex-ci
  kubectl_ate resume actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_RUNNING" 300
  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "" 300

  printf -v token 'ci-token-%s' "$(date +%s%N)"
  token_b64="$(printf '%s' "${token}" | base64 | tr -d '\n')"
  write_workspace_handoff_token "http://127.0.0.1:18082/v1/files" "${host_header}" "${token_b64}" 60

  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "Authorization: Bearer ${token}" 60
  printf -v request_id 'orka-router-log-%s' "$(date +%s%N)"
  response="$(run_idempotent_workspace_exec \
    "http://127.0.0.1:18082/v1/exec" "${host_header}" "${token}" "${request_id}" 60)"
  if [[ "$(jq -r '.exitCode' <<< "${response}")" != "0" || "$(jq -r '.stdout' <<< "${response}")" != "direct-ok" ]]; then
    echo "unexpected direct exec response for actor/${actor_name}" >&2
    jq -c '{exitCode,stdout,stderr}' <<< "${response}" | redact >&2
    return 1
  fi
  verify_router_request_metadata_allowlist "${token}" "${request_id}" 30

  kubectl_ate suspend actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_SUSPENDED" 300
  kubectl_ate delete actor "${actor_name}"
  wait_actor_absent "${actor_name}" 120
}

# The ateom image is distroless, so the live fault injector must be a static
# executable rather than a shell wrapper. It fails one delete before delegating
# every later invocation to the checksum-verified runsc binary beside it.
build_runsc_delete_failure_injector() {
  local output_path="$1"
  local architecture="$2"
  local target_os="${3:-linux}"
  local source_path="${TMP_ROOT}/runsc-delete-failure-injector.go"

  cat >"${source_path}" <<'GO'
package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func main() {
	executable, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve runsc injector path: %v\n", err)
		os.Exit(87)
	}

	isDelete := false
	for _, arg := range os.Args[1:] {
		if arg == "delete" {
			isDelete = true
			break
		}
	}
	if isDelete {
		marker := executable + ".orka-delete-failure-observed"
		file, markerErr := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if markerErr == nil {
			_ = file.Close()
			fmt.Fprintln(os.Stderr, "injected one runsc delete failure before container removal")
			os.Exit(86)
		}
		if !errors.Is(markerErr, os.ErrExist) {
			fmt.Fprintf(os.Stderr, "create runsc delete injection marker: %v\n", markerErr)
			os.Exit(87)
		}
	}

	realPath := executable + ".orka-real"
	argv := append([]string{realPath}, os.Args[1:]...)
	if err := syscall.Exec(realPath, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "exec real runsc: %v\n", err)
		os.Exit(87)
	}
}
GO

  CGO_ENABLED=0 GOOS="${target_os}" GOARCH="${architecture}" go build -trimpath -o "${output_path}" "${source_path}"
}

install_runsc_delete_failure_injector() {
  local worker_name="$1"
  local node architecture runsc_hash runsc_path injector_path node_injector_path

  node="$(kubectl -n ate-demo get pod "${worker_name}" -o jsonpath='{.spec.nodeName}')"
  architecture="$(kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.architecture}')"
  case "${architecture}" in
    amd64 | arm64) ;;
    *)
      echo "unsupported kind node architecture for runsc delete injection: ${architecture:-<empty>}" >&2
      return 1
      ;;
  esac
  runsc_hash="$(kubectl -n ate-demo get actortemplate orka-codex-ci -o "jsonpath={.spec.runsc.${architecture}.sha256Hash}")"
  if [[ -z "${node}" || -z "${runsc_hash}" ]]; then
    echo "could not resolve the assigned worker node or runsc digest for delete injection" >&2
    return 1
  fi

  runsc_path="/run/ateom-gvisor/static-files/runsc-${runsc_hash}"
  injector_path="${TMP_ROOT}/runsc-delete-failure-injector-${architecture}"
  node_injector_path="/root/orka-runsc-delete-failure-injector-$$-${RANDOM}"
  build_runsc_delete_failure_injector "${injector_path}" "${architecture}"
  docker cp "${injector_path}" "${node}:${node_injector_path}"

  RUNSC_DELETE_INJECTION_NODE="${node}"
  RUNSC_DELETE_INJECTION_PATH="${runsc_path}"
  if ! docker exec "${node}" /bin/sh -ceu '
    path="$1"
    incoming="$2"
    test -f "${path}"
    test ! -e "${path}.orka-real"
    rm -f "${path}.orka-delete-failure-observed"
    mv "${path}" "${path}.orka-real"
    if ! cp "${incoming}" "${path}" || ! chmod 0755 "${path}"; then
      rm -f "${path}"
      mv "${path}.orka-real" "${path}"
      rm -f "${incoming}"
      exit 1
    fi
    rm -f "${incoming}"
  ' sh "${runsc_path}" "${node_injector_path}"; then
    restore_runsc_delete_injector >/dev/null 2>&1 || true
    return 1
  fi
}

runsc_delete_failure_was_injected() {
  [[ -n "${RUNSC_DELETE_INJECTION_NODE}" && -n "${RUNSC_DELETE_INJECTION_PATH}" ]] || return 1
  docker exec "${RUNSC_DELETE_INJECTION_NODE}" /bin/sh -ceu \
    'test -f "${1}.orka-delete-failure-observed"' sh "${RUNSC_DELETE_INJECTION_PATH}"
}

exercise_runsc_delete_retry_recovery() {
  local actor_name="orka-delete-retry-ci"
  local host_header="${actor_name}.actors.resources.substrate.ate.dev"
  local actor_json worker_name worker_logs

  log "Validating an injected runsc delete failure is retried on the live worker"
  kubectl_ate create actor "${actor_name}" --template ate-demo/orka-codex-ci
  kubectl_ate resume actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_RUNNING" 300
  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "" 300

  actor_json="$(kubectl_ate get actor "${actor_name}" -o json)"
  worker_name="$(jq -r '.actors[0].ateomPodName // empty' <<<"${actor_json}")"
  if [[ -z "${worker_name}" ]]; then
    echo "actor/${actor_name} did not expose its assigned worker before delete injection" >&2
    return 1
  fi

  install_runsc_delete_failure_injector "${worker_name}"
  kubectl_ate suspend actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_SUSPENDED" 300
  if ! runsc_delete_failure_was_injected; then
    restore_runsc_delete_injector >/dev/null 2>&1 || true
    echo "runsc delete failure injector was not invoked for actor/${actor_name}" >&2
    return 1
  fi
  restore_runsc_delete_injector

  worker_logs="$(kubectl -n ate-demo logs "pod/${worker_name}" -c ateom --since=5m 2>&1)"
  if ! grep -Fq 'runsc delete did not remove the container; retrying' <<<"${worker_logs}"; then
    echo "actor/${actor_name} suspended without live evidence from the patched runsc delete retry path" >&2
    return 1
  fi
  log "actor/${actor_name}: observed injected failure, verified-presence retry, and successful suspension"

  kubectl -n ate-demo wait --for=condition=Ready "pod/${worker_name}" --timeout=2m
  kubectl_ate delete actor "${actor_name}"
  wait_actor_absent "${actor_name}" 120

  # A fresh lifecycle after restoring the original binary proves that cleanup
  # left the worker fleet routable rather than merely settling actor metadata.
  run_direct_actor_lifecycle "orka-post-delete-retry-ci"
  assert_no_suspending_actors
}

exercise_worker_replacement_recovery() {
  local actor_name="orka-worker-loss-ci"
  local host_header="${actor_name}.actors.resources.substrate.ate.dev"
  local actor_json worker_name replacement_name

  log "Validating worker-loss settlement and post-replacement direct routing"
  kubectl_ate create actor "${actor_name}" --template ate-demo/orka-codex-ci
  kubectl_ate resume actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_RUNNING" 300
  wait_http_ok "http://127.0.0.1:18082/healthz" "${host_header}" "" 300

  actor_json="$(kubectl_ate get actor "${actor_name}" -o json)"
  worker_name="$(jq -r '.actors[0].ateomPodName // empty' <<< "${actor_json}")"
  if [[ -z "${worker_name}" ]]; then
    echo "actor/${actor_name} did not expose its assigned worker before replacement" >&2
    return 1
  fi

  kubectl -n ate-demo delete pod "${worker_name}" --wait=true
  wait_worker_absent "${worker_name}" 120
  kubectl -n ate-demo rollout status deployment/orka-workers-deployment --timeout=5m
  wait_worker_count_at_least 3 180

  kubectl_ate suspend actor "${actor_name}"
  wait_actor_status "${actor_name}" "STATUS_SUSPENDED" 120
  kubectl_ate delete actor "${actor_name}"
  wait_actor_absent "${actor_name}" 120

  replacement_name="orka-post-worker-loss-ci"
  run_direct_actor_lifecycle "${replacement_name}"

  # The dangling-worker path above cannot invoke CheckpointWorkload because the
  # assigned Pod is gone. Inject one live runsc failure on the replacement fleet
  # so the extended E2E proves the reviewed retry and verified-absence path.
  exercise_runsc_delete_retry_recovery
  assert_no_suspending_actors
}

exercise_direct_substrate() {
  log "Running direct Substrate workspace-agent smoke"
  kubectl -n ate-system port-forward svc/atenet-router 18082:80 >/tmp/orka-atenet-router-port-forward.log 2>&1 &
  PORT_FORWARD_PID="$!"
  sleep 3

  run_direct_actor_lifecycle "orka-direct-ci"
  if [[ "${SUBSTRATE_E2E_EXTENDED}" == "1" ]]; then
    exercise_worker_replacement_recovery
  fi

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
  [[ "${ORKA_NAMESPACE}" == "orka-system" ]] || {
    echo "ORKA_NAMESPACE must be orka-system for the canonical config/acp-workload deployment" >&2
    exit 1
  }

  TMP_ROOT="$(mktemp -d)"
  export KUBECONFIG="${TMP_ROOT}/kubeconfig"
  DOCKER_CONFIG_DIR="$(mktemp -d)"
  printf '{"auths":{}}\n' > "${DOCKER_CONFIG_DIR}/config.json"
  SUBSTRATE_DIR="${TMP_ROOT}/substrate"

  log "Cloning Substrate ${SUBSTRATE_REF}"
  git clone --quiet "${SUBSTRATE_REPO}" "${SUBSTRATE_DIR}"
  git -C "${SUBSTRATE_DIR}" checkout --quiet "${SUBSTRATE_REF}"
  apply_substrate_workspace_agent_capability_patch
  apply_substrate_atenet_authorization_redaction_patch
  apply_substrate_ateom_delete_recovery_patch
  verify_reviewed_substrate_patch_set
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

  local registry_ip registry_addr controller_image workspace_push_image workspace_actor_image mcp_push_image mcp_actor_image tool_client_image ateom_image
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
  workspace_push_image="${registry_addr}/orka/workspace-agent-root:${IMAGE_TAG}"
  workspace_actor_image="${registry_ip}:5000/orka/workspace-agent-root:${IMAGE_TAG}"
  mcp_push_image="${registry_addr}/orka/mcp-e2e-server:${IMAGE_TAG}"
  mcp_actor_image="${registry_ip}:5000/orka/mcp-e2e-server:${IMAGE_TAG}"
  tool_client_image="${registry_addr}/orka/tool-e2e-client:${IMAGE_TAG}"

  log "Building and pushing Orka images"
  docker build -t "${controller_image}" -f "${ROOT_DIR}/Dockerfile" "${ROOT_DIR}"
  docker build -t "${workspace_push_image}" -f "${ROOT_DIR}/cmd/orka-workspace-agent/Dockerfile" "${ROOT_DIR}"
  docker build -t "${mcp_push_image}" -f "${ROOT_DIR}/cmd/orka-mcp-e2e-server/Dockerfile" "${ROOT_DIR}"
  docker build -t "${tool_client_image}" -f "${ROOT_DIR}/cmd/orka-tool-e2e-client/Dockerfile" "${ROOT_DIR}"
  docker push "${controller_image}"
  docker push "${workspace_push_image}"
  docker push "${mcp_push_image}"
  docker push "${tool_client_image}"

  log "Publishing Substrate ateom-gvisor image"
  ateom_image="$(publish_ateom_image)"
  create_substrate_resources "${ateom_image}" "${workspace_actor_image}" "${mcp_actor_image}"
  deploy_orka "${controller_image}"
  exercise_direct_substrate
  exercise_orka_tasks "${tool_client_image}"
  assert_no_suspending_actors

  log "Agent Substrate E2E passed"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
