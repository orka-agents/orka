#!/usr/bin/env bash
# Shared upstream-only local setup. Call from the bundled installer/E2E.

substrate_prepare_upstream() {
  local orka_root="$1" run_dir="$2" cluster="$3"
  # shellcheck source=hack/agent-substrate/upstream.env
  source "${orka_root}/hack/agent-substrate/upstream.env"
  python3 - <<'PY'
import subprocess, sys
try:
    result = subprocess.run(['docker', 'info'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
    if result.returncode:
        raise RuntimeError()
except (subprocess.TimeoutExpired, RuntimeError, OSError):
    print('Docker engine is unavailable; restore it before running native Substrate conformance.', file=sys.stderr)
    sys.exit(1)
PY
  local docker_status=$?
  [[ "${docker_status}" == 0 ]] || return "${docker_status}"
  if [[ "${SUBSTRATE_REPO:-${SUBSTRATE_UPSTREAM_REPOSITORY}}" != "${SUBSTRATE_UPSTREAM_REPOSITORY}" ||
        "${SUBSTRATE_REF:-${SUBSTRATE_UPSTREAM_COMMIT}}" != "${SUBSTRATE_UPSTREAM_COMMIT}" ]]; then
    printf 'Substrate source must match hack/agent-substrate/upstream.env; provider forks and patches are unsupported\n' >&2
    return 1
  fi
  mkdir -p "${run_dir}"
  chmod 700 "${run_dir}"
  SUBSTRATE_DIR="${run_dir}/substrate"
  export KUBECONFIG="${run_dir}/kubeconfig"
  export KIND_CLUSTER_NAME="${cluster}"
  export KUBECTL_CONTEXT="kind-${cluster}"
  export KIND_REGISTRY_PORT="${KIND_REGISTRY_PORT:-5001}"
  export KO_DOCKER_REPO="localhost:${KIND_REGISTRY_PORT}"
  export NO_DEV_ENV=true
  if [[ ! -d "${SUBSTRATE_DIR}/.git" ]]; then
    git clone --quiet --filter=blob:none --no-checkout "${SUBSTRATE_UPSTREAM_REPOSITORY}" "${SUBSTRATE_DIR}"
    git -C "${SUBSTRATE_DIR}" checkout --quiet --detach "${SUBSTRATE_UPSTREAM_COMMIT}"
  fi
  [[ "$(git -C "${SUBSTRATE_DIR}" remote get-url origin)" == "${SUBSTRATE_UPSTREAM_REPOSITORY}" &&
     "$(git -C "${SUBSTRATE_DIR}" rev-parse HEAD)" == "${SUBSTRATE_UPSTREAM_COMMIT}" ]] || {
    printf 'Substrate checkout does not match the official upstream pin\n' >&2; return 1;
  }
  substrate_require_clean_upstream "${SUBSTRATE_DIR}" || return 1
  local registry_port
  registry_port="$(docker inspect -f '{{(index (index .HostConfig.PortBindings "5000/tcp") 0).HostPort}}' kind-registry 2>/dev/null || true)"
  if [[ -n "${registry_port}" && "${registry_port}" != "${KIND_REGISTRY_PORT}" ]]; then
    printf 'Existing kind-registry uses another port; refusing to replace shared infrastructure\n' >&2
    return 1
  fi
  if kind get clusters 2>/dev/null | grep -Fxq -- "${cluster}"; then
    [[ "${SUBSTRATE_REUSE_CLUSTER:-0}" == 1 ]] || {
      printf 'Cluster %s already exists; set SUBSTRATE_REUSE_CLUSTER=1 to reuse it\n' "${cluster}" >&2; return 1;
    }
    kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
  else
    (cd "${SUBSTRATE_DIR}" && bash hack/create-kind-cluster.sh) || return 1
  fi
  substrate_install_ate_system || return 1
  substrate_require_clean_upstream "${SUBSTRATE_DIR}"
}

# Install the pinned control plane with ate-setup. Authorization is always
# enforced in v1: only the shared ate-client test identity is a bootstrap
# owner, and Orka receives least-privilege access policies afterwards. Only
# Orka's controller identity may reach Actors through the router. Actors get no
# egress unless an EgressPolicy allows it (v1 has no direct egress). The egress
# gateway injects credentials from the Kubernetes Secret provider, which is
# default-deny until an Atespace is granted a namespace.
substrate_install_ate_system() {
  local orka_spiffe_id="spiffe://cluster.local/ns/${ORKA_NAMESPACE:-orka-system}/sa/${ORKA_CONTROLLER_SERVICE_ACCOUNT:-orka-controller-manager}"
  (cd "${SUBSTRATE_DIR}" && go run ./cmd/ate-setup --kind --kind-cluster-name="${KIND_CLUSTER_NAME}" \
    --kubeconfig="${KUBECONFIG}" --context="${KUBECTL_CONTEXT}" --no-dev-env --rollout-timeout=5m \
    deploy ate-system \
    --credential-provider='{"name":"k8s.io"}' \
    --ateapi-authz-bootstrap-owners="system:serviceaccount:ate-system:ate-client" \
    --ingress-allowed-spiffe-ids="${orka_spiffe_id}") || return 1
  kubectl -n ate-system rollout status deployment/ate-api-server --timeout=5m
}

substrate_require_clean_upstream() {
  local state
  state="$(git -C "$1" status --porcelain --untracked-files=all)" || return 1
  if [[ -n "${state}" ]]; then
    printf 'Substrate checkout contains modified or untracked files; refusing upstream conformance\n' >&2
    return 1
  fi
}
