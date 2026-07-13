#!/usr/bin/env bash

set -Eeuo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

kind_cluster="${KIND_CLUSTER:-orka-live-copilot-proxy-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
copilot_proxy_namespace="${COPILOT_PROXY_NAMESPACE:-default}"
copilot_proxy_service="${COPILOT_PROXY_SERVICE:-copilot-proxy}"
copilot_proxy_service_port="${COPILOT_PROXY_SERVICE_PORT:-1337}"
copilot_proxy_local_port="${COPILOT_PROXY_LOCAL_PORT:-18081}"
copilot_proxy_image="${COPILOT_PROXY_IMAGE:-ghcr.io/sozercan/vekil:latest}"
proxy_token_secret_name="${COPILOT_PROXY_TOKEN_SECRET_NAME:-live-copilot-proxy-token}"
token_value="${COPILOT_GITHUB_TOKEN:-}"
proxy_pf_pid=""
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-copilot-proxy-e2e.XXXXXX")"

cleanup_port_forward() {
  local pid="$1"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    kill "${pid}" 2>/dev/null || true
    wait "${pid}" 2>/dev/null || true
  fi
}

redact() {
  local text
  text="$(cat)"
  if [[ -n "${token_value}" ]]; then
    text="${text//${token_value}/[REDACTED]}"
  fi
  printf '%s' "${text}" | sed -E \
    -e 's/(Authorization: (Bearer|token) )[[:graph:]]+/\1[REDACTED]/g' \
    -e 's/COPILOT_GITHUB_TOKEN=[^[:space:]]+/COPILOT_GITHUB_TOKEN=[REDACTED]/g' \
    -e 's/GITHUB_TOKEN=[^[:space:]]+/GITHUB_TOKEN=[REDACTED]/g' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/("access_token":"[^"]*")/"access_token":"[REDACTED]"/g' \
    -e 's/("token":"[^"]*")/"token":"[REDACTED]"/g'
}

dump_diagnostics() {
  log "Collecting redacted diagnostics"

  {
    echo "=== Orka Namespace Resources ==="
    kubectl get pods,svc,deploy,provider -n "${orka_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Orka Namespace Events ==="
    kubectl get events -n "${orka_namespace}" --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Proxy Namespace Resources ==="
    kubectl get pods,svc,deploy -n "${copilot_proxy_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Proxy Namespace Events ==="
    kubectl get events -n "${copilot_proxy_namespace}" --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    local controller_pod
    controller_pod="$(kubectl get pods -l control-plane=controller-manager -n "${orka_namespace}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${controller_pod}" ]]; then
      kubectl logs "${controller_pod}" -n "${orka_namespace}" --tail=200 2>/dev/null || true
    fi
    echo
    echo "=== Proxy Logs ==="
    local proxy_pod
    proxy_pod="$(kubectl get pods -l app="${copilot_proxy_service}" -n "${copilot_proxy_namespace}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [[ -n "${proxy_pod}" ]]; then
      kubectl logs "${proxy_pod}" -n "${copilot_proxy_namespace}" --tail=200 2>/dev/null || true
    fi
  } | redact >&2
}

on_exit() {
  local status="$1"
  cleanup_port_forward "${proxy_pf_pid}"
  if [[ "${status}" -ne 0 ]]; then
    dump_diagnostics
    log "Live copilot-proxy e2e failed"
  fi
  make cleanup-test-e2e KIND_CLUSTER="${kind_cluster}" >/dev/null 2>&1 || true
  rm -rf "${work_dir}" >/dev/null 2>&1 || true
}

wait_for_http() {
  local url="$1"
  local description="$2"
  local attempt

  for attempt in $(seq 1 60); do
    if curl -fsS "${url}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done

  die "${description} never became available at ${url}"
}

