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
# Reuse-or-recreate behavior when the kind cluster already exists. The Substrate
# standup (scripts/agent-substrate-e2e.sh -> hack/create-kind-cluster.sh) does
# `kind delete cluster` then recreate, so re-running this bootstrap on a live
# cluster would DESTROY it (and any completed vekil device-code login, which is
# cached in an emptyDir). When a cluster already exists we prompt the operator
# reuse / recreate / cancel. Set DEMO_CLUSTER_REUSE=reuse|recreate|cancel to skip
# the prompt (required for non-interactive runs that should not just recreate).
DEMO_CLUSTER_REUSE="${DEMO_CLUSTER_REUSE:-}"
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

# cluster_exists: true when a kind cluster named ${KIND_CLUSTER} is present.
cluster_exists() {
  command -v kind >/dev/null 2>&1 || return 1
  kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"
}

# cluster_health: prints a one-line health summary to stdout and returns 0 when
# the existing cluster looks healthy enough to reuse (node Ready + Orka
# controller 1/1 + at least one Running ate-system pod), 1 otherwise. Minimal by
# design: over-strict checks would force needless destructive recreates.
cluster_health() {
  local ctx="kind-${KIND_CLUSTER}" node ctrl ate healthy=0
  node="$(kubectl --context "${ctx}" get nodes \
    -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  ctrl="$(kubectl --context "${ctx}" -n orka-system get deploy orka-controller-manager \
    -o jsonpath='{.status.readyReplicas}/{.status.replicas}' 2>/dev/null || true)"
  ate="$(kubectl --context "${ctx}" -n ate-system get pods --no-headers 2>/dev/null \
    | grep -c Running || true)"
  [[ "${node}" == "True" && "${ctrl}" == "1/1" && "${ate:-0}" -ge 1 ]] && healthy=1
  printf 'node Ready=%s, controller=%s, ate-system Running=%s' \
    "${node:-?}" "${ctrl:-?}" "${ate:-0}"
  [[ "${healthy}" == 1 ]]
}

# prompt_cluster_action: resolves reuse|recreate|cancel for an existing cluster.
# Honors DEMO_CLUSTER_REUSE when set; otherwise prompts on /dev/tty (so it works
# even when the script's stdin is redirected, e.g. via make). With no tty and no
# override it preserves the historical behavior (recreate). Echoes the choice.
prompt_cluster_action() {
  local healthy_flag="$1"  # "1" healthy, "0" unhealthy
  case "${DEMO_CLUSTER_REUSE}" in
    reuse|recreate|cancel) printf '%s' "${DEMO_CLUSTER_REUSE}"; return 0 ;;
    "") : ;;
    *) die "invalid DEMO_CLUSTER_REUSE='${DEMO_CLUSTER_REUSE}' (expected reuse|recreate|cancel)" ;;
  esac
  if [[ ! -r /dev/tty ]]; then
    # Non-interactive, no override: keep today's behavior so automation that
    # expects a fresh cluster is not silently changed.
    printf 'recreate'
    return 0
  fi
  local default_hint="reuse" ans
  [[ "${healthy_flag}" == 1 ]] || default_hint="recreate"
  {
    printf '\n'
    printf 'A kind cluster named %q already exists.\n' "${KIND_CLUSTER}"
    if [[ "${healthy_flag}" == 1 ]]; then
      printf 'It looks healthy. Reusing keeps it (and any vekil login) intact.\n'
    else
      printf 'WARNING: it does NOT look healthy — reusing may carry that breakage forward.\n'
    fi
    printf '  [r] reuse    — keep the cluster; reconcile add-ons only (non-destructive)\n'
    printf '  [c] recreate — DELETE and rebuild from scratch (destroys vekil login + state)\n'
    printf '  [x] cancel   — exit without changes\n'
    printf 'Choose r/c/x [default: %s]: ' "${default_hint}"
  } >/dev/tty
  read -r ans </dev/tty || ans=""
  case "${ans:-}" in
    r|R|reuse)    printf 'reuse' ;;
    c|C|recreate) printf 'recreate' ;;
    x|X|cancel)   printf 'cancel' ;;
    "")           printf '%s' "${default_hint}" ;;
    *)            printf '%s' "${default_hint}" ;;
  esac
}

run_e2e=1
if cluster_exists; then
  health_summary="$(cluster_health)" && health_flag=1 || health_flag=0
  log "Existing cluster kind-${KIND_CLUSTER} detected (${health_summary})"
  action="$(prompt_cluster_action "${health_flag}")"
  case "${action}" in
    reuse)
      log "Reusing existing cluster — skipping the Substrate standup (kind delete + rebuild)."
      log "Reconciling the agentic add-ons idempotently below."
      run_e2e=0
      ;;
    recreate)
      log "Recreating cluster — the Substrate standup will delete and rebuild kind-${KIND_CLUSTER}."
      run_e2e=1
      ;;
    cancel)
      log "Cancelled — leaving cluster kind-${KIND_CLUSTER} untouched."
      exit 0
      ;;
    *)
      die "unexpected cluster action '${action}'"
      ;;
  esac
fi

if [[ "${run_e2e}" == 1 ]]; then
  log "Standing up Agent Substrate (ref ${SUBSTRATE_REF}, cluster kind-${KIND_CLUSTER})"
  log "This builds 4 images and the Substrate control plane — first run takes several minutes."

  KEEP_CLUSTER=1 \
    SUBSTRATE_REF="${SUBSTRATE_REF}" \
    KIND_CLUSTER="${KIND_CLUSTER}" \
    SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED}" \
    bash "${e2e_script}"
