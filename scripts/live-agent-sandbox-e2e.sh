#!/usr/bin/env bash

set -Eeuo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_sha256() {
  if command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1; then
    return
  fi
  die "missing required command: sha256sum or shasum"
}

sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
    return
  fi
  shasum -a 256 | awk '{print $1}'
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

agent_sandbox_version="${AGENT_SANDBOX_VERSION:-v0.4.6}"
kind_cluster="${KIND_CLUSTER:-orka-live-agent-sandbox-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18084}"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-agent-sandbox-e2e}"
fake_claude_image="${ORKA_FAKE_CLAUDE_WORKER_IMAGE:-orka-agent-sandbox-fake-claude:live-agent-sandbox-e2e}"
sandbox_router_image="${ORKA_AGENT_SANDBOX_ROUTER_IMAGE:-orka-agent-sandbox-router:live-agent-sandbox-e2e}"
sandbox_template_name="${ORKA_AGENT_SANDBOX_TEMPLATE:-orka-agent-sandbox-e2e-template}"
agent_name="${ORKA_AGENT_SANDBOX_AGENT:-orka-agent-sandbox-e2e-agent}"
delete_task_name="${ORKA_AGENT_SANDBOX_DELETE_TASK:-orka-agent-sandbox-delete-smoke}"
retain_task_one="${ORKA_AGENT_SANDBOX_RETAIN_TASK_ONE:-orka-agent-sandbox-retain-one}"
retain_task_two="${ORKA_AGENT_SANDBOX_RETAIN_TASK_TWO:-orka-agent-sandbox-retain-two}"
session_name="${ORKA_AGENT_SANDBOX_SESSION:-orka-agent-sandbox-session}"
wait_timeout="${ORKA_AGENT_SANDBOX_WAIT_TIMEOUT:-8m}"
api_pf_pid=""
router_namespace=""
created_kind_cluster="0"
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-agent-sandbox-e2e.XXXXXX")"
kind_config="${ORKA_AGENT_SANDBOX_KIND_CONFIG:-${work_dir}/kind-config.yaml}"
fake_dockerfile="${work_dir}/Dockerfile.fake-claude"
api_pf_log="${work_dir}/api-port-forward.log"

if [[ "${agent_sandbox_version}" != "v0.4.6" ]]; then
  die "this e2e is pinned to agent-sandbox v0.4.6 to match go.mod"
fi

cleanup_one_port_forward() {
  local pid="$1"
  if [[ -n "${pid}" ]]; then
    if kill -0 "${pid}" 2>/dev/null; then
      kill "${pid}" 2>/dev/null || true
    fi
    wait "${pid}" 2>/dev/null || true
  fi
}

cleanup_port_forward() {
  cleanup_one_port_forward "${api_pf_pid}"
  api_pf_pid=""
}

dump_diagnostics() {
  log "Collecting diagnostics"

  {
    echo "=== Current Kubernetes Context ==="
    kubectl config current-context 2>/dev/null || true
    echo
    echo "=== Orka Namespace Resources ==="
    kubectl get pods,svc,deploy,jobs,tasks,agents,sandboxclaims,sandboxes,sandboxtemplates -n "${orka_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Agent Sandbox Resources ==="
    kubectl get pods,svc,deploy,sandboxclaims,sandboxes,sandboxtemplates -A -o wide 2>/dev/null || true
    echo
    echo "=== Orka Namespace Events ==="
    kubectl get events -n "${orka_namespace}" --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Agent Sandbox System Events ==="
    kubectl get events -n agent-sandbox-system --sort-by=.lastTimestamp 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    kubectl logs deployment/"${orka_controller_deployment}" -n "${orka_namespace}" -c manager --tail=300 2>/dev/null || true
    echo
    echo "=== Worker Logs ==="
    for task in "${delete_task_name}" "${retain_task_one}" "${retain_task_two}"; do
      kubectl logs -n "${orka_namespace}" -l "orka.ai/task=${task}" --all-containers --tail=300 --prefix 2>/dev/null || true
    done
    echo
    echo "=== Agent Sandbox Controller Logs ==="
    kubectl logs deployment/agent-sandbox-controller -n agent-sandbox-system --tail=300 2>/dev/null || true
    echo
    echo "=== Sandbox Router Logs ==="
    if [[ -n "${router_namespace}" ]]; then
      kubectl logs deployment/sandbox-router-deployment -n "${router_namespace}" --tail=300 2>/dev/null || true
    fi
    echo
    echo "=== API Port-forward Log ==="
    if [[ -f "${api_pf_log}" ]]; then
      cat "${api_pf_log}" 2>/dev/null || true
    fi
  } >&2
}

