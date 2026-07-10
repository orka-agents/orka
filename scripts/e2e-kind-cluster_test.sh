#!/usr/bin/env bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
helper="${script_dir}/e2e-kind-cluster.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/e2e-kind-cluster-test.XXXXXX")"
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
  grep -Fx -- "${name}" "${FAKE_KIND_CLUSTERS}" >/dev/null 2>&1 || fail "expected cluster '${name}' to exist"
}

assert_cluster_absent() {
  local name="$1"
  if grep -Fx -- "${name}" "${FAKE_KIND_CLUSTERS}" >/dev/null 2>&1; then
    fail "expected cluster '${name}' to be absent"
  fi
}

write_fake_commands() {
  mkdir -p "${test_root}/bin"

  cat >"${test_root}/bin/kind" <<'FAKE_KIND'
#!/usr/bin/env bash
set -euo pipefail

log() {
  printf 'kind:%s KUBECONFIG=%s\n' "$*" "${KUBECONFIG:-}" >>"${FAKE_LOG}"
}

exact_exists() {
  local name="$1"
  grep -Fx -- "${name}" "${FAKE_KIND_CLUSTERS}" >/dev/null 2>&1
}

remove_exact() {
  local name="$1"
  local next="${FAKE_KIND_CLUSTERS}.next"
  awk -v target="${name}" '$0 != target' "${FAKE_KIND_CLUSTERS}" >"${next}"
  mv "${next}" "${FAKE_KIND_CLUSTERS}"
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
            *)
              exit 71
              ;;
          esac
        done
        log "get kubeconfig ${name}"
        exact_exists "${name}" || exit 72
        context="${FAKE_CONTEXT_OVERRIDE:-kind-${name}}"
        cluster="${FAKE_CLUSTER_OVERRIDE:-kind-${name}}"
        identity="${FAKE_GET_IDENTITY_OVERRIDE:-${FAKE_IDENTITY_OVERRIDE:-identity-${name}}}"
        printf 'context=%s\ncluster=%s\nname=%s\nserver=https://%s\nca=ca-%s\n' \
          "${context}" "${cluster}" "${name}" "${identity}" "${identity}"
        ;;
      *)
        exit 73
        ;;
    esac
    ;;
  create)
    [[ "${1:-}" == "cluster" ]] || exit 74
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
        *)
          exit 75
          ;;
      esac
    done
    log "create ${name}"
    [[ -n "${name}" ]] || exit 76
    if exact_exists "${name}"; then
      exit 77
    fi
    printf '%s\n' "${name}" >>"${FAKE_KIND_CLUSTERS}"
    if [[ -n "${kubeconfig}" ]]; then
      identity="${FAKE_IDENTITY_OVERRIDE:-identity-${name}}"
      printf 'context=kind-%s\ncluster=kind-%s\nname=%s\nserver=https://%s\nca=ca-%s\n' \
        "${name}" "${name}" "${name}" "${identity}" "${identity}" >"${kubeconfig}"
    fi
    if [[ "${FAKE_KIND_SIGNAL_PARENT:-0}" == "1" ]]; then
      kill -TERM "${PPID}"
    fi
    if [[ "${FAKE_KIND_CREATE_FAIL:-0}" == "1" ]]; then
      exit 84
    fi
    ;;
  load)
    [[ "${1:-}" == "docker-image" ]] || exit 86
    image="${2:-}"
    shift 2
    name=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --name)
          name="$2"
          shift 2
          ;;
        *)
          exit 87
          ;;
      esac
    done
    log "load ${name} ${image}"
    exact_exists "${name}" || exit 88
    ;;
  delete)
    [[ "${1:-}" == "cluster" ]] || exit 78
    shift
    name=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --name)
          name="$2"
          shift 2
          ;;
        *)
          exit 79
          ;;
      esac
    done
    log "delete ${name}"
    exact_exists "${name}" || exit 80
    if [[ "${FAKE_KIND_DELETE_FAIL:-0}" == "1" ]]; then
      exit 85
    fi
    remove_exact "${name}"
    ;;
  *)
    exit 81
    ;;
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
[[ -n "${kubeconfig}" && -f "${kubeconfig}" ]] || exit 82
context="$(sed -n 's/^context=//p' "${kubeconfig}")"
cluster="$(sed -n 's/^cluster=//p' "${kubeconfig}")"

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

printf 'kubectl:%s context=%s cluster=%s kubeconfig=%s\n' "$*" "${context}" "${cluster}" "${kubeconfig}" >>"${FAKE_LOG}"
if [[ "${FAKE_KUBECTL_FAIL:-0}" == "1" ]]; then
  exit 83
