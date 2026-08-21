#!/usr/bin/env bash

set -Eeuo pipefail

sanitize_image_tag() {
  printf '%s' "$1" | LC_ALL=C tr -c 'A-Za-z0-9_.-' '-'
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "${script_dir}/lib/e2e-common.sh"
# shellcheck source=scripts/lib/kind-local-registry.sh
. "${script_dir}/lib/kind-local-registry.sh"
# shellcheck source=scripts/lib/e2e-admission-tls.sh
. "${script_dir}/lib/e2e-admission-tls.sh"

agent_sandbox_version="${AGENT_SANDBOX_VERSION:-v0.5.4}"
kind_cluster="${KIND_CLUSTER:-orka-live-agent-sandbox-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
orka_api_service="${ORKA_API_SERVICE:-orka-api}"
orka_api_service_port="${ORKA_API_SERVICE_PORT:-8080}"
orka_api_local_port="${ORKA_API_LOCAL_PORT:-18084}"
orka_api_client_service_account="${ORKA_API_CLIENT_SERVICE_ACCOUNT:-orka-client}"
router_api_local_port="${ORKA_AGENT_SANDBOX_ROUTER_LOCAL_PORT:-18085}"
e2e_run_id="$(sanitize_image_tag "${ORKA_AGENT_SANDBOX_RUN_ID:-${GITHUB_RUN_ID:-manual}-$(date -u +%Y%m%d%H%M%S)}")"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:live-agent-sandbox-e2e-${e2e_run_id}}"
publisher_image="${ORKA_WORKSPACE_PUBLISHER_IMAGE:-orka-workspace-publisher:live-agent-sandbox-e2e-${e2e_run_id}}"
sandbox_fixture_image="${ORKA_AGENT_SANDBOX_FIXTURE_IMAGE:-orka-agent-sandbox-fixture:live-agent-sandbox-e2e-${e2e_run_id}}"
sandbox_router_image="${ORKA_AGENT_SANDBOX_ROUTER_IMAGE:-orka-agent-sandbox-router:live-agent-sandbox-e2e-${e2e_run_id}}"
responses_fixture_image="${ORKA_RESPONSES_FIXTURE_IMAGE:-orka-openai-responses-fixture:live-agent-sandbox-e2e-${e2e_run_id}}"
sandbox_template_name="${ORKA_AGENT_SANDBOX_TEMPLATE:-orka-agent-sandbox-e2e-template}"
smoke_claim_name="${ORKA_AGENT_SANDBOX_SMOKE_CLAIM:-orka-agent-sandbox-e2e-retained-smoke}"
# The workspace-backed ACP Task smoke builds the real Codex runtime image and
# proves the workspace-provider-backed RuntimePool path live against upstream
# agent-sandbox (admission, claim materialization, authenticated Serving, and
# cleanup), then completes a real prompt through the authenticated provider
# proxy and the local Responses-compatible fixture. Set to 0 to skip the
# runtime image build and smoke.
acp_task_smoke_enabled="${ORKA_AGENT_SANDBOX_ACP_TASK_SMOKE:-1}"
acp_codex_runtime_image="${ORKA_ACP_CODEX_RUNTIME_IMAGE:-orka-acp-codex-runtime:live-agent-sandbox-e2e-${e2e_run_id}}"
acp_runtime_namespace="${ORKA_ACP_RUNTIME_NAMESPACE:-orka-runtimes}"
acp_task_namespace="${ORKA_AGENT_SANDBOX_ACP_TASK_NAMESPACE:-${orka_namespace}}"
acp_task_name="orka-ws-sandbox-smoke"
acp_agent_name="orka-ws-sandbox-agent"
api_pf_pid=""
router_pf_pid=""
router_namespace=""
created_kind_cluster="0"
agent_sandbox_module_cache=""
work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/live-agent-sandbox-e2e.XXXXXX")"
kind_config="${ORKA_AGENT_SANDBOX_KIND_CONFIG:-${work_dir}/kind-config.yaml}"
fixture_dockerfile="${work_dir}/Dockerfile.sandbox-fixture"
api_pf_log="${work_dir}/api-port-forward.log"
router_pf_log="${work_dir}/router-port-forward.log"
smoke_go_dir="${repo_root}/.tmp-live-agent-sandbox-smoke-${e2e_run_id}"
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"

if [[ "${agent_sandbox_version}" != "v0.5.4" ]]; then
  die "this e2e is pinned to agent-sandbox v0.5.4 to match go.mod"
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
  cleanup_one_port_forward "${router_pf_pid}"
  router_pf_pid=""
}

