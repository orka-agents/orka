#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
renderer="${root}/scripts/render-acp-runtime-images.sh"
kustomize="${KUSTOMIZE:-${root}/bin/kustomize}"
kubectl="${KUBECTL:-kubectl}"

for command in "${renderer}" "${kustomize}" "${kubectl}" jq; do
  command -v "${command}" >/dev/null 2>&1 || {
    echo "required command not found: ${command}" >&2
    exit 1
  }
done

test_root="$(mktemp -d "${TMPDIR:-/tmp}/acp-runtime-image-rollout-test.XXXXXX")"
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT
cp -R "${root}/config" "${test_root}/config"
overlay="${test_root}/config/acp-production"

codex_a="docker.io/example/acp-codex@sha256:$(printf 'a%.0s' {1..64})"
claude_a="docker.io/example/acp-claude@sha256:$(printf 'b%.0s' {1..64})"
copilot_a="docker.io/example/acp-copilot@sha256:$(printf 'c%.0s' {1..64})"
opencode_a="docker.io/example/acp-opencode@sha256:$(printf '0%.0s' {1..64})"
codex_b="registry--prod.example.com:5000/team/acp-codex@sha256:$(printf 'd%.0s' {1..64})"
claude_b="registry--prod.example.com:5000/team/acp-claude@sha256:$(printf 'e%.0s' {1..64})"
copilot_b="registry--prod.example.com:5000/team/acp-copilot@sha256:$(printf 'f%.0s' {1..64})"
opencode_b="registry--prod.example.com:5000/team/acp-opencode@sha256:$(printf '9%.0s' {1..64})"
ipv6_image="[2001:db8::1]:5000/team/acp-codex@sha256:$(printf 'a%.0s' {1..64})"
multiline_image="docker.io/example/acp-codex
#@sha256:$(printf 'b%.0s' {1..64})"

render_snapshot() {
  local codex_image="$1"
  local claude_image="$2"
  local copilot_image="$3"
  local opencode_image="$4"
  local output="$5"

  "${renderer}" "${overlay}" "${codex_image}" "${claude_image}" "${copilot_image}" "${opencode_image}"
  "${kustomize}" build "${overlay}" \
    | "${kubectl}" create --dry-run=client --validate=false -f - -o json \
    | jq -sc '
        [.[] | if .kind == "List" then .items[] else . end] as $items |
        ($items | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true")) | .[0]) as $config |
        ($items | map(select(.kind == "Deployment" and .metadata.name == "orka-controller-manager")) | .[0]) as $deployment |
        {
          configName: $config.metadata.name,
          config: $config.data,
          references: [
            $deployment.spec.template.spec.containers[]?.env[]? |
            select(.name == "ORKA_ACP_CODEX_RUNTIME_IMAGE" or .name == "ORKA_ACP_CLAUDE_RUNTIME_IMAGE" or .name == "ORKA_ACP_COPILOT_RUNTIME_IMAGE" or .name == "ORKA_ACP_OPENCODE_RUNTIME_IMAGE") |
            {name, configMapName: .valueFrom.configMapKeyRef.name, key: .valueFrom.configMapKeyRef.key}
          ] | sort_by(.name)
        }
      ' >"${output}"
}

assert_snapshot() {
  local snapshot="$1"
  local codex_image="$2"
  local claude_image="$3"
  local copilot_image="$4"
  local opencode_image="$5"

  jq -e --arg codex "${codex_image}" --arg claude "${claude_image}" --arg copilot "${copilot_image}" --arg opencode "${opencode_image}" '
    .configName as $configName |
    ($configName | test("^acp-runtime-images-[a-z0-9]+$")) and
    .config.ORKA_ACP_CODEX_RUNTIME_IMAGE == $codex and
    .config.ORKA_ACP_CLAUDE_RUNTIME_IMAGE == $claude and
    .config.ORKA_ACP_COPILOT_RUNTIME_IMAGE == $copilot and
    .config.ORKA_ACP_OPENCODE_RUNTIME_IMAGE == $opencode and
    (.references | length) == 4 and
    ([.references[].configMapName] | all(. == $configName)) and
    ([.references[].key] | sort) == ["ORKA_ACP_CLAUDE_RUNTIME_IMAGE", "ORKA_ACP_CODEX_RUNTIME_IMAGE", "ORKA_ACP_COPILOT_RUNTIME_IMAGE", "ORKA_ACP_OPENCODE_RUNTIME_IMAGE"]
  ' "${snapshot}" >/dev/null
}

first="${test_root}/first.json"
repeat="${test_root}/repeat.json"
copilot_changed="${test_root}/copilot-changed.json"
opencode_changed="${test_root}/opencode-changed.json"
changed="${test_root}/changed.json"

render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}" "${first}"
assert_snapshot "${first}" "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}"
render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_a}" "${repeat}"
cmp -s "${first}" "${repeat}"

