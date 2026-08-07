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

sanitize_image_tag() {
  printf '%s' "$1" | LC_ALL=C tr -c 'A-Za-z0-9_.-' '-'
}

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/kind-local-registry.sh
. "${script_dir}/lib/kind-local-registry.sh"
# shellcheck source=scripts/lib/e2e-admission-tls.sh
. "${script_dir}/lib/e2e-admission-tls.sh"

kind_cluster="${KIND_CLUSTER:-orka-security-scan-e2e}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-default}"
orka_controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
wait_timeout="${ORKA_SECURITY_SCAN_WAIT_TIMEOUT:-25m}"
target_repo="${ORKA_SECURITY_SCAN_TARGET_REPO:-https://github.com/sozercan/nodejs-goof}"
target_branch="${ORKA_SECURITY_SCAN_TARGET_BRANCH:-main}"
target_ref="${ORKA_SECURITY_SCAN_TARGET_REF:-add14ba59e98240d9e00a235dd7d42cd61ae9912}"
agent_name="${ORKA_SECURITY_SCAN_AGENT:-security-scan-e2e-agent}"
scan_name="${ORKA_SECURITY_SCAN_NAME:-security-goof}"
bad_scan_name="${ORKA_SECURITY_BAD_SCAN_NAME:-security-goof-tool-transcript}"
keep_cluster="${KEEP_CLUSTER:-0}"
created_kind_cluster="0"

e2e_run_id="$(sanitize_image_tag "${ORKA_SECURITY_SCAN_RUN_ID:-${GITHUB_RUN_ID:-manual}-$(date -u +%Y%m%d%H%M%S)}")"
manager_image="${ORKA_MANAGER_IMAGE:-orka-controller:security-scan-e2e-${e2e_run_id}}"
publisher_image="${ORKA_WORKSPACE_PUBLISHER_IMAGE:-orka-workspace-publisher:security-scan-e2e-${e2e_run_id}}"
general_worker_image="${ORKA_GENERAL_WORKER_IMAGE:-orka-general-worker:security-scan-e2e-${e2e_run_id}}"

work_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/security-scan-e2e.XXXXXX")"
kind_config="${ORKA_SECURITY_SCAN_KIND_CONFIG:-${work_dir}/kind-config.yaml}"
manager_kustomization="${repo_root}/config/manager/kustomization.yaml"
manager_kustomization_backup="${work_dir}/manager-kustomization.yaml.bak"

redact() {
  sed -E \
    -e 's/(Authorization:[[:space:]]*Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/(Bearer[[:space:]]+)[A-Za-z0-9._~+\/=-]+/\1[REDACTED]/Ig' \
    -e 's/gh[opusr]_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g' \
    -e 's/github_pat_[A-Za-z0-9_]+/[REDACTED_GITHUB_TOKEN]/g'
}

run() {
  printf '+ ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
  "$@"
}

run_redacted() {
  set +e
  "$@" 2>&1 | redact
  local rc=${PIPESTATUS[0]}
  set -e
  return "${rc}"
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
    kubectl -n "${orka_namespace}" get pods,svc,deploy,jobs -o wide 2>/dev/null || true
    echo
    echo "=== Test Namespace Security Resources ==="
    kubectl -n "${test_namespace}" get agents,repositoryscans,tasks,jobs,pods -o wide 2>/dev/null || true
    echo
    echo "=== RepositoryScan YAML ==="
    kubectl -n "${test_namespace}" get repositoryscan "${scan_name}" "${bad_scan_name}" -o yaml 2>/dev/null || true
    echo
    echo "=== Security Tasks YAML ==="
    kubectl -n "${test_namespace}" get tasks \
      -l "orka.ai/security-target" \
      -o yaml 2>/dev/null || true
    echo
    echo "=== Controller Logs ==="
    kubectl -n "${orka_namespace}" logs deployment/"${orka_controller_deployment}" -c manager --tail=500 2>/dev/null || true
    echo
    echo "=== Worker Logs ==="
    for job in $(kubectl -n "${test_namespace}" get jobs -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null || true); do
      echo "--- job/${job} ---"
      kubectl -n "${test_namespace}" logs "job/${job}" --all-containers --tail=300 --prefix 2>/dev/null || true
    done
  } 2>&1 | redact >&2
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
  restore_manager_kustomization
  orka_kind_registry_stop
  if [[ "${created_kind_cluster}" == "1" && "${keep_cluster}" != "1" ]]; then
    kind delete cluster --name "${kind_cluster}" >/dev/null 2>&1 || true
  elif [[ "${keep_cluster}" == "1" ]]; then
    log "KEEP_CLUSTER=1, leaving kind cluster ${kind_cluster}"
  fi
  rm -rf "${work_dir}" >/dev/null 2>&1 || true
  if [[ "${status}" -ne 0 ]]; then
    log "Security scan e2e failed"
  fi
}