fi

if [[ "${AGENTIC}" == "1" ]]; then
  ctx="kind-${KIND_CLUSTER}"
  log "Ensuring substrate demo namespace ${SUBSTRATE_NS}"
  kubectl --context "${ctx}" create namespace "${SUBSTRATE_NS}" --dry-run=client -o yaml \
    | kubectl --context "${ctx}" apply -f -
  log "Allowing namespace ${SUBSTRATE_NS} to reach the vekil NetworkPolicy"
  kubectl --context "${ctx}" label namespace "${SUBSTRATE_NS}" \
    vekil.sozercan.io/access=true --overwrite >/dev/null

  # ---- 1. Codex-capable Actor image -------------------------------------
  # The agentic run executes a real codex CLI INSIDE the gVisor Actor, so the
  # Actor image must carry codex + git (the e2e's stripped workspace-agent-root
  # does not). The production agent-harness-wrapper image has the daemon + codex +
  # git. Build it LOCALLY (the kind registry is on localhost; a remote builder
  # cannot push there) for the kind node arch, push to the registry, and point
  # the ActorTemplate at it.
  node_arch="$(kubectl --context "${ctx}" get nodes \
    -o jsonpath='{.items[0].status.nodeInfo.architecture}' 2>/dev/null || echo amd64)"
  reg_ip="$(docker inspect -f '{{with index .NetworkSettings.Networks "kind"}}{{.IPAddress}}{{end}}' "${KIND_REGISTRY_NAME}" 2>/dev/null || true)"
  [[ -n "${reg_ip}" ]] || die "could not determine ${KIND_REGISTRY_NAME} kind-network IP"
  push_image="localhost:${KIND_REGISTRY_PORT}/orka/agent-harness-wrapper:${CODEX_ACTOR_TAG}"
  actor_image="${reg_ip}:5000/orka/agent-harness-wrapper:${CODEX_ACTOR_TAG}"

  log "Building codex Actor image ${push_image} (arch ${node_arch})"
  docker build --platform "linux/${node_arch}" -t "${push_image}" \
    -f "${repo_root}/workers/harness/Dockerfile" "${repo_root}"
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
  ph="$(kubectl --context "${ctx}" -n "${SUBSTRATE_TEMPLATE_NS}" get actortemplate "${SUBSTRATE_TEMPLATE_NAME}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [[ "${ph}" == "Ready" ]] || die "ActorTemplate ${SUBSTRATE_TEMPLATE_NS}/${SUBSTRATE_TEMPLATE_NAME} did not become Ready (phase=${ph:-unknown})"

  # ---- 2. In-cluster model proxy (vekil) --------------------------------
  # Zero-config Copilot upstream via device-code login (a gho_ gh token has no
  # Copilot entitlement). The deploy script prints a github.com/login/device
  # code in the pod logs; the operator completes it once, and vekil caches the
  # session. We do NOT pass a token secret here.
  vekil_script=""
  for candidate in \
    "${repo_root}/.codex/skills/vekil-reverse-proxy-deploy/scripts/deploy_vekil_reverse_proxy.sh" \
    "${repo_root}/.claude/skills/vekil-reverse-proxy-deploy/scripts/deploy_vekil_reverse_proxy.sh"; do
    if [[ -x "${candidate}" ]]; then
      vekil_script="${candidate}"
      break
    fi
  done
  if kubectl --context "${ctx}" -n "${VEKIL_NS}" get deploy vekil >/dev/null 2>&1; then
    log "vekil already deployed in ${VEKIL_NS} — leaving it (re-run device-code login if /readyz is down)"
  elif [[ -x "${vekil_script}" ]]; then
    log "Deploying vekil model proxy to ${VEKIL_NS} (device-code login)"
    bash "${vekil_script}" --context "${ctx}" --namespace "${VEKIL_NS}" --skip-wait
    log "ACTION REQUIRED: complete the GitHub device-code login printed in vekil's logs:"
    log "  kubectl --context ${ctx} -n ${VEKIL_NS} logs deploy/vekil | grep 'login/device'"
    log "  (visit the URL, enter the code; then /readyz returns 200)"
  else
    die "vekil deploy script not found under .codex/skills or .claude/skills; install the skill or deploy an in-cluster OpenAI-compatible proxy before Demo 70"
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
  kubectl --context "${ctx}" create namespace "${orka_client_ns}" --dry-run=client -o yaml \
    | kubectl --context "${ctx}" apply -f -
  kubectl --context "${ctx}" create serviceaccount "${orka_client_sa}" -n "${orka_client_ns}" \
    --dry-run=client -o yaml | kubectl --context "${ctx}" apply -f -

  log "Agentic add-ons ready (model ${SUBSTRATE_MODEL} via vekil; codex Actor image; git secret; API client SA)."
fi

log "Agent Substrate installed. Demo 70 can run against context kind-${KIND_CLUSTER}:"
log "  kubectl config use-context kind-${KIND_CLUSTER}"
log "  DEMO_SUBSTRATE_NAMESPACE=default ./hack/demos/70-agent-substrate.sh"
log "Tear down with: kind delete cluster --name ${KIND_CLUSTER}"
