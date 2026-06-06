#!/usr/bin/env bash
# Provision the model Provider + secrets the model-backed demos (10/20/30/40)
# need, pointing them at the in-cluster vekil proxy. The workspace demos
# (50/60/70) bring their own model wiring; this script covers the SDLC demos.
#
# What it creates in the demo namespace (DEMO_NAMESPACE, default demo-magic):
#   - a Provider CR (DEMO_PROVIDER_REF) used by the type: ai coordinator in
#     demos 10/20, type openai, baseURL -> vekil /v1, defaultModel an Opus id
#     (demo 10 requires Opus). The provider api-key is a placeholder; vekil
#     holds the real Copilot session.
#   - the provider api-key Secret (DEMO_PROVIDER_SECRET_REF).
#   - the runtime Secret (DEMO_RUNTIME_SECRET_REF) for the CLI agents:
#     OPENAI_BASE_URL -> vekil /v1 + placeholder OPENAI_API_KEY (=> codex).
#   - a git Secret (DEMO_GIT_SECRET_REF) with username/password (PR demos) AND
#     a token key (demo 30 reads GH_TOKEN from the 'token' key). Token from
#     GIT_TOKEN/GITHUB_TOKEN or the local gh CLI; never printed.
#
# Idempotent. Context-flexible: prefer kind-<ORKA_DEMO_CLUSTER> if it exists,
# else the current context. Requires kubectl (+ gh for the git token default).

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
demo_namespace="${DEMO_NAMESPACE:-demo-magic}"
vekil_ns="${VEKIL_NAMESPACE:-vekil-system}"

provider_ref="${DEMO_PROVIDER_REF:-vekil-proxy}"
provider_secret="${DEMO_PROVIDER_SECRET_REF:-demo-provider-key}"
provider_secret_key="${DEMO_PROVIDER_SECRET_KEY:-api-key}"
provider_model="${DEMO_AI_MODEL:-claude-opus-4.7}"
runtime_secret="${DEMO_RUNTIME_SECRET_REF:-demo-runtime-key}"
git_secret="${DEMO_GIT_SECRET_REF:-github-credentials}"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"

if command -v kind >/dev/null 2>&1 && kind get clusters 2>/dev/null | grep -qx "${cluster_name}"; then
  log "Selecting kubectl context kind-${cluster_name}"
  kubectl config use-context "kind-${cluster_name}" >/dev/null
else
  log "kind cluster ${cluster_name} not found; using current context $(kubectl config current-context)"
fi

vekil_url="http://vekil.${vekil_ns}.svc.cluster.local:1337/v1"

log "Ensuring namespace ${demo_namespace}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# --- Provider api-key Secret (placeholder; vekil holds the real session) ----
log "Creating provider api-key Secret ${demo_namespace}/${provider_secret}"
kubectl -n "${demo_namespace}" create secret generic "${provider_secret}" \
  --from-literal="${provider_secret_key}=proxy-placeholder" \
  --dry-run=client -o yaml | kubectl apply -f -

# --- Provider CR (type: ai coordinator in demos 10/20) ----------------------
log "Applying Provider ${demo_namespace}/${provider_ref} (baseURL -> vekil, model ${provider_model})"
kubectl apply -f - <<YAML
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: ${provider_ref}
  namespace: ${demo_namespace}
spec:
  type: openai
  baseURL: ${vekil_url}
  secretRef:
    name: ${provider_secret}
    key: ${provider_secret_key}
  defaultModel: ${provider_model}
YAML

# --- Runtime Secret (CLI agents: codex via vekil /v1) -----------------------
log "Creating runtime Secret ${demo_namespace}/${runtime_secret} (endpoint -> vekil)"
kubectl -n "${demo_namespace}" create secret generic "${runtime_secret}" \
  --from-literal=OPENAI_BASE_URL="${vekil_url}" \
  --from-literal=OPENAI_API_KEY=proxy-placeholder \
  --dry-run=client -o yaml | kubectl apply -f -

# --- Git Secret (username/password for PRs + token key for demo 30) ---------
git_token="${GIT_TOKEN:-${GITHUB_TOKEN:-}}"
if [[ -z "${git_token}" ]] && command -v gh >/dev/null 2>&1; then
  git_token="$(gh auth token 2>/dev/null || true)"
fi
if [[ -n "${git_token}" ]]; then
  log "Creating git Secret ${demo_namespace}/${git_secret} (token not printed)"
  kubectl -n "${demo_namespace}" create secret generic "${git_secret}" \
    --from-literal=username=oauth2 \
    --from-literal=password="${git_token}" \
    --from-literal=token="${git_token}" \
    --dry-run=client -o yaml | kubectl apply -f -
  unset git_token