duration_to_seconds() {
  local value="$1"
  local rest="$1"
  local total=0
  local number unit amount

  if [[ "${value}" =~ ^[0-9]+$ ]]; then
    printf '%s\n' "${value}"
    return
  fi

  while [[ -n "${rest}" ]]; do
    if [[ ! "${rest}" =~ ^([0-9]+)([hms])(.*)$ ]]; then
      die "unsupported duration ${value}; use digits with h, m, or s units"
    fi
    number="${BASH_REMATCH[1]}"
    unit="${BASH_REMATCH[2]}"
    rest="${BASH_REMATCH[3]}"
    amount=$((10#${number}))
    case "${unit}" in
      h) total=$((total + amount * 3600)) ;;
      m) total=$((total + amount * 60)) ;;
      s) total=$((total + amount)) ;;
    esac
  done

  [[ "${total}" -gt 0 ]] || die "duration ${value} must be positive"
  printf '%s\n' "${total}"
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

  if [[ -z "${ORKA_SECURITY_SCAN_KIND_CONFIG:-}" ]]; then
    write_default_kind_config
  fi
  [[ -f "${kind_config}" ]] || die "Kind config not found: ${kind_config}"

  log "Creating Kind cluster ${kind_cluster}"
  run kind create cluster --name "${kind_cluster}" --config "${kind_config}"
  created_kind_cluster="1"
}