restore_manager_kustomization() {
  if [[ -f "${manager_kustomization_backup}" ]]; then
    cp "${manager_kustomization_backup}" "${manager_kustomization}" || true
  fi
}

dump_diagnostics() {
  log "Collecting diagnostics"

  {
    echo "=== Current Kubernetes Context ==="
    kubectl config current-context 2>/dev/null || true
    echo
    echo "=== Orka Namespace Resources ==="
    kubectl get pods,svc,deploy,jobs,tasks,agents,sandboxclaims,sandboxes,sandboxtemplates,sandboxwarmpools -n "${orka_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Agent Sandbox Resources ==="
    kubectl get pods,svc,deploy,sandboxclaims,sandboxes,sandboxtemplates,sandboxwarmpools -A -o wide 2>/dev/null || true
    echo
    echo "=== Workspace-backed RuntimePools ==="
    kubectl get runtimepools -A -o wide 2>/dev/null || true
    kubectl get runtimepools -A -o yaml 2>/dev/null || true
    echo
    echo "=== ACP Runtime Namespace Resources ==="
    kubectl get pods,secrets,sandboxclaims,sandboxes,sandboxtemplates,sandboxwarmpools -n "${acp_runtime_namespace}" -o wide 2>/dev/null || true
    echo
    echo "=== Responses Fixture ==="
    kubectl get pods,svc,deploy -n vekil-system -o wide 2>/dev/null || true
    kubectl logs deployment/vekil -n vekil-system --tail=300 2>/dev/null || true
    echo
    echo "=== Workspace-backed ACP Task ==="
    kubectl -n "${acp_task_namespace}" get task "${acp_task_name}" -o yaml 2>/dev/null || true
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
    echo
    echo "=== Router Port-forward Log ==="
    if [[ -f "${router_pf_log}" ]]; then
      cat "${router_pf_log}" 2>/dev/null || true
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
  restore_manager_kustomization
  orka_kind_registry_stop
  if [[ "${created_kind_cluster}" == "1" ]]; then
    kind delete cluster --name "${kind_cluster}" >/dev/null 2>&1 || true
  fi
  rm -rf "${smoke_go_dir}" >/dev/null 2>&1 || true
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

assert_task_result_contains() {
  local namespace_arg="$1"
  local task_name="$2"
  local expected_marker="$3"
  local api_base="http://127.0.0.1:${orka_api_local_port}"
  local api_token result_file status attempts_remaining

  wait_for_http "${api_base}/readyz" "Orka API /readyz"
  api_token="$(kubectl -n "${namespace_arg}" create token "${orka_api_client_service_account}")"
  result_file="${work_dir}/${task_name}-result.json"
  attempts_remaining=15
  while (( attempts_remaining > 0 )); do
    status="$(curl --silent --show-error --connect-timeout 5 --max-time 30 \
      --header "Authorization: Bearer ${api_token}" \
      --output "${result_file}" --write-out '%{http_code}' \
      "${api_base}/api/v1/tasks/${task_name}/result?namespace=${namespace_arg}" \
      2>>"${api_pf_log}" || true)"
    if [[ "${status}" == "200" ]] &&
      jq -er '.result' "${result_file}" | grep -Fq "${expected_marker}"; then
      log "Task/${task_name} result contains ${expected_marker}"
      return 0
    fi
    attempts_remaining=$((attempts_remaining - 1))
    sleep 2
  done

  die "Task/${task_name} result did not contain the expected marker ${expected_marker} (last HTTP status: ${status:-none})"
}

deploy_responses_fixture() {
  log "Deploying local Responses-compatible provider fixture"
  kubectl -n vekil-system apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vekil
  labels:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/component: responses-fixture
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: vekil
      app.kubernetes.io/component: responses-fixture
  template:
    metadata:
      labels:
        app.kubernetes.io/name: vekil
        app.kubernetes.io/component: responses-fixture
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: responses
          image: ${responses_fixture_image}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 1337
          readinessProbe:
            httpGet:
              path: /healthz
              port: http
          livenessProbe:
            httpGet:
              path: /healthz
              port: http
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
---
apiVersion: v1
kind: Service
metadata:
  name: vekil
  labels:
    app.kubernetes.io/name: vekil
spec:
  selector:
    app.kubernetes.io/name: vekil
    app.kubernetes.io/component: responses-fixture
  ports:
    - name: http
      port: 1337
      targetPort: http
YAML
  run kubectl -n vekil-system rollout status deployment/vekil --timeout=2m
}

ensure_api_client_identity() {
  log "Creating scoped Orka API client identity ${acp_task_namespace}/${orka_api_client_service_account}"
  kubectl apply -f - <<YAML
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${orka_api_client_service_account}
  namespace: ${acp_task_namespace}
automountServiceAccountToken: false
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${orka_api_client_service_account}
  namespace: ${acp_task_namespace}
rules:
  - apiGroups: ["core.orka.ai"]
    resources: ["tasks"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: ${orka_api_client_service_account}
  namespace: ${acp_task_namespace}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: ${orka_api_client_service_account}
subjects:
  - kind: ServiceAccount
    name: ${orka_api_client_service_account}
    namespace: ${acp_task_namespace}
YAML
}

write_sandbox_fixture_dockerfile() {
  cat >"${fixture_dockerfile}" <<'DOCKERFILE'
FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
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

COPY --from=builder /out/sandbox-runtime /usr/local/bin/sandbox-runtime

RUN chmod 0755 /usr/local/bin/sandbox-runtime

RUN groupadd -g 1000 worker \
    && useradd -u 1000 -g worker -m worker \
    && mkdir -p /workspace /app /tmp \
    && chown -R 1000:1000 /workspace /app /home/worker /tmp

USER 1000:1000
ENV HOME=/home/worker
ENTRYPOINT ["/usr/local/bin/sandbox-runtime"]
DOCKERFILE
}

install_agent_sandbox() {
  log "Installing upstream agent-sandbox ${agent_sandbox_version}"
  run kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/sandbox.yaml"
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
  local module_dir

  if [[ -n "${agent_sandbox_module_cache}" ]]; then
    printf '%s\n' "${agent_sandbox_module_cache}"
    return
  fi

  module_dir="$(go list -m -f '{{.Dir}}' sigs.k8s.io/agent-sandbox)"
  if [[ -z "${module_dir}" ]]; then
    log "Downloading agent-sandbox module source"
    run go mod download sigs.k8s.io/agent-sandbox
    module_dir="$(go list -m -f '{{.Dir}}' sigs.k8s.io/agent-sandbox)"
  fi

  [[ -n "${module_dir}" ]] || die "failed to resolve agent-sandbox module directory"
  agent_sandbox_module_cache="${module_dir}"
  printf '%s\n' "${module_dir}"
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
  awk -v image="${sandbox_router_image}" '
    {
      gsub(/\$\{ROUTER_IMAGE\}/, image)
      if ($0 ~ /name: ALLOW_UNAUTHENTICATED_ROUTER/) { allow = 1 }
      if (allow == 1 && $0 ~ /value: "false"/) {
        sub(/value: "false"/, "value: \"true\"")
        allow = 0
      }
      print
    }
  ' "${router_yaml}" | kubectl -n "${router_namespace}" apply -f -
  run kubectl -n "${router_namespace}" rollout status deployment/sandbox-router-deployment --timeout=5m
}

patch_controller_for_agent_sandbox() {
  local router_url rollout_id
  router_url="http://sandbox-router-svc.${router_namespace}.svc.cluster.local:8080"
  rollout_id="${e2e_run_id}"

  log "Configuring Orka controller for agent-sandbox"
  kubectl -n "${orka_namespace}" get deployment "${orka_controller_deployment}" -o json |
    jq \
      --arg routerURL "${router_url}" \
      --arg rolloutID "${rollout_id}" \
      --arg template "${sandbox_template_name}" '
      def upsert_arg($name; $value):
        . as $args
        | if any($args[]?; startswith($name + "=")) then
            map(if startswith($name + "=") then $name + "=" + $value else . end)
          else
            $args + [$name + "=" + $value]
          end;
      .spec.template.metadata.annotations = ((.spec.template.metadata.annotations // {}) + {
        "orka.ai/live-agent-sandbox-e2e-run": $rolloutID
      })
      |
      .spec.template.spec.containers |= map(
        if .name == "manager" then
          .imagePullPolicy = "IfNotPresent"
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-enabled"; "true"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-router-url"; $routerURL))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-default-template"; $template))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-warm-pool-policy"; "disabled"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-namespace-strategy"; "task"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-claim-timeout"; "3m"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-command-timeout"; "5m"))
          | .args = ((.args // []) | upsert_arg("--agent-sandbox-cleanup-policy"; "delete"))
          | .args = ((.args // []) | upsert_arg("--acp-workspace-dispatch-enabled"; "true"))
        else . end
      )
    ' | kubectl apply -f -

  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
}

apply_sandbox_template() {
  log "Creating agent-sandbox template and warm pool ${sandbox_template_name}"
  kubectl apply -f - <<YAML
apiVersion: extensions.agents.x-k8s.io/v1beta1
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
          image: ${sandbox_fixture_image}
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
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata:
  name: ${sandbox_template_name}
  namespace: ${orka_namespace}
spec:
  replicas: 0
  sandboxTemplateRef:
    name: ${sandbox_template_name}
YAML
}

write_workspace_smoke_go() {
  rm -rf "${smoke_go_dir}"
  mkdir -p "${smoke_go_dir}"
  cat >"${smoke_go_dir}/main.go" <<'GO'
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	sandbox "sigs.k8s.io/agent-sandbox/clients/go/sandbox"

	"github.com/orka-agents/orka/internal/workspace"
)

func main() {
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintln(os.Stderr, recovered)
			os.Exit(1)
		}
	}()

	namespace := mustEnv("ORKA_NAMESPACE")
	warmPool := mustEnv("ORKA_AGENT_SANDBOX_TEMPLATE")
	routerURL := mustEnv("ORKA_AGENT_SANDBOX_ROUTER_URL")
	retainedClaim := mustEnv("ORKA_AGENT_SANDBOX_SMOKE_CLAIM")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	helper, err := sandbox.NewK8sHelper(nil, logr.Discard())
	must("create Kubernetes helper", err)

	executor := workspace.NewAgentSandboxExecutor(workspace.WithAgentSandboxAPIURL(routerURL))
	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace:         namespace,
		TaskName:          "live-agent-sandbox-delete-smoke",
		Template:          workspace.TemplateRef{Name: warmPool},
		Timeout:           3 * time.Minute,
		MaxRequestTimeout: 5 * time.Minute,
	})
	must("claim delete workspace", err)
	deleteRef := claim.Ref
	cleanupDelete := true
	defer func() {
		if cleanupDelete {
			cleanupWorkspace(executor, deleteRef)
		}
	}()
	verifyWarmPoolRef(ctx, helper, namespace, deleteRef.ClaimName, warmPool)
	execContains(ctx, executor, deleteRef, "delete workspace exec", "test \"$ORKA_SMOKE_ENV\" = env-ok && printf delete-smoke-ok", "delete-smoke-ok")
	_, err = executor.Delete(ctx, workspace.DeleteRequest{Ref: deleteRef, Reason: "live smoke delete cleanup", Timeout: 2 * time.Minute})
	must("delete delete workspace", err)
	cleanupDelete = false
	waitClaimDeleted(ctx, helper, namespace, deleteRef.ClaimName)
	fmt.Printf("delete workspace claim %s executed and deleted\n", deleteRef.ClaimName)

	retainedExecutor := workspace.NewAgentSandboxExecutor(workspace.WithAgentSandboxAPIURL(routerURL))
	retained, err := retainedExecutor.Claim(ctx, workspace.ClaimRequest{
		Namespace:         namespace,
		TaskName:          "live-agent-sandbox-retain-smoke",
		ClaimName:         retainedClaim,
		CreateIfMissing:   true,
		Template:          workspace.TemplateRef{Name: warmPool},
		ReuseKey:          "live-smoke-session",
		Timeout:           3 * time.Minute,
		MaxRequestTimeout: 5 * time.Minute,
	})
	must("claim retained workspace", err)
	retainedRef := retained.Ref
	cleanupRetained := true
	defer func() {
		if cleanupRetained {
			cleanupWorkspace(retainedExecutor, retainedRef)
		}
	}()
	verifyWarmPoolRef(ctx, helper, namespace, retainedRef.ClaimName, warmPool)
	execContains(ctx, retainedExecutor, retainedRef, "retained workspace marker write", "printf retained-smoke-ok > retained-marker.txt && cat retained-marker.txt", "retained-smoke-ok")
	_, err = retainedExecutor.Release(ctx, workspace.ReleaseRequest{Ref: retainedRef, Retain: true, Reason: "live smoke retain", Timeout: 2 * time.Minute})
	must("release retained workspace", err)
	verifyWarmPoolRef(ctx, helper, namespace, retainedRef.ClaimName, warmPool)

	reuseExecutor := workspace.NewAgentSandboxExecutor(workspace.WithAgentSandboxAPIURL(routerURL))
	reused, err := reuseExecutor.Claim(ctx, workspace.ClaimRequest{
		Namespace:         namespace,
		TaskName:          "live-agent-sandbox-reuse-smoke",
		ClaimName:         retainedClaim,
		CreateIfMissing:   true,
		Template:          workspace.TemplateRef{Name: warmPool},
		ReuseKey:          "live-smoke-session",
		Timeout:           3 * time.Minute,
		MaxRequestTimeout: 5 * time.Minute,
	})
	must("reattach retained workspace", err)
	if !reused.Reused {
		fatalf("reattach retained workspace: Reused=%v, want true", reused.Reused)
	}
	retainedExecutor = reuseExecutor
	retainedRef = reused.Ref
	verifyWarmPoolRef(ctx, helper, namespace, retainedRef.ClaimName, warmPool)
	execContains(ctx, reuseExecutor, retainedRef, "retained workspace marker read", "cat retained-marker.txt", "retained-smoke-ok")
	_, err = reuseExecutor.Delete(ctx, workspace.DeleteRequest{Ref: retainedRef, Reason: "live smoke retained cleanup", Timeout: 2 * time.Minute})
	must("delete retained workspace", err)
	cleanupRetained = false
	waitClaimDeleted(ctx, helper, namespace, retainedRef.ClaimName)
	fmt.Printf("retained workspace claim %s reused and deleted\n", retainedRef.ClaimName)
	fmt.Println("agent-sandbox workspace adapter smoke passed")
}

func mustEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		fatalf("%s is required", name)
	}
	return value
}

