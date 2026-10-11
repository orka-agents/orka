#!/usr/bin/env bash
# shellcheck disable=SC2016 # This test extracts literal shell expressions.
# shellcheck disable=SC2030,SC2031 # Test cases deliberately isolate environment changes in subshells.
# shellcheck disable=SC2154,SC2329 # Evaluated source invokes these mocks and assigns checksum state.
set -Eeuo pipefail

if [[ "${BASH_VERSINFO[0]}" -lt 4 ]]; then
  echo "error: this test suite requires bash >= 4; found ${BASH_VERSION}" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/scripts/agent-runtime-e2e.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/agent-runtime-toolbox-shell.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT
bash_bin="$(command -v bash)"
shasum_bin="$(command -v shasum)"

assert_equal() {
  [[ "$1" == "$2" ]] || {
    printf 'expected: %s\nactual: %s\n' "$2" "$1" >&2
    return 1
  }
}

# Source-only bootstrap libraries do not perform cluster operations. Mock the
# kubectl executable used by live_acp_kind_run and check both commands exactly.
(
  # shellcheck source=scripts/lib/agent-runtime-kind-bootstrap.sh
  . "${root}/scripts/lib/agent-runtime-kind-bootstrap.sh"
  kubectl() { printf '%s\n' "$*" >>"${test_root}/kubectl.calls"; }
  export ACP_E2E_TOOLBOX_REGISTRY=registry.example:5000 LIVE_ACP_ROLLOUT_TIMEOUT=90s
  unset ACP_E2E_TOOLBOX_MOUNT_METHOD ORKA_NAMESPACE ORKA_CONTROLLER_DEPLOYMENT

  check_enablement() {
    local expected_namespace="$1" expected_deployment="$2" expected_mount="$3"
    : >"${test_root}/kubectl.calls"
    live_acp_kind_enable_toolboxes
    local -a calls
    mapfile -t calls <"${test_root}/kubectl.calls"
    assert_equal "${#calls[@]}" 2
    assert_equal "${calls[0]}" "-n ${expected_namespace} set env deployment/${expected_deployment} ORKA_ACP_TOOLBOXES_ENABLED=true ORKA_ACP_TOOLBOX_ALLOWED_REGISTRIES=registry.example:5000 ORKA_ACP_TOOLBOX_MOUNT_METHOD=${expected_mount}"
    assert_equal "${calls[1]}" "-n ${expected_namespace} rollout status deployment/${expected_deployment} --timeout=90s"
  }

  check_enablement orka-system orka-controller-manager copy
  export ORKA_CONTROLLER_DEPLOYMENT=orka-controller
  check_enablement orka-system orka-controller copy
  export ORKA_NAMESPACE=custom-system ACP_E2E_TOOLBOX_MOUNT_METHOD=imageVolume
  check_enablement custom-system orka-controller imageVolume
  export ORKA_NAMESPACE='' ORKA_CONTROLLER_DEPLOYMENT=''
  check_enablement orka-system orka-controller-manager imageVolume
  unset ACP_E2E_TOOLBOX_REGISTRY
  : >"${test_root}/kubectl.calls"
  live_acp_kind_enable_toolboxes
  [[ ! -s "${test_root}/kubectl.calls" ]]
)
printf '%s\n' 'ok - toolbox enablement and rollout use the selected Helm or default controller and namespace'

# Use the existing function-extraction seam, not a rewritten checksum formula.
checksum_preflight="$(awk '
  /^toolbox_checksum_command=\(\)$/ {copy=1}
  copy && /^release_gate=0$/ {exit}
  copy {print}
' "${script}")"
[[ -n "${checksum_preflight}" ]]
for function in sanitize_name run_toolbox_check; do
  body="$(awk -v name="${function}" '
    $0 == name "() {" {copy=1}
    copy {print}
    copy && /^}$/ {exit}
  ' "${script}")"
  [[ -n "${body}" ]]
  eval "${body}"
done
# shellcheck source=scripts/lib/e2e-common.sh
. "${root}/scripts/lib/e2e-common.sh"

# Restrict PATH so no host sha256sum, Docker, or kubectl can be discovered.
# shasum uses its native absolute Perl interpreter on macOS and Linux.
portable_bin="${test_root}/portable-bin"
mkdir -p "${portable_bin}"
for command in awk cut date dirname grep jq mktemp sed sort tr; do
  ln -s "$(command -v "${command}")" "${portable_bin}/${command}"
done
ln -s "${shasum_bin}" "${portable_bin}/shasum"
for command in kubectl curl; do
  printf '#!%s\nprintf "%%s\n" "%s" >>"$MOCK_LIVE_CALLS"\nexit 90\n' \
    "${bash_bin}" "${command}" >"${portable_bin}/${command}"
  chmod +x "${portable_bin}/${command}"