wait_for_kube_service_proxy() {
  local namespace_arg="$1"
  local service_name="$2"
  local service_port="$3"
  local endpoint_path="$4"
  local description="$5"
  local output_file="${6:-}"
  local proxy_path
  local attempt

  proxy_path="/api/v1/namespaces/${namespace_arg}/services/http:${service_name}:${service_port}/proxy${endpoint_path}"

  for attempt in $(seq 1 60); do
    if [[ -n "${output_file}" ]]; then
      if kubectl get --raw "${proxy_path}" >"${output_file}" 2>/dev/null; then
        return 0
      fi
    else
      if kubectl get --raw "${proxy_path}" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 2
  done

  die "${description} never became available via Kubernetes service proxy at ${proxy_path}"
}

wait_for_proxy_deployment() {
  log "Waiting for copilot-proxy rollout"
  kubectl rollout status deployment/"${copilot_proxy_service}" -n "${copilot_proxy_namespace}" --timeout=5m
}

start_port_forward() {
  local namespace_arg="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local logfile="$5"

  kubectl -n "${namespace_arg}" port-forward "${resource}" "${local_port}:${remote_port}" >"${logfile}" 2>&1 &
  echo $!
}

hash_text() {
  local value="$1"
  local digest=""

  if command -v sha256sum >/dev/null 2>&1; then
    digest="$(printf '%s' "${value}" | sha256sum | awk '{print $1}')" ||
      die "failed to compute SHA-256 digest"
  elif command -v shasum >/dev/null 2>&1; then
    digest="$(printf '%s' "${value}" | shasum -a 256 | awk '{print $1}')" ||
      die "failed to compute SHA-256 digest"
  elif command -v openssl >/dev/null 2>&1; then
    digest="$(printf '%s' "${value}" | openssl dgst -sha256 | awk '{print $NF}')" ||
      die "failed to compute SHA-256 digest"
  else
    die "no SHA-256 tool available (need sha256sum, shasum, or openssl)"
  fi

  [[ "${#digest}" -eq 64 && "${digest}" != *[!0-9a-fA-F]* ]] ||
    die "SHA-256 tool returned an invalid digest"
  printf '%s\n' "${digest}"
}