func execContains(ctx context.Context, executor *workspace.AgentSandboxExecutor, ref workspace.WorkspaceRef, label, command, expected string) {
	result, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:     ref,
		Command: []string{"sh", "-c", command},
		Env:     map[string]string{"ORKA_SMOKE_ENV": "env-ok"},
		WorkDir: "/workspace",
		Timeout: 90 * time.Second,
	})
	must(label, err)
	if !strings.Contains(result.Stdout, expected) {
		fatalf("%s stdout = %q, want substring %q (stderr=%q)", label, result.Stdout, expected, result.Stderr)
	}
}

func verifyWarmPoolRef(ctx context.Context, helper *sandbox.K8sHelper, namespace, claimName, warmPool string) {
	// agent-sandbox v0.5 K8sHelper uses the extensions v1beta1 client, where
	// SandboxClaimSpec requires warmPoolRef. This compile-checks that Orka's
	// adapter creates the v1beta1 claim shape expected by the upgraded SDK.
	claim, err := helper.ExtensionsClient.SandboxClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	must("get SandboxClaim "+claimName, err)
	if claim.Spec.WarmPoolRef.Name != warmPool {
		fatalf("SandboxClaim/%s warmPoolRef.name = %q, want %q", claimName, claim.Spec.WarmPoolRef.Name, warmPool)
	}
	if strings.TrimSpace(claim.Status.SandboxStatus.Name) == "" {
		fatalf("SandboxClaim/%s has empty status.sandbox.name", claimName)
	}
}

