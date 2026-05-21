#!/usr/bin/env bash
# Tear down the kind cluster used for Orka demos.

set -Eeuo pipefail

cluster_name="${ORKA_DEMO_CLUSTER:-orka-demo}"

if kind get clusters | grep -qx "${cluster_name}"; then
  printf '==> Deleting kind cluster %s\n' "${cluster_name}" >&2
  kind delete cluster --name "${cluster_name}"
else
  printf '==> kind cluster %s not found; nothing to do\n' "${cluster_name}" >&2
fi
