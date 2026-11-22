#!/usr/bin/env bash

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
agent_sandbox_version="${ORKA_AGENT_SANDBOX_VERSION:-v1.0.3}"
demo_namespace="${DEMO_NAMESPACE:-orka-system}"

AGENTIC="${AGENTIC:-1}"
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
VEKIL_NS="${VEKIL_NAMESPACE:-vekil-system}"
sandbox_runtime_tag="${ORKA_SANDBOX_RUNTIME_TAG:-demo}"
sandbox_router_tag="${ORKA_SANDBOX_ROUTER_TAG:-demo}"
sandbox_model_secret="${DEMO_RUNTIME_SECRET_REF:-sandbox-model-key}"
sandbox_git_secret="${DEMO_GIT_SECRET_REF:-github-credentials}"

if [[ "${AGENTIC}" == "1" ]]; then
  runtime_image="${ORKA_SANDBOX_RUNTIME_IMAGE:-localhost:${KIND_REGISTRY_PORT}/orka-sandbox-runtime:${sandbox_runtime_tag}}"
else
  runtime_image="${ORKA_SANDBOX_RUNTIME_IMAGE:-orka-sandbox-runtime:demo}"
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
template_file="${script_dir}/templates/orka-live-template.yaml"
# shellcheck source=scripts/lib/kind-local-registry.sh
. "${repo_root}/scripts/lib/kind-local-registry.sh"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

cleanup_agent_sandbox_v05_webhook_resources() {
  [[ "${agent_sandbox_version}" == v1.* ]] || return 0

  log "Removing obsolete agent-sandbox v0.5 conversion-webhook resources"
  kubectl delete -n agent-sandbox-system \
    service/agent-sandbox-webhook-service \
    secret/agent-sandbox-webhook-certs \
    role/agent-sandbox-controller \
    rolebinding/agent-sandbox-controller \
    --ignore-not-found
}

command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"
if [[ "${AGENTIC}" == "1" ]]; then
  for c in docker go gh; do
    command -v "${c}" >/dev/null 2>&1 || die "missing required command: ${c} (needed for AGENTIC=1)"
  done
  docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
fi

registry_cluster=""
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  log "Selecting kubectl context kind-${cluster_name}"
  kubectl config use-context "kind-${cluster_name}" >/dev/null
  registry_cluster="${cluster_name}"
else
  current_context="$(kubectl config current-context)"
  log "kind cluster ${cluster_name} not found; using current context ${current_context}"
  if [[ "${current_context}" == kind-* ]] && command -v kind >/dev/null 2>&1 \
     && kind get clusters 2>/dev/null | grep -qx "${current_context#kind-}"; then
    registry_cluster="${current_context#kind-}"
  fi
fi

registry_ready=0
ensure_kind_registry() {
  if [[ "${registry_ready}" == "1" ]]; then
    return 0
  fi
  [[ -n "${registry_cluster}" ]] \
    || die "context $(kubectl config current-context) is not a local kind cluster; images cannot be published to a local registry it can pull from (set ORKA_SANDBOX_RUNTIME_IMAGE to a pre-published image and run without AGENTIC=1, or target a kind cluster)"
  orka_kind_registry_start "${registry_cluster}"
  registry_ready=1
}

if [[ "${AGENTIC}" == "1" ]]; then
  node_arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"

  if [[ -z "${ORKA_SANDBOX_RUNTIME_IMAGE:-}" ]]; then
    ensure_kind_registry
    local_runtime_image="orka-sandbox-runtime:${sandbox_runtime_tag}"
    log "Building sandbox-runtime image ${local_runtime_image} (arch ${node_arch}; real codex + git + gh)"
    docker build --platform "linux/${node_arch}" -t "${local_runtime_image}" \
      -f "${repo_root}/hack/demos/images/sandbox-runtime/Dockerfile" "${repo_root}"
    runtime_image="$(orka_kind_registry_push "${local_runtime_image}" "orka/sandbox-runtime")"
    log "sandbox-runtime image pinned as ${runtime_image}"
  fi
fi

log "Installing agent-sandbox ${agent_sandbox_version} CRDs + controllers"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/sandbox.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/extensions.yaml"
cleanup_agent_sandbox_v05_webhook_resources

log "Ensuring namespace ${demo_namespace}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f -

log "Applying orka-live SandboxTemplate (runtime image: ${runtime_image})"
sed "s|REPLACE_RUNTIME_IMAGE|${runtime_image}|g; s|namespace: demo-magic|namespace: ${demo_namespace}|" \
  "${template_file}" \
  | kubectl apply -f -

