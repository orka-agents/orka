#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
test_tmp_root="${TMPDIR:-/tmp}"
test_root="$(mktemp -d "${test_tmp_root%/}/live-github-e2e-kind-scope-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local path="$1"
  local expected="$2"
  if ! grep -F -- "${expected}" "${path}" >/dev/null 2>&1; then
    printf '%s\n' "--- ${path} ---" >&2
    cat "${path}" >&2 || true
    fail "expected ${path} to contain: ${expected}"
  fi
}

assert_not_contains() {
  local path="$1"
  local unexpected="$2"
  if grep -F -- "${unexpected}" "${path}" >/dev/null 2>&1; then
    printf '%s\n' "--- ${path} ---" >&2
    cat "${path}" >&2 || true
    fail "expected ${path} not to contain: ${unexpected}"
  fi
}

assert_cluster_exists() {
  local name="$1"
  grep -Fx -- "${name}" "${fake_kind_clusters}" >/dev/null 2>&1 ||
    fail "expected fake Kind cluster '${name}' to exist"
}

assert_cluster_absent() {
  local name="$1"
  if grep -Fx -- "${name}" "${fake_kind_clusters}" >/dev/null 2>&1; then
    fail "expected fake Kind cluster '${name}' to be absent"
  fi
}

assert_no_decoy_mutations() {
  [[ ! -s "${decoy_mutations}" ]] || {
    cat "${decoy_mutations}" >&2
    fail "ambient decoy received Kubernetes mutations"
  }
  [[ "$(cat "${ambient_kubeconfig}")" == "${ambient_before}" ]] ||
    fail "ambient kubeconfig changed"
  assert_cluster_exists decoy
}

assert_all_kubectl_calls_scoped() {
  local target_context="kind-${kind_cluster}"
  local line
  local count=0

  while IFS= read -r line; do
    [[ "${line}" == kubectl:* ]] || continue
    count=$((count + 1))
    [[ "${line}" == *" context=${target_context} cluster=${target_context} "* ]] ||
      fail "kubectl call did not target ${target_context}: ${line}"
    if [[ "${line}" != *" kubeconfig=${state_dir}/"* &&
      "${line}" != *" kubeconfig=${runner_temp}/orka-e2e-kubeconfig."* ]]; then
      fail "kubectl call escaped helper-owned kubeconfig paths: ${line}"
    fi
    [[ "${line}" != *" kubeconfig=${ambient_kubeconfig} "* ]] ||
      fail "kubectl call used ambient kubeconfig: ${line}"
  done <"${call_log}"

  [[ "${count}" -gt 0 ]] || fail "expected at least one kubectl call"
  assert_not_contains "${call_log}" "config use-context"
}

write_fake_commands() {
  mkdir -p "${test_root}/bin"

  cat >"${test_root}/bin/kind" <<'FAKE_KIND'
#!/usr/bin/env bash
set -euo pipefail

log() {
  printf 'kind:args=%s kubeconfig=%s\n' "$*" "${KUBECONFIG:-}" >>"${FAKE_CALL_LOG}"
}

exact_exists() {
  grep -Fx -- "$1" "${FAKE_KIND_CLUSTERS}" >/dev/null 2>&1
}

remove_exact() {
  local name="$1"
  local next="${FAKE_KIND_CLUSTERS}.next"
  awk -v target="${name}" '$0 != target' "${FAKE_KIND_CLUSTERS}" >"${next}"
  mv "${next}" "${FAKE_KIND_CLUSTERS}"
}

write_kubeconfig() {
  local name="$1"
  printf 'context=kind-%s\ncluster=kind-%s\nname=%s\nserver=https://identity-%s\nca=ca-identity-%s\n' \
    "${name}" "${name}" "${name}" "${name}" "${name}"
}

command="${1:-}"
shift || true
case "${command}" in
  get)
    subcommand="${1:-}"
    shift || true
    case "${subcommand}" in
      clusters)
        log "get clusters"
        cat "${FAKE_KIND_CLUSTERS}"
        ;;
      kubeconfig)
        name=""
        while [[ "$#" -gt 0 ]]; do
          case "$1" in
            --name)
              name="$2"
              shift 2
              ;;
            *) exit 71 ;;
          esac
        done
        log "get kubeconfig ${name}"
        exact_exists "${name}" || exit 72
        [[ "${FAKE_KIND_GET_KUBECONFIG_FAIL:-0}" != "1" ]] || exit 73
        write_kubeconfig "${name}"
        ;;
      *) exit 74 ;;
    esac
    ;;
  create)
    [[ "${1:-}" == "cluster" ]] || exit 75
    shift
    name=""
    kubeconfig=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --name)
          name="$2"
          shift 2
          ;;
        --config)
          shift 2
          ;;
        --kubeconfig)
          kubeconfig="$2"
          shift 2
          ;;
        *) exit 76 ;;
      esac
    done
    log "create ${name}"
    [[ -n "${name}" && -n "${kubeconfig}" ]] || exit 77
    exact_exists "${name}" && exit 78
    printf '%s\n' "${name}" >>"${FAKE_KIND_CLUSTERS}"
    write_kubeconfig "${name}" >"${kubeconfig}"
    ;;
  delete)
    [[ "${1:-}" == "cluster" ]] || exit 79
    shift
    name=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --name)
          name="$2"
          shift 2
          ;;
        *) exit 80 ;;
      esac
    done
    log "delete ${name}"
    exact_exists "${name}" || exit 81
    remove_exact "${name}"
    ;;
  load)
    log "load $*"
    ;;
  *) exit 82 ;;