fi
FAKE_KUBECTL

  chmod +x "${test_root}/bin/kind" "${test_root}/bin/kubectl"
}

begin_case() {
  local name="$1"
  CASE_DIR="${test_root}/${name}"
  rm -rf "${CASE_DIR}"
  mkdir -p "${CASE_DIR}"
  FAKE_KIND_CLUSTERS="${CASE_DIR}/clusters"
  FAKE_LOG="${CASE_DIR}/calls.log"
  STATE_DIR="${CASE_DIR}/state"
  LOCK_ROOT="${CASE_DIR}/locks"
  AMBIENT_KUBECONFIG="${CASE_DIR}/ambient.kubeconfig"
  : >"${FAKE_KIND_CLUSTERS}"
  : >"${FAKE_LOG}"
  printf 'context=kind-decoy\ncluster=kind-decoy\nname=decoy\nserver=https://identity-decoy\nca=ca-identity-decoy\n' >"${AMBIENT_KUBECONFIG}"
  KEEP_CLUSTER=0
  unset MAKEFLAGS MAKEOVERRIDES MFLAGS GNUMAKEFLAGS MAKEFILES RUN_KIND_CLUSTER
  unset FAKE_CONTEXT_OVERRIDE FAKE_CLUSTER_OVERRIDE FAKE_IDENTITY_OVERRIDE FAKE_GET_IDENTITY_OVERRIDE
  unset FAKE_KUBECTL_FAIL FAKE_KIND_CREATE_FAIL FAKE_KIND_DELETE_FAIL FAKE_KIND_SIGNAL_PARENT
}

run_helper() {
  run_helper_for_cluster "${RUN_KIND_CLUSTER:-target}" "$@"
}

run_helper_for_cluster() {
  local cluster="$1"
  shift
  env \
    PATH="${test_root}/bin:${PATH}" \
    KIND="${test_root}/bin/kind" \
    KUBECTL="${test_root}/bin/kubectl" \
    KIND_CLUSTER="${cluster}" \
    KEEP_CLUSTER="${KEEP_CLUSTER:-0}" \
    KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    E2E_KIND_CONFIG="${repo_root}/test/e2e/kind-config.yaml" \
    E2E_CLUSTER_STATE_DIR="${STATE_DIR}" \
    E2E_CLUSTER_LOCK_ROOT="${LOCK_ROOT}" \
    FAKE_KIND_CLUSTERS="${FAKE_KIND_CLUSTERS}" \
    FAKE_LOG="${FAKE_LOG}" \
    FAKE_CONTEXT_OVERRIDE="${FAKE_CONTEXT_OVERRIDE:-}" \
    FAKE_CLUSTER_OVERRIDE="${FAKE_CLUSTER_OVERRIDE:-}" \
    FAKE_IDENTITY_OVERRIDE="${FAKE_IDENTITY_OVERRIDE:-}" \
    FAKE_GET_IDENTITY_OVERRIDE="${FAKE_GET_IDENTITY_OVERRIDE:-}" \
    FAKE_KUBECTL_FAIL="${FAKE_KUBECTL_FAIL:-0}" \
    FAKE_KIND_CREATE_FAIL="${FAKE_KIND_CREATE_FAIL:-0}" \
    FAKE_KIND_DELETE_FAIL="${FAKE_KIND_DELETE_FAIL:-0}" \
    FAKE_KIND_SIGNAL_PARENT="${FAKE_KIND_SIGNAL_PARENT:-0}" \
    AMBIENT_KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    FAKE_NESTED_MAKEFILE="${CASE_DIR}/nested.mk" \
    "${helper}" "$@"
}

write_nested_probe() {
  cat >"${CASE_DIR}/nested.mk" <<'MAKEFILE'
.PHONY: nested
nested:
	@test "$${E2E_KIND_TARGET_READY:-}" = "1"
	@test "$${KUBECONFIG}" != "$${AMBIENT_KUBECONFIG}"
	@kubectl delete configmap nested-probe
MAKEFILE
}