else
  log "No git token (set GIT_TOKEN/GITHUB_TOKEN or 'gh auth login'); create ${git_secret} before the PR demos:"
  log "  kubectl -n ${demo_namespace} create secret generic ${git_secret} --from-literal=username=oauth2 --from-literal=password=<token> --from-literal=token=<token>"
fi

# --- Git-capable codex worker image -----------------------------------------
# The model-backed demos (10/20/30/40) run the agent directly in the worker pod
# (no sandbox/substrate workspace), so the worker image itself must contain git
# to clone the repo. The Substrate e2e deploys Orka with the STRIPPED codex
# image (workers/agent/codex/Dockerfile.substrate-e2e = distroless, NO git),
# which fails these demos with "git: executable not found". Build the PRODUCTION
# codex image (workers/agent/codex/Dockerfile has git + codex) and repoint the
# controller's --codex-worker-image at it.
orka_namespace="${ORKA_NAMESPACE:-orka-system}"
controller_deployment="${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}"
codex_image="${DEMO_CODEX_WORKER_IMAGE:-localhost:${KIND_REGISTRY_PORT:-5001}/orka/agent-worker-codex:demo}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
if command -v docker >/dev/null 2>&1 && [[ "${DEMO_BUILD_CODEX_IMAGE:-1}" == "1" ]]; then
  node_arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"
  log "Building git-capable codex worker image ${codex_image} (arch ${node_arch})"
  docker build --platform "linux/${node_arch}" -t "${codex_image}" \
    -f "${repo_root}/workers/agent/codex/Dockerfile" "${repo_root}"
  docker push "${codex_image}"
fi
if kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" >/dev/null 2>&1; then
  log "Repointing ${controller_deployment} --codex-worker-image -> ${codex_image}"
  kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" -o json \
    | jq --arg img "${codex_image}" '
        .spec.template.spec.containers |= map(
          if .name == "manager" then
            .args = ((.args // []) | map(if startswith("--codex-worker-image=") then "--codex-worker-image=" + $img else . end))
          else . end)' \
    | kubectl apply -f -
  kubectl -n "${orka_namespace}" rollout status deployment/"${controller_deployment}" --timeout=300s
fi

# --- AI worker image (type: ai coordinator in demos 10/20) ------------------
# The manual/chat PR coordinators run as a `type: ai` Task, which uses the AI
# worker image (workers/ai/Dockerfile), NOT the codex worker. The Substrate e2e
# never builds or wires this image, so the controller falls back to the code
# default ghcr.io/sozercan/orka/ai-worker:latest, which the kind cluster cannot
# pull -> ImagePullBackOff and the coordinator never starts. Build the AI worker
# for the node arch, push to the local registry, and repoint the controller's
# --ai-worker-image (ADD the flag if the e2e deployment omits it).
ai_image="${DEMO_AI_WORKER_IMAGE:-localhost:${KIND_REGISTRY_PORT:-5001}/orka/ai-worker:demo}"
if command -v docker >/dev/null 2>&1 && [[ "${DEMO_BUILD_AI_IMAGE:-1}" == "1" ]]; then
  node_arch="$(kubectl get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"
  log "Building AI worker image ${ai_image} (arch ${node_arch})"
  docker build --platform "linux/${node_arch}" -t "${ai_image}" \
    -f "${repo_root}/workers/ai/Dockerfile" "${repo_root}"
  docker push "${ai_image}"
fi
if kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" >/dev/null 2>&1; then
  log "Repointing ${controller_deployment} --ai-worker-image -> ${ai_image}"
  kubectl -n "${orka_namespace}" get deployment "${controller_deployment}" -o json \
    | jq --arg img "${ai_image}" '
        .spec.template.spec.containers |= map(
          if .name == "manager" then
            .args = (
              (.args // []) as $a |
              if ($a | map(startswith("--ai-worker-image=")) | any)
              then ($a | map(if startswith("--ai-worker-image=") then "--ai-worker-image=" + $img else . end))
              else ($a + ["--ai-worker-image=" + $img])
              end)
          else . end)' \
    | kubectl apply -f -
  kubectl -n "${orka_namespace}" rollout status deployment/"${controller_deployment}" --timeout=300s
fi

log "Demo model stack ready: Provider ${provider_ref} + runtime/provider/git secrets in ${demo_namespace}."
log "Run demos 10/20/30/40 with: source hack/demos/cluster/demo-env.sh"