render_snapshot "${codex_a}" "${claude_a}" "${copilot_b}" "${opencode_a}" "${copilot_changed}"
assert_snapshot "${copilot_changed}" "${codex_a}" "${claude_a}" "${copilot_b}" "${opencode_a}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${copilot_changed}")" ]]; then
  echo 'Copilot runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

render_snapshot "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_b}" "${opencode_changed}"
assert_snapshot "${opencode_changed}" "${codex_a}" "${claude_a}" "${copilot_a}" "${opencode_b}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${opencode_changed}")" ]]; then
  echo 'OpenCode runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

render_snapshot "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}" "${changed}"
assert_snapshot "${changed}" "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}"
if [[ "$(jq -r .configName "${first}")" == "$(jq -r .configName "${changed}")" ]]; then
  echo 'runtime image change did not create a new immutable ConfigMap generation' >&2
  exit 1
fi

[[ -f "${overlay}/runtime-images.env" ]]
[[ ! -e "${overlay}/runtime-images-configmap.yaml" ]]
[[ ! -e "${overlay}/runtime-images-rollout-patch.yaml" ]]
grep -F 'scripts/render-acp-runtime-images.sh' "${root}/Makefile" >/dev/null
grep -F 'docker-build-acp-copilot-runtime' "${root}/Makefile" >/dev/null
grep -F 'ACP_COPILOT_RUNTIME_IMG' "${root}/Makefile" >/dev/null
grep -F 'docker-build-acp-opencode-runtime' "${root}/Makefile" >/dev/null
grep -F 'ACP_OPENCODE_RUNTIME_IMG' "${root}/Makefile" >/dev/null
if grep -F 'rollout restart deployment/orka-controller-manager' "${root}/Makefile" >/dev/null; then
  echo 'deploy still relies on an imperative, non-retryable controller restart' >&2
  exit 1
fi

"${renderer}" "${overlay}" "${ipv6_image}" "${claude_b}" "${copilot_b}" "${opencode_b}"
"${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "${copilot_b}" "${opencode_b}"

for invalid_image in \
  'not-digest-pinned' \
  "https://registry.example.com/team/acp@sha256:$(printf '1%.0s' {1..64})" \
  "registry.example.com:notaport/team/acp@sha256:$(printf '2%.0s' {1..64})" \
  "[127.0.0.1]/team/acp@sha256:$(printf '3%.0s' {1..64})" \
  "${multiline_image}"; do
  if "${renderer}" "${overlay}" "${invalid_image}" "${claude_b}" "${copilot_b}" "${opencode_b}" >/dev/null 2>&1; then
    echo "renderer accepted invalid runtime image: ${invalid_image}" >&2
    exit 1
  fi
done

if "${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "not-digest-pinned" "${opencode_b}" >/dev/null 2>&1; then
  echo 'renderer accepted an invalid Copilot runtime image' >&2
  exit 1
fi

if "${renderer}" "${overlay}" "${codex_b}" "${claude_b}" "${copilot_b}" "not-digest-pinned" >/dev/null 2>&1; then
  echo 'renderer accepted an invalid OpenCode runtime image' >&2
  exit 1
fi

apply_script="${root}/scripts/apply-acp-production.sh"
real_kubectl="$(command -v "${kubectl}")"
fake_bin="${test_root}/fake-bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/kubectl" <<'EOF_FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$1" == "create" ]]; then
  exec "${REAL_KUBECTL}" "$@"
