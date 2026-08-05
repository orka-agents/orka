#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
kustomize="${KUSTOMIZE:-${root}/bin/kustomize}"
kubectl="${KUBECTL:-kubectl}"
helm="${HELM:-helm}"

for command in "${kustomize}" "${kubectl}" "${helm}" jq; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

assert_secret_mounts() {
  local label="$1"
  jq -se --arg label "${label}" '
    ([.[] | if .kind == "List" then .items[] else . end]) as $items |
    ($items | map(select(.kind == "Deployment" and .metadata.name == "orka-scm-egress-proxy"))) as $scm |
    ($items | map(select(.kind == "Deployment" and .metadata.name == "orka-workspace-publisher"))) as $publisher |
    ($scm | length) == 1 and
    ($publisher | length) == 1 and
    ($scm[0] |
      any(.spec.template.spec.containers[]?.volumeMounts[]?;
        .name == "auth" and
        .mountPath == "/var/run/secrets/orka/scm-egress/token" and
        .subPath == "token" and
        .readOnly == true
      ) and
      any(.spec.template.spec.volumes[]?;
        .name == "auth" and .secret.defaultMode == 288
      )
    ) and
    ($publisher[0] |
      any(.spec.template.spec.containers[]?.volumeMounts[]?;
        .name == "publisher-auth" and
        .mountPath == "/var/run/orka/publisher-auth/controller-token" and
        .subPath == "controller-token" and
        .readOnly == true
      ) and
      any(.spec.template.spec.containers[]?.volumeMounts[]?;
        .name == "publisher-auth" and
        .mountPath == "/var/run/orka/publisher-auth/operation-capability-secret" and
        .subPath == "operation-capability-secret" and
        .readOnly == true
      ) and
      any(.spec.template.spec.volumes[]?;
        .name == "publisher-auth"
        and .secret.defaultMode == 288
        and ((.secret.items // []) | map({key, path}) | sort_by(.path)) == [
          {key:"controller-token", path:"controller-token"},
          {key:"operation-capability-secret", path:"operation-capability-secret"}
        ]
      ) and
      any(.spec.template.spec.containers[]?.env[]?;
        .name == "ORKA_PUBLISHER_TEMP_ROOT" and
        .value == "/tmp/orka-workspace-publisher/runtime"
      )
    )
  ' >/dev/null || {
    echo "${label} does not expose regular-file Secret mounts and a process-owned temporary root" >&2
    return 1
  }
}

"${kustomize}" build "${root}/config/acp-production" \
  | "${kubectl}" create --dry-run=client --validate=false -f - -o json \
  | assert_secret_mounts "Kustomize production overlay"

"${helm}" template orka "${root}/charts/orka" \
  --set publisher.enabled=true \
  --set scmEgressProxy.enabled=true \
  --set controller.image.repository=docker.io/sozercan/orka \
  --set controller.image.digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
  --set publisher.image.repository=docker.io/sozercan/orka-workspace-publisher \
  --set publisher.image.digest=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb \
  | "${kubectl}" create --dry-run=client --validate=false -f - -o json \
  | assert_secret_mounts "Helm chart"


printf '%s\n' 'ok - ACP Secret mounts are regular-file compatible in Kustomize and Helm renders'
