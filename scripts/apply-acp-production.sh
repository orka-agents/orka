#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  echo "Usage: $0 OVERLAY_DIR KUSTOMIZE KUBECTL" >&2
}

[[ $# -eq 3 ]] || {
  usage
  exit 2
}

overlay_dir="$1"
kustomize="$2"
kubectl="$3"

for command in "${kustomize}" "${kubectl}" jq; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done
[[ -d "${overlay_dir}" && ! -L "${overlay_dir}" ]] || {
  echo "ACP production overlay must be a real directory: ${overlay_dir}" >&2
  exit 1
}

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/apply-acp-production.XXXXXX")"
cleanup() {
  rm -rf "${work_dir}"
}
trap cleanup EXIT

manifest="${work_dir}/manifest.yaml"
rendered_json="${work_dir}/rendered.json"
namespace_resource="${work_dir}/namespace.json"
runtime_config="${work_dir}/runtime-images-configmap.json"
"${kustomize}" build "${overlay_dir}" >"${manifest}"
"${kubectl}" create --dry-run=client --validate=false -f "${manifest}" -o json >"${rendered_json}"

jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "Namespace" and .metadata.name == "orka-system"))
  | if length == 1 then .[0] else error("expected exactly one orka-system Namespace") end
' "${rendered_json}" >"${namespace_resource}"
jq -sc '
  [.[] | if .kind == "List" then .items[] else . end]
  | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true"))
  | if length == 1 then .[0] else error("expected exactly one generated ACP runtime image ConfigMap") end
' "${rendered_json}" >"${runtime_config}"

# Establish the namespace and immutable, hash-named ConfigMap before applying
# the Deployment that references it. Every retry repeats these idempotent phases,
# so interruption after any apply still converges on the desired generation.
"${kubectl}" apply -f "${namespace_resource}"
"${kubectl}" apply -f "${runtime_config}"
"${kubectl}" apply -f "${manifest}"