fi
[[ "$1" == "apply" && "$2" == "-f" && $# -eq 3 ]] || {
  echo "unexpected fake kubectl invocation: $*" >&2
  exit 2
}

payload="$("${REAL_KUBECTL}" create --dry-run=client --validate=false -f "$3" -o json)"
summary="$(printf '%s\n' "${payload}" | jq -sc '
  [.[] | if .kind == "List" then .items[] else . end] as $items |
  {
    namespace: (($items | map(select(.kind == "Namespace" and .metadata.name == "orka-system")) | .[0].metadata.name) // ""),
    runtimeConfig: (($items | map(select(.kind == "ConfigMap" and .metadata.labels["orka.ai/acp-runtime-images"] == "true")) | .[0].metadata.name) // ""),
    deploymentRef: (($items | map(select(.kind == "Deployment" and .metadata.name == "orka-controller-manager")) | .[0].spec.template.spec.containers[0].env[]? | select(.name == "ORKA_ACP_CODEX_RUNTIME_IMAGE") | .valueFrom.configMapKeyRef.name) // "")
  }
')"
namespace_name="$(jq -r .namespace <<<"${summary}")"
config_name="$(jq -r .runtimeConfig <<<"${summary}")"
deployment_ref="$(jq -r .deploymentRef <<<"${summary}")"
mkdir -p "${FAKE_KUBE_STATE}/configmaps"

if [[ -n "${namespace_name}" && -z "${config_name}" && -z "${deployment_ref}" ]]; then
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "namespace" && ! -e "${FAKE_KUBE_STATE}/failed-namespace" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-namespace"
    printf 'fail-namespace:%s\n' "${namespace_name}" >>"${FAKE_KUBE_LOG}"
    exit 18
  fi
  : >"${FAKE_KUBE_STATE}/namespace"
  printf 'namespace:%s\n' "${namespace_name}" >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ -z "${deployment_ref}" ]]; then
  [[ -n "${config_name}" ]] || { echo 'runtime ConfigMap apply was not identifiable' >&2; exit 1; }
  [[ -e "${FAKE_KUBE_STATE}/namespace" ]] || { echo 'runtime ConfigMap applied before namespace' >&2; exit 17; }
  if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "config" && ! -e "${FAKE_KUBE_STATE}/failed-config" ]]; then
    : >"${FAKE_KUBE_STATE}/failed-config"
    printf 'fail-config:%s\n' "${config_name}" >>"${FAKE_KUBE_LOG}"
    exit 19
  fi
  : >"${FAKE_KUBE_STATE}/configmaps/${config_name}"
  printf 'config:%s\n' "${config_name}" >>"${FAKE_KUBE_LOG}"
  exit 0
fi

if [[ "${FAKE_KUBE_FAIL_MODE:-}" == "full" && ! -e "${FAKE_KUBE_STATE}/failed-full" ]]; then
  : >"${FAKE_KUBE_STATE}/failed-full"
  printf 'fail-full:%s\n' "${deployment_ref}" >>"${FAKE_KUBE_LOG}"
  exit 20
fi
[[ -e "${FAKE_KUBE_STATE}/namespace" ]] || {
  echo 'Deployment applied before namespace' >&2
  exit 22
}
[[ -e "${FAKE_KUBE_STATE}/configmaps/${deployment_ref}" ]] || {
  echo "Deployment referenced missing ConfigMap ${deployment_ref}" >&2
  exit 21
}
printf '%s\n' "${deployment_ref}" >"${FAKE_KUBE_STATE}/deployment-ref"
printf 'full:%s\n' "${deployment_ref}" >>"${FAKE_KUBE_LOG}"
EOF_FAKE_KUBECTL
chmod +x "${fake_bin}/kubectl"

assert_converged() {
  local state_dir="$1"
  local reference
  reference="$(cat "${state_dir}/deployment-ref")"
  [[ -e "${state_dir}/namespace" ]]
  [[ -e "${state_dir}/configmaps/${reference}" ]]
}

run_apply_scenario() {
  local mode="$1"
  local state_dir="${test_root}/state-${mode:-success}"
  local log_file="${state_dir}/apply.log"
  mkdir -p "${state_dir}"
  : >"${log_file}"

  if [[ -n "${mode}" ]]; then
    if REAL_KUBECTL="${real_kubectl}" FAKE_KUBE_STATE="${state_dir}" FAKE_KUBE_LOG="${log_file}" FAKE_KUBE_FAIL_MODE="${mode}" \
      "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null 2>&1; then
      echo "expected injected ${mode} apply failure" >&2
      exit 1
    fi
  fi
  REAL_KUBECTL="${real_kubectl}" FAKE_KUBE_STATE="${state_dir}" FAKE_KUBE_LOG="${log_file}" FAKE_KUBE_FAIL_MODE="" \
    "${apply_script}" "${overlay}" "${kustomize}" "${fake_bin}/kubectl" >/dev/null
  assert_converged "${state_dir}"
}

run_apply_scenario ""
run_apply_scenario namespace
run_apply_scenario config
run_apply_scenario full

grep -F 'scripts/apply-acp-production.sh' "${root}/Makefile" >/dev/null

printf '%s\n' 'ok - ACP runtime image deployment uses one atomic env generation, validates registry syntax, rewrites the controller to an immutable hash-named ConfigMap, and converges after namespace, ConfigMap, or full apply is interrupted'
