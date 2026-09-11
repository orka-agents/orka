#!/usr/bin/env bash
# Install and validate the official Substrate pin in a dedicated local cluster.
set -Eeuo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
export KEEP_CLUSTER=1
export KIND_CLUSTER="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
export SUBSTRATE_E2E_RUN_DIR="${SUBSTRATE_E2E_RUN_DIR:-${repo_root}/bin/substrate-e2e-${KIND_CLUSTER}}"
# Reuse is explicit and never deletes an existing cluster. Repeated conformance
# uses fixed test resource names; remove completed test objects before rerunning.
if [[ "${DEMO_CLUSTER_REUSE:-}" == reuse ]]; then
  export SUBSTRATE_REUSE_CLUSTER=1
elif [[ -n "${DEMO_CLUSTER_REUSE:-}" ]]; then
  printf 'Use DEMO_CLUSTER_REUSE=reuse or choose a new KIND_CLUSTER; this installer does not recreate existing clusters.\n' >&2
  exit 1
fi
export PATH="${repo_root}/bin:$(go env GOPATH)/bin:${PATH}"
bash "${repo_root}/scripts/agent-substrate-e2e.sh"
printf 'Scoped kubeconfig: %s/kubeconfig\n' "${SUBSTRATE_E2E_RUN_DIR}"
