#!/usr/bin/env bash
# Install the kubernetes-sigs agent-sandbox stack + everything Demo 60 needs to
# run a REAL agent that opens a PR, on a kind cluster.
#
# Base layer (always): agent-sandbox CRDs + controller, the orka-live
# SandboxTemplate, and the Orka controller agent-sandbox flags.
#
# Agentic layer (AGENTIC=1, default): the pieces the demo's e2e provides but
# the base install historically did not — so Demo 60 is self-contained:
#   1. Build + push the sandbox-runtime image (real codex CLI + git + gh).
#   2. Build + deploy the upstream sandbox-router (the agent-sandbox SDK's
#      exec data-path; from the agent-sandbox Go module's python client).
#   3. Deploy the vekil model proxy (one-time GitHub device-code login) +
#      create the model Secret (OPENAI_BASE_URL -> vekil) and the git Secret.
#   4. Make agent-sandbox the controller's default workspace provider so
#      Demo 60 Tasks (which set no explicit provider) route to it.
#
# IMPORTANT (kind registry addressing): normal pods on a kind cluster pull from
# the local registry via "localhost:<port>" (the containerd mirror host), NOT
# the registry's bridge IP. Substrate ACTORS pull via the bridge IP; agent-
# sandbox pods + the router are normal pods, so they use localhost:<port>.
#
# Requires: kind, kubectl, jq, docker, go (and gh for the git token). Set
# AGENTIC=0 to install only the base layer.

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
agent_sandbox_version="${ORKA_AGENT_SANDBOX_VERSION:-v0.4.6}"
demo_namespace="${DEMO_NAMESPACE:-demo-magic}"
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
sandbox_default_template="${ORKA_SANDBOX_DEFAULT_TEMPLATE:-orka-live-template}"
sandbox_cleanup_policy="${ORKA_SANDBOX_CLEANUP_POLICY:-retain}"

# Agentic layer knobs.
AGENTIC="${AGENTIC:-1}"
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
VEKIL_NS="${VEKIL_NAMESPACE:-vekil-system}"
sandbox_runtime_tag="${ORKA_SANDBOX_RUNTIME_TAG:-demo}"
sandbox_router_tag="${ORKA_SANDBOX_ROUTER_TAG:-demo}"
sandbox_model_secret="${DEMO_RUNTIME_SECRET_REF:-sandbox-model-key}"
sandbox_git_secret="${DEMO_GIT_SECRET_REF:-github-credentials}"
# Router lives in the demo namespace so the SDK's same-namespace service DNS
# resolves; the controller flag is set to match below.
sandbox_router_url="${ORKA_SANDBOX_ROUTER_URL:-http://sandbox-router-svc.${demo_namespace}.svc.cluster.local:8080}"

# The runtime image: built by the agentic layer when AGENTIC=1, else taken from
# ORKA_SANDBOX_RUNTIME_IMAGE (you build/load it yourself).
if [[ "${AGENTIC}" == "1" ]]; then
  runtime_image="${ORKA_SANDBOX_RUNTIME_IMAGE:-localhost:${KIND_REGISTRY_PORT}/orka-sandbox-runtime:${sandbox_runtime_tag}}"
else
  runtime_image="${ORKA_SANDBOX_RUNTIME_IMAGE:-orka-sandbox-runtime:demo}"
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
template_file="${script_dir}/templates/orka-live-template.yaml"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"
command -v jq      >/dev/null 2>&1 || die "missing required command: jq"
if [[ "${AGENTIC}" == "1" ]]; then
  for c in docker go gh; do
    command -v "${c}" >/dev/null 2>&1 || die "missing required command: ${c} (needed for AGENTIC=1)"
  done
  docker info >/dev/null 2>&1 || die "docker daemon is not reachable"
fi

# Select context: prefer kind-<cluster_name> if that kind cluster exists,
# otherwise use the current context (lets Demo 60 share an existing cluster,
# e.g. the Substrate demo cluster).
if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  log "Selecting kubectl context kind-${cluster_name}"
  kubectl config use-context "kind-${cluster_name}" >/dev/null
else
  log "kind cluster ${cluster_name} not found; using current context $(kubectl config current-context)"
fi

# Agentic layer: build + push the runtime + router images to the kind registry
# (normal pods pull via localhost:<port>). Done BEFORE the template is applied
# so the SandboxTemplate references an image that exists.
if [[ "${AGENTIC}" == "1" ]]; then
  node_arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"

  log "Building sandbox-runtime image ${runtime_image} (arch ${node_arch}; real codex + git + gh)"
  docker build --platform "linux/${node_arch}" -t "${runtime_image}" \
    -f "${repo_root}/hack/demos/images/sandbox-runtime/Dockerfile" "${repo_root}"
  docker push "${runtime_image}"
fi

log "Installing agent-sandbox ${agent_sandbox_version} CRDs + controllers"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/manifest.yaml"
kubectl apply -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/${agent_sandbox_version}/extensions.yaml"

log "Ensuring namespace ${demo_namespace}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f -

