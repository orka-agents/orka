#!/usr/bin/env bash
# Install the archived Agent Substrate infrastructure in a dedicated kind
# cluster for direct workspace/MCP evaluation.
#
# This is intentionally not an agent-runtime installer. The ACP v2 hard cutover
# removed the legacy turn-wrapper path, and RuntimeSession-to-Substrate Actor
# dispatch remains deferred until an Actor-backed orka.harness.v2 supervisor
# exists. Current built-in Codex, Claude, and Copilot validation uses controller-owned
# RuntimePools via scripts/live-acp-runtime-e2e.sh.
#
# The underlying scripts/agent-substrate-e2e.sh flow clones the pinned
# Substrate revision, verifies and applies the reviewed evaluation patches in
# hack/agent-substrate, creates its dedicated kind cluster and registry,
# deploys the control plane, and validates direct Substrate plus Orka-brokered
# MCP workspace paths. KEEP_CLUSTER=1 leaves that infrastructure available for
# inspection after the smoke checks finish.
#
# Unlike install-kontxt.sh / install-agent-sandbox.sh (which attach to the
# shared demo-magic kind cluster), Substrate needs its own cluster because its
# bootstrap installs custom registry and gVisor node configuration.
#
# Requires: kind, ko, docker, go, git, jq, kubectl, and curl. The cluster is
# named by KIND_CLUSTER (default orka-agent-substrate-e2e); its context is
# kind-<KIND_CLUSTER>. Tear down with:
# kind delete cluster --name <KIND_CLUSTER>

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

if [[ "${AGENTIC:-0}" != "0" ]]; then
  die "AGENTIC mode was removed with the ACP v2 hard cutover; use scripts/live-acp-runtime-e2e.sh for built-in agent runtime validation"
fi

# Pin to the same Substrate revision CI uses, unless overridden.
SUBSTRATE_REF="${SUBSTRATE_REF:-b80031d260959b1fc5c6f61e3099fe2a6d368af1}"
if [[ ! "${SUBSTRATE_REF}" =~ ^[[:xdigit:]]{40}$ ]]; then
  die "SUBSTRATE_REF must be an immutable full 40-hex commit SHA; branches and movable tags cannot identify a reusable cluster"
fi
SUBSTRATE_REF="$(printf '%s' "${SUBSTRATE_REF}" | tr '[:upper:]' '[:lower:]')"
KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
SUBSTRATE_KUBECONFIG="${SUBSTRATE_KUBECONFIG:-${HOME}/.kube/orka-substrate-${KIND_CLUSTER}.config}"
DIRECT_EVAL_MARKER_NAMESPACE="orka-system"
DIRECT_EVAL_MARKER_NAME="orka-substrate-direct-evaluation-v2"
DIRECT_EVAL_MARKER_SCHEMA_VERSION="4"
DIRECT_EVAL_PATCH_SET_VERSION="2026-08-27.1"
DIRECT_EVAL_HARDENING="atenet-routing-metadata-allowlist+envoy-info+streaming-route-timeout-disabled,atelet-root-supervisor-capabilities,ateom-runsc-delete-recovery"
DIRECT_EVAL_PATCH_FILES=(
  "hack/agent-substrate/atelet-root-supervisor-capabilities.patch"
  "hack/agent-substrate/atenet-router-authorization-redaction.patch"
  "hack/agent-substrate/ateom-runsc-delete-recovery.patch"
)
# Reuse-or-recreate behavior when the kind cluster already exists. The Substrate
# standup (scripts/agent-substrate-e2e.sh -> hack/create-kind-cluster.sh) does
# `kind delete cluster` then recreate, so re-running this bootstrap on a live
# cluster would DESTROY it and its direct-evaluation state. When a cluster
# already exists we prompt the operator to reuse, recreate, or cancel. Set
# DEMO_CLUSTER_REUSE=reuse|recreate|cancel to skip the prompt (required for
# non-interactive runs that should not just recreate).
DEMO_CLUSTER_REUSE="${DEMO_CLUSTER_REUSE:-}"
# Exercise the extended direct workspace and MCP checks during standup.
# Override to 0 to run only the base smoke checks.
SUBSTRATE_E2E_EXTENDED="${SUBSTRATE_E2E_EXTENDED:-1}"

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

reviewed_patch_set_manifest() {
  local relative_path patch_path patch_blob
  for relative_path in "${DIRECT_EVAL_PATCH_FILES[@]}"; do
    patch_path="${repo_root}/${relative_path}"
    [[ -f "${patch_path}" ]] || die "expected reviewed patch ${patch_path} to exist"
    patch_blob="$(git hash-object "${patch_path}")"
    [[ -n "${patch_blob}" ]] || die "could not fingerprint reviewed patch ${patch_path}"
    printf '%s git-blob:%s\n' "${relative_path}" "${patch_blob}"
  done
}

# The marker stores this exact, ordered manifest in addition to its schema and
# human-reviewed version. Any patch-byte change therefore invalidates reuse even
# if a developer forgets to bump DIRECT_EVAL_PATCH_SET_VERSION.
DIRECT_EVAL_REVIEWED_PATCH_SET="$(reviewed_patch_set_manifest)"

# cluster_exists: true when a kind cluster named ${KIND_CLUSTER} is present.
cluster_exists() {
  command -v kind >/dev/null 2>&1 || return 1
  kind get clusters 2>/dev/null | grep -qx "${KIND_CLUSTER}"
}