patch_controller_images() {
  local rollout_id
  rollout_id="${e2e_run_id}"

  log "Configuring Orka controller worker images"
  kubectl -n "${orka_namespace}" get deployment "${orka_controller_deployment}" -o json |
    jq \
      --arg generalImage "${general_worker_image}" \
      --arg rolloutID "${rollout_id}" '
      def upsert_arg($name; $value):
        . as $args
        | if any($args[]?; startswith($name + "=")) then
            map(if startswith($name + "=") then $name + "=" + $value else . end)
          else
            $args + [$name + "=" + $value]
          end;
      .spec.template.metadata.annotations = ((.spec.template.metadata.annotations // {}) + {
        "orka.ai/security-scan-e2e-run": $rolloutID
      })
      |
      .spec.template.spec.containers |= map(
        if .name == "manager" then
          .imagePullPolicy = "IfNotPresent"
          | .args = ((.args // []) | upsert_arg("--general-worker-image"; $generalImage))
        else . end
      )
    ' | kubectl apply -f -

  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
}

reset_e2e_resources() {
  log "Resetting security scan e2e resources"
  run kubectl -n "${test_namespace}" delete repositoryscan "${scan_name}" "${bad_scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete task \
    -l "orka.ai/security-target=${scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete task \
    -l "orka.ai/security-target=${bad_scan_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
  run kubectl -n "${test_namespace}" delete agent "${agent_name}" \
    --ignore-not-found=true --wait=true --timeout=2m
}

apply_agent() {
  log "Creating ACP Codex Agent fixture ${agent_name}"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: ${agent_name}
  namespace: ${test_namespace}
spec:
  runtime:
    contractVersion: orka.harness.v2
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: false
  model:
    name: gpt-5.4
YAML
}

apply_repository_scan() {
  local name="$1"
  log "Creating RepositoryScan ${name} for ${target_repo}"
  kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: RepositoryScan
metadata:
  name: ${name}
  namespace: ${test_namespace}
spec:
  provider: github
  repoURL: ${target_repo}
  owner: sozercan
  repository: nodejs-goof
  branch: ${target_branch}
  ref: ${target_ref}
  validationMode: "off"
  maxFindingsPerRun: 20
  analysisAgentRef:
    name: ${agent_name}
YAML
}

wait_repo_phase() {
  local name="$1"
  local expected="$2"
  local timeout_seconds
  timeout_seconds="$(duration_to_seconds "${wait_timeout}")"
  local deadline=$((SECONDS + timeout_seconds))
  local phase

  log "Waiting for RepositoryScan/${name} phase ${expected}"
  while (( SECONDS < deadline )); do
    phase="$(kubectl -n "${test_namespace}" get repositoryscan "${name}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    if [[ "${phase}" == "${expected}" ]]; then
      return 0
    fi
    sleep 5
  done
  die "RepositoryScan/${name} did not reach phase ${expected}; current phase ${phase:-<empty>}"
}

main() {
  require_cmd make
  require_cmd go
  require_cmd docker
  require_cmd kind
  require_cmd kubectl
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

  log "Building manager image ${manager_image}"
  run make docker-build IMG="${manager_image}"
  log "Building workspace publisher image ${publisher_image}"
  run make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="${publisher_image}"

  log "Building general worker image ${general_worker_image}"
  run docker build -t "${general_worker_image}" -f workers/general/Dockerfile .

  log "Loading images into Kind cluster ${kind_cluster}"
  run kind load docker-image "${manager_image}" --name "${kind_cluster}"
  run kind load docker-image "${general_worker_image}" --name "${kind_cluster}"

  local manager_ref publisher_ref
  manager_ref="$(orka_kind_registry_push "${manager_image}" "orka/controller")"
  publisher_ref="$(orka_kind_registry_push "${publisher_image}" "orka/workspace-publisher")"

  log "Bootstrapping test-only admission TLS"
  orka_e2e_bootstrap_admission_tls

  log "Deploying Orka manager with inert digest-pinned ACP images for the deferred RepositoryScan agent path"
  local placeholder_digest
  placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
  run make deploy \
    IMG="${manager_ref}" \
    WORKSPACE_PUBLISHER_IMG="${publisher_ref}" \
    ACP_CODEX_RUNTIME_IMG="example.invalid/orka/acp-codex@${placeholder_digest}" \
    ACP_CLAUDE_RUNTIME_IMG="example.invalid/orka/acp-claude@${placeholder_digest}" \
    ACP_COPILOT_RUNTIME_IMG="example.invalid/orka/acp-copilot@${placeholder_digest}" \
    ACP_OPENCODE_RUNTIME_IMG="example.invalid/orka/acp-opencode@${placeholder_digest}"
  run kubectl wait --for=condition=Established crd/repositoryscans.core.orka.ai --timeout=60s
  run kubectl -n "${orka_namespace}" rollout status deployment/"${orka_controller_deployment}" --timeout=5m
  patch_controller_images

  reset_e2e_resources
  apply_agent

  apply_repository_scan "${scan_name}"
  wait_repo_phase "${scan_name}" "Error"
  log "Verifying the ACP v2 hard-cutover gate for RepositoryScan agent Tasks"
  local deferred_messages
  deferred_messages="$(kubectl -n "${test_namespace}" get tasks \
    -l "orka.ai/security-target=${scan_name}" -o json | \
    jq -r '[.items[].status.message // ""] | join("\n")')"
  if ! grep -Fq "type: agent ACP runtime tasks do not support arbitrary task env" <<<"${deferred_messages}"; then
    run_redacted kubectl -n "${test_namespace}" get tasks -l "orka.ai/security-target=${scan_name}" -o yaml || true
    die "RepositoryScan did not fail at the expected ACP task-env compatibility gate"
  fi
  log "RepositoryScan workspace-backed agent execution is explicitly deferred under ACP v2; fail-closed gate validated"
}

main "$@"