func waitClaimDeleted(ctx context.Context, helper *sandbox.K8sHelper, namespace, claimName string) {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, err := helper.ExtensionsClient.SandboxClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return
		}
		if err != nil {
			must("wait for SandboxClaim deletion", err)
		}
		if time.Now().After(deadline) {
			fatalf("SandboxClaim/%s was not deleted within timeout", claimName)
		}
		time.Sleep(2 * time.Second)
	}
}

func cleanupWorkspace(executor *workspace.AgentSandboxExecutor, ref workspace.WorkspaceRef) {
	if executor == nil || ref.IsZero() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = executor.Delete(ctx, workspace.DeleteRequest{Ref: ref, Reason: "live smoke deferred cleanup", Timeout: 2 * time.Minute})
}

func must(label string, err error) {
	if err != nil {
		fatalf("%s: %v", label, err)
	}
}

func fatalf(format string, args ...any) {
	panic(fmt.Sprintf(format, args...))
}
GO
}

run_workspace_smoke() {
  local router_url="$1"
  log "Running live agent-sandbox workspace adapter smoke"
  write_workspace_smoke_go
  (cd "${repo_root}" && \
    ORKA_NAMESPACE="${orka_namespace}" \
    ORKA_AGENT_SANDBOX_TEMPLATE="${sandbox_template_name}" \
    ORKA_AGENT_SANDBOX_ROUTER_URL="${router_url}" \
    ORKA_AGENT_SANDBOX_SMOKE_CLAIM="${smoke_claim_name}" \
    go run "./$(basename "${smoke_go_dir}")")
}