log "Applying orka-live SandboxTemplate (runtime image: ${runtime_image})"
sed "s|REPLACE_RUNTIME_IMAGE|${runtime_image}|g; s|namespace: demo-magic|namespace: ${demo_namespace}|" \
  "${template_file}" \
  | kubectl apply -f -

# Patch the Orka controller-manager Deployment so the sandbox-runner code
# path is enabled and points at the in-cluster router + the default
# template we just applied. Idempotent via jq upsert_arg, mirroring
# scripts/live-agent-sandbox-e2e.sh:565-602.
if kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" >/dev/null 2>&1; then
  log "Patching ${controller_deployment} agent-sandbox flags"
  kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" -o json \
    | jq \
        --arg routerURL "${sandbox_router_url}" \
        --arg template "${sandbox_default_template}" \
        --arg cleanup  "${sandbox_cleanup_policy}" \
        --arg defaultProvider "agent-sandbox" \
        '
        def upsert_arg($name; $value):
          . as $args
          | if any($args[]?; startswith($name + "=")) then
              map(if startswith($name + "=") then $name + "=" + $value else . end)
            else
              $args + [$name + "=" + $value]
            end;
        .spec.template.spec.containers |= map(
          if .name == "manager" then
            .args = ((.args // []) | upsert_arg("--agent-sandbox-enabled"; "true"))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-router-url"; $routerURL))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-default-template"; $template))
            | .args = ((.args // []) | upsert_arg("--agent-sandbox-cleanup-policy"; $cleanup))
            | .args = ((.args // []) | upsert_arg("--execution-workspace-default-provider"; $defaultProvider))
          else . end
        )
        ' \
    | kubectl apply -f -
  kubectl -n "${orka_namespace}" rollout status deployment/"${controller_deployment}" --timeout=300s
else
  log "orka controller deployment ${controller_deployment} not found — skipping flag patch"
fi

if [[ "${AGENTIC}" == "1" ]]; then
  # ---- sandbox-router (the SDK's exec data-path) ------------------------
  # The agent-sandbox Go SDK reaches sandbox pods through a "sandbox-router"
  # Service. The base agent-sandbox release does NOT ship it; its source lives
  # in the agent-sandbox Go module's python client. Build + deploy it.
  router_src="$(go env GOMODCACHE)/sigs.k8s.io/agent-sandbox@${agent_sandbox_version}/clients/python/agentic-sandbox-client/sandbox-router"
  router_image="localhost:${KIND_REGISTRY_PORT}/orka-sandbox-router:${sandbox_router_tag}"
  if [[ -d "${router_src}" ]]; then
    log "Building sandbox-router image ${router_image}"
    router_build_dir="$(mktemp -d)"
    cp -R "${router_src}/." "${router_build_dir}/" && chmod -R u+w "${router_build_dir}"
    docker build --platform "linux/${node_arch}" -t "${router_image}" "${router_build_dir}"
    docker push "${router_image}"
    rm -rf "${router_build_dir}"

    log "Deploying sandbox-router into ${demo_namespace}"
    awk -v image="${router_image}" '{ gsub(/\$\{ROUTER_IMAGE\}/, image); print }' \
      "${router_src}/sandbox_router.yaml" \
      | kubectl -n "${demo_namespace}" apply -f -
    kubectl -n "${demo_namespace}" rollout status deployment/sandbox-router-deployment --timeout=180s
  else
    log "sandbox-router source not found at ${router_src}"
    log "Run 'go mod download sigs.k8s.io/agent-sandbox' or deploy a router manually."
  fi

  # ---- vekil model proxy (one-time GitHub device-code login) ------------
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

  # ---- model Secret + git Secret ----------------------------------------
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
      --dry-run=client -o yaml | kubectl apply -f -
    unset git_token
  else
    log "No git token (set GIT_TOKEN/GITHUB_TOKEN or 'gh auth login'); create ${sandbox_git_secret} before Demo 60."
  fi

  # ---- Orka API client ServiceAccount -----------------------------------
  # The demos authenticate to the Orka API with a token minted for this SA
  # (prepare_api_env -> get_orka_token -> kubectl create token orka-client).
  # The Orka API validates any SA token via Kubernetes TokenReview, so the SA
  # just needs to exist. On the shared demo cluster cluster-up.sh provides it;
  # on this e2e-built cluster nothing does, so ensure it here.
  orka_client_sa="${ORKA_TOKEN_SERVICE_ACCOUNT:-orka-client}"
  orka_client_ns="${ORKA_TOKEN_NAMESPACE:-${demo_namespace}}"
  log "Ensuring Orka API client ServiceAccount ${orka_client_ns}/${orka_client_sa}"
  kubectl create serviceaccount "${orka_client_sa}" -n "${orka_client_ns}" \
    --dry-run=client -o yaml | kubectl apply -f -

  log "Demo 60 agent-sandbox stack ready (real codex via vekil; router; secrets; API client SA)."
fi

log "agent-sandbox stack installed. Verify with:"
log "  kubectl get sandboxtemplate -n ${demo_namespace} ${sandbox_default_template}"