on_exit() {
  local status="$1"
  set +e

  if [[ "${status}" -ne 0 ]]; then
    if [[ "$(kubectl config current-context 2>/dev/null || true)" == "kind-${kind_cluster}" ]]; then
      dump_diagnostics
    else
      warn "skipping Kubernetes diagnostics because the current context is not kind-${kind_cluster}"
    fi
  fi

  cleanup_port_forward
  if [[ "${created_kind_cluster}" == "1" ]]; then
    kind delete cluster --name "${kind_cluster}" >/dev/null 2>&1 || true
  fi
  rm -rf "${work_dir}" >/dev/null 2>&1 || true

  if [[ "${status}" -ne 0 ]]; then
    log "Live agent-sandbox e2e failed"
  fi
}

run() {
  printf '+ ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
  "$@"
}

kind_cluster_exists() {
  kind get clusters | grep -Fxq "${kind_cluster}"
}

write_default_kind_config() {
  cat >"${kind_config}" <<'YAML'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
YAML
}

setup_kind_cluster() {
  if kind_cluster_exists; then
    log "Kind cluster ${kind_cluster} already exists; reusing it"
    return
  fi

  if [[ -z "${ORKA_AGENT_SANDBOX_KIND_CONFIG:-}" ]]; then
    write_default_kind_config
  fi
  [[ -f "${kind_config}" ]] || die "Kind config not found: ${kind_config}"

  log "Creating Kind cluster ${kind_cluster}"
  run kind create cluster --name "${kind_cluster}" --config "${kind_config}"
  created_kind_cluster="1"
}

start_port_forward() {
  local namespace_arg="$1"
  local resource="$2"
  local local_port="$3"
  local remote_port="$4"
  local logfile="$5"

  kubectl -n "${namespace_arg}" port-forward "${resource}" "${local_port}:${remote_port}" >>"${logfile}" 2>&1 &
  echo $!
}

wait_for_http() {
  local url="$1"
  local description="$2"
  local attempts_remaining=90

  while (( attempts_remaining > 0 )); do
    if curl -fsS --connect-timeout 5 --max-time 10 "${url}" >/dev/null 2>&1; then
      return 0
    fi
    if [[ -n "${api_pf_pid}" ]] && ! kill -0 "${api_pf_pid}" 2>/dev/null; then
      warn "API port-forward exited while waiting for ${description}; restarting"
      wait "${api_pf_pid}" 2>/dev/null || true
      api_pf_pid="$(start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}")"
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "${description} never became available at ${url}"
}

write_fake_claude_dockerfile() {
  cat >"${fake_dockerfile}" <<'DOCKERFILE'
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -a -o /out/worker ./workers/agent/claude

RUN cat >/tmp/sandbox-runtime.go <<'GO'
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const appRoot = "/app"

type executeRequest struct {
	Command string `json:"command"`
}

type executeResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

type listEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func main() {
	if err := os.MkdirAll(appRoot, 0o755); err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", health)
	mux.HandleFunc("/execute", execute)
	mux.HandleFunc("/upload", upload)
	mux.HandleFunc("/download/", download)
	mux.HandleFunc("/list/", list)
	mux.HandleFunc("/exists/", exists)
	if err := http.ListenAndServe(":8888", mux); err != nil {
		panic(err)
	}
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func execute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req executeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, executeResponse{Stderr: err.Error(), ExitCode: 1})
		return
	}
	cmd := exec.Command("/bin/sh", "-c", req.Command)
	cmd.Dir = appRoot
	out, err := cmd.Output()
	resp := executeResponse{Stdout: string(out)}
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			resp.Stderr = string(exitErr.Stderr)
			resp.ExitCode = exitErr.ExitCode()
		} else {
			resp.Stderr = err.Error()
			resp.ExitCode = 1
		}
	}
	writeJSON(w, resp)
}

func upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()
	target, err := safePath(header.Filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err := writeMultipartFile(target, file); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"message": "uploaded"})
}

func download(w http.ResponseWriter, r *http.Request) {
	target, err := safePath(strings.TrimPrefix(r.URL.Path, "/download/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	http.ServeFile(w, r, target)
}

func list(w http.ResponseWriter, r *http.Request) {
	target, err := safePath(strings.TrimPrefix(r.URL.Path, "/list/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]listEntry, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, listEntry{Name: entry.Name(), IsDir: entry.IsDir(), Size: info.Size()})
	}
	writeJSON(w, out)
}

func exists(w http.ResponseWriter, r *http.Request) {
	target, err := safePath(strings.TrimPrefix(r.URL.Path, "/exists/"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	_, err = os.Stat(target)
	writeJSON(w, map[string]bool{"exists": err == nil})
}

func safePath(name string) (string, error) {
	clean := filepath.Clean(strings.TrimLeft(name, "/"))
	if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid path %q", name)
	}
	target := filepath.Join(appRoot, clean)
	rel, err := filepath.Rel(appRoot, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("path escapes app root")
	}
	return target, nil
}

func writeMultipartFile(target string, file multipart.File) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o700)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, file)
	return err
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
GO

RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64} go build -o /out/sandbox-runtime /tmp/sandbox-runtime.go

FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    git \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/worker /worker
COPY --from=builder /out/sandbox-runtime /usr/local/bin/sandbox-runtime

RUN printf '%s\n' \
    '#!/bin/sh' \
    'set -eu' \
    'marker=/workspace/orka-agent-sandbox-retained-marker.txt' \
    'prompt="$*"' \
    'if [ "${ORKA_AGENT_SANDBOX_ENABLED:-}" != "false" ]; then echo "ORKA_AGENT_SANDBOX_ENABLED was ${ORKA_AGENT_SANDBOX_ENABLED:-missing}" >&2; exit 41; fi' \
    'if [ "${ORKA_AGENT_SANDBOX_DEPTH:-}" != "1" ]; then echo "ORKA_AGENT_SANDBOX_DEPTH was ${ORKA_AGENT_SANDBOX_DEPTH:-missing}" >&2; exit 42; fi' \
    'if [ -z "${ORKA_SA_TOKEN_PATH:-}" ] || [ ! -s "${ORKA_SA_TOKEN_PATH}" ]; then echo "missing staged service account token" >&2; exit 43; fi' \
    'state=absent' \
    'if [ -f "$marker" ]; then state=present; fi' \
    'printf "run=%s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > "$marker"' \
    'printf "ORKA_LIVE_SANDBOX_OK depth=%s enabled=%s retained_marker=%s pwd=%s prompt=%s\n" "${ORKA_AGENT_SANDBOX_DEPTH}" "${ORKA_AGENT_SANDBOX_ENABLED}" "$state" "$(pwd)" "$prompt"' \
    > /usr/local/bin/claude \
    && chmod 0755 /worker /usr/local/bin/sandbox-runtime /usr/local/bin/claude

RUN groupadd -g 1000 worker \
    && useradd -u 1000 -g worker -m worker \
    && mkdir -p /workspace /app /tmp \
    && chown -R 1000:1000 /workspace /app /home/worker /tmp

USER 1000:1000
ENV HOME=/home/worker
ENTRYPOINT ["/worker"]
DOCKERFILE
}

install_agent_sandbox() {
  log "Installing upstream agent-sandbox ${agent_sandbox_version}"
  run kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/manifest.yaml"
  run kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/extensions.yaml"

  for crd in \
    sandboxes.agents.x-k8s.io \
    sandboxclaims.extensions.agents.x-k8s.io \
    sandboxtemplates.extensions.agents.x-k8s.io \
    sandboxwarmpools.extensions.agents.x-k8s.io; do
    run kubectl wait --for=condition=Established "crd/${crd}" --timeout=90s
  done

  run kubectl -n agent-sandbox-system rollout status deployment/agent-sandbox-controller --timeout=5m
}

agent_sandbox_module_dir() {
  go list -m -f '{{.Dir}}' sigs.k8s.io/agent-sandbox
}

