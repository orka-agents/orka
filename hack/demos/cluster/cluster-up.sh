#!/usr/bin/env bash

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
img="${ORKA_DEMO_IMAGE:-orka-demo:dev}"
publisher_img="${ORKA_DEMO_PUBLISHER_IMAGE:-orka-workspace-publisher:demo}"
build_acp_codex="${ORKA_DEMO_BUILD_ACP_CODEX:-1}"
acp_codex_img="${ORKA_DEMO_ACP_CODEX_IMAGE:-orka-acp-codex-runtime:demo}"
namespace="${ORKA_NAMESPACE:-orka-system}"
demo_namespace="${DEMO_NAMESPACE:-orka-system}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
# shellcheck source=scripts/lib/kind-local-registry.sh
. "${repo_root}/scripts/lib/kind-local-registry.sh"
# shellcheck source=scripts/lib/e2e-admission-tls.sh
. "${repo_root}/scripts/lib/e2e-admission-tls.sh"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v kind    >/dev/null 2>&1 || die "missing required command: kind"
command -v docker  >/dev/null 2>&1 || die "missing required command: docker"
command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"
command -v curl    >/dev/null 2>&1 || die "missing required command: curl"
command -v jq      >/dev/null 2>&1 || die "missing required command: jq"
[[ "${namespace}" == "orka-system" ]] || die "demo cluster currently requires ORKA_NAMESPACE=orka-system"

if kind get clusters | grep -qx "${cluster_name}"; then
  log "kind cluster ${cluster_name} already exists; reusing"
else
  log "Creating kind cluster ${cluster_name}"
  kind create cluster --name "${cluster_name}"
fi

log "Selecting kubectl context kind-${cluster_name}"
kubectl config use-context "kind-${cluster_name}" >/dev/null

orka_kind_registry_start "${cluster_name}"

log "Building controller image ${img}"
(cd "${repo_root}" && make docker-build IMG="${img}")
log "Building workspace publisher image ${publisher_img}"
(cd "${repo_root}" && make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="${publisher_img}")

log "Loading ${img} into kind/${cluster_name}"
kind load docker-image "${img}" --name "${cluster_name}"
manager_ref="$(orka_kind_registry_push "${img}" "orka/controller")"
publisher_ref="$(orka_kind_registry_push "${publisher_img}" "orka/workspace-publisher")"
placeholder_digest="sha256:$(printf '0%.0s' {1..64})"
acp_codex_ref="example.invalid/orka/acp-codex@${placeholder_digest}"
if [[ "${build_acp_codex}" == "1" ]]; then
  log "Building immutable Codex ACP runtime image ${acp_codex_img}"
  (cd "${repo_root}" && make docker-build-acp-codex-runtime ACP_CODEX_RUNTIME_IMG="${acp_codex_img}")
  acp_codex_ref="$(orka_kind_registry_push "${acp_codex_img}" "orka/acp-codex-runtime")"
  log "Codex ACP runtime image pinned as ${acp_codex_ref}"
fi

log "Ensuring namespaces ${namespace}, ${demo_namespace}, and vekil-system"
bash "${repo_root}/scripts/lib/ensure-static-mode-namespace.sh" kubectl "${namespace}" harness-v2
log "Ensuring the admission webhook TLS secret in ${namespace}"
orka_e2e_bootstrap_admission_tls kubectl "${namespace}"
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace vekil-system         --dry-run=client -o yaml | kubectl apply -f -

log "Installing Orka harness v2 CRDs"
(cd "${repo_root}" && make install)

log "Deploying Orka (namespace ${namespace}, image ${manager_ref})"
(cd "${repo_root}" && make deploy \
  IMG="${manager_ref}" \
  WORKSPACE_PUBLISHER_IMG="${publisher_ref}" \
  ACP_CODEX_RUNTIME_IMG="${acp_codex_ref}" \
  ACP_CLAUDE_RUNTIME_IMG="example.invalid/orka/acp-claude@${placeholder_digest}" \
  ACP_COPILOT_RUNTIME_IMG="example.invalid/orka/acp-copilot@${placeholder_digest}" \
  ACP_OPENCODE_RUNTIME_IMG="example.invalid/orka/acp-opencode@${placeholder_digest}")

log "Waiting for orka-controller-manager rollout"
kubectl -n "${namespace}" rollout status deployment/orka-controller-manager --timeout=300s

log "Cluster up. Next steps (optional):"
log "  hack/demos/cluster/install-agent-sandbox.sh  # agent-sandbox backend"
log "  make demo-images                              # agent-sandbox runtime image"