esac
FAKE_KIND

  cat >"${test_root}/bin/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

kubeconfig="${KUBECONFIG:-}"
if [[ "${1:-}" == "--kubeconfig" ]]; then
  kubeconfig="$2"
  shift 2
fi

context="missing"
cluster="missing"
if [[ -n "${kubeconfig}" && -f "${kubeconfig}" ]]; then
  context="$(sed -n 's/^context=//p' "${kubeconfig}")"
  cluster="$(sed -n 's/^cluster=//p' "${kubeconfig}")"
fi
printf 'kubectl:args=%s context=%s cluster=%s kubeconfig=%s \n' \
  "$*" "${context}" "${cluster}" "${kubeconfig}" >>"${FAKE_CALL_LOG}"
[[ "${context}" != "missing" && "${cluster}" != "missing" ]] || exit 83

case " $* " in
  *" delete "*|*" apply "*|*" create "*|*" patch "*|*" set env "*)
    if [[ "${context}" == "kind-decoy" ]]; then
      printf 'decoy-mutation:%s\n' "$*" >>"${FAKE_DECOY_MUTATIONS}"
    fi
    ;;
esac

if [[ "${1:-}" == "config" && "${2:-}" == "current-context" ]]; then
  printf '%s\n' "${context}"
  exit 0
fi
if [[ "${1:-}" == "config" && "${2:-}" == "view" && "$*" == *".clusters[0].cluster.server"* ]]; then
  server="$(sed -n 's/^server=//p' "${kubeconfig}")"
  ca="$(sed -n 's/^ca=//p' "${kubeconfig}")"
  printf '%s\n%s\n' "${server}" "${ca}"
  exit 0
fi
if [[ "${1:-}" == "config" && "${2:-}" == "view" ]]; then
  printf '%s\n' "${cluster}"
  exit 0
fi
FAKE_KUBECTL

  cat >"${test_root}/bin/check-e2e-kind-scope" <<'FAKE_SCOPE'
#!/usr/bin/env bash
set -euo pipefail

expected="kind-${KIND_CLUSTER}"
lock_dir="${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation"
[[ "${E2E_KIND_TARGET_READY:-}" == "1" ]]
[[ "${E2E_KIND_EXPECTED_CONTEXT:-}" == "${expected}" ]]
[[ "${E2E_KIND_EXPECTED_CLUSTER:-}" == "${expected}" ]]
[[ -n "${KUBECONFIG:-}" && "${KUBECONFIG}" != "${AMBIENT_KUBECONFIG}" ]]
[[ "${KUBECONFIG}" == "${E2E_CLUSTER_STATE_DIR}/target.kubeconfig" ]]
[[ -d "${lock_dir}" ]]
[[ "$(<"${lock_dir}/token")" == "${E2E_KIND_OPERATION_TOKEN}" ]]
[[ "$(<"${lock_dir}/state_dir")" == "${E2E_CLUSTER_STATE_DIR}" ]]
printf 'ok\n'
FAKE_SCOPE

  cat >"${test_root}/bin/make" <<'FAKE_MAKE'
