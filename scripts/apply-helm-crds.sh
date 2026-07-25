#!/usr/bin/env bash
# Apply the exact CRD specs from a Helm chart without deleting existing custom resources.

set -euo pipefail

usage() {
  echo "usage: $0 CHART [KUBE_CONTEXT]" >&2
}

if [[ $# -lt 1 || $# -gt 2 ]]; then
  usage
  exit 2
fi

chart=$1
kube_context=${2:-}
field_manager=orka-crd-lifecycle

for command in helm kubectl jq; do
  if ! command -v "${command}" >/dev/null 2>&1; then
    echo "required command not found: ${command}" >&2
    exit 2
  fi
done

kubectl_cmd=(kubectl)
if [[ -n "${kube_context}" ]]; then
  kubectl_cmd+=(--context "${kube_context}")
fi

target_crds=$(mktemp)
cleanup() {
  rm -f "${target_crds}"
}
trap cleanup EXIT

helm show crds "${chart}" > "${target_crds}"
if [[ ! -s "${target_crds}" ]]; then
  echo "chart contains no CRDs: ${chart}" >&2
  exit 1
fi

# Create missing CRDs and transfer ownership of fields present in the target.
"${kubectl_cmd[@]}" apply \
  --server-side \
  --force-conflicts \
  --field-manager="${field_manager}" \
  -f "${target_crds}"

# SSA cannot remove fields omitted by a new manager during first adoption. Replace
# each CRD spec atomically, guarded by resourceVersion, so stale schema fields are
# removed without deleting the CRD or its custom resources.
kubectl create --dry-run=client -f "${target_crds}" -o json | \
  jq -c '{name: .metadata.name, spec: .spec}' | \
  while IFS= read -r target; do
    name=$(jq -er '.name' <<< "${target}")
    spec=$(jq -ec '.spec' <<< "${target}")
    resource_version=$("${kubectl_cmd[@]}" get customresourcedefinition "${name}" -o jsonpath='{.metadata.resourceVersion}')
    patch=$(jq -cn \
      --arg resourceVersion "${resource_version}" \
      --argjson spec "${spec}" \
      '[
        {"op":"test","path":"/metadata/resourceVersion","value":$resourceVersion},
        {"op":"replace","path":"/spec","value":$spec}
      ]')
    "${kubectl_cmd[@]}" patch customresourcedefinition "${name}" --type=json -p "${patch}" >/dev/null
    "${kubectl_cmd[@]}" wait --for=condition=Established --timeout=60s "customresourcedefinition/${name}"
  done