validate_scoped_target() {
  local expected_context="kind-${kind_cluster}"
  local state_dir="${E2E_CLUSTER_STATE_DIR:-}"
  local lock_root="${E2E_CLUSTER_LOCK_ROOT:-}"
  local operation_token="${E2E_KIND_OPERATION_TOKEN:-}"
  local lock_dir
  local current_context
  local current_cluster
  local identity_material
  local server
  local certificate_authority_data
  local fingerprint
  local state_fingerprint

  [[ "${E2E_KIND_TARGET_READY:-}" == "1" ]] ||
    die "refusing to run outside e2e-kind-cluster.sh run: target is not ready"
  [[ -n "${KUBECONFIG:-}" && "${KUBECONFIG}" != *:* ]] ||
    die "refusing to run without one isolated KUBECONFIG"
  [[ -n "${state_dir}" && -n "${lock_root}" && -n "${operation_token}" ]] ||
    die "refusing to run without helper state and operation lock metadata"
  [[ -d "${state_dir}" && ! -L "${state_dir}" ]] ||
    die "helper state directory is missing or unsafe"
  [[ "${E2E_KIND_EXPECTED_CONTEXT:-}" == "${expected_context}" ]] ||
    die "helper expected context does not match ${expected_context}"
  [[ "${E2E_KIND_EXPECTED_CLUSTER:-}" == "${expected_context}" ]] ||
    die "helper expected cluster does not match ${expected_context}"
  [[ "${KUBECONFIG}" == "${state_dir}/target.kubeconfig" ]] ||
    die "KUBECONFIG is not the helper-owned target kubeconfig"
  [[ -f "${KUBECONFIG}" && ! -L "${KUBECONFIG}" ]] ||
    die "helper-owned target kubeconfig is missing or unsafe"

  local state_file
  for state_file in cluster context kubeconfig fingerprint status; do
    [[ -f "${state_dir}/${state_file}" && ! -L "${state_dir}/${state_file}" ]] ||
      die "helper state is missing safe ${state_file} metadata"
  done
  [[ "$(<"${state_dir}/cluster")" == "${kind_cluster}" ]] ||
    die "helper state cluster does not match ${kind_cluster}"
  [[ "$(<"${state_dir}/context")" == "${expected_context}" ]] ||
    die "helper state context does not match ${expected_context}"
  [[ "$(<"${state_dir}/kubeconfig")" == "${KUBECONFIG}" ]] ||
    die "helper state kubeconfig does not match KUBECONFIG"
  [[ "$(<"${state_dir}/status")" == "ready" ]] ||
    die "helper target state is not ready"

  lock_dir="${lock_root}/kind-${kind_cluster}.operation"
  [[ -d "${lock_dir}" && ! -L "${lock_dir}" ]] ||
    die "helper operation lock is not held"
  [[ -f "${lock_dir}/token" && ! -L "${lock_dir}/token" ]] ||
    die "helper operation lock token is missing or unsafe"
  [[ "$(<"${lock_dir}/token")" == "${operation_token}" ]] ||
    die "helper operation lock token does not match"
  [[ -f "${lock_dir}/state_dir" && ! -L "${lock_dir}/state_dir" ]] ||
    die "helper operation lock state metadata is missing or unsafe"
  [[ "$(<"${lock_dir}/state_dir")" == "${state_dir}" ]] ||
    die "helper operation lock targets different state"

  current_context="$(kubectl --kubeconfig "${KUBECONFIG}" config current-context)" ||
    die "failed to read scoped Kubernetes context"
  current_cluster="$(kubectl --kubeconfig "${KUBECONFIG}" config view --minify -o 'jsonpath={.contexts[0].context.cluster}')" ||
    die "failed to read scoped Kubernetes cluster"
  [[ "${current_context}" == "${expected_context}" ]] ||
    die "scoped Kubernetes context ${current_context} does not match ${expected_context}"
  [[ "${current_cluster}" == "${expected_context}" ]] ||
    die "scoped Kubernetes cluster ${current_cluster} does not match ${expected_context}"

  identity_material="$(kubectl --kubeconfig "${KUBECONFIG}" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.server}{"\n"}{.clusters[0].cluster.certificate-authority-data}')" ||
    die "failed to read scoped Kubernetes target identity"
  server="${identity_material%%$'\n'*}"
  [[ "${identity_material}" == *$'\n'* ]] ||
    die "scoped Kubernetes target identity is incomplete"
  certificate_authority_data="${identity_material#*$'\n'}"
  [[ -n "${server}" && -n "${certificate_authority_data}" ]] ||
    die "scoped Kubernetes target identity is incomplete"
  fingerprint="$(hash_text "${identity_material}")"
  state_fingerprint="$(<"${state_dir}/fingerprint")"
  [[ "${#state_fingerprint}" -eq 64 && "${state_fingerprint}" != *[!0-9a-fA-F]* ]] ||
    die "helper state fingerprint is malformed"
  [[ "${fingerprint}" == "${state_fingerprint}" ]] ||
    die "scoped Kubernetes target identity does not match helper state"
}