build_sandbox_router_image() {
  local module_dir router_dir
  module_dir="$(agent_sandbox_module_dir)"
  router_dir="${module_dir}/clients/python/agentic-sandbox-client/sandbox-router"
  [[ -d "${router_dir}" ]] || die "agent-sandbox router source not found: ${router_dir}"

  log "Building upstream sandbox router image ${sandbox_router_image}"
  run docker build -t "${sandbox_router_image}" "${router_dir}"
}

deploy_sandbox_router() {
  local module_dir router_yaml
  module_dir="$(agent_sandbox_module_dir)"
  router_yaml="${module_dir}/clients/python/agentic-sandbox-client/sandbox-router/sandbox_router.yaml"
  [[ -f "${router_yaml}" ]] || die "agent-sandbox router manifest not found: ${router_yaml}"

  router_namespace="${orka_namespace}"
  log "Deploying upstream sandbox router into ${router_namespace}"
  awk -v image="${sandbox_router_image}" '{ gsub(/\$\{ROUTER_IMAGE\}/, image); print }' "${router_yaml}" |
    kubectl -n "${router_namespace}" apply -f -
  run kubectl -n "${router_namespace}" rollout status deployment/sandbox-router-deployment --timeout=5m
}

patch_controller_for_agent_sandbox() {
  local router_url
  router_url="http://sandbox-router-svc.${router_namespace}.svc.cluster.local:8080"

  log "Configuring Orka controller for agent-sandbox"
  kubectl -n "${orka_namespace}" get deployment "${orka_controller_deployment}" -o json |
    jq \
      --arg claudeImage "${fake_claude_image}" \
      --arg routerURL "${router_url}" \
      --arg template "${sandbox_template_name}" '
      def upsert_arg($name; $value):
        . as $args
        | if any($args[]?; startswith($name + "=")) then
            map(if startswith($name + "=") then $name + "=" + $value else . end)
          else
            $args + [$name + "=" + $value]
          end;
      .spec.template.spec.containers |= map(
        if .name == "manager" then
          .imagePullPolicy = "IfNotPresent"
          | .args = ((.args // []) | upsert_arg("--claude-worker-image"; $claudeImage))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-enabled"; "true"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-router-url"; $routerURL))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-default-template"; $template))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-warm-pool-policy"; "disabled"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-namespace-strategy"; "task"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-claim-timeout"; "3m"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-command-timeout"; "5m"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-cleanup-policy"; "delete"))
        else . end
      )
    ' | kubectl apply -f -

  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
}

apply_sandbox_template() {
  log "Creating agent-sandbox template ${sandbox_template_name}"
  kubectl apply -f - <<YAML
apiVersion: extensions.agents.x-k8s.io/v1alpha1
kind: SandboxTemplate
metadata:
  name: ${sandbox_template_name}
  namespace: ${orka_namespace}
spec:
  networkPolicyManagement: Unmanaged
  service: true
  podTemplate:
    spec:
      dnsPolicy: ClusterFirst
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
        fsGroup: 1000
        runAsNonRoot: true
      containers:
        - name: agent
          image: ${fake_claude_image}
          imagePullPolicy: IfNotPresent
          command: ["/usr/local/bin/sandbox-runtime"]
          ports:
            - containerPort: 8888
              protocol: TCP
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop:
                - ALL
          volumeMounts:
            - name: workspace
              mountPath: /workspace
            - name: app
              mountPath: /app
            - name: tmp
              mountPath: /tmp
      volumes:
        - name: workspace
          emptyDir: {}
        - name: app
          emptyDir: {}
        - name: tmp
          emptyDir: {}
YAML
}

apply_agent() {
  log "Creating fake Claude runtime Agent ${agent_name}"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${agent_name}
  namespace: ${orka_namespace}
spec:
  runtime:
    type: claude
    defaultMaxTurns: 1
    defaultAllowBash: false
  model:
    name: fake-model-no-network
YAML
}

apply_agent_task() {
  local task_name="$1"
  local prompt="$2"
  local reuse_policy="$3"
  local cleanup_policy="$4"
  local session_block=""

  if [[ "${reuse_policy}" == "session" ]]; then
    session_block="$(cat <<YAML
  sessionRef:
    name: ${session_name}
    create: true
    append: true
YAML
)"
  fi

  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${task_name}
  namespace: ${orka_namespace}
spec:
  type: agent
  agentRef:
    name: ${agent_name}
  agentRuntime:
    maxTurns: 1
  timeout: 8m0s
${session_block}
  execution:
    workspace:
      enabled: true
      templateRef:
        name: ${sandbox_template_name}
      reusePolicy: ${reuse_policy}
      cleanupPolicy: ${cleanup_policy}
  prompt: "${prompt}"
YAML
}

wait_for_task_succeeded() {
  local task_name="$1"
  local phase message
  log "Waiting for Task/${task_name} to succeed"

  for _ in $(seq 1 240); do
    phase="$(kubectl -n "${orka_namespace}" get task "${task_name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "${phase}" in
      Succeeded)
        run kubectl -n "${orka_namespace}" wait --for=jsonpath='{.status.resultRef.available}'=true "task/${task_name}" --timeout=2m
        return
        ;;
      Failed)
        message="$(kubectl -n "${orka_namespace}" get task "${task_name}" -o jsonpath='{.status.message}' 2>/dev/null || true)"
        die "Task/${task_name} failed: ${message:-no status message}"
        ;;
    esac
    sleep 2
  done

  die "Task/${task_name} did not succeed within ${wait_timeout}"
}

