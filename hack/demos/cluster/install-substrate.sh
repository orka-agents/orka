#!/usr/bin/env bash
# Install Agent Substrate + an Orka-compatible workspace template into a
# dedicated kind cluster for Demo 70.
#
# Substrate's standup is heavy (clone Substrate at a pinned ref, create a kind
# cluster + local registry, deploy the ate-system control plane, build/push the
# controller + codex-worker + workspace-agent images via docker + ko, create a
# WorkerPool + gVisor ActorTemplate, deploy Orka wired with --substrate-* flags,
# and a local RustFS snapshot bucket). That exact sequence is already proven in
# CI by scripts/agent-substrate-e2e.sh.
#
# Rather than duplicate ~600 lines (and drift from CI), this installer is a thin
# wrapper over that script with KEEP_CLUSTER=1, so it leaves behind a fully
# wired cluster Demo 70 can drive. The e2e's own task exercises run as a
# built-in smoke test (a few seconds each) before it hands the cluster back.
#
# Unlike install-kontxt.sh / install-agent-sandbox.sh (which attach to the
# shared demo-magic kind cluster), Substrate needs its OWN cluster: it uses
# Substrate's create-kind-cluster.sh (custom registry + gVisor node config),
# so it cannot bolt onto an existing cluster.
#
# Requires: kind, ko, docker, go, git, jq, kubectl, curl (and gh for the git
# token convenience). The cluster is named by KIND_CLUSTER (default
# orka-agent-substrate-e2e); its context is kind-<KIND_CLUSTER>. Tear down with:
# kind delete cluster --name <KIND_CLUSTER>
#
# The base Substrate standup is secret-free. The agentic layer (AGENTIC=1,
# default) additionally: builds a codex-capable Actor image and points the
# ActorTemplate at it; deploys the vekil model proxy (one-time GitHub
# device-code login — the operator completes it from the pod logs); and creates
# the model + git Secrets. Set AGENTIC=0 to skip the agentic layer. The git
# token comes from GIT_TOKEN/GITHUB_TOKEN or the local gh CLI; the model proxy
# needs a Copilot-enabled GitHub account for the device-code login.

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

# Pin to the same Substrate revision CI uses, unless overridden.
SUBSTRATE_REF="${SUBSTRATE_REF:-b80031d260959b1fc5c6f61e3099fe2a6d368af1}"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
# Exercise the retained-workspace path during standup so the warm-reuse beat is
# smoke-tested before Demo 70 runs. Override to 0 to skip.
SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED:-1}"

# Agentic Demo 70 add-ons (set AGENTIC=0 to stop after the base standup and
# leave the demo model-free). The agentic layer makes Demo 70 a REAL model run
# that opens a PR, so it needs a model proxy + a codex-capable Actor image.
AGENTIC="${AGENTIC:-1}"
SUBSTRATE_NS="${DEMO_SUBSTRATE_NAMESPACE:-default}"
SUBSTRATE_TEMPLATE_NS="${DEMO_SUBSTRATE_TEMPLATE_NAMESPACE:-ate-demo}"
SUBSTRATE_TEMPLATE_NAME="${DEMO_SUBSTRATE_TEMPLATE_NAME:-orka-codex-ci}"
SUBSTRATE_MODEL_SECRET="${DEMO_SUBSTRATE_MODEL_SECRET:-substrate-model-key}"
SUBSTRATE_GIT_SECRET="${DEMO_SUBSTRATE_GIT_SECRET:-github-credentials}"
SUBSTRATE_MODEL="${DEMO_SUBSTRATE_RUNTIME_MODEL:-gpt-5.5}"
VEKIL_NS="${VEKIL_NAMESPACE:-vekil-system}"
KIND_REGISTRY_NAME="${KIND_REGISTRY_NAME:-kind-registry}"
KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
CODEX_ACTOR_TAG="${CODEX_ACTOR_TAG:-demo}"

# Put the Go bin dir on PATH so a `go install`ed ko is found.
if command -v go >/dev/null 2>&1; then
  PATH="$(go env GOPATH)/bin:${PATH}"
  export PATH
fi

