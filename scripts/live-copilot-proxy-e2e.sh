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
copilot_proxy_image="${COPILOT_PROXY_IMAGE:-docker.io/sozercan/copilot-proxy:latest}"
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

main() {
  require_cmd make
  require_cmd go
  require_cmd kubectl
  require_cmd kind
  require_cmd curl
  require_cmd jq

  [[ -n "${token_value}" ]] || die "COPILOT_GITHUB_TOKEN is required"

  trap 'on_exit $?' EXIT

  log "Creating Kind cluster ${kind_cluster}"
  make setup-test-e2e KIND_CLUSTER="${kind_cluster}"

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
  CERT_MANAGER_INSTALL_SKIP=true \
  KIND_CLUSTER="${kind_cluster}" \
  E2E_GITHUB_TOKEN="${token_value}" \
  E2E_LIVE_COPILOT_PROXY_BASE_URL="http://${copilot_proxy_service}.${copilot_proxy_namespace}.svc.cluster.local:${copilot_proxy_service_port}/v1" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAMESPACE="${copilot_proxy_namespace}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_NAME="${copilot_proxy_service}" \
  E2E_LIVE_COPILOT_PROXY_SERVICE_PORT="${copilot_proxy_service_port}" \
  go test -tags=e2e ./test/e2e/ -timeout 45m -v -ginkgo.v \
    -ginkgo.focus="Live Copilot Proxy Provider|Live Chat API|Live Agent Runtime Matrix"

  log "Live copilot-proxy e2e passed"
}

main "$@"