test_similar_name_requires_exact_match_and_scopes_nested_commands() {
  begin_case similar-name
  printf 'target-extra\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  write_nested_probe

  # The nested shell expands FAKE_NESTED_MAKEFILE after the helper exports
  # the scoped environment.
  # shellcheck disable=SC2016
  run_helper run --create --cleanup -- bash -c \
    'kubectl apply -f direct-probe.yaml && make -f "${FAKE_NESTED_MAKEFILE}" nested'

  assert_contains "${FAKE_LOG}" "kind:create target"
  assert_contains "${FAKE_LOG}" "kind:delete target"
  assert_contains "${FAKE_LOG}" "kubectl:apply -f direct-probe.yaml context=kind-target cluster=kind-target"
  assert_contains "${FAKE_LOG}" "kubectl:delete configmap nested-probe context=kind-target cluster=kind-target"
  assert_not_contains "${FAKE_LOG}" "context=kind-decoy cluster=kind-decoy"
  assert_cluster_exists target-extra
  assert_cluster_exists decoy
  assert_cluster_absent target
}

test_reuse_only_rejects_similar_name() {
  begin_case reuse-similar
  printf 'target-extra\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"

  if run_helper run --reuse-only --cleanup -- kubectl apply -f should-not-run.yaml; then
    fail "reuse-only accepted a substring cluster match"
  fi

  assert_not_contains "${FAKE_LOG}" "kind:create target"
  assert_not_contains "${FAKE_LOG}" "kubectl:apply -f should-not-run.yaml"
  assert_cluster_exists target-extra
  assert_cluster_exists decoy
}

test_state_kubeconfig_path_cannot_escape_state_directory() {
  begin_case canonical-kubeconfig
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  run_helper setup --create
  local ambient_before
  ambient_before="$(cat "${AMBIENT_KUBECONFIG}")"
  printf '%s\n' "${AMBIENT_KUBECONFIG}" >"${STATE_DIR}/kubeconfig"

  if run_helper setup --create; then
    fail "noncanonical state kubeconfig path unexpectedly validated"
  fi

  [[ "$(cat "${AMBIENT_KUBECONFIG}")" == "${ambient_before}" ]] || fail "ambient kubeconfig was modified"
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
}

test_state_storage_is_verified_before_cluster_creation() {
  begin_case state-before-create
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  local invalid_parent="${CASE_DIR}/not-a-directory"
  : >"${invalid_parent}"
  STATE_DIR="${invalid_parent}/state"

  if run_helper setup --create; then
    fail "cluster creation proceeded without writable state storage"
  fi

  assert_not_contains "${FAKE_LOG}" "kind:create target"
  assert_cluster_absent target
  assert_cluster_exists decoy
  [[ ! -e "${LOCK_ROOT}/kind-target.lease" ]] || fail "failed preflight left a cluster lease"
}

test_state_directory_rejects_kubeconfig_path_separator() {
  begin_case state-path-separator
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  STATE_DIR="${CASE_DIR}/state:ambient"

  if run_helper setup --create; then
    fail "state path containing KUBECONFIG separator unexpectedly succeeded"
  fi

  assert_not_contains "${FAKE_LOG}" "kind:create target"
  assert_cluster_absent target
  assert_cluster_exists decoy
}

test_state_directory_is_lexically_normalized() {
  begin_case normalized-state
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  mkdir -p "${CASE_DIR}/subdir"
  STATE_DIR="${CASE_DIR}/subdir/../normalized-state/"
  local normalized_case
  local normalized_state
  normalized_case="$(printf '%s\n' "${CASE_DIR}" | sed -E 's:/+:/:g')"
  normalized_state="${normalized_case}/normalized-state"

  run_helper setup --create
  [[ -d "${normalized_state}" ]] || fail "normalized state directory was not created"
  [[ "$(cat "${normalized_state}/kubeconfig")" == "${normalized_state}/target.kubeconfig" ]] || \
    fail "state persisted a noncanonical kubeconfig path"

  run_helper cleanup
  assert_cluster_exists target
  [[ ! -e "${normalized_state}" ]] || fail "normalized reused state was not removed"
}

test_reused_state_rejects_replacement_identity() {
  begin_case reused-replacement
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  run_helper setup --create
  local original_fingerprint
  original_fingerprint="$(cat "${STATE_DIR}/fingerprint")"
  FAKE_IDENTITY_OVERRIDE=identity-replacement

  if run_helper setup --create; then
    fail "reused persisted state accepted a replacement cluster identity"
  fi

  [[ "$(cat "${STATE_DIR}/fingerprint")" == "${original_fingerprint}" ]] || fail "replacement rewrote persisted identity"
  assert_cluster_exists target
  assert_not_contains "${FAKE_LOG}" "kind:delete target"

  unset FAKE_IDENTITY_OVERRIDE
  run_helper cleanup
  assert_cluster_exists target
}