for cmd in kind ko docker go git jq kubectl curl; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    if [[ "${cmd}" == "ko" ]]; then
      die "missing required command: ko — install with: go install github.com/google/ko@v0.18.1"
    fi
    die "missing required command: ${cmd}"
  fi
done

docker info >/dev/null 2>&1 || die "docker daemon is not reachable — start Docker and retry"

e2e_script="${repo_root}/scripts/agent-substrate-e2e.sh"
[[ -f "${e2e_script}" ]] || die "expected ${e2e_script} to exist"

log "Standing up Agent Substrate (ref ${SUBSTRATE_REF}, cluster kind-${KIND_CLUSTER})"
log "This builds 4 images and the Substrate control plane — first run takes several minutes."

KEEP_CLUSTER=1 \
  SUBSTRATE_REF="${SUBSTRATE_REF}" \
  KIND_CLUSTER="${KIND_CLUSTER}" \
  SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED}" \
  bash "${e2e_script}"

if [[ "${AGENTIC}" == "1" ]]; then
  ctx="kind-${KIND_CLUSTER}"

  # ---- 1. Codex-capable Actor image -------------------------------------
  # The agentic run executes a real codex CLI INSIDE the gVisor Actor, so the
  # Actor image must carry codex + git (the e2e's stripped workspace-agent-root
  # does not). The production agent-worker-codex image has the daemon + codex +
  # git. Build it LOCALLY (the kind registry is on localhost; a remote builder
  # cannot push there) for the kind node arch, push to the registry, and point
  # the ActorTemplate at it.
  node_arch="$(kubectl --context "${ctx}" get nodes \
    -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"
  reg_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${KIND_REGISTRY_NAME}" 2>/dev/null || true)"
  [[ -n "${reg_ip}" ]] || die "could not determine ${KIND_REGISTRY_NAME} kind-network IP"
  push_image="localhost:${KIND_REGISTRY_PORT}/orka/agent-worker-codex:${CODEX_ACTOR_TAG}"
  actor_image="${reg_ip}:5000/orka/agent-worker-codex:${CODEX_ACTOR_TAG}"

  log "Building codex Actor image ${push_image} (arch ${node_arch})"
  docker build --platform "linux/${node_arch}" -t "${push_image}" \
    -f "${repo_root}/workers/agent/codex/Dockerfile" "${repo_root}"
  docker push "${push_image}"

  log "Pointing ActorTemplate ${SUBSTRATE_TEMPLATE_NS}/${SUBSTRATE_TEMPLATE_NAME} at the codex image"
  kubectl --context "${ctx}" -n "${SUBSTRATE_TEMPLATE_NS}" patch actortemplate "${SUBSTRATE_TEMPLATE_NAME}" \
    --type=json -p "[{\"op\":\"replace\",\"path\":\"/spec/containers/0/image\",\"value\":\"${actor_image}\"}]"
  log "Waiting for the ActorTemplate to rebuild its golden snapshot on the new image"
  for _ in $(seq 1 72); do
    ph="$(kubectl --context "${ctx}" -n "${SUBSTRATE_TEMPLATE_NS}" get actortemplate "${SUBSTRATE_TEMPLATE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    [[ "${ph}" == "Ready" ]] && break
    sleep 5
  done

  # ---- 2. In-cluster model proxy (vekil) --------------------------------
  # Zero-config Copilot upstream via device-code login (a gho_ gh token has no
  # Copilot entitlement). The deploy script prints a github.com/login/device
  # code in the pod logs; the operator completes it once, and vekil caches the
  # session. We do NOT pass a token secret here.
  vekil_script="${repo_root}/.claude/skills/vekil-reverse-proxy-deploy/scripts/deploy_vekil_reverse_proxy.sh"
  if [[ -x "${vekil_script}" ]]; then
    if kubectl --context "${ctx}" -n "${VEKIL_NS}" get deploy vekil >/dev/null 2>&1; then
      log "vekil already deployed in ${VEKIL_NS} — leaving it (re-run device-code login if /readyz is down)"
    else
      log "Deploying vekil model proxy to ${VEKIL_NS} (device-code login)"
      bash "${vekil_script}" --context "${ctx}" --namespace "${VEKIL_NS}" --skip-wait || true
      log "ACTION REQUIRED: complete the GitHub device-code login printed in vekil's logs:"
      log "  kubectl --context ${ctx} -n ${VEKIL_NS} logs deploy/vekil | grep 'login/device'"
      log "  (visit the URL, enter the code; then /readyz returns 200)"
    fi
  else
    log "vekil deploy script not found at ${vekil_script}"
    log "Deploy any in-cluster OpenAI-compatible proxy and set DEMO_SUBSTRATE_MODEL endpoint accordingly."
  fi
  vekil_url="http://vekil.${VEKIL_NS}.svc.cluster.local:1337/v1"

  # ---- 3. Model Secret + git Secret -------------------------------------
  # The codex Agent's secretRef carries the model endpoint as env (EnvFrom).
  # The api-key is a placeholder — vekil holds the real Copilot session.
  log "Creating model Secret ${SUBSTRATE_NS}/${SUBSTRATE_MODEL_SECRET} (endpoint -> vekil)"
  kubectl --context "${ctx}" -n "${SUBSTRATE_NS}" create secret generic "${SUBSTRATE_MODEL_SECRET}" \
    --from-literal=OPENAI_BASE_URL="${vekil_url}" \
    --from-literal=OPENAI_API_KEY=proxy-placeholder \
    --dry-run=client -o yaml | kubectl --context "${ctx}" apply -f -

  # Git credentials for push + PR. Prefer an explicit GIT_TOKEN/GITHUB_TOKEN
  # env; otherwise fall back to the local gh CLI token (never printed).
  git_token="${GIT_TOKEN:-${GITHUB_TOKEN:-}}"
  if [[ -z "${git_token}" ]] && command -v gh >/dev/null 2>&1; then
    git_token="$(gh auth token 2>/dev/null || true)"
  fi
  if [[ -n "${git_token}" ]]; then
    log "Creating git Secret ${SUBSTRATE_NS}/${SUBSTRATE_GIT_SECRET} (token not printed)"
    kubectl --context "${ctx}" -n "${SUBSTRATE_NS}" create secret generic "${SUBSTRATE_GIT_SECRET}" \
      --from-literal=username=oauth2 \
      --from-literal=password="${git_token}" \
      --dry-run=client -o yaml | kubectl --context "${ctx}" apply -f -
    unset git_token
  else
    log "No git token found (set GIT_TOKEN, GITHUB_TOKEN, or 'gh auth login')."
    log "Create it before running Demo 70:"
    log "  kubectl -n ${SUBSTRATE_NS} create secret generic ${SUBSTRATE_GIT_SECRET} --from-literal=username=oauth2 --from-literal=password=<token>"
  fi

  # The demos authenticate to the Orka API with a token minted for this SA
  # (prepare_api_env -> kubectl create token orka-client). The API validates
  # any SA token via TokenReview, so the SA just needs to exist. cluster-up.sh
  # provides it on the shared cluster; this e2e-built cluster needs it created.
  orka_client_sa="${ORKA_TOKEN_SERVICE_ACCOUNT:-orka-client}"
  orka_client_ns="${ORKA_TOKEN_NAMESPACE:-${SUBSTRATE_NS}}"
  log "Ensuring Orka API client ServiceAccount ${orka_client_ns}/${orka_client_sa}"
  kubectl --context "${ctx}" create serviceaccount "${orka_client_sa}" -n "${orka_client_ns}" \
    --dry-run=client -o yaml | kubectl --context "${ctx}" apply -f -

  log "Agentic add-ons ready (model ${SUBSTRATE_MODEL} via vekil; codex Actor image; git secret; API client SA)."
fi

log "Agent Substrate installed. Demo 70 can run against context kind-${KIND_CLUSTER}:"
log "  kubectl config use-context kind-${KIND_CLUSTER}"
log "  DEMO_SUBSTRATE_NAMESPACE=default ./hack/demos/70-agent-substrate.sh"
log "Tear down with: kind delete cluster --name ${KIND_CLUSTER}"