#!/usr/bin/env bash
set -euo pipefail
scope="$(check-e2e-kind-scope)"
printf 'body:make args=%s scope=%s ready=%s kubeconfig=%s\n' \
  "$*" "${scope}" "${E2E_KIND_TARGET_READY:-}" "${KUBECONFIG:-}" >>"${FAKE_BODY_LOG}"
exit "${FAKE_MAKE_EXIT:-0}"
FAKE_MAKE

  cat >"${test_root}/bin/go" <<'FAKE_GO'
#!/usr/bin/env bash
set -euo pipefail
scope="$(check-e2e-kind-scope)"
printf 'body:go args=%s scope=%s ready=%s kubeconfig=%s\n' \
  "$*" "${scope}" "${E2E_KIND_TARGET_READY:-}" "${KUBECONFIG:-}" >>"${FAKE_BODY_LOG}"
exit "${FAKE_GO_EXIT:-0}"
FAKE_GO

  cat >"${test_root}/bin/python3" <<'FAKE_PYTHON'
#!/usr/bin/env bash
cat >/dev/null
printf '%064d\n' 0
FAKE_PYTHON

  local command
  for command in docker curl jq; do
    cat >"${test_root}/bin/${command}" <<'FAKE_COMMAND'
#!/usr/bin/env bash
exit 0
FAKE_COMMAND
  done

  chmod +x "${test_root}/bin/"*
}

begin_case() {
  local name="$1"
  kind_cluster="$2"
  case_dir="${test_root}/${name}"
  state_dir="${case_dir}/state"
  lock_root="${case_dir}/locks"
  ambient_kubeconfig="${case_dir}/ambient.kubeconfig"
  fake_kind_clusters="${case_dir}/clusters"
  call_log="${case_dir}/calls.log"
  body_log="${case_dir}/body.log"
  decoy_mutations="${case_dir}/decoy-mutations.log"
  runner_temp="${case_dir}/runner-temp"

  mkdir -p "${case_dir}" "${runner_temp}"
  printf 'decoy\n' >"${fake_kind_clusters}"
  : >"${call_log}"
  : >"${body_log}"
  : >"${decoy_mutations}"
  printf 'context=kind-decoy\ncluster=kind-decoy\nname=decoy\nserver=https://identity-decoy\nca=ca-identity-decoy\n' >"${ambient_kubeconfig}"
  ambient_before="$(cat "${ambient_kubeconfig}")"
}

run_live_script() {
  local script="$1"
  local get_kubeconfig_fail="$2"
  local make_exit="$3"
  local go_exit="$4"
  local oidc_token="$5"
  local target_number="$6"

  env \
    PATH="${test_root}/bin:${PATH}" \
    KIND="${test_root}/bin/kind" \
    KUBECTL="${test_root}/bin/kubectl" \
    KIND_CLUSTER="${kind_cluster}" \
    KEEP_CLUSTER=0 \
    KUBECONFIG="${ambient_kubeconfig}" \
    E2E_KIND_CONFIG="${repo_root}/test/e2e/kind-config.yaml" \
    E2E_CLUSTER_STATE_DIR="${state_dir}" \
    E2E_CLUSTER_LOCK_ROOT="${lock_root}" \
    RUNNER_TEMP="${runner_temp}" \
    ORKA_GITHUB_OIDC_TOKEN="${oidc_token}" \
    ACTIONS_ID_TOKEN_REQUEST_TOKEN= \
    ACTIONS_ID_TOKEN_REQUEST_URL= \
    GITHUB_LABEL_TRIGGER_TARGET_NUMBER="${target_number}" \
    FAKE_KIND_CLUSTERS="${fake_kind_clusters}" \
    FAKE_KIND_GET_KUBECONFIG_FAIL="${get_kubeconfig_fail}" \
    FAKE_CALL_LOG="${call_log}" \
    FAKE_BODY_LOG="${body_log}" \
    FAKE_DECOY_MUTATIONS="${decoy_mutations}" \
    FAKE_MAKE_EXIT="${make_exit}" \
    FAKE_GO_EXIT="${go_exit}" \
    AMBIENT_KUBECONFIG="${ambient_kubeconfig}" \
    "${script}"
}

test_label_preflight_failure_has_no_cluster_side_effects() {
  begin_case label-preflight-failure label-preflight-failure

  if run_live_script "${script_dir}/live-github-label-trigger-e2e.sh" 0 0 0 test-oidc-token 0 >/dev/null 2>&1; then
    fail "invalid label target number unexpectedly passed preflight"
  fi

  [[ ! -s "${call_log}" ]] || fail "label preflight failure invoked Kind or kubectl"
  [[ ! -s "${body_log}" ]] || fail "label preflight failure entered the E2E body"
  assert_no_decoy_mutations
  assert_cluster_absent "${kind_cluster}"
}