test_state_directory_claim_is_atomic_across_clusters() {
  begin_case atomic-state-claim
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  STATE_DIR="${CASE_DIR}/shared-state"
  local alpha_status="${CASE_DIR}/alpha.status"
  local beta_status="${CASE_DIR}/beta.status"

  (
    set +e
    run_helper_for_cluster alpha setup --create >"${CASE_DIR}/alpha.out" 2>&1
    printf '%s\n' "$?" >"${alpha_status}"
  ) &
  local alpha_pid=$!
  (
    set +e
    run_helper_for_cluster beta setup --create >"${CASE_DIR}/beta.out" 2>&1
    printf '%s\n' "$?" >"${beta_status}"
  ) &
  local beta_pid=$!
  wait "${alpha_pid}" || true
  wait "${beta_pid}" || true

  local alpha_rc beta_rc
  alpha_rc="$(cat "${alpha_status}")"
  beta_rc="$(cat "${beta_status}")"
  if [[ "${alpha_rc}" == "0" && "${beta_rc}" == "0" ]] || [[ "${alpha_rc}" != "0" && "${beta_rc}" != "0" ]]; then
    cat "${CASE_DIR}/alpha.out" "${CASE_DIR}/beta.out" >&2
    fail "atomic state claim should allow exactly one cluster setup"
  fi
  local create_count
  create_count="$(grep -Ec '^kind:create (alpha|beta) ' "${FAKE_LOG}" || true)"
  [[ "${create_count}" == "1" ]] || fail "expected exactly one Kind create after state claim race"

  local winner
  winner="$(grep -Ex 'alpha|beta' "${FAKE_KIND_CLUSTERS}")"
  run_helper_for_cluster "${winner}" cleanup
  assert_cluster_absent "${winner}"
  assert_cluster_exists decoy
}

test_cluster_lease_blocks_other_state_directories() {
  begin_case cluster-lease
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  local owner_state="${STATE_DIR}"
  local competing_state="${CASE_DIR}/competing-state"

  run_helper setup --create
  assert_cluster_exists target

  STATE_DIR="${competing_state}"
  if run_helper setup --create; then
    fail "a second state directory acquired the same exact Kind cluster"
  fi
  [[ ! -e "${competing_state}" ]] || fail "competing state should not be published"
  assert_not_contains "${FAKE_LOG}" "kind:delete target"

  STATE_DIR="${owner_state}"
  run_helper cleanup
  assert_cluster_absent target
  assert_cluster_exists decoy
}

test_exact_reuse_is_never_deleted() {
  begin_case exact-reuse
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"

  run_helper run --create --cleanup -- kubectl apply -f reused-probe.yaml

  assert_not_contains "${FAKE_LOG}" "kind:create target"
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  assert_contains "${FAKE_LOG}" "kubectl:apply -f reused-probe.yaml context=kind-target cluster=kind-target"
  assert_cluster_exists target
  assert_cluster_exists decoy
  [[ ! -e "${STATE_DIR}" ]] || fail "reused cluster state should be removed by cleanup"
}

test_mismatched_context_fails_closed() {
  begin_case mismatched-context
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  FAKE_CONTEXT_OVERRIDE=kind-decoy

  if run_helper run --create --cleanup -- kubectl apply -f should-not-run.yaml; then
    fail "mismatched kubeconfig context was accepted"
  fi

  assert_not_contains "${FAKE_LOG}" "kubectl:apply -f should-not-run.yaml"
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  assert_cluster_exists target
  assert_cluster_exists decoy
  [[ ! -e "${STATE_DIR}" ]] || fail "invalid reused state should not be published"
}

test_failed_create_never_deletes_unconfirmed_cluster() {
  begin_case partial-create-failure
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  FAKE_KIND_CREATE_FAIL=1

  if run_helper run --create --cleanup -- true; then
    fail "partially failing Kind create unexpectedly succeeded"
  fi

  assert_contains "${FAKE_LOG}" "kind:create target"
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  assert_not_contains "${FAKE_LOG}" "kind:delete decoy"
  assert_cluster_exists target
  assert_cluster_exists decoy
  [[ -d "${STATE_DIR}" ]] || fail "ambiguous create failure lost pending ownership state"
  [[ "$(cat "${STATE_DIR}/status")" == "blocked" ]] || fail "ambiguous create should retain blocked state"

  # Once external inspection proves the ambiguous cluster absent, cleanup can
  # discard the blocked metadata without deleting anything.
  awk '$0 != "target"' "${FAKE_KIND_CLUSTERS}" >"${FAKE_KIND_CLUSTERS}.next"
  mv "${FAKE_KIND_CLUSTERS}.next" "${FAKE_KIND_CLUSTERS}"
  run_helper cleanup
  [[ ! -e "${STATE_DIR}" ]] || fail "absent ambiguous cluster left stale state"
}

