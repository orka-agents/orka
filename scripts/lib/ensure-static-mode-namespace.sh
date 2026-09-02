#!/usr/bin/env bash
set -Eeuo pipefail

usage() {
  echo "Usage: $0 KUBECTL NAMESPACE harness-v1|harness-v2" >&2
}

[[ $# -eq 3 ]] || {
  usage
  exit 2
}

kubectl_bin="$1"
namespace="$2"
controller_mode="$3"

command -v "${kubectl_bin}" >/dev/null 2>&1 || {
  echo "kubectl command not found: ${kubectl_bin}" >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  echo "required command not found: jq" >&2
  exit 1
}
[[ -n "${namespace}" ]] || {
  echo "namespace must be non-empty" >&2
  exit 2
}
case "${controller_mode}" in
  harness-v1|harness-v2) ;;
  *)
    echo "controller mode must be harness-v1 or harness-v2" >&2
    exit 2
    ;;
esac

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/ensure-static-mode-namespace.XXXXXX")"
cleanup() {
  rm -rf -- "${work_dir}"
}
trap cleanup EXIT

namespace_json="${work_dir}/namespace.json"
namespace_manifest="${work_dir}/namespace-manifest.json"

read_namespace() {
  : >"${namespace_json}"
  if ! "${kubectl_bin}" get namespace "${namespace}" --ignore-not-found -o json >"${namespace_json}"; then
    echo "unable to inspect namespace ${namespace}" >&2
    return 1
  fi
}

has_exact_identity() {
  jq -es --arg namespace "${namespace}" --arg mode "${controller_mode}" '
    length == 1 and
    .[0].apiVersion == "v1" and
    .[0].kind == "Namespace" and
    .[0].metadata.name == $namespace and
    .[0].metadata.labels["orka.ai/controller-mode"] == $mode
  ' "${namespace_json}" >/dev/null
}

identity_error() {
  echo "namespace ${namespace} must already claim orka.ai/controller-mode=${controller_mode}; unlabeled, malformed, or opposite-mode namespaces cannot be adopted in place" >&2
}

read_namespace || exit 1
if [[ -s "${namespace_json}" ]]; then
  has_exact_identity || {
    identity_error
    exit 1
  }
  exit 0
fi

jq -n --arg namespace "${namespace}" --arg mode "${controller_mode}" '{
  apiVersion: "v1",
  kind: "Namespace",
  metadata: {
    name: $namespace,
    labels: {"orka.ai/controller-mode": $mode}
  }
}' >"${namespace_manifest}"

if "${kubectl_bin}" create -f "${namespace_manifest}" >/dev/null 2>&1; then
  read_namespace || exit 1
  if [[ ! -s "${namespace_json}" ]] || ! has_exact_identity; then
    echo "namespace ${namespace} was created but its static controller identity could not be verified" >&2
    exit 1
  fi
  exit 0
fi

# A concurrent installer may have won the create race. Accept only the exact
# static identity it created; every other create failure remains fail-closed.
read_namespace || exit 1
if [[ -s "${namespace_json}" ]] && has_exact_identity; then
  exit 0
fi
if [[ -s "${namespace_json}" ]]; then
  identity_error
else
  echo "unable to create namespace ${namespace} with orka.ai/controller-mode=${controller_mode}" >&2
fi
exit 1