if [[ "${AGENTIC}" == "1" ]]; then
  router_src="$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@${agent_sandbox_version}/clients/python/agentic-sandbox-client/sandbox-router"
  local_router_image="orka-sandbox-router:${sandbox_router_tag}"
  router_image=""
  if [[ -d "${router_src}" ]]; then
    log "Building sandbox-router image ${local_router_image}"
    router_build_dir="$(mktemp -d)"
    cp -R "${router_src}/." "${router_build_dir}/" && chmod -R u+w "${router_build_dir}"
    docker build --platform "linux/${node_arch}" -t "${local_router_image}" "${router_build_dir}"
    ensure_kind_registry
    router_image="$(orka_kind_registry_push "${local_router_image}" "orka/sandbox-router")"
    log "sandbox-router image pinned as ${router_image}"
    rm -rf "${router_build_dir}"

    log "Deploying sandbox-router into ${demo_namespace}"
    awk -v image="${router_image}" '
      {
        gsub(/\$\{ROUTER_IMAGE\}/, image)
        if ($0 ~ /name: ALLOW_UNAUTHENTICATED_ROUTER/) { allow = 1 }
        if (allow == 1 && $0 ~ /value: "false"/) {
          sub(/value: "false"/, "value: \"true\"")
          allow = 0
        }
        print
      }
    ' "${router_src}/sandbox_router.yaml" \
      | kubectl -n "${demo_namespace}" apply -f -
    kubectl -n "${demo_namespace}" rollout status deployment/sandbox-router-deployment --timeout=180s
  else
    log "sandbox-router source not found at ${router_src}"
    log "Run 'go mod download sigs.k8s.io/agent-sandbox' or deploy a router manually."
  fi

  vekil_script="${repo_root}/.claude/skills/vekil-reverse-proxy-deploy/scripts/deploy_vekil_reverse_proxy.sh"
  if kubectl -n "${VEKIL_NS}" get deploy vekil >/dev/null 2>&1; then
    log "vekil already deployed in ${VEKIL_NS} — reusing it"
  elif [[ -x "${vekil_script}" ]]; then
    log "Deploying vekil model proxy to ${VEKIL_NS} (device-code login)"
    bash "${vekil_script}" --context "$(kubectl config current-context)" --namespace "${VEKIL_NS}" --skip-wait || true
    log "ACTION REQUIRED: complete the GitHub device-code login in vekil's logs:"
    log "  kubectl -n ${VEKIL_NS} logs deploy/vekil | grep 'login/device'"
  else
    log "vekil deploy script not found; provide an OpenAI-compatible proxy and set ${sandbox_model_secret}."
  fi
  vekil_url="http://vekil.${VEKIL_NS}.svc.cluster.local:1337/v1"

  log "Creating model Secret ${demo_namespace}/${sandbox_model_secret} (endpoint -> vekil)"
  kubectl -n "${demo_namespace}" create secret generic "${sandbox_model_secret}" \
    --from-literal=OPENAI_BASE_URL="${vekil_url}" \
    --from-literal=OPENAI_API_KEY=proxy-placeholder \
    --dry-run=client -o yaml | kubectl apply -f -

  git_token="${GIT_TOKEN:-${GITHUB_TOKEN:-}}"
  if [[ -z "${git_token}" ]] && command -v gh >/dev/null 2>&1; then
    git_token="$(gh auth token 2>/dev/null || true)"
  fi
  if [[ -n "${git_token}" ]]; then
    log "Creating git Secret ${demo_namespace}/${sandbox_git_secret} (token not printed)"
    kubectl -n "${demo_namespace}" create secret generic "${sandbox_git_secret}" \
      --from-literal=username=oauth2 --from-literal=password="${git_token}" \
      --from-literal=token="${git_token}" \
      --dry-run=client -o yaml | kubectl apply -f -
    unset git_token
  else
    log "No git token (set GIT_TOKEN/GITHUB_TOKEN or 'gh auth login'); create ${sandbox_git_secret} before Demo 60."
  fi

  orka_client_sa="${ORKA_TOKEN_SERVICE_ACCOUNT:-orka-client}"
  orka_client_ns="${ORKA_TOKEN_NAMESPACE:-${demo_namespace}}"
  log "Ensuring Orka API client ServiceAccount ${orka_client_ns}/${orka_client_sa}"
  kubectl create serviceaccount "${orka_client_sa}" -n "${orka_client_ns}" \
    --dry-run=client -o yaml | kubectl apply -f -

  log "Archived agent-sandbox demo assets installed (runtime; router; secrets; API client SA)."
fi

log "agent-sandbox stack installed. Verify with:"
log "  kubectl get sandboxtemplate -n ${demo_namespace} orka-live-template"
log "ACP provider, profiles, and ExecutionWorkspaceClass must be installed separately; see docs/development/workspace-provider-authoring.md."