test_oidc_preflight_failure_has_no_cluster_side_effects() {
  begin_case oidc-preflight-failure oidc-preflight-failure

  if run_live_script "${script_dir}/live-github-oidc-e2e.sh" 0 0 0 "" 1 >/dev/null 2>&1; then
    fail "missing OIDC token source unexpectedly passed preflight"
  fi

  [[ ! -s "${call_log}" ]] || fail "OIDC preflight failure invoked Kind or kubectl"
  [[ ! -s "${body_log}" ]] || fail "OIDC preflight failure entered the E2E body"
  assert_no_decoy_mutations
  assert_cluster_absent "${kind_cluster}"
}

test_setup_failure_never_arms_label_cleanup() {
  begin_case label-setup-failure label-setup-failure

  if run_live_script "${script_dir}/live-github-label-trigger-e2e.sh" 1 0 0 test-oidc-token 1 >/dev/null 2>&1; then
    fail "label trigger setup failure unexpectedly succeeded"
  fi

  [[ ! -s "${body_log}" ]] || fail "label E2E body ran before helper setup completed"
  assert_no_decoy_mutations
  assert_all_kubectl_calls_scoped
  assert_cluster_exists "${kind_cluster}"
}

test_setup_failure_never_arms_oidc_cleanup() {
  begin_case oidc-setup-failure oidc-setup-failure

  if run_live_script "${script_dir}/live-github-oidc-e2e.sh" 1 0 0 test-oidc-token 1 >/dev/null 2>&1; then
    fail "OIDC setup failure unexpectedly succeeded"
  fi

  [[ ! -s "${body_log}" ]] || fail "OIDC E2E body ran before helper setup completed"
  assert_no_decoy_mutations
  assert_all_kubectl_calls_scoped
  assert_cluster_exists "${kind_cluster}"
}

test_label_body_and_cleanup_stay_scoped() {
  begin_case label-body-failure label-body-failure

  if run_live_script "${script_dir}/live-github-label-trigger-e2e.sh" 0 91 0 test-oidc-token 1 >/dev/null 2>&1; then
    fail "label trigger body failure unexpectedly succeeded"
  fi

  assert_contains "${body_log}" "body:make"
  assert_contains "${body_log}" "scope=ok ready=1 kubeconfig=${state_dir}/target.kubeconfig"
  assert_contains "${call_log}" "kubectl:args=delete agent github-label-ci-agent"
  assert_no_decoy_mutations
  assert_all_kubectl_calls_scoped
  assert_cluster_absent "${kind_cluster}"
  assert_contains "${call_log}" "kind:args=delete ${kind_cluster}"
}

test_oidc_body_and_cleanup_stay_scoped() {
  begin_case oidc-body-failure oidc-body-failure

  if run_live_script "${script_dir}/live-github-oidc-e2e.sh" 0 0 92 test-oidc-token 1 >/dev/null 2>&1; then
    fail "OIDC body failure unexpectedly succeeded"
  fi

  assert_contains "${body_log}" "body:go"
  assert_contains "${body_log}" "scope=ok ready=1 kubeconfig=${state_dir}/target.kubeconfig"
  assert_contains "${call_log}" "kubectl:args=delete provider live-tts-provider"
  assert_no_decoy_mutations
  assert_all_kubectl_calls_scoped
  assert_cluster_absent "${kind_cluster}"
  assert_contains "${call_log}" "kind:args=delete ${kind_cluster}"
}

write_fake_commands

assert_not_contains "${script_dir}/live-github-label-trigger-e2e.sh" "kubectl config use-context"
assert_not_contains "${script_dir}/live-github-oidc-e2e.sh" "kubectl config use-context"

test_label_preflight_failure_has_no_cluster_side_effects
test_oidc_preflight_failure_has_no_cluster_side_effects
test_setup_failure_never_arms_label_cleanup
test_setup_failure_never_arms_oidc_cleanup
test_label_body_and_cleanup_stay_scoped
test_oidc_body_and_cleanup_stay_scoped

printf 'PASS: live GitHub E2E Kind scope safety tests (0 decoy mutations; all kubectl calls scoped)\n'
