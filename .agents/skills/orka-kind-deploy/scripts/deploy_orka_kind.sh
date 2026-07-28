#!/usr/bin/env bash
set -euo pipefail
umask 077

script_path="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/$(basename "${BASH_SOURCE[0]}")"

usage() {
  cat <<'USAGE'
Usage: deploy_orka_kind.sh [options]

Rebuild all Orka images, load them into an existing local kind cluster, render
fresh manifests without changing the worktree, deploy through an isolated
kubeconfig, and verify both rollouts.

Options:
  --repo PATH                     Orka repository root.
  --cluster NAME                  Exact existing kind cluster name.
  --context NAME                  Context in the scoped source kubeconfig.
  --kubeconfig PATH               Scoped source kubeconfig used for identity verification.
  --controller-image IMAGE        Controller image (default: IMG or controller:kind).
  --ai-worker-image IMAGE         AI worker image (default: AI_WORKER_IMG or repository default).
  --general-worker-image IMAGE    General worker image (default: GENERAL_WORKER_IMG or repository default).
  --harness-wrapper-image IMAGE   Harness wrapper image (default: HARNESS_WRAPPER_IMG or repository default).
  -h, --help                      Show this help.

The script never reads or writes ~/.kube/config and ignores ambient
KUBECONFIG. When --kubeconfig is unset, it asks the repo-vendored kindctl for
the current worktree's scoped kubeconfig. The selected context's server and CA
must match a fresh kubeconfig exported from the exact kind cluster name.
USAGE
}

log() {
  printf '==> %s\n' "$*" >&2
}

error() {
  printf 'error: %s\n' "$*" >&2
}

die() {
  error "$*"
  exit 1
}

