#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/e2e-common.sh
. "${script_dir}/lib/e2e-common.sh"
release_tag="${1:-}"
release_namespace="${RELEASE_NAMESPACE:-orka-system}"

chart_field() {
  local path="$1"
  local field="$2"
  local value
  value="$(awk -v key="${field}:" '$1 == key { print $2; exit }' "${path}")"
  value="${value#\"}"
  value="${value%\"}"
  value="${value#\'}"
  value="${value%\'}"
  [[ -n "${value}" ]] || die "could not read ${field} from ${path}"
  printf '%s\n' "${value}"
}

image_tag() {
  local values_path="$1"
  local repository="$2"
  awk -v repository="${repository}" '
    $1 == "repository:" && $2 == repository {
      repositories++
      pending = 1
      next
    }
    pending && $1 == "tag:" {
      value = $2
      gsub(/^["'"'"']|["'"'"']$/, "", value)
      print value
      tags++
      pending = 0
    }
    END {
      if (repositories != 1 || tags != 1) {
        exit 1
      }
    }
  ' "${values_path}"
}

for command in awk diff find grep helm sed sort wc; do
  require_cmd "${command}"
done

cd "${repo_root}"
if [[ -z "${release_tag}" ]]; then
  release_tag="$(awk '$1 == "VERSION" && $2 == ":=" { print $3; exit }' Makefile)"
fi
[[ "${release_tag}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-(beta|rc)\.[0-9]+)?$ ]] || \
  die "release tag must match vX.Y.Z, vX.Y.Z-beta.N, or vX.Y.Z-rc.N; got ${release_tag}"

expected_version="${release_tag#v}"
static_chart="cmd/build/helmify/static"
staging_chart="manifest_staging/charts/orka"
promoted_chart="charts/orka"

diff --no-dereference --recursive --unified "${staging_chart}" "${promoted_chart}"
diff --no-dereference --recursive --unified manifest_staging/deploy deploy

for chart in "${static_chart}" "${staging_chart}" "${promoted_chart}"; do
  [[ "$(chart_field "${chart}/Chart.yaml" version)" == "${expected_version}" ]] || \
    die "${chart}/Chart.yaml version must be ${expected_version}"
  [[ "$(chart_field "${chart}/Chart.yaml" appVersion)" == "${release_tag}" ]] || \
    die "${chart}/Chart.yaml appVersion must be ${release_tag}"
done

values_path="${promoted_chart}/values.yaml"
for repository in \
  ghcr.io/orka-agents/orka \
  ghcr.io/orka-agents/orka/workspace-publisher \
  ghcr.io/orka-agents/orka/agent-harness-wrapper \
  ghcr.io/orka-agents/orka/ai-worker \
  ghcr.io/orka-agents/orka/general-worker; do
  tag="$(image_tag "${values_path}" "${repository}")" || \
    die "expected exactly one tag for ${repository} in ${values_path}"
  [[ "${tag}" == "${expected_version}" ]] || \
    die "${repository} tag must be ${expected_version}; got ${tag:-<empty>}"
done

validation_digest="sha256:$(printf '0%.0s' {1..64})"
render_args=(
  --namespace "${release_namespace}"
  --set-string controller.mode=harness-v2
  --set-string "controller.watchNamespace=${release_namespace}"
  --set-string "controller.image.digest=${validation_digest}"
  --set-string controller.agentExecutionSnapshot.existingSecret=release-validation-snapshot
  --set-string controller.agentExecutionSnapshot.key=key
  --set-string webhooks.tls.existingSecret=release-validation-webhook-tls
  --set-string webhooks.caBundle=Y2E=
  --set-string "publisher.image.digest=${validation_digest}"
  --set providerProxy.enabled=true
)

for chart in "${static_chart}" "${staging_chart}" "${promoted_chart}"; do
  helm lint "${chart}" "${render_args[@]}"
done

temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/orka-release-manifest.XXXXXX")"
cleanup() {
  rm -rf -- "${temp_dir}"
}
trap cleanup EXIT

rendered_chart="${temp_dir}/rendered.yaml"
rendered_images="${temp_dir}/images.txt"
helm template release-validation "${promoted_chart}" "${render_args[@]}" >"${rendered_chart}"

grep -E '^[[:space:]]+image:[[:space:]]+' "${rendered_chart}" |
  sed -E 's/^[[:space:]]+image:[[:space:]]+"?([^"[:space:]]+)"?.*$/\1/' |
  sort -u >"${rendered_images}"

image_count="$(wc -l <"${rendered_images}" | tr -d ' ')"
[[ "${image_count}" == "2" ]] || \
  die "expected exactly two unique digest-pinned controller/publisher images; found ${image_count}"
grep -Fxq "ghcr.io/orka-agents/orka@${validation_digest}" "${rendered_images}" || \
  die "rendered controller image is not digest-pinned"
grep -Fxq "ghcr.io/orka-agents/orka/workspace-publisher@${validation_digest}" "${rendered_images}" || \
  die "rendered publisher image is not digest-pinned"

for worker in ai general; do
  expected_ref="ghcr.io/orka-agents/orka/${worker}-worker:${expected_version}"
  count="$(grep -Fc -- "--${worker}-worker-image=${expected_ref}" "${rendered_chart}" || true)"
  [[ "${count}" == "1" ]] || \
    die "expected exactly one rendered ${worker} worker image using ${expected_version}; found ${count}"
done

if grep -Fq 'agent-harness-wrapper' "${rendered_chart}"; then
  die "harness-v2 release render unexpectedly contains the harness-v1 wrapper"
fi

expected_crds="$(find config/crd/bases -maxdepth 1 -type f -name '*.yaml' | wc -l | tr -d ' ')"
rendered_crds="$(helm show crds "${promoted_chart}" | grep -c '^kind: CustomResourceDefinition$')"
[[ "${rendered_crds}" == "${expected_crds}" ]] || \
  die "promoted chart CRD count ${rendered_crds} does not match config/crd/bases ${expected_crds}"

printf 'Validated harness-v2 release manifest contract for %s.\n' "${release_tag}"