test_signal_during_kind_create_preserves_ownership_state() {
  begin_case signal-during-create
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  FAKE_KIND_SIGNAL_PARENT=1

  if run_helper setup --create; then
    fail "signal during Kind create unexpectedly succeeded"
  fi

  assert_cluster_exists target
  [[ -d "${STATE_DIR}" ]] || fail "signal during create lost ownership state"
  [[ "$(cat "${STATE_DIR}/status")" == "recovery" ]] || fail "signal during create did not preserve recovery state"
  [[ -d "${LOCK_ROOT}/kind-target.lease" ]] || fail "signal during create released the cluster lease"

  unset FAKE_KIND_SIGNAL_PARENT
  run_helper cleanup
  assert_cluster_absent target
}

test_partial_cleanup_rejects_replacement_cluster_identity() {
  begin_case partial-replacement
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  FAKE_GET_IDENTITY_OVERRIDE=identity-replacement

  if run_helper run --create --cleanup -- true; then
    fail "post-create replacement identity unexpectedly succeeded"
  fi

  assert_cluster_exists target
  assert_cluster_exists decoy
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  [[ -d "${STATE_DIR}" ]] || fail "replacement mismatch should preserve recovery state"
  [[ "$(cat "${STATE_DIR}/fingerprint")" != "unavailable" ]] || fail "creation identity was not preserved"

  unset FAKE_GET_IDENTITY_OVERRIDE
  run_helper cleanup
  assert_cluster_absent target
}

test_stale_operation_lock_is_not_reclaimed() {
  begin_case stale-operation-lock
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  mkdir -p "${LOCK_ROOT}/kind-target.operation" "${LOCK_ROOT}/kind-target.lease"
  printf '99999999\n' >"${LOCK_ROOT}/kind-target.operation/pid"
  printf '%s\n' "${STATE_DIR}" >"${LOCK_ROOT}/kind-target.lease/state_dir"

  if run_helper setup --create; then
    fail "stale operation lock was reclaimed unsafely"
  fi

  assert_not_contains "${FAKE_LOG}" "kind:get kubeconfig target"
  assert_cluster_exists target
  assert_cluster_exists decoy
  [[ -d "${LOCK_ROOT}/kind-target.lease" ]] || fail "failed lock acquisition removed another operation's lease"
  rm -rf "${LOCK_ROOT}/kind-target.operation" "${LOCK_ROOT}/kind-target.lease"
}

test_failed_state_validation_releases_new_lease() {
  begin_case invalid-state-lease
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  mkdir -p "${STATE_DIR}"
  for field in version status context kubeconfig created fingerprint; do
    printf 'placeholder\n' >"${STATE_DIR}/${field}"
  done
  printf 'other-cluster\n' >"${STATE_DIR}/cluster"

  if run_helper setup --create; then
    fail "invalid state unexpectedly validated"
  fi

  [[ ! -e "${LOCK_ROOT}/kind-target.lease" ]] || fail "failed validation left a newly claimed lease"
}

test_setup_images_runs_inside_exact_cluster_operation() {
  begin_case setup-images
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  cat >"${CASE_DIR}/build-images" <<'BUILD_IMAGES'
#!/usr/bin/env bash
set -euo pipefail
[[ "${E2E_KIND_TARGET_READY:-}" == "1" ]]
[[ -f "${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation/token" ]]
BUILD_IMAGES
  chmod +x "${CASE_DIR}/build-images"
  export IMG=controller:test
  export AI_WORKER_IMG=ai-worker:test
  export GENERAL_WORKER_IMG=general-worker:test
  export HARNESS_WRAPPER_IMG=harness-wrapper:test

  run_helper setup-images -- "${CASE_DIR}/build-images"

  for image in "${IMG}" "${AI_WORKER_IMG}" "${GENERAL_WORKER_IMG}" "${HARNESS_WRAPPER_IMG}"; do
    assert_contains "${FAKE_LOG}" "kind:load target ${image}"
  done
  assert_not_contains "${FAKE_LOG}" "kind:load decoy"
  assert_cluster_exists target
  assert_cluster_exists decoy
  run_helper cleanup
  assert_cluster_absent target
  unset IMG AI_WORKER_IMG GENERAL_WORKER_IMG HARNESS_WRAPPER_IMG
}

