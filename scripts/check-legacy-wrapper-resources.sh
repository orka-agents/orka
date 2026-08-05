#!/usr/bin/env bash
set -Eeuo pipefail

kubectl_bin="${KUBECTL:-kubectl}"
command -v "${kubectl_bin}" >/dev/null 2>&1 || {
  echo "required command not found: ${kubectl_bin}" >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  echo "required command not found: jq" >&2
  exit 1
}

inventory="$(${kubectl_bin} get deployments.apps,services,secrets -A -o json)"
legacy="$(jq -r '
  .items[]?
  | select(
      .metadata.labels["app.kubernetes.io/component"] == "agent-harness-wrapper"
      or ((.kind == "Deployment" or .kind == "Service") and (.metadata.name == "agent-harness-wrapper" or (.metadata.name | endswith("-agent-harness-wrapper"))))
      or (.kind == "Secret" and (.metadata.name == "harness-wrapper-auth" or (.metadata.name | endswith("-harness-wrapper-auth"))))
    )
  | [.kind, .metadata.namespace, .metadata.name] | @tsv
' <<<"${inventory}")"
if [[ -n "${legacy}" ]]; then
  echo "legacy harness-wrapper resources remain:" >&2
  printf '%s\n' "${legacy}" >&2
  exit 1
fi