reset_e2e_resources() {
  log "Resetting fixed-name agent-sandbox e2e resources"
  run kubectl -n "${acp_task_namespace}" delete task "${acp_task_name}"     --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${acp_task_namespace}" delete agent "${acp_agent_name}"     --ignore-not-found=true --wait=true --timeout=1m
  run kubectl -n "${orka_namespace}" delete sandboxclaim "${smoke_claim_name}" \
    --ignore-not-found=true \
    --wait=true \
    --timeout=2m
  run kubectl -n "${orka_namespace}" delete sandboxwarmpool "${sandbox_template_name}" \
    --ignore-not-found=true \
    --wait=true \
    --timeout=2m
  run kubectl -n "${orka_namespace}" delete sandboxtemplate "${sandbox_template_name}" \
    --ignore-not-found=true \
    --wait=true \
    --timeout=2m
}

wait_for_jsonpath() {
  local kind="$1" namespace="$2" name="$3" path="$4" want="$5" timeout_seconds="$6"
  local started now value
  started="$(date +%s)"
  while true; do
    value="$(kubectl -n "${namespace}" get "${kind}" "${name}" -o jsonpath="${path}" 2>/dev/null || true)"
    if [[ "${value}" == "${want}" ]]; then
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout_seconds )); then
      die "timed out waiting for ${kind}/${name} ${path}=${want} (last: ${value:-<empty>})"
    fi
    sleep 3
  done
}