test_failed_kind_delete_preserves_state_and_lease() {
  begin_case failed-delete
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  run_helper setup --create
  FAKE_KIND_DELETE_FAIL=1

  if run_helper cleanup; then
    fail "failing Kind delete unexpectedly succeeded"
  fi

  assert_cluster_exists target
  [[ -d "${STATE_DIR}" ]] || fail "failed delete removed ownership state"
  [[ -d "${LOCK_ROOT}/kind-target.lease" ]] || fail "failed delete released ownership lease"

  unset FAKE_KIND_DELETE_FAIL
  run_helper cleanup
  assert_cluster_absent target
  [[ ! -e "${STATE_DIR}" ]] || fail "successful retry left ownership state"
}

test_recovery_state_rejects_replacement_cluster_identity() {
  begin_case recovery-replacement
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  KEEP_CLUSTER=1
  FAKE_CONTEXT_OVERRIDE=kind-decoy

  if run_helper run --create --cleanup -- true; then
    fail "mismatched post-create kubeconfig unexpectedly succeeded"
  fi
  [[ "$(cat "${STATE_DIR}/status")" == "recovery" ]] || fail "expected recovery state status"

  # Simulate external deletion/recreation under the same exact Kind name.
  FAKE_CONTEXT_OVERRIDE=""
  FAKE_IDENTITY_OVERRIDE=identity-replacement
  KEEP_CLUSTER=0
  if run_helper cleanup; then
    fail "recovery state accepted a replacement cluster identity"
  fi
  assert_cluster_exists target
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  [[ -d "${STATE_DIR}" ]] || fail "identity mismatch should preserve recovery state"

  FAKE_IDENTITY_OVERRIDE=identity-target
  run_helper cleanup
  assert_cluster_absent target
}

test_failed_partial_validation_preserves_recovery_state() {
  begin_case failed-partial-validation
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  FAKE_CONTEXT_OVERRIDE=kind-decoy

  if run_helper run --create --cleanup -- true; then
    fail "post-create validation failure unexpectedly succeeded"
  fi

  assert_cluster_exists target
  [[ -d "${STATE_DIR}" ]] || fail "failed partial cleanup should preserve ownership state"
  [[ "$(cat "${STATE_DIR}/created")" == "1" ]] || fail "failed cleanup lost ownership"
  [[ "$(cat "${STATE_DIR}/status")" == "recovery" ]] || fail "failed cleanup should preserve recovery status"

  unset FAKE_CONTEXT_OVERRIDE
  run_helper cleanup
  assert_cluster_absent target
  assert_cluster_exists decoy
}

test_command_failure_cleans_only_cluster_created_by_run() {
  begin_case failure-cleanup
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"

  if run_helper run --create --cleanup -- bash -c 'kubectl apply -f failure-probe.yaml; exit 23'; then
    fail "failing E2E command unexpectedly succeeded"
  fi

  assert_contains "${FAKE_LOG}" "kind:create target"
  assert_contains "${FAKE_LOG}" "kind:delete target"
  assert_not_contains "${FAKE_LOG}" "kind:delete decoy"
  assert_cluster_absent target
  assert_cluster_exists decoy
  [[ ! -e "${STATE_DIR}" ]] || fail "owned cluster state should be removed after successful failure cleanup"
}

test_keep_cluster_preserves_recovery_state_after_setup_failure() {
  begin_case keep-setup-failure
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  KEEP_CLUSTER=1
  FAKE_CONTEXT_OVERRIDE=kind-decoy

  if run_helper run --create --cleanup -- true; then
    fail "mismatched post-create kubeconfig unexpectedly succeeded"
  fi

  assert_cluster_exists target
  assert_cluster_exists decoy
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  [[ -d "${STATE_DIR}" ]] || fail "KEEP_CLUSTER setup failure should preserve recovery state"
  [[ "$(cat "${STATE_DIR}/created")" == "1" ]] || fail "recovery state lost cluster ownership"
  [[ "$(cat "${STATE_DIR}/status")" == "recovery" ]] || fail "expected recovery state status"

  KEEP_CLUSTER=0
  unset FAKE_CONTEXT_OVERRIDE
  run_helper cleanup
  assert_cluster_absent target
  assert_cluster_exists decoy
  assert_contains "${FAKE_LOG}" "kind:delete target"
  unset KEEP_CLUSTER
}