activate_scoped_kubeconfig() {
  mkdir -p "$(dirname "${SUBSTRATE_KUBECONFIG}")"
  kind export kubeconfig --name "${KIND_CLUSTER}" --kubeconfig "${SUBSTRATE_KUBECONFIG}" >/dev/null
  chmod 0600 "${SUBSTRATE_KUBECONFIG}"
  export KUBECONFIG="${SUBSTRATE_KUBECONFIG}"
  log "Using retained scoped kubeconfig ${SUBSTRATE_KUBECONFIG}"
}

direct_evaluation_marker_exists() {
  kubectl --context "kind-${KIND_CLUSTER}" -n "${DIRECT_EVAL_MARKER_NAMESPACE}" \
    get configmap "${DIRECT_EVAL_MARKER_NAME}" >/dev/null 2>&1
}

direct_evaluation_hardening_matches() {
  local marker_json
  marker_json="$(kubectl --context "kind-${KIND_CLUSTER}" -n "${DIRECT_EVAL_MARKER_NAMESPACE}" \
    get configmap "${DIRECT_EVAL_MARKER_NAME}" -o json 2>/dev/null)" || return 1

  jq -e \
    --arg marker_schema "${DIRECT_EVAL_MARKER_SCHEMA_VERSION}" \
    --arg substrate_commit "${SUBSTRATE_REF}" \
    --arg patch_set_version "${DIRECT_EVAL_PATCH_SET_VERSION}" \
    --arg reviewed_patch_set "${DIRECT_EVAL_REVIEWED_PATCH_SET}" \
    --arg hardening "${DIRECT_EVAL_HARDENING}" \
    '(
      .data["marker-schema-version"] == $marker_schema and
      .data["substrate-commit"] == $substrate_commit and
      .data["reviewed-patch-set-version"] == $patch_set_version and
      .data["reviewed-patch-set"] == $reviewed_patch_set and
      .data["pinned-hardening"] == $hardening
    )' <<<"${marker_json}" >/dev/null
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
      printf 'It looks healthy. Reusing keeps its direct-evaluation state intact.\n'
    else
      printf 'WARNING: it does NOT look healthy — reusing may carry that breakage forward.\n'
    fi
    printf '  [r] reuse    — keep the cluster unchanged (non-destructive)\n'
    printf '  [c] recreate — DELETE and rebuild from scratch (destroys cluster state)\n'
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
  activate_scoped_kubeconfig
  health_summary="$(cluster_health)" && health_flag=1 || health_flag=0
  log "Existing cluster kind-${KIND_CLUSTER} detected (${health_summary})"
  action="$(prompt_cluster_action "${health_flag}")"
  case "${action}" in
    reuse)
      if ! direct_evaluation_marker_exists; then
        die "existing cluster has no ACP v2 direct-evaluation marker and may retain retired agentic add-ons; choose DEMO_CLUSTER_REUSE=recreate or cancel"
      fi
      if ! direct_evaluation_hardening_matches; then
        die "existing cluster does not match marker schema ${DIRECT_EVAL_MARKER_SCHEMA_VERSION}, Substrate commit ${SUBSTRATE_REF}, and reviewed patch set ${DIRECT_EVAL_PATCH_SET_VERSION}; choose DEMO_CLUSTER_REUSE=recreate or cancel"
      fi
      log "Reusing existing marked cluster — skipping the Substrate standup (kind delete + rebuild)."
      log "Leaving the existing direct Substrate/MCP evaluation cluster unchanged."
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

# The CI-parity E2E deliberately uses an isolated temporary kubeconfig. Export
# the fresh or reused cluster into a stable, scoped file before follow-up
# marker and health operations. Never merge into ~/.kube/config.
activate_scoped_kubeconfig

log "Recording ACP v2 direct-evaluation marker ${DIRECT_EVAL_MARKER_NAMESPACE}/${DIRECT_EVAL_MARKER_NAME}"
kubectl --context "kind-${KIND_CLUSTER}" -n "${DIRECT_EVAL_MARKER_NAMESPACE}" \
  create configmap "${DIRECT_EVAL_MARKER_NAME}" \
  --from-literal=mode=direct-workspace-mcp \
  --from-literal=runtime-dispatch=deferred \
  --from-literal="marker-schema-version=${DIRECT_EVAL_MARKER_SCHEMA_VERSION}" \
  --from-literal="substrate-commit=${SUBSTRATE_REF}" \
  --from-literal="reviewed-patch-set-version=${DIRECT_EVAL_PATCH_SET_VERSION}" \
  --from-literal="reviewed-patch-set=${DIRECT_EVAL_REVIEWED_PATCH_SET}" \
  --from-literal="pinned-hardening=${DIRECT_EVAL_HARDENING}" \
  --dry-run=client -o yaml \
  | kubectl --context "kind-${KIND_CLUSTER}" apply -f - >/dev/null

log "Agent Substrate direct-evaluation cluster is available at context kind-${KIND_CLUSTER}."
log "This cluster does not provide an ACP agent runtime; Demo 70 remains archived."
log "Use scripts/live-acp-runtime-e2e.sh for live Codex, Claude, and Copilot RuntimePool validation."
log "Tear down with: kind delete cluster --name ${KIND_CLUSTER}"