task_job_name() {
  local task_name="$1"
  kubectl -n "${orka_namespace}" get task "${task_name}" -o jsonpath='{.status.jobName}'
}

task_logs() {
  local task_name="$1"
  local job_name
  job_name="$(task_job_name "${task_name}")"
  kubectl -n "${orka_namespace}" logs -l "job-name=${job_name}" --all-containers --tail=500
}

fetch_result() {
  local api_base="$1"
  local token="$2"
  local task_name="$3"
  curl -fsS \
    -H "Authorization: Bearer ${token}" \
    "${api_base}/api/v1/tasks/${task_name}/result?namespace=${orka_namespace}" |
    jq -er '.result'
}

assert_result_contains() {
  local result="$1"
  local expected="$2"
  if [[ "${result}" != *"${expected}"* ]]; then
    printf 'result did not contain %q:\n%s\n' "${expected}" "${result}" >&2
    exit 1
  fi
}

session_claim_name() {
  local hash
  hash="$(printf '%s\0%s\0%s\0%s\0%s' \
    "${orka_namespace}" \
    "${orka_namespace}" \
    "${orka_namespace}" \
    "${sandbox_template_name}" \
    "${session_name}" |
    sha256_hex |
    awk '{print substr($1, 1, 32)}')"
  printf 'orka-session-%s\n' "${hash}"
}

sandbox_pod_name() {
  local sandbox="$1"
  local selector=""
  local pod=""

  for _ in $(seq 1 60); do
    selector="$(kubectl -n "${orka_namespace}" get sandbox "${sandbox}" -o jsonpath='{.status.selector}' 2>/dev/null || true)"
    if [[ -n "${selector}" ]]; then
      pod="$(kubectl -n "${orka_namespace}" get pod -l "${selector}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    fi

    if [[ -z "${pod}" ]] && kubectl -n "${orka_namespace}" get pod "${sandbox}" >/dev/null 2>&1; then
      pod="${sandbox}"
    fi

    if [[ -n "${pod}" ]]; then
      printf '%s\n' "${pod}"
      return 0
    fi

    sleep 2
  done

  die "could not resolve pod for Sandbox/${sandbox}"
}

verify_delete_cleanup() {
  local logs="$1"
  local claim
  claim="$(printf '%s\n' "${logs}" | sed -n 's/.*completed in sandbox workspace \([^[:space:]]*\).*/\1/p' | tail -n 1)"
  [[ -n "${claim}" ]] || die "could not find claimed sandbox workspace in delete task logs"

  log "Verifying delete cleanup removed SandboxClaim/${claim}"
  run kubectl -n "${orka_namespace}" wait --for=delete "sandboxclaim/${claim}" --timeout=2m
}