test_keep_cluster_preserves_owned_cluster_and_state() {
  begin_case keep-cluster
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  KEEP_CLUSTER=1

  if run_helper run --create --cleanup -- bash -c 'exit 24'; then
    fail "failing E2E command unexpectedly succeeded"
  fi

  assert_cluster_exists target
  assert_cluster_exists decoy
  assert_not_contains "${FAKE_LOG}" "kind:delete target"
  [[ -d "${STATE_DIR}" ]] || fail "KEEP_CLUSTER=1 should preserve ownership state"

  KEEP_CLUSTER=0
  run_helper cleanup
  assert_cluster_absent target
  assert_cluster_exists decoy
  assert_contains "${FAKE_LOG}" "kind:delete target"
  assert_not_contains "${FAKE_LOG}" "kind:delete decoy"
  unset KEEP_CLUSTER
}

test_persisted_pending_state_is_not_erased_by_finalizer() {
  begin_case persisted-pending
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  run_helper setup --create
  printf 'pending\n' >"${STATE_DIR}/status"

  if run_helper cleanup; then
    fail "pending owned state unexpectedly allowed cluster deletion"
  fi
  [[ -d "${STATE_DIR}" ]] || fail "persisted pending state was erased"
  [[ "$(cat "${STATE_DIR}/status")" == "pending" ]] || fail "pending status changed unexpectedly"
  [[ -d "${LOCK_ROOT}/kind-target.lease" ]] || fail "pending state lease was released"
  assert_cluster_exists target

  awk '$0 != "target"' "${FAKE_KIND_CLUSTERS}" >"${FAKE_KIND_CLUSTERS}.next"
  mv "${FAKE_KIND_CLUSTERS}.next" "${FAKE_KIND_CLUSTERS}"
  run_helper cleanup
  [[ ! -e "${STATE_DIR}" ]] || fail "absent pending cluster left stale state"
  [[ ! -e "${LOCK_ROOT}/kind-target.lease" ]] || fail "absent pending cluster left stale lease"
}

test_recursive_make_overrides_cannot_escape_scoped_environment() {
  begin_case recursive-overrides
  printf 'target\ndecoy\n' >"${FAKE_KIND_CLUSTERS}"
  write_nested_probe
  export MAKEFLAGS=" -- KUBECONFIG=${AMBIENT_KUBECONFIG} E2E_CLUSTER_STATE_DIR=relative-state"
  export MAKEOVERRIDES="KUBECONFIG=${AMBIENT_KUBECONFIG} E2E_CLUSTER_STATE_DIR=relative-state"
  export GNUMAKEFLAGS="KUBECONFIG=${AMBIENT_KUBECONFIG} E2E_CLUSTER_STATE_DIR=relative-state"
  cat >"${CASE_DIR}/ambient.mk" <<MAKEFILES_EOF
export KUBECONFIG := ${AMBIENT_KUBECONFIG}
export E2E_CLUSTER_STATE_DIR := relative-state
MAKEFILES_EOF
  export MAKEFILES="${CASE_DIR}/ambient.mk"

  run_helper run --create --cleanup -- make -f "${CASE_DIR}/nested.mk" nested

  assert_contains "${FAKE_LOG}" "kubectl:delete configmap nested-probe context=kind-target cluster=kind-target"
  assert_not_contains "${FAKE_LOG}" "context=kind-decoy cluster=kind-decoy"
  assert_cluster_exists target
  unset MAKEFLAGS MAKEOVERRIDES GNUMAKEFLAGS MAKEFILES
}

test_make_dry_run_does_not_execute_cluster_lifecycle() {
  begin_case make-dry-run
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"

  env \
    PATH="${test_root}/bin:${PATH}" \
    KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    FAKE_KIND_CLUSTERS="${FAKE_KIND_CLUSTERS}" \
    FAKE_LOG="${FAKE_LOG}" \
    E2E_CLUSTER_LOCK_ROOT="${LOCK_ROOT}" \
    make -n -s -C "${repo_root}" test-e2e \
      E2E_RECURSIVE_MAKE="${CASE_DIR}/never-run" \
      KIND="${test_root}/bin/kind" \
      KUBECTL="${test_root}/bin/kubectl" \
      KIND_CLUSTER=target \
      E2E_KIND_CONFIG="${repo_root}/test/e2e/kind-config.yaml" \
      E2E_CLUSTER_STATE_DIR="${STATE_DIR}" \
      E2E_CLUSTER_LOCK_ROOT="${LOCK_ROOT}" \
      E2E_CLUSTER_HELPER="${helper}" >/dev/null

  assert_not_contains "${FAKE_LOG}" "kind:create target"
  assert_cluster_absent target
  assert_cluster_exists decoy
  [[ ! -e "${STATE_DIR}" ]] || fail "make -n created E2E state"
}