done

(
  export PATH="${portable_bin}" ACP_E2E_TOOLBOX_MOUNT_METHOD=imageVolume
  ACP_E2E_TOOLBOX_IMAGE="registry.example:5000/toolbox@sha256:$(printf '%064d' 0)"
  export ACP_E2E_TOOLBOX_IMAGE
  unset ACP_E2E_TOOLBOX_WRONG_ARCH_IMAGE ACP_E2E_TOOLBOX_FIFO_IMAGE
  unset ACP_E2E_TOOLBOX_MISSING_IMAGE ACP_E2E_TOOLBOX_HOSTILE_IMAGE
  if command -v sha256sum >/dev/null 2>&1; then
    echo 'portable PATH unexpectedly contains sha256sum' >&2
    exit 1
  fi
  eval "${checksum_preflight}"
  assert_equal "${toolbox_checksum_command[*]}" 'shasum -a 256'

  # Mock all Task/API operations while running the complete toolbox check.
  # shellcheck disable=SC2034 # Used by the evaluated run_toolbox_check function.
  run_id=toolbox-checksum-test
  apply_toolbox_agent() { printf '%s\n' "$3" >>"${test_root}/toolbox-images"; }
  apply_read_task() { :; }
  wait_task_terminal() { :; }
  task_phase_is() {
    case "$1:$2" in
      acp-codex-toolbox-run-*:Succeeded|acp-codex-toolbox-unknown-run-*:Failed) return 0 ;;
      *) return 1 ;;
    esac
  }
  api_task_result() {
    printf '%s\n' 'yq (https://github.com/mikefarah/yq/) version v4.54.1' \
      'NoNewPrivs: 1' 'CapEff: 0000000000000000'
  }
  task_json() {
    printf '%s\n' '{"status":{"execution":{"reason":"ToolboxUnavailable","message":"TOOLBOX_IMAGE_PULL"}}}'
  }
  k() { echo 'unexpected Kubernetes operation in checksum regression' >&2; return 1; }
  run_toolbox_check mock-model "${ACP_E2E_TOOLBOX_IMAGE}"
  mapfile -t images <"${test_root}/toolbox-images"
  assert_equal "${#images[@]}" 2
  assert_equal "${images[0]}" "${ACP_E2E_TOOLBOX_IMAGE}"
  # SHA-256 of the literal no-newline input orka-toolbox-unknown-digest-toolbox-checksum-test.
  assert_equal "${images[1]}" 'registry.example:5000/toolbox@sha256:147073788b0f333909b2193112de5d970beab147655376c8f9053da888c833b4'
)
printf '%s\n' 'ok - the toolbox unknown-image case uses exact SHA-256 bytes with shasum and no GNU sha256sum'

# Both commands present: GNU sha256sum is preferred and receives no shasum flags.
cat >"${portable_bin}/sha256sum" <<STUB
#!${bash_bin}
[[ \$# -eq 0 ]] || exit 91
exec "${shasum_bin}" -a 256
STUB
chmod +x "${portable_bin}/sha256sum"
(
  export PATH="${portable_bin}" ACP_E2E_TOOLBOX_IMAGE=enabled
  eval "${checksum_preflight}"
  assert_equal "${toolbox_checksum_command[*]}" sha256sum
  digest="$(printf 'orka-toolbox-unknown-digest-%s' toolbox-checksum-test | "${toolbox_checksum_command[@]}" | cut -c1-64)"
  assert_equal "${digest}" 147073788b0f333909b2193112de5d970beab147655376c8f9053da888c833b4
)
printf '%s\n' 'ok - checksum preflight prefers sha256sum when both commands are available'

rm "${portable_bin}/sha256sum" "${portable_bin}/shasum"
(
  export PATH="${portable_bin}"
  unset ACP_E2E_TOOLBOX_IMAGE
  eval "${checksum_preflight}"
  assert_equal "${#toolbox_checksum_command[@]}" 0
)
# Execute the real validator startup, not just the extracted selection block.
# Missing hashing tools must fail before any mocked cluster or HTTP operation.
export MOCK_LIVE_CALLS="${test_root}/live.calls"
: >"${MOCK_LIVE_CALLS}"
rc=0
PATH="${portable_bin}" ACP_E2E_TOOLBOX_IMAGE=enabled \
  "${bash_bin}" "${script}" --context mock-context >"${test_root}/missing-checksum.out" 2>&1 || rc=$?
assert_equal "${rc}" 1
grep -Fx 'error: toolbox checks require sha256sum or shasum' "${test_root}/missing-checksum.out" >/dev/null
[[ ! -s "${MOCK_LIVE_CALLS}" ]]
printf '%s\n' 'ok - missing checksum tools fail during preflight only when toolbox checks are enabled'