verify_retained_claim_reused() {
  local claim="$1"
  log "Verifying retained session SandboxClaim/${claim}"
  kubectl -n "${orka_namespace}" get sandboxclaim "${claim}" -o json |
    jq -e --arg template "${sandbox_template_name}" '
      .spec.sandboxTemplateRef.name == $template
      and ((.status.sandbox.name // "") != "")
    ' >/dev/null

  local sandbox
  sandbox="$(kubectl -n "${orka_namespace}" get sandboxclaim "${claim}" -o jsonpath='{.status.sandbox.name}')"
  kubectl -n "${orka_namespace}" get sandbox "${sandbox}" >/dev/null
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq
  require_sha256

  cd "${repo_root}"
  trap 'status=$?; on_exit "${status}"; exit "${status}"' EXIT

  setup_kind_cluster
  run kubectl config use-context "kind-${kind_cluster}"

  install_agent_sandbox

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"

  write_fake_claude_dockerfile
  log "Building fake Claude worker/runtime image ${fake_claude_image}"
  run docker build -t "${fake_claude_image}" -f "${fake_dockerfile}" .
  build_sandbox_router_image

  log "Loading images into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"
  run kind load docker-image "${fake_claude_image}" --name "${kind_cluster}"
  run kind load docker-image "${sandbox_router_image}" --name "${kind_cluster}"

  log "Deploying Orka manager"
  run make deploy IMG="${manager_image}"
  run kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s
  deploy_sandbox_router
  patch_controller_for_agent_sandbox

  apply_sandbox_template
  apply_agent

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}")"
  local api_base token
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"
  token="$(kubectl -n "${orka_namespace}" create token orka-controller-manager --duration=20m)"
  [[ -n "${token}" ]] || die "failed to create Orka API token"

  log "Running delete-policy sandbox smoke task"
  apply_agent_task "${delete_task_name}" "delete policy sandbox smoke" "none" "delete"
  wait_for_task_succeeded "${delete_task_name}"
  local delete_result delete_logs
  delete_result="$(fetch_result "${api_base}" "${token}" "${delete_task_name}")"
  assert_result_contains "${delete_result}" "ORKA_LIVE_SANDBOX_OK"
  assert_result_contains "${delete_result}" "depth=1"
  assert_result_contains "${delete_result}" "enabled=false"
  assert_result_contains "${delete_result}" "retained_marker=absent"
  delete_logs="$(task_logs "${delete_task_name}")"
  assert_result_contains "${delete_logs}" "completed in sandbox workspace"
  verify_delete_cleanup "${delete_logs}"

  log "Running retained session sandbox task"
  local session_claim
  session_claim="$(session_claim_name)"
  apply_agent_task "${retain_task_one}" "write retained marker" "session" "retain"
  wait_for_task_succeeded "${retain_task_one}"
  local retain_one_result
  retain_one_result="$(fetch_result "${api_base}" "${token}" "${retain_task_one}")"
  assert_result_contains "${retain_one_result}" "ORKA_LIVE_SANDBOX_OK"
  assert_result_contains "${retain_one_result}" "retained_marker=absent"
  verify_retained_claim_reused "${session_claim}"

  log "Running second retained session task to verify reattach"
  apply_agent_task "${retain_task_two}" "read retained marker" "session" "retain"
  wait_for_task_succeeded "${retain_task_two}"
  local retain_two_result
  retain_two_result="$(fetch_result "${api_base}" "${token}" "${retain_task_two}")"
  assert_result_contains "${retain_two_result}" "ORKA_LIVE_SANDBOX_OK"
  assert_result_contains "${retain_two_result}" "retained_marker=present"
  verify_retained_claim_reused "${session_claim}"

  log "Verifying staged token files were scrubbed from retained workspace"
  local sandbox pod
  sandbox="$(kubectl -n "${orka_namespace}" get sandboxclaim "${session_claim}" -o jsonpath='{.status.sandbox.name}')"
  pod="$(sandbox_pod_name "${sandbox}")"
  if kubectl -n "${orka_namespace}" exec "pod/${pod}" -c agent -- /bin/sh -c 'test ! -e /app/orka-sa-token && test ! -e /app/orka-transaction-token && test ! -e /app/orka-context-subject-token'; then
    log "Retained workspace token scrub verified"
  else
    die "retained workspace still contains staged token files"
  fi

  log "Cleaning up retained session claim"
  run kubectl -n "${orka_namespace}" delete sandboxclaim "${session_claim}" --ignore-not-found=true --wait=true --timeout=2m

  log "Live agent-sandbox e2e passed"
}

main "$@"