test_make_test_e2e_scopes_recursive_make_and_cleans_failure() {
  begin_case make-integration
  printf 'decoy\n' >"${FAKE_KIND_CLUSTERS}"
  cat >"${CASE_DIR}/recursive-make" <<'RECURSIVE_MAKE'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == "_test-e2e-scoped" ]]
[[ "${E2E_KIND_TARGET_READY:-}" == "1" ]]
[[ "${KUBECONFIG}" != "${AMBIENT_KUBECONFIG}" ]]
kubectl apply -f make-recursive-probe.yaml
exit 25
RECURSIVE_MAKE
  chmod +x "${CASE_DIR}/recursive-make"

  if env \
    PATH="${test_root}/bin:${PATH}" \
    KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    FAKE_KIND_CLUSTERS="${FAKE_KIND_CLUSTERS}" \
    FAKE_LOG="${FAKE_LOG}" \
    FAKE_CONTEXT_OVERRIDE="" \
    FAKE_CLUSTER_OVERRIDE="" \
    FAKE_IDENTITY_OVERRIDE="" \
    FAKE_GET_IDENTITY_OVERRIDE="" \
    FAKE_KUBECTL_FAIL=0 \
    FAKE_KIND_CREATE_FAIL=0 \
    FAKE_KIND_DELETE_FAIL=0 \
    FAKE_KIND_SIGNAL_PARENT=0 \
    AMBIENT_KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    E2E_CLUSTER_LOCK_ROOT="${LOCK_ROOT}" \
    make -s -C "${repo_root}" test-e2e \
      E2E_RECURSIVE_MAKE="${CASE_DIR}/recursive-make" \
      KIND="${test_root}/bin/kind" \
      KUBECTL="${test_root}/bin/kubectl" \
      KIND_CLUSTER=target \
      KEEP_CLUSTER=0 \
      E2E_KIND_CONFIG="${repo_root}/test/e2e/kind-config.yaml" \
      E2E_CLUSTER_STATE_DIR="${STATE_DIR}" \
      E2E_CLUSTER_LOCK_ROOT="${LOCK_ROOT}" \
      E2E_CLUSTER_HELPER="${helper}"; then
    fail "Make test-e2e failure probe unexpectedly succeeded"
  fi

  assert_contains "${FAKE_LOG}" "kubectl:apply -f make-recursive-probe.yaml context=kind-target cluster=kind-target"
  assert_contains "${FAKE_LOG}" "kind:delete target"
  assert_not_contains "${FAKE_LOG}" "kind:delete decoy"
  assert_cluster_absent target
  assert_cluster_exists decoy
}

write_fake_commands

test_similar_name_requires_exact_match_and_scopes_nested_commands
test_reuse_only_rejects_similar_name
test_state_kubeconfig_path_cannot_escape_state_directory
test_state_storage_is_verified_before_cluster_creation
test_state_directory_rejects_kubeconfig_path_separator
test_state_directory_is_lexically_normalized
test_reused_state_rejects_replacement_identity
test_state_directory_claim_is_atomic_across_clusters
test_cluster_lease_blocks_other_state_directories
test_exact_reuse_is_never_deleted
test_mismatched_context_fails_closed
test_failed_create_never_deletes_unconfirmed_cluster
test_signal_during_kind_create_preserves_ownership_state
test_partial_cleanup_rejects_replacement_cluster_identity
test_stale_operation_lock_is_not_reclaimed
test_failed_state_validation_releases_new_lease
test_setup_images_runs_inside_exact_cluster_operation
test_failed_kind_delete_preserves_state_and_lease
test_recovery_state_rejects_replacement_cluster_identity
test_failed_partial_validation_preserves_recovery_state
test_command_failure_cleans_only_cluster_created_by_run
test_keep_cluster_preserves_recovery_state_after_setup_failure
test_keep_cluster_preserves_owned_cluster_and_state
test_persisted_pending_state_is_not_erased_by_finalizer
test_recursive_make_overrides_cannot_escape_scoped_environment
test_make_dry_run_does_not_execute_cluster_lifecycle
test_make_test_e2e_scopes_recursive_make_and_cleans_failure

printf 'PASS: e2e Kind cluster targeting safety tests\n'
