#!/usr/bin/env bash

set -uo pipefail

log() {
  printf '==> %s\n' "$*" >&2
}

error() {
  printf 'error: %s\n' "$*" >&2
}

normalize_absolute_path() {
  local input="$1"
  local part
  local result=""
  local -a parts=()
  local -a normalized=()

  IFS='/' read -r -a parts <<<"${input}"
  for part in "${parts[@]}"; do
    case "${part}" in
      ""|.) ;;
      ..)
        if [[ "${#normalized[@]}" -gt 0 ]]; then
          unset "normalized[$((${#normalized[@]} - 1))]"
        fi
        ;;
      *) normalized+=("${part}") ;;
    esac
  done
  for part in "${normalized[@]}"; do
    result+="/${part}"
  done
  if [[ -z "${result}" ]]; then
    result="/"
  fi
  printf '%s\n' "${result}"
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
kind_bin="${KIND:-kind}"
kubectl_bin="${KUBECTL:-kubectl}"
kind_cluster="${KIND_CLUSTER:-orka-test-e2e}"
kind_config="${E2E_KIND_CONFIG:-test/e2e/kind-config.yaml}"
state_dir="${E2E_CLUSTER_STATE_DIR:-${repo_root}/bin/e2e-kind-state/${kind_cluster}}"
if [[ "${state_dir}" != /* ]]; then
  state_dir="${repo_root}/${state_dir}"
fi
state_dir="$(normalize_absolute_path "${state_dir}")"
if [[ "${state_dir}" == *:* ]]; then
  error "E2E cluster state path may not contain the KUBECONFIG path-list separator ':'"
  exit 1
fi
canonical_kubeconfig="${state_dir}/target.kubeconfig"
if [[ -n "${E2E_CLUSTER_LOCK_ROOT:-}" ]]; then
  lock_root="${E2E_CLUSTER_LOCK_ROOT}"
elif [[ -n "${HOME:-}" ]]; then
  lock_root="${HOME}/.cache/orka/e2e-kind-locks"
else
  lock_root="${TMPDIR:-/tmp}/orka-e2e-kind-locks"
fi
if [[ "${lock_root}" != /* ]]; then
  lock_root="${repo_root}/${lock_root}"
fi
lock_root="$(normalize_absolute_path "${lock_root}")"
operation_lock_dir="${lock_root}/kind-${kind_cluster}.operation"
lease_dir="${lock_root}/kind-${kind_cluster}.lease"
keep_cluster="${KEEP_CLUSTER:-0}"
expected_context="kind-${kind_cluster}"

operation_lock_held=0
operation_token=""
lease_claimed=0
state_validated=0
state_pending=0
state_initialized_this_invocation=0
creation_pending=0
pending_kubeconfig=""
pending_identity=""
cleanup_after_run=0

usage() {
  cat >&2 <<'USAGE'
Usage:
  e2e-kind-cluster.sh setup --create
  e2e-kind-cluster.sh setup --reuse-only
  e2e-kind-cluster.sh run [--create|--reuse-only] [--cleanup] -- command [args...]
  e2e-kind-cluster.sh setup-images -- build-command [args...]
  e2e-kind-cluster.sh cleanup

setup writes an isolated kubeconfig and ownership metadata under
E2E_CLUSTER_STATE_DIR. cleanup deletes the Kind cluster only when that state
records that this E2E setup created it. KEEP_CLUSTER=1 preserves both cluster
and state for later inspection or cleanup.
USAGE
}

validate_inputs() {
  case "${keep_cluster}" in
    0|1) ;;
    *)
      error "KEEP_CLUSTER must be 0 or 1, got '${keep_cluster}'"
      return 1
      ;;
  esac

  if [[ ! "${kind_cluster}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
    error "KIND_CLUSTER must contain only letters, digits, '.', '_' or '-' and may not start with punctuation"
    return 1
  fi

  if ! command -v "${kind_bin}" >/dev/null 2>&1; then
    error "Kind is not installed or KIND is not executable: ${kind_bin}"
    return 1
  fi
  if ! command -v "${kubectl_bin}" >/dev/null 2>&1; then
    error "kubectl is not installed or KUBECTL is not executable: ${kubectl_bin}"
    return 1
  fi
}

acquire_operation_lock() {
  mkdir -p "${lock_root}" || {
    error "failed to create E2E cluster lock root ${lock_root}"
    return 1
  }
  chmod 700 "${lock_root}" || {
    error "failed to secure E2E cluster lock root ${lock_root}"
    return 1
  }

  if ! mkdir "${operation_lock_dir}" 2>/dev/null; then
    error "lifecycle lock already exists for Kind cluster '${kind_cluster}' (active or stale): ${operation_lock_dir}"
    return 1
  fi

  operation_lock_held=1
  operation_token="$$-${RANDOM}-${RANDOM}-${SECONDS}"
  if ! chmod 700 "${operation_lock_dir}" ||
    ! printf '%s\n' "$$" >"${operation_lock_dir}/pid" ||
    ! printf '%s\n' "${operation_token}" >"${operation_lock_dir}/token" ||
    ! printf '%s\n' "${state_dir}" >"${operation_lock_dir}/state_dir" ||
    ! chmod 600 "${operation_lock_dir}/pid" "${operation_lock_dir}/token" "${operation_lock_dir}/state_dir"; then
    error "failed to initialize E2E lifecycle lock ${operation_lock_dir}"
    return 1
  fi
}

claim_cluster_lease() {
  local lease_state=""

  if [[ -e "${lease_dir}" ]]; then
    if [[ ! -d "${lease_dir}" || ! -f "${lease_dir}/state_dir" ]]; then
      error "invalid E2E cluster lease for '${kind_cluster}': ${lease_dir}"
      return 1
    fi
    lease_state="$(cat "${lease_dir}/state_dir")"
    if [[ "${lease_state}" == "${state_dir}" ]]; then
      return 0
    fi
    if [[ ! -e "${lease_state}" ]]; then
      error "Kind cluster '${kind_cluster}' has a stale lease owned by ${lease_state}; use that state path to clean it explicitly"
    else
      error "Kind cluster '${kind_cluster}' is reserved by another E2E state directory: ${lease_state}"
    fi
    return 1
  fi

  if ! mkdir "${lease_dir}" 2>/dev/null; then
    error "failed to reserve Kind cluster '${kind_cluster}'"
    return 1
  fi
  lease_claimed=1
  if ! chmod 700 "${lease_dir}" ||
    ! printf '%s\n' "${state_dir}" >"${lease_dir}/state_dir" ||
    ! chmod 600 "${lease_dir}/state_dir"; then
    rm -rf "${lease_dir}"
    error "failed to initialize E2E cluster lease ${lease_dir}"
    return 1
  fi
}

acquire_lock() {
  acquire_operation_lock || return 1
  claim_cluster_lease
}

release_lock() {
  local lease_state=""

  if [[ "${operation_lock_held}" != "1" ]]; then
    return 0
  fi
  if [[ -f "${lease_dir}/state_dir" ]]; then
    lease_state="$(cat "${lease_dir}/state_dir" 2>/dev/null || true)"
  fi
  if [[ "${lease_state}" == "${state_dir}" ]] && {
    [[ ! -e "${state_dir}" ]] || [[ "${lease_claimed}" == "1" && "${state_validated}" != "1" ]]
  }; then
    rm -rf "${lease_dir}"
  fi
  rm -rf "${operation_lock_dir}"
  operation_lock_held=0
  operation_token=""
}

cluster_list() {
  "${kind_bin}" get clusters
}

cluster_exists_exact() {
  local clusters
  local existing

  if ! clusters="$(cluster_list)"; then
    error "failed to list Kind clusters"
    return 2
  fi

  while IFS= read -r existing; do
    if [[ "${existing}" == "${kind_cluster}" ]]; then
      return 0
    fi
  done <<<"${clusters}"

  return 1
}

new_kubeconfig_path() {
  local temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
  local path

  if ! path="$(mktemp "${temp_root%/}/orka-e2e-kubeconfig.XXXXXX")"; then
    error "failed to create a temporary kubeconfig"
    return 1
  fi
  chmod 600 "${path}" || {
    rm -f "${path}"
    error "failed to secure temporary kubeconfig ${path}"
    return 1
  }
  printf '%s\n' "${path}"
}

write_kind_kubeconfig() {
  local destination="$1"
  local fresh

  if ! fresh="$(new_kubeconfig_path)"; then
    return 1
  fi
  if ! "${kind_bin}" get kubeconfig --name "${kind_cluster}" >"${fresh}"; then
    rm -f "${fresh}"
    error "failed to get kubeconfig for Kind cluster '${kind_cluster}'"
    return 1
  fi
  chmod 600 "${fresh}" || {
    rm -f "${fresh}"
    error "failed to secure kubeconfig for Kind cluster '${kind_cluster}'"
    return 1
  }
  if ! mv -f "${fresh}" "${destination}"; then
    rm -f "${fresh}"
    error "failed to store temporary kubeconfig for Kind cluster '${kind_cluster}'"
    return 1
  fi
}

kubectl_config_value() {
  local kubeconfig="$1"
  shift
  KUBECONFIG="${kubeconfig}" "${kubectl_bin}" --kubeconfig "${kubeconfig}" "$@"
}

validate_kubeconfig_target() {
  local kubeconfig="$1"
  local current_context
  local current_cluster

  if [[ ! -f "${kubeconfig}" ]]; then
    error "temporary kubeconfig does not exist: ${kubeconfig}"
    return 1
  fi

  if ! current_context="$(kubectl_config_value "${kubeconfig}" config current-context)"; then
    error "failed to read current context from temporary kubeconfig"
    return 1
  fi
  if ! current_cluster="$(kubectl_config_value "${kubeconfig}" config view --minify -o 'jsonpath={.contexts[0].context.cluster}')"; then
    error "failed to read current cluster from temporary kubeconfig"
    return 1
  fi

  current_context="${current_context//$'\r'/}"
  current_context="${current_context//$'\n'/}"
  current_cluster="${current_cluster//$'\r'/}"
  current_cluster="${current_cluster//$'\n'/}"

  if [[ "${current_context}" != "${expected_context}" ]]; then
    error "refusing E2E operation: kubeconfig context '${current_context}' does not equal target '${expected_context}'"
    return 1
  fi
  if [[ "${current_cluster}" != "${expected_context}" ]]; then
    error "refusing E2E operation: kubeconfig cluster '${current_cluster}' does not equal target '${expected_context}'"
    return 1
  fi
}

hash_text() {
  local value="$1"
  local digest=""

  if command -v sha256sum >/dev/null 2>&1; then
    if ! digest="$(printf '%s' "${value}" | sha256sum | awk '{print $1}')"; then
      error "failed to compute SHA-256 digest"
      return 1
    fi
  elif command -v shasum >/dev/null 2>&1; then
    if ! digest="$(printf '%s' "${value}" | shasum -a 256 | awk '{print $1}')"; then
      error "failed to compute SHA-256 digest"
      return 1
    fi
  elif command -v openssl >/dev/null 2>&1; then
    if ! digest="$(printf '%s' "${value}" | openssl dgst -sha256 | awk '{print $NF}')"; then
      error "failed to compute SHA-256 digest"
      return 1
    fi
  else
    error "no SHA-256 tool available (need sha256sum, shasum, or openssl)"
    return 1
  fi

  if [[ "${#digest}" -ne 64 || "${digest}" == *[!0-9a-fA-F]* ]]; then
    error "SHA-256 tool returned an invalid digest"
    return 1
  fi
  printf '%s\n' "${digest}"
}

kubeconfig_identity() {
  local kubeconfig="$1"
  local identity_material
  local server
  local certificate_authority_data

  if ! identity_material="$(kubectl_config_value "${kubeconfig}" config view --raw --minify -o 'jsonpath={.clusters[0].cluster.server}{"\n"}{.clusters[0].cluster.certificate-authority-data}')"; then
    error "failed to read cluster identity from temporary kubeconfig"
    return 1
  fi
  server="${identity_material%%$'\n'*}"
  if [[ "${identity_material}" != *$'\n'* ]]; then
    error "temporary kubeconfig cluster identity is incomplete"
    return 1
  fi
  certificate_authority_data="${identity_material#*$'\n'}"
  if [[ -z "${server}" || -z "${certificate_authority_data}" ]]; then
    error "temporary kubeconfig cluster identity is incomplete"
    return 1
  fi

  hash_text "${identity_material}"
}

state_value() {
  local name="$1"
  local path="${state_dir}/${name}"

  if [[ ! -f "${path}" ]]; then
    error "E2E cluster state is missing ${name}: ${state_dir}"
    return 1
  fi
  cat "${path}"
}

load_state() {
  local version

  if [[ ! -d "${state_dir}" ]]; then
    error "no E2E cluster state found at ${state_dir}"
    return 1
  fi

  version="$(state_value version)" || return 1
  state_cluster="$(state_value cluster)" || return 1
  state_context="$(state_value context)" || return 1
  state_kubeconfig="$(state_value kubeconfig)" || return 1
  state_created="$(state_value created)" || return 1
  state_fingerprint="$(state_value fingerprint)" || return 1
  state_status="$(state_value status)" || return 1

  if [[ "${version}" != "1" ]]; then
    error "unsupported E2E cluster state version '${version}'"
    return 1
  fi
  if [[ "${state_cluster}" != "${kind_cluster}" ]]; then
    error "E2E cluster state targets '${state_cluster}', not requested cluster '${kind_cluster}'"
    return 1
  fi
  if [[ "${state_context}" != "${expected_context}" ]]; then
    error "E2E cluster state context '${state_context}' does not equal '${expected_context}'"
    return 1
  fi
  if [[ "${state_created}" != "0" && "${state_created}" != "1" ]]; then
    error "invalid E2E cluster ownership value '${state_created}'"
    return 1
  fi
  if [[ "${state_status}" != "ready" && "${state_status}" != "recovery" && \
    "${state_status}" != "blocked" && "${state_status}" != "pending" ]]; then
    error "invalid E2E cluster state status '${state_status}'"
    return 1
  fi
  if [[ "${state_kubeconfig}" != "${canonical_kubeconfig}" ]]; then
    error "E2E cluster state kubeconfig path is not canonical: ${state_kubeconfig}"
    return 1
  fi
  if [[ -L "${state_kubeconfig}" || ! -f "${state_kubeconfig}" ]]; then
    error "E2E cluster state kubeconfig is not a regular helper-owned file: ${state_kubeconfig}"
    return 1
  fi
  if [[ "${state_fingerprint}" == "unavailable" ]]; then
    if [[ "${state_status}" != "blocked" && "${state_status}" != "pending" ]]; then
      error "invalid E2E cluster state fingerprint at ${state_dir}"
      return 1
    fi
  elif [[ "${#state_fingerprint}" -ne 64 || "${state_fingerprint}" == *[!0-9a-fA-F]* ]]; then
    error "invalid E2E cluster state fingerprint at ${state_dir}"
    return 1
  fi
  state_validated=1
  if [[ "${state_status}" == "pending" ]]; then
    state_pending=1
  fi
}

initialize_state() {
  local created="$1"
  local parent

  parent="$(dirname "${state_dir}")"
  mkdir -p "${parent}" || {
    error "failed to create E2E state parent ${parent}"
    return 1
  }
  if ! mkdir "${state_dir}" 2>/dev/null; then
    error "E2E cluster state is already claimed: ${state_dir}"
    return 1
  fi
  state_initialized_this_invocation=1
  chmod 700 "${state_dir}" || {
    remove_state_files || true
    error "failed to secure E2E state directory"
    return 1
  }

  if ! {
    : >"${canonical_kubeconfig}" &&
      printf '1\n' >"${state_dir}/version" &&
      printf 'pending\n' >"${state_dir}/status" &&
      printf '%s\n' "${kind_cluster}" >"${state_dir}/cluster" &&
      printf '%s\n' "${expected_context}" >"${state_dir}/context" &&
      printf '%s\n' "${canonical_kubeconfig}" >"${state_dir}/kubeconfig" &&
      printf '%s\n' "${created}" >"${state_dir}/created" &&
      printf 'unavailable\n' >"${state_dir}/fingerprint" &&
      chmod 600 "${state_dir}"/*
  }; then
    remove_state_files || true
    error "failed to write pending E2E cluster state"
    return 1
  fi

  state_validated=1
  state_pending=1
}

update_state() {
  local created="$1"
  local fingerprint="$2"
  local status="$3"
  local created_tmp="${state_dir}/.created.$$"
  local fingerprint_tmp="${state_dir}/.fingerprint.$$"
  local status_tmp="${state_dir}/.status.$$"

  if ! printf '%s\n' "${created}" >"${created_tmp}" ||
    ! printf '%s\n' "${fingerprint}" >"${fingerprint_tmp}" ||
    ! printf '%s\n' "${status}" >"${status_tmp}" ||
    ! chmod 600 "${created_tmp}" "${fingerprint_tmp}" "${status_tmp}"; then
    rm -f "${created_tmp}" "${fingerprint_tmp}" "${status_tmp}"
    error "failed to stage E2E cluster state update"
    return 1
  fi
  if ! mv -f "${created_tmp}" "${state_dir}/created" ||
    ! mv -f "${fingerprint_tmp}" "${state_dir}/fingerprint" ||
    ! mv -f "${status_tmp}" "${state_dir}/status"; then
    rm -f "${created_tmp}" "${fingerprint_tmp}" "${status_tmp}"
    error "failed to finalize E2E cluster state update"
    return 1
  fi
  state_validated=1
  if [[ "${status}" == "pending" ]]; then
    state_pending=1
  else
    state_pending=0
  fi
}

remove_state_files() {
  if ! rm -rf "${state_dir}"; then
    error "failed to remove E2E cluster state ${state_dir}"
    return 1
  fi
  state_validated=0
  state_pending=0
  state_initialized_this_invocation=0
}

refresh_existing_state() {
  local fresh_kubeconfig
  local fresh_fingerprint
  local exists_status

  load_state || return 1

  cluster_exists_exact
  exists_status=$?
  if [[ "${exists_status}" -eq 2 ]]; then
    return 1
  fi
  if [[ "${exists_status}" -ne 0 ]]; then
    error "E2E state exists, but Kind cluster '${kind_cluster}' does not; run cleanup-test-e2e before retrying"
    return 1
  fi

  fresh_kubeconfig="$(new_kubeconfig_path)" || return 1
  if ! write_kind_kubeconfig "${fresh_kubeconfig}"; then
    rm -f "${fresh_kubeconfig}"
    return 1
  fi
  if ! validate_kubeconfig_target "${fresh_kubeconfig}"; then
    rm -f "${fresh_kubeconfig}"
    return 1
  fi
  fresh_fingerprint="$(kubeconfig_identity "${fresh_kubeconfig}")" || {
    rm -f "${fresh_kubeconfig}"
    return 1
  }

  if [[ "${state_status}" == "blocked" || "${state_status}" == "pending" ]]; then
    rm -f "${fresh_kubeconfig}"
    error "E2E ownership state for '${kind_cluster}' is ${state_status} and cannot be reused"
    return 1
  fi
  if [[ "${fresh_fingerprint}" != "${state_fingerprint}" ]]; then
    rm -f "${fresh_kubeconfig}"
    error "refusing to reuse ownership state: cluster '${kind_cluster}' no longer has the kubeconfig identity created by this run"
    return 1
  fi

  mv -f "${fresh_kubeconfig}" "${canonical_kubeconfig}" || {
    rm -f "${fresh_kubeconfig}"
    error "failed to refresh temporary kubeconfig ${state_kubeconfig}"
    return 1
  }
  if [[ "${state_status}" == "recovery" ]]; then
    if ! update_state "${state_created}" "${fresh_fingerprint}" ready; then
      error "failed to refresh E2E cluster state metadata"
      return 1
    fi
    state_fingerprint="${fresh_fingerprint}"
    state_status="ready"
  fi

  active_kubeconfig="${canonical_kubeconfig}"
  active_created="${state_created}"
  log "Using existing E2E state for Kind cluster '${kind_cluster}' (created_by_run=${active_created})"
}

prepare_cluster() {
  local allow_create="$1"
  local exists_status
  local create_exists_status
  local created=0
  local fingerprint
  local config_path="${kind_config}"

  if [[ -e "${state_dir}" ]]; then
    if [[ ! -d "${state_dir}" ]]; then
      error "E2E cluster state path is not a directory: ${state_dir}"
      return 1
    fi
    refresh_existing_state
    return $?
  fi

  cluster_exists_exact
  exists_status=$?
  if [[ "${exists_status}" -eq 2 ]]; then
    return 1
  fi
  if [[ "${exists_status}" -ne 0 && "${allow_create}" != "1" ]]; then
    error "Kind cluster '${kind_cluster}' does not exist (exact match required)"
    return 1
  fi

  if [[ "${exists_status}" -ne 0 ]]; then
    created=1
    if [[ "${config_path}" != /* ]]; then
      config_path="${repo_root}/${config_path}"
    fi
    if [[ ! -f "${config_path}" ]]; then
      error "Kind config does not exist: ${config_path}"
      return 1
    fi
  fi

  # Publish secured pending ownership metadata before Kind can create anything.
  initialize_state "${created}" || return 1
  pending_kubeconfig="${canonical_kubeconfig}"

  if [[ "${created}" == "1" ]]; then
    log "Creating Kind cluster '${kind_cluster}' with an isolated kubeconfig"
    creation_pending=1
    if ! KUBECONFIG="${canonical_kubeconfig}" "${kind_bin}" create cluster \
      --name "${kind_cluster}" \
      --config "${config_path}" \
      --kubeconfig "${canonical_kubeconfig}"; then
      # A failed create does not prove ownership: another worktree may have won
      # a same-name race, or Kind may have failed after partial creation.
      cluster_exists_exact
      create_exists_status=$?
      if [[ "${create_exists_status}" -eq 1 ]]; then
        remove_state_files || true
      else
        update_state 1 unavailable blocked || true
        # Retain even a still-pending record if the blocked update itself fails.
        state_initialized_this_invocation=0
      fi
      pending_kubeconfig=""
      creation_pending=0
      error "failed to create Kind cluster '${kind_cluster}'; ambiguous ownership remains blocked and will not be deleted"
      return 1
    fi
    if ! pending_identity="$(kubeconfig_identity "${canonical_kubeconfig}")"; then
      error "failed to capture creation identity for Kind cluster '${kind_cluster}'"
      return 1
    fi
  else
    log "Reusing exact Kind cluster '${kind_cluster}'; this run will never delete it"
  fi

  if ! write_kind_kubeconfig "${canonical_kubeconfig}"; then
    return 1
  fi
  if ! validate_kubeconfig_target "${canonical_kubeconfig}"; then
    return 1
  fi
  fingerprint="$(kubeconfig_identity "${canonical_kubeconfig}")" || return 1
  if [[ "${created}" == "1" && "${fingerprint}" != "${pending_identity}" ]]; then
    error "refusing E2E operation: refreshed kubeconfig identity differs from the cluster just created"
    return 1
  fi
  update_state "${created}" "${fingerprint}" ready || return 1

  creation_pending=0
  pending_kubeconfig=""
  pending_identity=""
  active_kubeconfig="${canonical_kubeconfig}"
  active_created="${created}"
  log "Prepared isolated kubeconfig ${active_kubeconfig} for '${expected_context}'"
}

preserve_pending_creation_state() {
  local fingerprint="${pending_identity}"
  local status="recovery"

  if [[ -z "${fingerprint}" ]] && [[ -f "${canonical_kubeconfig}" ]]; then
    fingerprint="$(kubeconfig_identity "${canonical_kubeconfig}")" || true
  fi
  if [[ -z "${fingerprint}" ]]; then
    fingerprint="unavailable"
    status="blocked"
  fi
  update_state 1 "${fingerprint}" "${status}" || return 1
  creation_pending=0
  pending_kubeconfig=""
  pending_identity=""
  log "Preserved ${status} ownership state for Kind cluster '${kind_cluster}' at ${state_dir}"
}

validated_pending_cleanup_kubeconfig() {
  local fresh_kubeconfig
  local fresh_identity

  if [[ -z "${pending_identity}" ]]; then
    error "cannot verify cleanup identity for Kind cluster '${kind_cluster}'"
    return 1
  fi
  fresh_kubeconfig="$(new_kubeconfig_path)" || return 1
  if ! write_kind_kubeconfig "${fresh_kubeconfig}" ||
    ! validate_kubeconfig_target "${fresh_kubeconfig}"; then
    rm -f "${fresh_kubeconfig}"
    return 1
  fi
  fresh_identity="$(kubeconfig_identity "${fresh_kubeconfig}")" || {
    rm -f "${fresh_kubeconfig}"
    return 1
  }
  if [[ "${fresh_identity}" != "${pending_identity}" ]]; then
    rm -f "${fresh_kubeconfig}"
    error "refusing partial cleanup: exact-name Kind cluster identity changed after creation"
    return 1
  fi
  printf '%s\n' "${fresh_kubeconfig}"
}

cleanup_partial_creation() {
  local exists_status

  if [[ "${creation_pending}" != "1" ]]; then
    return 0
  fi

  if [[ "${keep_cluster}" == "1" ]]; then
    log "KEEP_CLUSTER=1; preserving newly created Kind cluster '${kind_cluster}' after setup failure"
    preserve_pending_creation_state
    return $?
  fi

  cluster_exists_exact
  exists_status=$?
  if [[ "${exists_status}" -eq 2 ]]; then
    error "could not confirm cleanup of Kind cluster '${kind_cluster}'; preserving ownership state"
    preserve_pending_creation_state || true
    return 1
  fi
  if [[ "${exists_status}" -eq 0 ]]; then
    local cleanup_kubeconfig
    if ! cleanup_kubeconfig="$(validated_pending_cleanup_kubeconfig)"; then
      error "could not validate partial cleanup for Kind cluster '${kind_cluster}'; preserving ownership state"
      preserve_pending_creation_state || true
      return 1
    fi
    log "Removing Kind cluster '${kind_cluster}' after setup failure"
    if ! KUBECONFIG="${cleanup_kubeconfig}" "${kind_bin}" delete cluster --name "${kind_cluster}"; then
      rm -f "${cleanup_kubeconfig}"
      error "failed to delete Kind cluster '${kind_cluster}' after setup failure; preserving ownership state"
      preserve_pending_creation_state || true
      return 1
    fi
    rm -f "${cleanup_kubeconfig}"
  fi

  remove_state_files || return 1
  pending_kubeconfig=""
  pending_identity=""
  creation_pending=0
}

cleanup_cluster_state() {
  local fresh_kubeconfig
  local fresh_fingerprint
  local exists_status

  if [[ ! -e "${state_dir}" ]]; then
    log "No E2E cluster state at ${state_dir}; refusing to delete any cluster"
    return 0
  fi
  load_state || return 1

  if [[ "${keep_cluster}" == "1" ]]; then
    log "KEEP_CLUSTER=1; preserving Kind cluster '${kind_cluster}' and E2E state ${state_dir}"
    return 0
  fi

  if [[ "${state_created}" == "0" ]]; then
    log "Leaving reused Kind cluster '${kind_cluster}' intact"
    remove_state_files || return 1
    return 0
  fi

  cluster_exists_exact
  exists_status=$?
  if [[ "${exists_status}" -eq 2 ]]; then
    return 1
  fi
  if [[ "${exists_status}" -ne 0 ]]; then
    log "Owned Kind cluster '${kind_cluster}' is already absent; removing stale E2E state"
    remove_state_files || return 1
    return 0
  fi

  fresh_kubeconfig="$(new_kubeconfig_path)" || return 1
  if ! write_kind_kubeconfig "${fresh_kubeconfig}"; then
    rm -f "${fresh_kubeconfig}"
    return 1
  fi
  if ! validate_kubeconfig_target "${fresh_kubeconfig}"; then
    rm -f "${fresh_kubeconfig}"
    error "refusing to delete Kind cluster '${kind_cluster}' because its target identity could not be validated"
    return 1
  fi
  fresh_fingerprint="$(kubeconfig_identity "${fresh_kubeconfig}")" || {
    rm -f "${fresh_kubeconfig}"
    return 1
  }
  if [[ "${state_status}" == "blocked" || "${state_status}" == "pending" ]]; then
    rm -f "${fresh_kubeconfig}"
    error "refusing to delete Kind cluster '${kind_cluster}': ownership state is ${state_status}"
    return 1
  fi
  if [[ "${fresh_fingerprint}" != "${state_fingerprint}" ]]; then
    rm -f "${fresh_kubeconfig}"
    error "refusing to delete Kind cluster '${kind_cluster}': its kubeconfig identity differs from the cluster created by this run"
    return 1
  fi
  if [[ "${state_status}" == "recovery" ]]; then
    log "Validated recovery state for owned Kind cluster '${kind_cluster}' before deletion"
  fi

  log "Deleting Kind cluster '${kind_cluster}' created by this E2E run"
  if ! KUBECONFIG="${fresh_kubeconfig}" "${kind_bin}" delete cluster --name "${kind_cluster}"; then
    rm -f "${fresh_kubeconfig}"
    error "failed to delete Kind cluster '${kind_cluster}'"
    return 1
  fi

  rm -f "${fresh_kubeconfig}"
  remove_state_files
}

activate_scoped_environment() {
  export KUBECONFIG="${active_kubeconfig}"
  export KIND="${kind_bin}"
  export KUBECTL="${kubectl_bin}"
  export KIND_CLUSTER="${kind_cluster}"
  export E2E_CLUSTER_STATE_DIR="${state_dir}"
  export E2E_CLUSTER_LOCK_ROOT="${lock_root}"
  export E2E_KIND_OPERATION_TOKEN="${operation_token}"
  export E2E_KIND_TARGET_READY=1
  export E2E_KIND_EXPECTED_CONTEXT="${expected_context}"
  export E2E_KIND_EXPECTED_CLUSTER="${expected_context}"
  unset MAKEFLAGS MAKEOVERRIDES MFLAGS GNUMAKEFLAGS MAKEFILES
}

run_scoped() {
  local allow_create="$1"
  local should_cleanup="$2"
  shift 2

  if [[ "$#" -eq 0 ]]; then
    error "run requires a command after --"
    return 1
  fi

  cleanup_after_run="${should_cleanup}"
  prepare_cluster "${allow_create}" || return 1

  (
    activate_scoped_environment
    "$@"
  )
}

run_setup_images() {
  if [[ "$#" -eq 0 ]]; then
    error "setup-images requires a build command after --"
    return 1
  fi
  prepare_cluster 1 || return 1

  (
    activate_scoped_environment
    "$@" || exit $?
    local image
    for image in "${IMG:-}" "${AI_WORKER_IMG:-}" "${GENERAL_WORKER_IMG:-}" "${HARNESS_WRAPPER_IMG:-}"; do
      if [[ -z "${image}" ]]; then
        error "setup-images requires all E2E image variables"
        exit 1
      fi
      "${kind_bin}" load docker-image "${image}" --name "${kind_cluster}" || exit $?
    done
  )
}

finalize() {
  local status=$?
  local cleanup_status=0

  trap - EXIT INT TERM

  if [[ "${creation_pending}" == "1" ]]; then
    cleanup_partial_creation || cleanup_status=$?
  elif [[ "${state_pending}" == "1" && "${state_initialized_this_invocation}" == "1" ]]; then
    remove_state_files || cleanup_status=$?
  elif [[ "${cleanup_after_run}" == "1" ]]; then
    cleanup_cluster_state || cleanup_status=$?
  fi
  if [[ -n "${pending_kubeconfig}" && "${pending_kubeconfig}" != "${canonical_kubeconfig}" ]]; then
    rm -f "${pending_kubeconfig}"
  fi
  pending_identity=""

  release_lock

  if [[ "${status}" -eq 0 && "${cleanup_status}" -ne 0 ]]; then
    status="${cleanup_status}"
  elif [[ "${status}" -ne 0 && "${cleanup_status}" -ne 0 ]]; then
    error "E2E command failed with status ${status}; cleanup also failed with status ${cleanup_status}"
  fi

  exit "${status}"
}

main() {
  local command="${1:-}"
  local allow_create=1
  local should_cleanup=0

  validate_inputs || return 1
  acquire_lock || return 1

  case "${command}" in
    setup)
      shift
      case "${1:---create}" in
        --create) allow_create=1 ;;
        --reuse-only) allow_create=0 ;;
        *) usage; return 2 ;;
      esac
      if [[ "$#" -gt 0 ]]; then
        shift
      fi
      if [[ "$#" -ne 0 ]]; then
        usage
        return 2
      fi
      prepare_cluster "${allow_create}"
      ;;
    run)
      shift
      while [[ "$#" -gt 0 ]]; do
        case "$1" in
          --create)
            allow_create=1
            shift
            ;;
          --reuse-only)
            allow_create=0
            shift
            ;;
          --cleanup)
            should_cleanup=1
            shift
            ;;
          --)
            shift
            break
            ;;
          *)
            usage
            return 2
            ;;
        esac
      done
      run_scoped "${allow_create}" "${should_cleanup}" "$@"
      ;;
    setup-images)
      shift
      if [[ "${1:-}" != "--" ]]; then
        usage
        return 2
      fi
      shift
      run_setup_images "$@"
      ;;
    cleanup)
      shift
      if [[ "$#" -ne 0 ]]; then
        usage
        return 2
      fi
      cleanup_cluster_state
      ;;
    -h|--help|help|"")
      usage
      if [[ -z "${command}" ]]; then
        return 2
      fi
      ;;
    *)
      usage
      return 2
      ;;
  esac
}

trap finalize EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
main "$@"