run_under_e2e_kind_helper() {
  local script_path="${script_dir}/$(basename "${BASH_SOURCE[0]}")"
  local e2e_kind_helper="${script_dir}/e2e-kind-cluster.sh"

  [[ -x "${e2e_kind_helper}" ]] || die "missing executable E2E Kind helper: ${e2e_kind_helper}"
  log "Running live copilot-proxy e2e through isolated Kind helper"
  env KIND_CLUSTER="${kind_cluster}" \
    "${e2e_kind_helper}" run --create --cleanup -- \
    "${script_path}" --e2e-kind-scoped "$@"
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kubectl
  require_cmd kind
  require_cmd curl
  require_cmd jq

  [[ -n "${token_value}" ]] || die "COPILOT_GITHUB_TOKEN is required"

  if [[ "${1:-}" != "--e2e-kind-scoped" ]]; then
    local helper_status=0
    if run_under_e2e_kind_helper "$@"; then
      helper_status=0
    else
      helper_status=$?
    fi
    rm -rf "${work_dir}" >/dev/null 2>&1 || true
    return "${helper_status}"
  fi
  shift

  validate_scoped_target
  trap 'on_exit $?' EXIT

  log "Creating proxy namespace ${copilot_proxy_namespace}"
  kubectl create namespace "${copilot_proxy_namespace}" --dry-run=client -o yaml | kubectl apply -f -

  log "Deploying copilot-proxy"
  kubectl apply -f - <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: ${proxy_token_secret_name}
  namespace: ${copilot_proxy_namespace}
type: Opaque
stringData:
  COPILOT_GITHUB_TOKEN: "${token_value}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${copilot_proxy_service}
  namespace: ${copilot_proxy_namespace}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${copilot_proxy_service}
  template:
    metadata:
      labels:
        app: ${copilot_proxy_service}
    spec:
      containers:
        - name: ${copilot_proxy_service}
          image: ${copilot_proxy_image}
          imagePullPolicy: Always
          ports:
            - containerPort: 1337
          env:
            - name: COPILOT_GITHUB_TOKEN
              valueFrom:
                secretKeyRef:
                  name: ${proxy_token_secret_name}
                  key: COPILOT_GITHUB_TOKEN
            - name: PORT
              value: "1337"
            - name: TOKEN_DIR
              value: /home/nonroot/.config/copilot-proxy
          volumeMounts:
            - name: token-cache
              mountPath: /home/nonroot/.config/copilot-proxy
      volumes:
        - name: token-cache
          emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: ${copilot_proxy_service}
  namespace: ${copilot_proxy_namespace}
spec:
  selector:
    app: ${copilot_proxy_service}
  ports:
    - port: ${copilot_proxy_service_port}
      targetPort: 1337
YAML

  wait_for_proxy_deployment

  log "Waiting for copilot-proxy /readyz via Kubernetes service proxy"
  wait_for_kube_service_proxy \
    "${copilot_proxy_namespace}" \
    "${copilot_proxy_service}" \
    "${copilot_proxy_service_port}" \
    "/readyz" \
    "copilot-proxy /readyz"

  log "Validating copilot-proxy live models"
  wait_for_kube_service_proxy \
    "${copilot_proxy_namespace}" \
    "${copilot_proxy_service}" \
    "${copilot_proxy_service_port}" \
    "/v1/models" \
    "copilot-proxy /v1/models" \
    "${work_dir}/copilot-proxy-models.json"
  jq -e '.data | length > 0' "${work_dir}/copilot-proxy-models.json" >/dev/null
  jq -e '
    (.data | map(.id // "")) as $ids
    | ($ids | any(startswith("gpt-")))
    and ($ids | any(startswith("claude-")))
    and ($ids | any(startswith("gemini-")))
  ' "${work_dir}/copilot-proxy-models.json" >/dev/null

  log "Live proxy model families detected"
  jq -r '.data[].id' "${work_dir}/copilot-proxy-models.json" | redact >&2

  log "Running focused live copilot-proxy Go e2e specs"
  KIND_CLUSTER="${kind_cluster}" \
  E2E_GITHUB_TOKEN="${token_value}" \
  E2E_LIVE_COPILOT_PROXY_BASE_URL="http://${copilot_proxy_service}.${copilot_proxy_namespace}.svc.cluster.local:${copilot_proxy_service_port}/v1" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE="${copilot_proxy_namespace}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAME="${copilot_proxy_service}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_PORT="${copilot_proxy_service_port}" \
  go test -tags=e2e ./test/e2e/ -timeout 45m -v -ginkgo.v \
    -ginkgo.focus="Live Copilot Proxy Provider|Live Chat API|Live Anthropic Compat API|Live Agent Runtime Matrix"

  log "Live copilot-proxy e2e passed"
}

main "$@"
