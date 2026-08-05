#!/usr/bin/env bash
# Tear down the kind cluster used for Orka demos.

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../.." && pwd)"
# shellcheck source=scripts/lib/kind-local-registry.sh
. "${repo_root}/scripts/lib/kind-local-registry.sh"

if kind get clusters | grep -qx "${cluster_name}"; then
  printf '==> Deleting kind cluster %s\n' "${cluster_name}" >&2
  kind delete cluster --name "${cluster_name}"
else
  printf '==> kind cluster %s not found; nothing to do\n' "${cluster_name}" >&2
fi

orka_kind_registry_stop "${cluster_name}"