wait_for_nonempty_jsonpath() {
  local kind="$1" namespace="$2" name="$3" path="$4" timeout_seconds="$5"
  local started now value
  started="$(date +%s)"
  while true; do
    value="$(kubectl -n "${namespace}" get "${kind}" "${name}" -o jsonpath="${path}" 2>/dev/null || true)"
    if [[ -n "${value}" ]]; then
      printf '%s' "${value}"
      return 0
    fi
    now="$(date +%s)"
    if (( now - started >= timeout_seconds )); then
      die "timed out waiting for ${kind}/${name} ${path} to be set"
    fi
    sleep 3
  done
}

# run_workspace_backed_acp_task_smoke proves the Phase-1 workspace-provider
# adapter live against upstream agent-sandbox:
#   1. a Task.spec.execution.workspace agent Task is admitted (not rejected
#      with WorkspaceValidationFailed) and binds a dedicated acp-ws-* pool;
#   2. the pool materializes a controller-rendered SandboxTemplate, a
#      zero-replica SandboxWarmPool, and one SandboxClaim through the real
#      provider controller, and the sandbox Pod runs the immutable Codex
#      runtime image;
#   3. the authenticated exact-instance fence probe reaches Serving/Accepting;
#   4. a real Codex prompt succeeds through the authenticated provider proxy;
#   5. Task status stays provider-neutral (no claim identifiers);
#   6. pool deletion removes the claim, warm pool, and template.
run_workspace_backed_acp_task_smoke() {
  log "Running workspace-backed ACP Task infrastructure smoke"

  bash "${repo_root}/scripts/lib/ensure-static-mode-namespace.sh" \
    kubectl "${acp_task_namespace}" harness-v2

  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${acp_agent_name}
  namespace: ${acp_task_namespace}
spec:
  runtime:
    type: codex
    contractVersion: orka.harness.v2
    defaultMaxTurns: 1
  model:
    name: gpt-5.5
---
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: ${acp_task_name}
  namespace: ${acp_task_namespace}
spec:
  type: agent
  agentRef:
    name: ${acp_agent_name}
  agentRuntime:
    maxTurns: 1
  timeout: 10m0s
  execution:
    workspace:
      enabled: true
      provider: agent-sandbox
      reusePolicy: none
      cleanupPolicy: delete
  prompt: "Reply exactly: ORKA_WS_SANDBOX_OK"
YAML

  local pool_name
  pool_name="$(wait_for_nonempty_jsonpath task "${acp_task_namespace}" "${acp_task_name}"     '{.status.execution.runtimePoolName}' 120)"
  log "Workspace-backed Task bound RuntimePool ${pool_name}"
  [[ "${pool_name}" == acp-ws-codex-* ]] ||
    die "runtime pool ${pool_name} is not a workspace-backed pool"

  local workspace_provider workspace_reason
  workspace_provider="$(kubectl -n "${acp_task_namespace}" get task "${acp_task_name}"     -o jsonpath='{.status.executionWorkspace.provider}')"
  workspace_reason="$(kubectl -n "${acp_task_namespace}" get task "${acp_task_name}"     -o jsonpath='{.status.executionWorkspace.reason}')"
  [[ "${workspace_provider}" == "agent-sandbox" ]] ||
    die "workspace status provider ${workspace_provider}, want agent-sandbox"
  [[ "${workspace_reason}" != "WorkspaceValidationFailed" ]] ||
    die "workspace-backed Task was rejected with WorkspaceValidationFailed"

  log "Waiting for workspace-backed RuntimePool ${pool_name} to reach Serving"
  wait_for_jsonpath runtimepool "${acp_task_namespace}" "${pool_name}"     '{.status.lifecycle}' "Serving" 480

  local active_pod_uid
  active_pod_uid="$(kubectl -n "${acp_task_namespace}" get runtimepool "${pool_name}"     -o jsonpath='{.status.activeInstance.podUID}')"
  [[ -n "${active_pod_uid}" ]] || die "Serving pool has no active instance"

  local claim_count claim_name
  claim_count="$(kubectl get sandboxclaims -A     -l "orka.ai/runtime-pool-name=${pool_name}" -o name | wc -l | tr -d ' ')"
  [[ "${claim_count}" == "1" ]] ||
    die "expected exactly one SandboxClaim for ${pool_name}, found ${claim_count}"
  claim_name="$(kubectl get sandboxclaims -n "${acp_runtime_namespace}"     -l "orka.ai/runtime-pool-name=${pool_name}" -o jsonpath='{.items[0].metadata.name}')"
  log "Workspace-backed pool is Serving through SandboxClaim ${claim_name}"

  local sandbox_pod_image
  sandbox_pod_image="$(kubectl get pods -n "${acp_runtime_namespace}"     -l "orka.ai/runtime-pool-name=${pool_name}"     -o jsonpath='{.items[0].spec.containers[0].image}')"
  [[ "${sandbox_pod_image}" == *"acp-codex"* ]] ||
    die "sandbox Pod image ${sandbox_pod_image} is not the immutable Codex runtime image"

  if kubectl -n "${acp_task_namespace}" get task "${acp_task_name}" -o yaml | grep -q "${claim_name}"; then
    die "public Task status leaked the provider claim identifier ${claim_name}"
  fi

  log "Waiting for the workspace-backed Task to succeed"
  local started now task_payload phase execution_state execution_outcome result_available
  started="$(date +%s)"
  while true; do
    task_payload="$(kubectl -n "${acp_task_namespace}" get task "${acp_task_name}" -o json 2>/dev/null || true)"
    phase="$(jq -r '.status.phase // ""' <<<"${task_payload}")"
    execution_state="$(jq -r '.status.execution.state // ""' <<<"${task_payload}")"
    execution_outcome="$(jq -r '.status.execution.outcome // ""' <<<"${task_payload}")"
    result_available="$(jq -r '.status.resultRef.available // false' <<<"${task_payload}")"
    if [[ "${phase}" == "Succeeded" && "${execution_state}" == "Succeeded" &&
          "${execution_outcome}" == "Succeeded" && "${result_available}" == "true" ]]; then
      break
    fi
    if [[ "${phase}" == "Failed" || "${phase}" == "Cancelled" ||
          "${execution_state}" == "Failed" || "${execution_state}" == "Cancelled" ]]; then
      kubectl -n "${acp_task_namespace}" get task "${acp_task_name}" -o yaml >&2 || true
      die "workspace-backed Task reached terminal failure (phase=${phase:-<empty>}, state=${execution_state:-<empty>}, outcome=${execution_outcome:-<empty>})"
    fi
    now="$(date +%s)"
    if (( now - started >= 300 )); then
      kubectl -n "${acp_task_namespace}" get task "${acp_task_name}" -o yaml >&2 || true
      die "workspace-backed Task did not succeed (phase=${phase:-<empty>}, state=${execution_state:-<empty>}, outcome=${execution_outcome:-<empty>}, resultAvailable=${result_available})"
    fi
    sleep 3
  done
  log "Workspace-backed Task reached Succeeded/Succeeded with an available result"
  assert_task_result_contains "${acp_task_namespace}" "${acp_task_name}" "ORKA_WS_SANDBOX_OK"

  workspace_reason="$(kubectl -n "${acp_task_namespace}" get task "${acp_task_name}"     -o jsonpath='{.status.executionWorkspace.reason}')"
  [[ "${workspace_reason}" != "WorkspaceValidationFailed" ]] ||
    die "workspace-backed Task regressed to WorkspaceValidationFailed after dispatch"

  log "Cleaning up the workspace-backed Task and pool"
  run kubectl -n "${acp_task_namespace}" delete task "${acp_task_name}" --wait=true --timeout=3m
  run kubectl -n "${acp_task_namespace}" delete runtimepool "${pool_name}" --wait=true --timeout=4m
  local remaining
  remaining="$(kubectl get sandboxclaims,sandboxwarmpools,sandboxtemplates -n "${acp_runtime_namespace}"     -l "orka.ai/runtime-pool-name=${pool_name}" -o name | wc -l | tr -d ' ')"
  [[ "${remaining}" == "0" ]] ||
    die "pool finalization left ${remaining} provider objects for ${pool_name}"
  log "Workspace-backed ACP Task infrastructure smoke passed"
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
  require_cmd curl
  require_cmd jq
  require_cmd openssl

  cd "${repo_root}"
  [[ -f "${manager_kustomization}" ]] || die "missing ${manager_kustomization}"
  cp "${manager_kustomization}" "${manager_kustomization_backup}"

  trap 'status=$?; on_exit "${status}"; exit "${status}"' EXIT

  setup_kind_cluster
  run kubectl config use-context "kind-${kind_cluster}"
  log "Installing current Orka CRDs into the test cluster"
  run make install
  log "Creating the Vekil namespace required by the production ingress policy"
  kubectl create namespace vekil-system --dry-run=client -o yaml | kubectl apply -f -
  orka_kind_registry_start "${kind_cluster}"

  install_agent_sandbox

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"
  log "Building workspace publisher image ${publisher_image}"
  run make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="${publisher_image}"
  if [[ "${acp_task_smoke_enabled}" == "1" ]]; then
    log "Building immutable Codex ACP runtime image ${acp_codex_runtime_image} for the workspace-backed Task smoke"
    run make docker-build-acp-codex-runtime ACP_CODEX_RUNTIME_IMG="${acp_codex_runtime_image}"
    log "Building local Responses-compatible provider fixture image ${responses_fixture_image}"
    run docker build -t "${responses_fixture_image}" -f scripts/fixtures/openai-responses/Dockerfile .
  fi

  write_sandbox_fixture_dockerfile
  log "Building agent-sandbox HTTP fixture image ${sandbox_fixture_image}"
  run docker build -t "${sandbox_fixture_image}" -f "${fixture_dockerfile}" .
  build_sandbox_router_image

  log "Loading images into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"
  run kind load docker-image "${sandbox_fixture_image}" --name "${kind_cluster}"
  run kind load docker-image "${sandbox_router_image}" --name "${kind_cluster}"
  if [[ "${acp_task_smoke_enabled}" == "1" ]]; then
    run kind load docker-image "${responses_fixture_image}" --name "${kind_cluster}"
  fi

  local manager_ref publisher_ref
  manager_ref="$(orka_kind_registry_push "${manager_image}" "orka/controller")"
  publisher_ref="$(orka_kind_registry_push "${publisher_image}" "orka/workspace-publisher")"
  local placeholder_digest codex_runtime_ref
  placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
  codex_runtime_ref="example.invalid/orka/acp-codex@${placeholder_digest}"
  if [[ "${acp_task_smoke_enabled}" == "1" ]]; then
    codex_runtime_ref="$(orka_kind_registry_push "${acp_codex_runtime_image}" "orka/acp-codex-runtime")"
  fi

  log "Bootstrapping test-only admission TLS"
  orka_e2e_bootstrap_admission_tls

  if [[ "${acp_task_smoke_enabled}" == "1" ]]; then
    deploy_responses_fixture
  fi

  log "Deploying Orka manager (Codex runtime image real when the workspace-backed Task smoke is enabled; other runtimes inert)"
  run make deploy \
    IMG="${manager_ref}" \
    WORKSPACE_PUBLISHER_IMG="${publisher_ref}" \
    ACP_CODEX_RUNTIME_IMG="${codex_runtime_ref}" \
    ACP_CLAUDE_RUNTIME_IMG="example.invalid/orka/acp-claude@${placeholder_digest}" \
    ACP_COPILOT_RUNTIME_IMG="example.invalid/orka/acp-copilot@${placeholder_digest}" \
    ACP_OPENCODE_RUNTIME_IMG="example.invalid/orka/acp-opencode@${placeholder_digest}"
  run kubectl wait --for=condition=Established crd/tasks.core.orka.ai --timeout=60s
  ensure_api_client_identity
  deploy_sandbox_router
  patch_controller_for_agent_sandbox

  reset_e2e_resources
  apply_sandbox_template

  log "Port-forwarding Orka API service"
  api_pf_pid="$(start_port_forward "${orka_namespace}" "svc/${orka_api_service}" "${orka_api_local_port}" "${orka_api_service_port}" "${api_pf_log}")"
  local api_base
  api_base="http://127.0.0.1:${orka_api_local_port}"
  wait_for_http "${api_base}/readyz" "Orka API /readyz"

  log "Port-forwarding sandbox router service"
  router_pf_pid="$(start_port_forward "${router_namespace}" "svc/sandbox-router-svc" "${router_api_local_port}" "8080" "${router_pf_log}")"
  local router_base
  router_base="http://127.0.0.1:${router_api_local_port}"
  wait_for_http "${router_base}/healthz" "sandbox router /healthz"

  run_workspace_smoke "${router_base}"

  if [[ "${acp_task_smoke_enabled}" == "1" ]]; then
    run_workspace_backed_acp_task_smoke
  else
    log "Skipping workspace-backed ACP Task smoke (ORKA_AGENT_SANDBOX_ACP_TASK_SMOKE=0)"
  fi
  log "Live agent-sandbox installation/configuration/workspace-adapter e2e passed"
}

main "$@"
