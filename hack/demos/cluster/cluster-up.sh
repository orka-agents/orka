#!/usr/bin/env bash
# Bootstrap a kind cluster for the Orka demo set.
#
# Idempotent — if the cluster already exists, only Orka images get rebuilt
# and reloaded. The follow-on install-{kontxt,agent-sandbox}.sh scripts
# layer on the optional kontxt + sandbox stacks needed by Demos 50 / 60.
#
# Requires: kind, docker, kubectl, helm (or `kustomize` + `kubectl apply`).

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
img="${ORKA_DEMO_IMAGE:-orka-demo:dev}"
namespace="${ORKA_NAMESPACE:-orka-system}"
demo_namespace="${DEMO_NAMESPACE:-demo-magic}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

command -v kind    >/dev/null 2>&1 || die "missing required command: kind"
command -v docker  >/dev/null 2>&1 || die "missing required command: docker"
command -v kubectl >/dev/null 2>&1 || die "missing required command: kubectl"

if kind get clusters | grep -qx "${cluster_name}"; then
  log "kind cluster ${cluster_name} already exists; reusing"
else
  log "Creating kind cluster ${cluster_name}"
  kind create cluster --name "${cluster_name}"
fi

log "Selecting kubectl context kind-${cluster_name}"
kubectl config use-context "kind-${cluster_name}" >/dev/null

log "Building controller image ${img}"
(cd "${repo_root}" && make docker-build IMG="${img}")

log "Loading ${img} into kind/${cluster_name}"
kind load docker-image "${img}" --name "${cluster_name}"

log "Ensuring namespace ${namespace} and ${demo_namespace}"
kubectl create namespace "${namespace}"      --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "${demo_namespace}" --dry-run=client -o yaml | kubectl apply -f -

log "Deploying Orka (namespace ${namespace}, image ${img})"
if [[ "${namespace}" == "orka-system" ]]; then
  (cd "${repo_root}" && make deploy IMG="${img}")
else
  (cd "${repo_root}" && make manifests kustomize)
  tmp_config="$(mktemp -d)"
  cp -R "${repo_root}/config" "${tmp_config}/config"
  (cd "${tmp_config}/config/manager" && "${repo_root}/bin/kustomize" edit set image controller="${img}")
  perl -0pi -e "s#--controller-url=http://orka-api\.orka-system\.svc:8080#--controller-url=http://orka-api.${namespace}.svc:8080#g" \
    "${tmp_config}/config/manager/manager.yaml"
  (cd "${tmp_config}/config/default" && "${repo_root}/bin/kustomize" edit set namespace "${namespace}")
  "${repo_root}/bin/kustomize" build "${tmp_config}/config/default" | kubectl apply -f -
  rm -rf "${tmp_config}"
fi

log "Waiting for orka-controller-manager rollout"
kubectl -n "${namespace}" rollout status deployment/orka-controller-manager --timeout=300s

log "Cluster up. Next steps (optional):"
log "  hack/demos/cluster/install-kontxt.sh         # for Demo 50"
log "  hack/demos/cluster/install-agent-sandbox.sh  # for Demo 60"
log "  make demo-images                              # for Demo 50's caller image"