resolve_command() {
  local candidate="$1"
  local label="$2"
  local resolved=""

  if [[ "${candidate}" == */* ]]; then
    [[ -x "${candidate}" ]] || die "${label} is not executable: ${candidate}"
    resolved="$(cd "$(dirname "${candidate}")" && pwd -P)/$(basename "${candidate}")"
  else
    resolved="$(command -v "${candidate}" 2>/dev/null || true)"
    [[ -n "${resolved}" ]] || die "Missing required command: ${candidate} (${label})"
  fi
  printf '%s\n' "${resolved}"
}

validate_image() {
  local label="$1"
  local image="$2"

  [[ -n "${image}" ]] || die "${label} image must not be empty"
  if [[ "${image}" == *$'\n'* || "${image}" == *$'\r'* || "${image}" == *$'\t'* || "${image}" == *' '* ]]; then
    die "${label} image contains whitespace: ${image}"
  fi
  if [[ ! "${image}" =~ ^[A-Za-z0-9][A-Za-z0-9._/:@+-]*$ ]]; then
    die "${label} image contains unsupported characters"
  fi
}

normalize_single_line() {
  local value="$1"
  value="${value//$'\r'/}"
  value="${value//$'\n'/}"
  printf '%s' "${value}"
}

kubectl_config_value() {
  local kubeconfig="$1"
  local context="$2"
  shift 2
  local -a command=("${kubectl_bin}" --kubeconfig "${kubeconfig}")

  if [[ -n "${context}" ]]; then
    command+=(--context "${context}")
  fi
  KUBECONFIG="${kubeconfig}" "${command[@]}" "$@"
}

context_cluster_for() {
  local kubeconfig="$1"
  local context="$2"
  local cluster

  cluster="$(kubectl_config_value "${kubeconfig}" "${context}" \
    config view --raw --minify -o 'jsonpath={.contexts[0].context.cluster}')" || return 1
  normalize_single_line "${cluster}"
}

kubeconfig_identity() {
  local kubeconfig="$1"
  local context="$2"
  local server
  local certificate_authority_data
  local certificate_authority_path
  local certificate_authority_fingerprint
  local resolved_ca_path
  local login_home

  server="$(kubectl_config_value "${kubeconfig}" "${context}" config view --raw --minify \
    -o 'jsonpath={.clusters[0].cluster.server}')" || return 1
  certificate_authority_data="$(kubectl_config_value "${kubeconfig}" "${context}" config view --raw --minify \
    -o 'jsonpath={.clusters[0].cluster.certificate-authority-data}')" || return 1
  certificate_authority_path="$(kubectl_config_value "${kubeconfig}" "${context}" config view --raw --minify \
    -o 'jsonpath={.clusters[0].cluster.certificate-authority}')" || return 1
  server="$(normalize_single_line "${server}")"
  certificate_authority_data="$(normalize_single_line "${certificate_authority_data}")"
  certificate_authority_path="$(normalize_single_line "${certificate_authority_path}")"

  if [[ -z "${server}" ]]; then
    error "kubeconfig cluster identity is missing its API server"
    return 1
  fi
  if [[ -n "${certificate_authority_data}" && -n "${certificate_authority_path}" ]]; then
    error "kubeconfig cluster identity has both CA data and a CA file"
    return 1
  fi
  if [[ -n "${certificate_authority_data}" ]]; then
    certificate_authority_fingerprint="$("${python_bin}" - "${certificate_authority_data}" <<'PY'
import base64
import hashlib
import sys

try:
    data = base64.b64decode(sys.argv[1], validate=True)
except Exception as exc:
    raise SystemExit(f"invalid certificate-authority-data: {exc}")
print(hashlib.sha256(data).hexdigest())
PY
)" || return 1
  elif [[ -n "${certificate_authority_path}" ]]; then
    resolved_ca_path="$("${python_bin}" - "${kubeconfig}" "${certificate_authority_path}" <<'PY'
import os
import sys

kubeconfig, reference = sys.argv[1:]
if not os.path.isabs(reference):
    reference = os.path.join(os.path.dirname(kubeconfig), reference)
print(os.path.realpath(reference))
PY
)" || return 1
    [[ -f "${resolved_ca_path}" ]] || {
      error "kubeconfig certificate authority file does not exist: ${resolved_ca_path}"
      return 1
    }
    login_home="$(login_home_directory)" || return 1
    if is_global_kubeconfig_file "${resolved_ca_path}" "${login_home}" "${HOME:-}"; then
      error "refusing to read the global kubeconfig as certificate authority data"
      return 1
    fi
    certificate_authority_fingerprint="$("${python_bin}" - "${resolved_ca_path}" <<'PY'
import hashlib
import sys
from pathlib import Path

print(hashlib.sha256(Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)" || return 1
  else
    error "kubeconfig cluster identity is missing certificate authority data"
    return 1
  fi

  printf '%s\n%s' "${server}" "${certificate_authority_fingerprint}"
}

hash_text() {
  local value="$1"

  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "${value}" | sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s' "${value}" | shasum -a 256 | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    printf '%s' "${value}" | openssl dgst -sha256 | awk '{print $NF}'
  else
    error "no SHA-256 tool available (need sha256sum, shasum, or openssl)"
    return 1
  fi
}

count_nonempty_lines() {
  awk 'NF { count++ } END { print count + 0 }'
}

validate_isolated_source_kubeconfig() {
  local kubeconfig="$1"
  local contexts
  local clusters
  local users
  local count

  contexts="$(kubectl_config_value "${kubeconfig}" "" config view --raw \
    -o 'jsonpath={range .contexts[*]}{.name}{"\n"}{end}')" || return 1
  clusters="$(kubectl_config_value "${kubeconfig}" "" config view --raw \
    -o 'jsonpath={range .clusters[*]}{.name}{"\n"}{end}')" || return 1
  users="$(kubectl_config_value "${kubeconfig}" "" config view --raw \
    -o 'jsonpath={range .users[*]}{.name}{"\n"}{end}')" || return 1

  count="$(printf '%s\n' "${contexts}" | count_nonempty_lines)"
  [[ "${count}" == "1" ]] || {
    error "scoped source kubeconfig must contain exactly one context, found ${count}"
    return 1
  }
  count="$(printf '%s\n' "${clusters}" | count_nonempty_lines)"
  [[ "${count}" == "1" ]] || {
    error "scoped source kubeconfig must contain exactly one cluster, found ${count}"
    return 1
  }
  count="$(printf '%s\n' "${users}" | count_nonempty_lines)"
  [[ "${count}" == "1" ]] || {
    error "scoped source kubeconfig must contain exactly one user, found ${count}"
    return 1
  }
}

login_home_directory() {
  "${python_bin}" -c 'import os, pwd; print(pwd.getpwuid(os.getuid()).pw_dir)'
}

is_global_kubeconfig_file() {
  local source="$1"
  local login_home="$2"
  local environment_home="$3"

  "${python_bin}" - "${source}" "${login_home}" "${environment_home}" <<'PY'
import os
import sys

source = os.path.realpath(sys.argv[1])
homes = {home for home in sys.argv[2:] if home}
for home in homes:
    candidate = os.path.join(home, ".kube", "config")
    if source == os.path.realpath(candidate):
        raise SystemExit(0)
    if os.path.exists(candidate):
        try:
            if os.path.samefile(source, candidate):
                raise SystemExit(0)
        except OSError:
            pass
raise SystemExit(1)
PY
}

path_is_within() {
  local child="$1"
  local parent="$2"

  "${python_bin}" - "${child}" "${parent}" <<'PY'
import os
import sys

child = os.path.abspath(sys.argv[1])
parent = os.path.realpath(sys.argv[2])
probe = child
while not os.path.lexists(probe):
    next_probe = os.path.dirname(probe)
    if next_probe == probe:
        break
    probe = next_probe
probe = os.path.realpath(probe)

while True:
    try:
        if os.path.samefile(probe, parent):
            raise SystemExit(0)
    except OSError:
        pass
    next_probe = os.path.dirname(probe)
    if next_probe == probe:
        break
    probe = next_probe
raise SystemExit(1)
PY
}

cluster_exists_exact() {
  local clusters
  local existing

  clusters="$("${kind_bin}" get clusters)" || return 1
  while IFS= read -r existing; do
    if [[ "${existing}" == "${cluster_name}" ]]; then
      return 0
    fi
  done <<<"${clusters}"
  return 1
}

write_kind_kubeconfig() {
  local destination="$1"
  local staged="${destination}.tmp"

  rm -f "${staged}"
  if ! "${kind_bin}" get kubeconfig --name "${cluster_name}" >"${staged}"; then
    rm -f "${staged}"
    error "failed to export kubeconfig for kind cluster '${cluster_name}'"
    return 1
  fi
  chmod 600 "${staged}"
  mv -f "${staged}" "${destination}"
}

validate_exported_kubeconfig() {
  local kubeconfig="$1"
  local expected_context="kind-${cluster_name}"
  local current_context
  local current_cluster

  current_context="$(kubectl_config_value "${kubeconfig}" "" config current-context)" || {
    error "failed to read the exported kind kubeconfig context"
    return 1
  }
  current_context="$(normalize_single_line "${current_context}")"
  current_cluster="$(context_cluster_for "${kubeconfig}" "${expected_context}")" || {
    error "failed to read the exported kind kubeconfig cluster"
    return 1
  }

  if [[ "${current_context}" != "${expected_context}" ]]; then
    error "exported kind kubeconfig context '${current_context}' does not equal '${expected_context}'"
    return 1
  fi
  if [[ "${current_cluster}" != "${expected_context}" ]]; then
    error "exported kind kubeconfig cluster '${current_cluster}' does not equal '${expected_context}'"
    return 1
  fi
}

verify_kind_identity_unchanged() {
  local expected_fingerprint="$1"
  local fresh_kubeconfig="${runtime_dir}/identity-check.kubeconfig"
  local fresh_identity
  local fresh_fingerprint

  write_kind_kubeconfig "${fresh_kubeconfig}" || return 1
  validate_exported_kubeconfig "${fresh_kubeconfig}" || return 1
  fresh_identity="$(kubeconfig_identity "${fresh_kubeconfig}" "kind-${cluster_name}")" || return 1
  fresh_fingerprint="$(hash_text "${fresh_identity}")" || return 1
  rm -f "${fresh_kubeconfig}"

  if [[ "${fresh_fingerprint}" != "${expected_fingerprint}" ]]; then
    error "kind cluster '${cluster_name}' server/CA identity changed during deployment"
    return 1
  fi
}

resolve_render_tools() {
  local tools_dir="${runtime_dir}/tools"
  local candidate
  local -a install_targets=()

  mkdir -p "${tools_dir}"

  candidate="${KUSTOMIZE:-}"
  if [[ -n "${candidate}" ]]; then
    kustomize_bin="$(resolve_command "${candidate}" kustomize)"
  elif [[ -x "${repo_root}/bin/kustomize" ]]; then
    kustomize_bin="${repo_root}/bin/kustomize"
  else
    kustomize_bin="${tools_dir}/kustomize"
    install_targets+=(kustomize)
  fi

  candidate="${CONTROLLER_GEN:-}"
  if [[ -n "${candidate}" ]]; then
    controller_gen_bin="$(resolve_command "${candidate}" controller-gen)"
  elif [[ -x "${repo_root}/bin/controller-gen" ]]; then
    controller_gen_bin="${repo_root}/bin/controller-gen"
  else
    controller_gen_bin="${tools_dir}/controller-gen"
    install_targets+=(controller-gen)
  fi

  if [[ "${#install_targets[@]}" -gt 0 ]]; then
    "${make_bin}" -C "${repo_root}" \
      LOCALBIN="${tools_dir}" \
      KUSTOMIZE="${tools_dir}/kustomize" \
      CONTROLLER_GEN="${tools_dir}/controller-gen" \
      "${install_targets[@]}"
  fi

  [[ -x "${kustomize_bin}" ]] || die "kustomize was not installed at ${kustomize_bin}"
  [[ -x "${controller_gen_bin}" ]] || die "controller-gen was not installed at ${controller_gen_bin}"
}

patch_worker_images() {
  local manager_file="$1"

  "${python_bin}" - "${manager_file}" "${ai_worker_image}" "${general_worker_image}" <<'PY'
import re
import sys
from pathlib import Path

path = Path(sys.argv[1])
ai_image = sys.argv[2]
general_image = sys.argv[3]
text = path.read_text()

replacements = (
    (r"(?m)^(\s*-\s+--ai-worker-image=).*$", rf"\g<1>{ai_image}", "AI worker"),
    (r"(?m)^(\s*-\s+--general-worker-image=).*$", rf"\g<1>{general_image}", "general worker"),
)
for pattern, replacement, label in replacements:
    text, count = re.subn(pattern, replacement, text)
    if count != 1:
        raise SystemExit(f"expected exactly one {label} image argument in {path}, found {count}")

path.write_text(text)
PY
}

verify_rendered_manifest() {
  local manifest="$1"

  "${python_bin}" - \
    "${manifest}" \
    "${controller_image}" \
    "${ai_worker_image}" \
    "${general_worker_image}" \
    "${harness_wrapper_image}" <<'PY'
import re
import sys
from pathlib import Path

manifest = Path(sys.argv[1])
controller_image, ai_image, general_image, harness_image = sys.argv[2:]
text = manifest.read_text()
documents = re.split(r"(?m)^---[ \t]*\n", text)


def deployment(name):
    matches = []
    for document in documents:
        if not re.search(r"(?m)^kind:\s*Deployment\s*$", document):
            continue
        if re.search(rf"(?m)^\s{{2}}name:\s*{re.escape(name)}\s*$", document):
            matches.append(document)
    if len(matches) != 1:
        raise SystemExit(f"expected exactly one rendered Deployment/{name}, found {len(matches)}")
    return matches[0]


def image_values(document):
    values = []
    for line in document.splitlines():
        stripped = line.strip()
        if stripped.startswith("image:"):
            values.append(stripped.split(":", 1)[1].strip().strip("\"'"))
    return values


def option_values(document, option):
    prefix = f"- --{option}="
    values = []
    for line in document.splitlines():
        stripped = line.strip()
        if stripped.startswith(prefix):
            values.append(stripped[len(prefix):])
    return values

controller = deployment("orka-controller-manager")
harness = deployment("orka-agent-harness-wrapper")

if controller_image not in image_values(controller):
    raise SystemExit(f"controller image {controller_image!r} was not rendered")
if harness_image not in image_values(harness):
    raise SystemExit(f"harness wrapper image {harness_image!r} was not rendered")
if option_values(controller, "ai-worker-image") != [ai_image]:
    raise SystemExit("configured AI worker image argument was not rendered exactly once")
if option_values(controller, "general-worker-image") != [general_image]:
    raise SystemExit("configured general worker image argument was not rendered exactly once")
PY
}

prepare_rendered_manifests() {
  local render_config="${runtime_dir}/config"
  local manager_file

  cp -R "${repo_root}/config" "${render_config}"
  rm -rf "${render_config}/crd/bases"
  mkdir -p "${render_config}/crd/bases"

  (
    cd "${repo_root}"
    "${controller_gen_bin}" \
      rbac:roleName=manager-role \
      crd:allowDangerousTypes=true \
      webhook \
      paths="./..." \
      "output:crd:artifacts:config=${render_config}/crd/bases" \
      "output:rbac:artifacts:config=${render_config}/rbac" \
      "output:webhook:artifacts:config=${render_config}/webhook"
  )

  manager_file="${render_config}/manager/manager.yaml"
  patch_worker_images "${manager_file}"

  (
    cd "${render_config}/manager"
    "${kustomize_bin}" edit set image "controller=${controller_image}"
  )
  (
    cd "${render_config}/harness-wrapper"
    "${kustomize_bin}" edit set image \
      "ghcr.io/orka-agents/orka/agent-harness-wrapper=${harness_wrapper_image}"
  )

  crd_manifest="${runtime_dir}/crds.yaml"
  deployment_manifest="${runtime_dir}/deployment.yaml"
  "${kustomize_bin}" build "${render_config}/crd" >"${crd_manifest}"
  "${kustomize_bin}" build "${render_config}/default" >"${deployment_manifest}"
  [[ -s "${crd_manifest}" ]] || die "rendered CRD manifest is empty"
  [[ -s "${deployment_manifest}" ]] || die "rendered deployment manifest is empty"
  verify_rendered_manifest "${deployment_manifest}"
}

acquire_image_lock() {
  local lock_root="${ORKA_KIND_DEPLOY_IMAGE_LOCK_ROOT:-}"
  local login_home

  if [[ -z "${lock_root}" ]]; then
    login_home="$(login_home_directory)" || return 1
    lock_root="${login_home}/.cache/orka/kind-deploy-image-locks"
  fi
  image_lock_root="$("${python_bin}" -c 'import os, sys; print(os.path.abspath(sys.argv[1]))' "${lock_root}")"
  if path_is_within "${image_lock_root}" "${repo_root}"; then
    error "image lock root must be outside the repository: ${image_lock_root}"
    return 1
  fi
  image_lock_dir="${image_lock_root}/build-load.operation"
  image_lock_token="$$-${RANDOM}-${RANDOM}-${SECONDS}"

	mkdir -p "${image_lock_root}"
	if ! mkdir "${image_lock_dir}" 2>/dev/null; then
		error "Orka image build/load lock already exists (active or stale): ${image_lock_dir}"
		return 1
	fi
	if ! chmod 700 "${image_lock_dir}" ||
		! printf '%s\n' "${image_lock_token}" >"${image_lock_dir}/token" ||
		! printf '%s\n' "$$" >"${image_lock_dir}/pid" ||
		! chmod 600 "${image_lock_dir}/token" "${image_lock_dir}/pid"; then
    rm -rf "${image_lock_dir}"
    error "failed to initialize Orka image build/load lock: ${image_lock_dir}"
    return 1
  fi
}

release_image_lock() {
  local observed_token=""

  if [[ -n "${image_lock_dir:-}" && -f "${image_lock_dir}/token" ]]; then
    observed_token="$(cat "${image_lock_dir}/token" 2>/dev/null || true)"
  fi
  if [[ -n "${image_lock_token:-}" && "${observed_token}" == "${image_lock_token}" ]]; then
    rm -rf "${image_lock_dir}"
  fi
}

build_and_load_images() {
  local expected_fingerprint="$1"
  local image

  (
    image_lock_root=""
    image_lock_dir=""
    image_lock_token=""
    acquire_image_lock
    trap release_image_lock EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    "${make_bin}" -C "${repo_root}" \
      "CONTAINER_TOOL=${container_tool_bin}" \
      "IMG=${controller_image}" \
      "AI_WORKER_IMG=${ai_worker_image}" \
      "GENERAL_WORKER_IMG=${general_worker_image}" \
      "HARNESS_WRAPPER_IMG=${harness_wrapper_image}" \
      docker-build-all

    for image in \
      "${controller_image}" \
      "${ai_worker_image}" \
      "${general_worker_image}" \
      "${harness_wrapper_image}"; do
      verify_kind_identity_unchanged "${expected_fingerprint}"
      "${kind_bin}" load docker-image "${image}" --name "${cluster_name}"
    done
    verify_kind_identity_unchanged "${expected_fingerprint}"
  )
}

deploy_and_verify() {
  local expected_context="kind-${cluster_name}"
  local namespace_manifest="${runtime_dir}/namespace.yaml"
  local token_file="${runtime_dir}/harness-wrapper-token"
  local deployment
  local -a kube_cmd=("${kubectl_bin}" --kubeconfig "${isolated_kubeconfig}" --context "${expected_context}")

  "${kube_cmd[@]}" apply -f "${crd_manifest}"
  "${kube_cmd[@]}" create namespace "${namespace}" --dry-run=client -o yaml >"${namespace_manifest}"
  "${kube_cmd[@]}" apply -f "${namespace_manifest}"
  if ! "${kube_cmd[@]}" -n "${namespace}" get secret harness-wrapper-auth >/dev/null 2>&1; then
    "${dd_bin}" if=/dev/urandom bs=32 count=1 2>/dev/null | "${base64_bin}" | "${tr_bin}" -d '\n' >"${token_file}"
    "${kube_cmd[@]}" -n "${namespace}" create secret generic harness-wrapper-auth \
      "--from-file=token=${token_file}" >/dev/null
  fi
  "${kube_cmd[@]}" apply -f "${deployment_manifest}"

  for deployment in orka-controller-manager orka-agent-harness-wrapper; do
    "${kube_cmd[@]}" -n "${namespace}" rollout restart "deployment/${deployment}"
  done
  for deployment in orka-controller-manager orka-agent-harness-wrapper; do
    "${kube_cmd[@]}" -n "${namespace}" rollout status "deployment/${deployment}" --timeout=180s
  done
  "${kube_cmd[@]}" -n "${namespace}" get pods,svc,deploy
}

run_internal_deploy() {
  local current_identity
  local current_fingerprint
  local expected_internal_kubeconfig
  local internal_state_dir
  local internal_lock_root
  local internal_operation_dir
  local internal_lease_dir
  local login_home

  [[ -n "${runtime_dir}" && -d "${runtime_dir}" ]] || die "internal deploy runtime directory is missing"
  [[ -n "${expected_identity_fingerprint}" ]] || die "internal deploy identity fingerprint is missing"
  [[ "${E2E_KIND_TARGET_READY:-}" == "1" ]] || die "internal deploy requires validated E2E helper scope"
  [[ -n "${E2E_KIND_OPERATION_TOKEN:-}" ]] || die "internal deploy requires the E2E operation token"
  [[ "${E2E_KIND_EXPECTED_CONTEXT:-}" == "kind-${cluster_name}" ]] || die "internal deploy context scope is invalid"
  [[ "${E2E_KIND_EXPECTED_CLUSTER:-}" == "kind-${cluster_name}" ]] || die "internal deploy cluster scope is invalid"
  [[ -n "${E2E_CLUSTER_LOCK_ROOT:-}" ]] || die "internal deploy cluster lock root is missing"
  [[ -n "${KUBECONFIG:-}" && -f "${KUBECONFIG}" && ! -L "${KUBECONFIG}" ]] || \
    die "internal deploy requires a regular helper-scoped kubeconfig"

  internal_state_dir="$("${python_bin}" -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' \
    "${runtime_dir}/e2e-state")"
  internal_lock_root="$("${python_bin}" -c 'import os, sys; print(os.path.abspath(sys.argv[1]))' \
    "${E2E_CLUSTER_LOCK_ROOT}")"
  internal_operation_dir="${internal_lock_root}/kind-${cluster_name}.operation"
  internal_lease_dir="${internal_lock_root}/kind-${cluster_name}.lease"
  [[ "$(cat "${internal_operation_dir}/token" 2>/dev/null || true)" == "${E2E_KIND_OPERATION_TOKEN}" ]] || \
    die "internal deploy does not own the held operation lock"
  [[ "$(cat "${internal_operation_dir}/state_dir" 2>/dev/null || true)" == "${internal_state_dir}" ]] || \
    die "internal deploy operation lock targets another state directory"
  [[ "$(cat "${internal_lease_dir}/state_dir" 2>/dev/null || true)" == "${internal_state_dir}" ]] || \
    die "internal deploy does not own the cluster lease"

  isolated_kubeconfig="$("${python_bin}" -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "${KUBECONFIG}")"
  expected_internal_kubeconfig="$("${python_bin}" -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' \
    "${internal_state_dir}/target.kubeconfig")"
  if [[ "${isolated_kubeconfig}" != "${expected_internal_kubeconfig}" ]]; then
    die "internal deploy kubeconfig is outside the helper-owned runtime state"
  fi
  login_home="$(login_home_directory)" || die "failed to resolve the login home directory"
  if is_global_kubeconfig_file "${isolated_kubeconfig}" "${login_home}" "${HOME:-}"; then
    die "internal deploy refuses the global kubeconfig"
  fi
  validate_exported_kubeconfig "${isolated_kubeconfig}"
  current_identity="$(kubeconfig_identity "${isolated_kubeconfig}" "kind-${cluster_name}")" || \
    die "failed to read helper-scoped kind identity"
  current_fingerprint="$(hash_text "${current_identity}")" || die "failed to fingerprint helper-scoped kind identity"
  if [[ "${current_fingerprint}" != "${expected_identity_fingerprint}" ]]; then
    die "helper-scoped kubeconfig identity differs from the selected kind cluster"
  fi
  export KUBECONFIG="${isolated_kubeconfig}"
  printf '%s\n' "${E2E_KIND_OPERATION_TOKEN}" >"${runtime_dir}/e2e-operation-token"
  chmod 600 "${runtime_dir}/e2e-operation-token"

  log "Repository: ${repo_root}"
  log "Kind cluster: ${cluster_name}"
  log "Controller image: ${controller_image}"
  log "AI worker image: ${ai_worker_image}"
  log "General worker image: ${general_worker_image}"
  log "Harness wrapper image: ${harness_wrapper_image}"

  build_and_load_images "${expected_identity_fingerprint}"
  resolve_render_tools
  prepare_rendered_manifests
  deploy_and_verify
}

run_locked_deploy() {
  local expected_fingerprint="$1"
  local state_dir="${runtime_dir}/e2e-state"
  local lock_root="${ORKA_KIND_DEPLOY_CLUSTER_LOCK_ROOT:-}"
  local helper_tmp="${runtime_dir}/e2e-tmp"
  local lease_path
  local operation_path
  local helper_status=0
  local cleanup_failed=0
  local recorded_operation_token=""
  local observed_operation_token=""
  local observed_operation_state=""
  local observed_lease_state=""

  if [[ -z "${lock_root}" ]]; then
    lock_root="$(login_home_directory)/.cache/orka/e2e-kind-locks"
  fi
  lock_root="$("${python_bin}" -c 'import os, sys; print(os.path.abspath(sys.argv[1]))' "${lock_root}")"
  if path_is_within "${lock_root}" "${repo_root}"; then
    error "cluster lock root must be outside the repository: ${lock_root}"
    return 1
  fi
  lease_path="${lock_root}/kind-${cluster_name}.lease"
  operation_path="${lock_root}/kind-${cluster_name}.operation"
  mkdir -p "${helper_tmp}"

  if KIND="${kind_bin}" \
    KUBECTL="${kubectl_bin}" \
    MAKE="${make_bin}" \
    PYTHON="${python_bin}" \
    CONTAINER_TOOL="${container_tool_bin}" \
    KUBECONFIG="${isolated_kubeconfig}" \
    KIND_CLUSTER="${cluster_name}" \
    KEEP_CLUSTER=0 \
    RUNNER_TEMP="${helper_tmp}" \
    E2E_KIND_CONFIG="${repo_root}/test/e2e/kind-config.yaml" \
    E2E_CLUSTER_STATE_DIR="${state_dir}" \
    E2E_CLUSTER_LOCK_ROOT="${lock_root}" \
    "${e2e_helper}" run --reuse-only --cleanup -- \
      "${script_path}" \
      --internal-deploy \
      --repo "${repo_root}" \
      --cluster "${cluster_name}" \
      --runtime-dir "${runtime_dir}" \
      --expected-kind-identity "${expected_fingerprint}" \
      --controller-image "${controller_image}" \
      --ai-worker-image "${ai_worker_image}" \
      --general-worker-image "${general_worker_image}" \
      --harness-wrapper-image "${harness_wrapper_image}"; then
    helper_status=0
  else
    helper_status=$?
  fi

  if [[ -f "${runtime_dir}/e2e-operation-token" ]]; then
    recorded_operation_token="$(cat "${runtime_dir}/e2e-operation-token" 2>/dev/null || true)"
  fi
  if [[ -f "${lease_path}/state_dir" ]]; then
    observed_lease_state="$(cat "${lease_path}/state_dir" 2>/dev/null || true)"
  fi
  if [[ -f "${operation_path}/token" ]]; then
    observed_operation_token="$(cat "${operation_path}/token" 2>/dev/null || true)"
  fi
  if [[ -f "${operation_path}/state_dir" ]]; then
    observed_operation_state="$(cat "${operation_path}/state_dir" 2>/dev/null || true)"
  fi

  if [[ -e "${state_dir}" ]]; then
    error "E2E helper left persistent state: ${state_dir}"
    cleanup_failed=1
    preserve_runtime=1
  fi
  if [[ "${observed_lease_state}" == "${state_dir}" ]]; then
    error "E2E helper left this deployment's cluster lease: ${lease_path}"
    cleanup_failed=1
    preserve_runtime=1
  fi
  if [[ "${observed_operation_state}" == "${state_dir}" ]] ||
    [[ -n "${recorded_operation_token}" && "${observed_operation_token}" == "${recorded_operation_token}" ]]; then
    error "E2E helper left this deployment's operation lock: ${operation_path}"
    cleanup_failed=1
    preserve_runtime=1
  fi

  if [[ "${helper_status}" -ne 0 ]]; then
    return "${helper_status}"
  fi
  if [[ "${cleanup_failed}" -ne 0 ]]; then
    return 1
  fi
}

repo_root=""
cluster_name=""
context_name=""
source_kubeconfig=""
controller_image="${IMG:-controller:kind}"
ai_worker_image="${AI_WORKER_IMG:-ghcr.io/orka-agents/orka/ai-worker:latest}"
general_worker_image="${GENERAL_WORKER_IMG:-ghcr.io/orka-agents/orka/general-worker:latest}"
harness_wrapper_image="${HARNESS_WRAPPER_IMG:-ghcr.io/orka-agents/orka/agent-harness-wrapper:latest}"
namespace="orka-system"
runtime_dir=""
kustomize_bin=""
controller_gen_bin=""
crd_manifest=""
deployment_manifest=""
preserve_runtime=0
internal_deploy=0
expected_identity_fingerprint=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo)
      [[ $# -ge 2 ]] || die "missing value for --repo"
      repo_root="$2"
      shift 2
      ;;
    --cluster)
      [[ $# -ge 2 ]] || die "missing value for --cluster"
      cluster_name="$2"
      shift 2
      ;;
    --context)
      [[ $# -ge 2 ]] || die "missing value for --context"
      context_name="$2"
      shift 2
      ;;
    --kubeconfig)
      [[ $# -ge 2 ]] || die "missing value for --kubeconfig"
      source_kubeconfig="$2"
      shift 2
      ;;
    --controller-image)
      [[ $# -ge 2 ]] || die "missing value for --controller-image"
      controller_image="$2"
      shift 2
      ;;
    --ai-worker-image)
      [[ $# -ge 2 ]] || die "missing value for --ai-worker-image"
      ai_worker_image="$2"
      shift 2
      ;;
    --general-worker-image)
      [[ $# -ge 2 ]] || die "missing value for --general-worker-image"
      general_worker_image="$2"
      shift 2
      ;;
    --harness-wrapper-image)
      [[ $# -ge 2 ]] || die "missing value for --harness-wrapper-image"
      harness_wrapper_image="$2"
      shift 2
      ;;
    --internal-deploy)
      internal_deploy=1
      shift
      ;;
    --runtime-dir)
      [[ $# -ge 2 ]] || die "missing value for --runtime-dir"
      runtime_dir="$2"
      shift 2
      ;;
    --expected-kind-identity)
      [[ $# -ge 2 ]] || die "missing value for --expected-kind-identity"
      expected_identity_fingerprint="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      die "Unknown argument: $1"
      ;;
  esac
done

validate_image controller "${controller_image}"
validate_image "AI worker" "${ai_worker_image}"
validate_image "general worker" "${general_worker_image}"
validate_image "harness wrapper" "${harness_wrapper_image}"

kind_bin="$(resolve_command "${KIND:-kind}" kind)"
kubectl_bin="$(resolve_command "${KUBECTL:-kubectl}" kubectl)"
make_bin="$(resolve_command "${MAKE:-make}" make)"
python_bin="$(resolve_command "${PYTHON:-python3}" python3)"
container_tool_bin="$(resolve_command "${CONTAINER_TOOL:-docker}" 'container tool')"
dd_bin="$(resolve_command dd dd)"
base64_bin="$(resolve_command base64 base64)"
tr_bin="$(resolve_command tr tr)"

if [[ -z "${repo_root}" ]]; then
  git_bin="$(resolve_command "${GIT:-git}" git)"
  repo_root="$("${git_bin}" rev-parse --show-toplevel 2>/dev/null || pwd)"
fi
repo_root="$(cd "${repo_root}" && pwd -P)"

[[ -f "${repo_root}/Makefile" ]] || die "Not an Orka repository root: missing Makefile in ${repo_root}"
[[ -f "${repo_root}/config/manager/kustomization.yaml" ]] || \
  die "Not an Orka repository root: missing config/manager/kustomization.yaml"
[[ -f "${repo_root}/config/harness-wrapper/kustomization.yaml" ]] || \
  die "Not an Orka repository root: missing config/harness-wrapper/kustomization.yaml"
[[ -d "${repo_root}/workers" ]] || die "Not an Orka repository root: missing workers directory"
e2e_helper="${repo_root}/scripts/e2e-kind-cluster.sh"
[[ -x "${e2e_helper}" ]] || die "Missing executable E2E cluster helper: ${e2e_helper}"

if [[ "${internal_deploy}" == "1" ]]; then
  run_internal_deploy
  exit 0
fi

if [[ -z "${source_kubeconfig}" ]]; then
  kindctl_bin="${KINDCTL:-${repo_root}/.agents/skills/kindctl/bin/kindctl}"
  [[ -x "${kindctl_bin}" ]] || \
    die "No scoped kubeconfig supplied and repo-vendored kindctl is unavailable; pass --kubeconfig"
  source_kubeconfig="$(cd "${repo_root}" && "${kindctl_bin}" path)" || \
    die "kindctl could not resolve a scoped kubeconfig; pass --kubeconfig explicitly"
fi
if [[ "${source_kubeconfig}" == *:* ]]; then
  die "source kubeconfig must be one isolated file, not a path list"
fi
[[ -f "${source_kubeconfig}" ]] || die "Scoped source kubeconfig not found: ${source_kubeconfig}"
source_kubeconfig="$("${python_bin}" -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "${source_kubeconfig}")"
login_home="$(login_home_directory)" || die "failed to resolve the login home directory"
if is_global_kubeconfig_file "${source_kubeconfig}" "${login_home}" "${HOME:-}"; then
  die "refusing to read the global kubeconfig; pass a kindctl-scoped kubeconfig"
fi
validate_isolated_source_kubeconfig "${source_kubeconfig}" || \
  die "source kubeconfig is not an isolated single-cluster config"

if [[ -z "${context_name}" ]]; then
  if [[ -n "${cluster_name}" ]]; then
    context_name="kind-${cluster_name}"
  else
    context_name="$(kubectl_config_value "${source_kubeconfig}" "" config current-context)" || \
      die "failed to read current context from scoped source kubeconfig"
    context_name="$(normalize_single_line "${context_name}")"
  fi
fi

source_cluster="$(context_cluster_for "${source_kubeconfig}" "${context_name}")" || \
  die "kubectl context '${context_name}' was not found in the scoped source kubeconfig"
if [[ -z "${cluster_name}" ]]; then
  if [[ "${source_cluster}" != kind-* ]]; then
    die "Context '${context_name}' targets '${source_cluster}', not a kind cluster; pass --cluster with a scoped kind kubeconfig"
  fi
  cluster_name="${source_cluster#kind-}"
fi
if [[ ! "${cluster_name}" =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  die "kind cluster name contains unsupported characters: ${cluster_name}"
fi
expected_cluster_ref="kind-${cluster_name}"
if [[ "${source_cluster}" != "${expected_cluster_ref}" ]]; then
  die "Context '${context_name}' targets '${source_cluster}', not '${expected_cluster_ref}'"
fi
cluster_exists_exact || die "kind cluster '${cluster_name}' not found by exact name"

runtime_parent="${ORKA_KIND_DEPLOY_RUNTIME_ROOT:-${login_home}/.cache/orka/kind-deploy-runtime}"
runtime_parent="$("${python_bin}" -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "${runtime_parent}")"
if path_is_within "${runtime_parent}" "${repo_root}"; then
  die "runtime directory must be outside the repository: ${runtime_parent}"
fi
mkdir -p "${runtime_parent}"
runtime_dir="$(mktemp -d "${runtime_parent%/}/orka-kind-deploy.XXXXXX")"
chmod 700 "${runtime_dir}"
cleanup_runtime() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n "${runtime_dir}" && -d "${runtime_dir}" ]]; then
    if [[ "${preserve_runtime}" == "1" ]]; then
      error "preserving runtime ownership state for recovery: ${runtime_dir}"
    else
      rm -rf "${runtime_dir}"
    fi
  fi
  exit "${status}"
}
trap cleanup_runtime EXIT
trap 'preserve_runtime=1; exit 130' INT
trap 'preserve_runtime=1; exit 143' TERM

isolated_kubeconfig="${runtime_dir}/target.kubeconfig"
write_kind_kubeconfig "${isolated_kubeconfig}"
validate_exported_kubeconfig "${isolated_kubeconfig}"
source_identity="$(kubeconfig_identity "${source_kubeconfig}" "${context_name}")" || \
  die "failed to read server/CA identity from scoped source context '${context_name}'"
kind_identity="$(kubeconfig_identity "${isolated_kubeconfig}" "${expected_cluster_ref}")" || \
  die "failed to read server/CA identity from exported kind kubeconfig"
if [[ "${source_identity}" != "${kind_identity}" ]]; then
  die "refusing split-cluster deployment: context '${context_name}' and kind cluster '${cluster_name}' have different server/CA identity"
fi
kind_identity_fingerprint="$(hash_text "${kind_identity}")" || die "failed to fingerprint kind cluster identity"

export KUBECONFIG="${isolated_kubeconfig}"
log "Scoped source context: ${context_name}"
run_locked_deploy "${kind_identity_fingerprint}"
