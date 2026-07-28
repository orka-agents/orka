#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../../../.." && pwd)"
deploy_script="${script_dir}/deploy_orka_kind.sh"
e2e_helper="${repo_root}/scripts/e2e-kind-cluster.sh"
test_root="$(mktemp -d)"
test_root="$(cd "${test_root}" && pwd -P)"
cleanup_test_root() {
  if [[ "${KEEP_TEST_ROOT:-0}" == "1" ]]; then
    printf 'Preserved test root: %s\n' "${test_root}" >&2
  else
    rm -rf "${test_root}"
  fi
}
trap cleanup_test_root EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local file="$1"
  local expected="$2"
  grep -Fq -- "${expected}" "${file}" || fail "${file} does not contain: ${expected}"
}

assert_not_contains() {
  local file="$1"
  local unexpected="$2"
  if grep -Fq -- "${unexpected}" "${file}"; then
    fail "${file} unexpectedly contains: ${unexpected}"
  fi
}

assert_path_absent() {
  local path="$1"
  [[ ! -e "${path}" ]] || fail "path should be absent: ${path}"
}

hash_file() {
  local file="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
  else
    shasum -a 256 "${file}" | awk '{print $1}'
  fi
}

path_mode() {
  local path="$1"
  if stat -f '%Lp' "${path}" >/dev/null 2>&1; then
    stat -f '%Lp' "${path}"
  else
    stat -c '%a' "${path}"
  fi
}

snapshot_tree() {
  local root="$1"
  (
    cd "${root}"
    find . -type f -print | LC_ALL=C sort | while IFS= read -r file; do
      printf '%s  ' "${file}"
      hash_file "${file}"
    done
  )
}

write_fake_repo() {
  local root="$1"

  mkdir -p \
    "${root}/config/manager" \
    "${root}/config/harness-wrapper" \
    "${root}/config/crd/bases" \
    "${root}/config/rbac" \
    "${root}/config/default" \
    "${root}/charts/orka/crds" \
    "${root}/scripts" \
    "${root}/test/e2e" \
    "${root}/workers/ai" \
    "${root}/workers/general" \
    "${root}/workers/harness"

  cat >"${root}/Makefile" <<'MAKEFILE'
.PHONY: docker-build-all

docker-build-all:
	@true
MAKEFILE

  cat >"${root}/config/manager/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- manager.yaml
images:
- name: controller
  newName: controller
  newTag: latest
YAML

  cat >"${root}/config/manager/manager.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        image: controller:latest
        args:
        - --leader-elect
        - --ai-worker-image=ghcr.io/orka-agents/orka/ai-worker:latest
        - --general-worker-image=ghcr.io/orka-agents/orka/general-worker:latest
YAML

  cat >"${root}/config/harness-wrapper/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- deployment.yaml
YAML

  cat >"${root}/config/harness-wrapper/deployment.yaml" <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent-harness-wrapper
spec:
  template:
    spec:
      containers:
      - name: wrapper
        image: ghcr.io/orka-agents/orka/agent-harness-wrapper:latest
YAML

  cat >"${root}/config/crd/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
- bases/core.orka.ai_tasks.yaml
YAML

  cat >"${root}/config/crd/bases/core.orka.ai_tasks.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: tasks.core.orka.ai
spec: {}
YAML

  cat >"${root}/config/rbac/role.yaml" <<'YAML'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: manager-role
rules: []
YAML

  cat >"${root}/config/default/kustomization.yaml" <<'YAML'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: orka-system
namePrefix: orka-
resources:
- ../manager
- ../harness-wrapper
YAML

  cp "${root}/config/crd/bases/core.orka.ai_tasks.yaml" \
    "${root}/charts/orka/crds/core.orka.ai_tasks.yaml"
  cp "${e2e_helper}" "${root}/scripts/e2e-kind-cluster.sh"
  chmod +x "${root}/scripts/e2e-kind-cluster.sh"
  : >"${root}/test/e2e/kind-config.yaml"
}

write_fake_commands() {
  local bin_dir="$1"
  mkdir -p "${bin_dir}"

  cat >"${bin_dir}/docker" <<'EOF_DOCKER'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker:%s\n' "$*" >>"${FAKE_LOG}"
EOF_DOCKER

  cat >"${bin_dir}/kind" <<'EOF_KIND'
#!/usr/bin/env bash
set -euo pipefail
printf 'kind:%s\n' "$*" >>"${FAKE_LOG}"
case "${1:-}:${2:-}" in
  get:clusters)
    printf '%s\n' target target-extra
    ;;
  get:kubeconfig)
    printf 'context=kind-target\ncluster=kind-target\nuser=kind-target\nserver=%s\nca=%s\n' \
      "${FAKE_KIND_SERVER:-https://target.example}" \
      "${FAKE_KIND_CA:-ca-target}"
    ;;
  load:docker-image)
    if [[ -n "${FAKE_EXPECT_IMAGE_LOCK:-}" && ! -d "${FAKE_EXPECT_IMAGE_LOCK}" ]]; then
      printf 'image build/load lock was not held for: %s\n' "$*" >&2
      exit 44
    fi
    ;;
  *)
    printf 'unexpected fake kind invocation: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF_KIND

  cat >"${bin_dir}/kubectl" <<'EOF_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

kubeconfig="${KUBECONFIG:-}"
context_override=""
args=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --kubeconfig)
      kubeconfig="${2:?missing kubeconfig value}"
      shift 2
      ;;
    --context)
      context_override="${2:?missing context value}"
      shift 2
      ;;
    *)
      args+=("$1")
      shift
      ;;
  esac
done
set -- "${args[@]}"

read_value() {
  local key="$1"
  [[ -f "${kubeconfig}" ]] || return 1
  sed -n "s/^${key}=//p" "${kubeconfig}" | head -n 1
}

context="$(read_value context || true)"
cluster="$(read_value cluster || true)"
user="$(read_value user || true)"
server="$(read_value server || true)"
ca="$(read_value ca || true)"
ca_path="$(read_value ca_path || true)"
if [[ -n "${context_override}" ]]; then
  context="${context_override}"
fi
joined="$*"

if [[ "${1:-}" == "config" && "${2:-}" == "current-context" ]]; then
  printf 'kubectl-config:%s kubeconfig=%s\n' "${joined}" "${kubeconfig}" >>"${FAKE_LOG}"
  printf '%s\n' "${context}"
  exit 0
fi
if [[ "${1:-}" == "config" && "${2:-}" == "view" ]]; then
  printf 'kubectl-config:%s kubeconfig=%s\n' "${joined}" "${kubeconfig}" >>"${FAKE_LOG}"
  if [[ "${joined}" == *'.contexts[*]'* ]]; then
    printf '%s\n' "${context}"
  elif [[ "${joined}" == *'.clusters[*]'* ]]; then
    printf '%s\n' "${cluster}"
  elif [[ "${joined}" == *'.users[*]'* ]]; then
    printf '%s\n' "${user}"
  elif [[ "${joined}" == *'.contexts[0].context.cluster'* ]]; then
    printf '%s' "${cluster}"
  elif [[ "${joined}" == *'.clusters[0].cluster.server'* && "${joined}" == *'certificate-authority-data'* ]]; then
    printf '%s\n' "${server}"
    printf '%s' "${ca}" | base64 | tr -d '\n'
  elif [[ "${joined}" == *'.clusters[0].cluster.server'* ]]; then
    printf '%s' "${server}"
  elif [[ "${joined}" == *'.clusters[0].cluster.certificate-authority-data'* ]]; then
    if [[ -n "${ca}" ]]; then
      printf '%s' "${ca}" | base64 | tr -d '\n'
    fi
  elif [[ "${joined}" == *'.clusters[0].cluster.certificate-authority}'* ]]; then
    printf '%s' "${ca_path}"
  else
    printf 'unexpected fake kubectl config view: %s\n' "${joined}" >&2
    exit 2
  fi
  exit 0
fi

printf 'kubectl:%s kubeconfig=%s context=%s cluster=%s server=%s\n' \
  "${joined}" "${kubeconfig}" "${context}" "${cluster}" "${server}" >>"${FAKE_LOG}"

if [[ -n "${FAKE_KUBECTL_FAIL_MATCH:-}" && "${joined}" == *"${FAKE_KUBECTL_FAIL_MATCH}"* ]]; then
  exit 42
fi
if [[ -n "${FAKE_EXPECT_OPERATION_LOCK:-}" && \
  ( "${joined}" == apply\ -f\ * || "${joined}" == *" apply -f "* || "${joined}" == *" rollout "* ) && \
  ! -d "${FAKE_EXPECT_OPERATION_LOCK}" ]]; then
  printf 'cluster operation lock was not held for: %s\n' "${joined}" >&2
  exit 43
fi

if [[ "${joined}" == create\ namespace\ * ]]; then
  cat <<'YAML'
apiVersion: v1
kind: Namespace
metadata:
  name: orka-system
YAML
elif [[ "${joined}" == *" get secret harness-wrapper-auth"* ]]; then
  if [[ -f "${FAKE_SECRET_MARKER}" ]]; then
    exit 0
  fi
  exit 1
elif [[ "${joined}" == *" create secret generic harness-wrapper-auth "* ]]; then
  : >"${FAKE_SECRET_MARKER}"
elif [[ "${joined}" == apply\ -f\ * || "${joined}" == *" apply -f "* ]]; then
  manifest="${joined#apply -f }"
  manifest="${manifest##* apply -f }"
  if [[ -f "${manifest}" ]] && grep -Fq 'orka-controller-manager' "${manifest}"; then
    cp "${manifest}" "${FAKE_APPLIED_MANIFEST}"
  fi
elif [[ "${joined}" == *" rollout restart deployment/"* || "${joined}" == *" rollout status deployment/"* ]]; then
  :
elif [[ "${joined}" == *" get pods,svc,deploy"* ]]; then
  printf 'mock resources ready\n'
else
  printf 'unexpected fake kubectl invocation: %s\n' "${joined}" >&2
  exit 2
fi
EOF_KUBECTL

  cat >"${bin_dir}/make" <<'EOF_MAKE'
#!/usr/bin/env bash
set -euo pipefail
printf 'make:%s\n' "$*" >>"${FAKE_LOG}"
joined="$*"
if [[ "${joined}" == *"docker-build-all"* && -n "${FAKE_EXPECT_IMAGE_LOCK:-}" && ! -d "${FAKE_EXPECT_IMAGE_LOCK}" ]]; then
  printf 'image build/load lock was not held for: %s\n' "${joined}" >&2
  exit 44
fi
if [[ -n "${FAKE_MAKE_FAIL_MATCH:-}" && "${joined}" == *"${FAKE_MAKE_FAIL_MATCH}"* ]]; then
  exit 41
fi
if [[ "${joined}" == *'test-e2e-setup-only'* ]]; then
  mkdir -p "${E2E_CLUSTER_STATE_DIR}"
  printf 'legacy state\n' >"${E2E_CLUSTER_STATE_DIR}/legacy"
  mkdir -p "${E2E_CLUSTER_LOCK_ROOT}/kind-target.lease"
  printf '%s\n' "${E2E_CLUSTER_STATE_DIR}" >"${E2E_CLUSTER_LOCK_ROOT}/kind-target.lease/state_dir"
fi
if [[ "${joined}" == *'install deploy'* ]]; then
  printf '\n# legacy manager mutation\n' >>"${FAKE_REPO}/config/manager/kustomization.yaml"
  printf '\n# legacy harness mutation\n' >>"${FAKE_REPO}/config/harness-wrapper/kustomization.yaml"
  printf '\n# legacy generated mutation\n' >>"${FAKE_REPO}/config/crd/bases/core.orka.ai_tasks.yaml"
  printf '\n# legacy chart mutation\n' >>"${FAKE_REPO}/charts/orka/crds/core.orka.ai_tasks.yaml"
fi
EOF_MAKE

  cat >"${bin_dir}/controller-gen" <<'EOF_CONTROLLER_GEN'
#!/usr/bin/env bash
set -euo pipefail
printf 'controller-gen:%s\n' "$*" >>"${FAKE_LOG}"
crd_dir=""
rbac_dir=""
for arg in "$@"; do
  case "${arg}" in
    output:crd:artifacts:config=*) crd_dir="${arg#*=}" ;;
    output:rbac:artifacts:config=*) rbac_dir="${arg#*=}" ;;
  esac
done
[[ -n "${crd_dir}" && -n "${rbac_dir}" ]] || {
  printf 'controller-gen outputs were not fully redirected\n' >&2
  exit 2
}
case "${crd_dir}" in
  "${FAKE_REPO}"/*) printf 'CRD output escaped into repository\n' >&2; exit 2 ;;
esac
case "${rbac_dir}" in
  "${FAKE_REPO}"/*) printf 'RBAC output escaped into repository\n' >&2; exit 2 ;;
esac
printf '\n# generated in temporary config\n' >>"${crd_dir}/core.orka.ai_tasks.yaml"
printf '\n# generated in temporary config\n' >>"${rbac_dir}/role.yaml"
EOF_CONTROLLER_GEN

  cat >"${bin_dir}/kustomize" <<'EOF_KUSTOMIZE'
#!/usr/bin/env bash
set -euo pipefail
printf 'kustomize:%s cwd=%s\n' "$*" "$(pwd)" >>"${FAKE_LOG}"
case "${1:-}" in
  edit)
    [[ "${2:-}" == "set" && "${3:-}" == "image" ]] || exit 2
    mapping="${4:?missing image mapping}"
    case "${mapping}" in
      controller=*) printf '%s\n' "${mapping#*=}" >.fake-controller-image ;;
      ghcr.io/orka-agents/orka/agent-harness-wrapper=*)
        printf '%s\n' "${mapping#*=}" >.fake-harness-image
        ;;
      *) printf 'unexpected image mapping: %s\n' "${mapping}" >&2; exit 2 ;;
    esac
    ;;
  build)
    target="${2:?missing build target}"
    if [[ "$(basename "${target}")" == "crd" ]]; then
      cat "${target}"/bases/*.yaml
      exit 0
    fi
    config_root="$(cd "${target}/.." && pwd)"
    controller_image="$(cat "${config_root}/manager/.fake-controller-image")"
    harness_image="$(cat "${config_root}/harness-wrapper/.fake-harness-image")"
    ai_image="$(sed -n 's/^[[:space:]]*-[[:space:]]*--ai-worker-image=//p' "${config_root}/manager/manager.yaml")"
    general_image="$(sed -n 's/^[[:space:]]*-[[:space:]]*--general-worker-image=//p' "${config_root}/manager/manager.yaml")"
    cat <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orka-controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        image: ${controller_image}
        args:
        - --ai-worker-image=${ai_image}
        - --general-worker-image=${general_image}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orka-agent-harness-wrapper
spec:
  template:
    spec:
      containers:
      - name: wrapper
        image: ${harness_image}
YAML
    ;;
  *)
    printf 'unexpected fake kustomize invocation: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF_KUSTOMIZE

  chmod +x "${bin_dir}"/*
}

begin_case() {
  local name="$1"
  CASE_DIR="${test_root}/${name}"
  REPO_DIR="${CASE_DIR}/repo"
  BIN_DIR="${CASE_DIR}/bin"
  TMP_CASE_DIR="${CASE_DIR}/tmp"
  FAKE_LOG="${CASE_DIR}/commands.log"
  SOURCE_KUBECONFIG="${CASE_DIR}/source.kubeconfig"
  AMBIENT_KUBECONFIG="${CASE_DIR}/ambient.kubeconfig"
  HOME_DIR="${CASE_DIR}/home"
  DEPLOY_KUBECONFIG="${SOURCE_KUBECONFIG}"
  EXTERNAL_STATE_DIR="${CASE_DIR}/external-e2e-state"
  EXTERNAL_LOCK_ROOT="${CASE_DIR}/external-e2e-locks"
  IMAGE_LOCK_ROOT="${CASE_DIR}/image-locks"
  APPLIED_MANIFEST="${CASE_DIR}/applied.yaml"
  SECRET_MARKER="${CASE_DIR}/secret-created"
  OUTPUT_FILE="${CASE_DIR}/output.log"

  mkdir -p "${CASE_DIR}" "${TMP_CASE_DIR}" "${HOME_DIR}/.kube"
  : >"${FAKE_LOG}"
  write_fake_repo "${REPO_DIR}"
  write_fake_commands "${BIN_DIR}"
  printf 'context=kind-target\ncluster=kind-target\nuser=kind-target\nserver=https://target.example\nca=ca-target\n' >"${SOURCE_KUBECONFIG}"
  printf 'context=kind-decoy\ncluster=kind-decoy\nuser=kind-decoy\nserver=https://ambient-decoy.example\nca=ca-ambient-decoy\n' >"${AMBIENT_KUBECONFIG}"
  cp "${SOURCE_KUBECONFIG}" "${HOME_DIR}/.kube/config"
  unset FAKE_KUBECTL_FAIL_MATCH FAKE_MAKE_FAIL_MATCH TEST_CONTROLLER_IMAGE TEST_AI_WORKER_IMAGE TEST_GENERAL_WORKER_IMAGE TEST_HARNESS_WRAPPER_IMAGE
}

run_deploy() {
  env \
    PATH="${BIN_DIR}:${PATH}" \
    KIND="${BIN_DIR}/kind" \
    KUBECTL="${BIN_DIR}/kubectl" \
    MAKE="${BIN_DIR}/make" \
    KUSTOMIZE="${BIN_DIR}/kustomize" \
    CONTROLLER_GEN="${BIN_DIR}/controller-gen" \
    HOME="${HOME_DIR}" \
    KUBECONFIG="${AMBIENT_KUBECONFIG}" \
    TMPDIR="${TMP_CASE_DIR}" \
    E2E_CLUSTER_STATE_DIR="${EXTERNAL_STATE_DIR}" \
    E2E_CLUSTER_LOCK_ROOT="${EXTERNAL_LOCK_ROOT}" \
    ORKA_KIND_DEPLOY_CLUSTER_LOCK_ROOT="${EXTERNAL_LOCK_ROOT}" \
    ORKA_KIND_DEPLOY_IMAGE_LOCK_ROOT="${IMAGE_LOCK_ROOT}" \
    ORKA_KIND_DEPLOY_RUNTIME_ROOT="${TMP_CASE_DIR}" \
    IMG="${TEST_CONTROLLER_IMAGE:-controller:test}" \
    AI_WORKER_IMG="${TEST_AI_WORKER_IMAGE:-registry.example/ai:test}" \
    GENERAL_WORKER_IMG="${TEST_GENERAL_WORKER_IMAGE:-registry.example/general:test}" \
    HARNESS_WRAPPER_IMG="${TEST_HARNESS_WRAPPER_IMAGE:-registry.example/harness:test}" \
    FAKE_REPO="${REPO_DIR}" \
    FAKE_LOG="${FAKE_LOG}" \
    FAKE_APPLIED_MANIFEST="${APPLIED_MANIFEST}" \
    FAKE_SECRET_MARKER="${SECRET_MARKER}" \
    FAKE_KUBECTL_FAIL_MATCH="${FAKE_KUBECTL_FAIL_MATCH:-}" \
    FAKE_MAKE_FAIL_MATCH="${FAKE_MAKE_FAIL_MATCH:-}" \
    FAKE_EXPECT_OPERATION_LOCK="${EXTERNAL_LOCK_ROOT}/kind-target.operation" \
    FAKE_EXPECT_IMAGE_LOCK="${IMAGE_LOCK_ROOT}/build-load.operation" \
    "${deploy_script}" \
      --repo "${REPO_DIR}" \
      --kubeconfig "${DEPLOY_KUBECONFIG}" \
      --cluster target \
      --controller-image controller:test \
      >"${OUTPUT_FILE}" 2>&1
}

assert_repo_unchanged() {
  local before="$1"
  local after
  after="$(snapshot_tree "${REPO_DIR}")"
  [[ "${before}" == "${after}" ]] || {
    diff -u <(printf '%s\n' "${before}") <(printf '%s\n' "${after}") >&2 || true
    fail "repository bytes changed"
  }
}

assert_no_e2e_residue() {
  assert_path_absent "${EXTERNAL_STATE_DIR}"
  assert_path_absent "${EXTERNAL_LOCK_ROOT}/kind-target.lease"
  assert_path_absent "${EXTERNAL_LOCK_ROOT}/kind-target.operation"
  assert_path_absent "${IMAGE_LOCK_ROOT}/build-load.operation"
  if find "${TMP_CASE_DIR}" -type f \( -name target.kubeconfig -o -name state_dir -o -name fingerprint \) -print -quit | grep -q .; then
    fail "temporary E2E state or lease residue remains under ${TMP_CASE_DIR}"
  fi
}

assert_identity_mismatch_rejected() {
  local name="$1"
  local server="$2"
  local ca="$3"

  begin_case "${name}"
  printf 'context=kind-target\ncluster=kind-target\nuser=kind-target\nserver=%s\nca=%s\n' \
    "${server}" "${ca}" >"${SOURCE_KUBECONFIG}"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "identity mismatch unexpectedly deployed"
  fi

  assert_contains "${OUTPUT_FILE}" "different server/CA identity"
  assert_not_contains "${FAKE_LOG}" "make:"
  assert_not_contains "${FAKE_LOG}" "kubectl:apply"
  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
}

test_server_identity_mismatch_stops_before_build_and_apply() {
  assert_identity_mismatch_rejected server-mismatch https://decoy.example ca-target
}

test_ca_identity_mismatch_stops_before_build_and_apply() {
  assert_identity_mismatch_rejected ca-mismatch https://target.example ca-decoy
}

test_global_kubeconfig_is_rejected_before_cluster_access() {
  begin_case global-kubeconfig
  DEPLOY_KUBECONFIG="${HOME_DIR}/.kube/config"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "global kubeconfig unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "refusing to read the global kubeconfig"
  assert_not_contains "${FAKE_LOG}" "kind:"
  assert_not_contains "${FAKE_LOG}" "kubectl:"
  assert_not_contains "${FAKE_LOG}" "kubectl-config:"
  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
}

test_global_kubeconfig_hardlink_is_rejected() {
  begin_case global-kubeconfig-hardlink
  DEPLOY_KUBECONFIG="${CASE_DIR}/global-hardlink.kubeconfig"
  ln "${HOME_DIR}/.kube/config" "${DEPLOY_KUBECONFIG}"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "hard link to global kubeconfig unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "refusing to read the global kubeconfig"
  assert_not_contains "${FAKE_LOG}" "kind:"
  assert_not_contains "${FAKE_LOG}" "kubectl-config:"
  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
}

test_ca_file_cannot_reference_global_kubeconfig() {
  begin_case global-ca-reference
  printf 'context=kind-target\ncluster=kind-target\nuser=kind-target\nserver=https://target.example\nca=\nca_path=%s\n' \
    "${HOME_DIR}/.kube/config" >"${SOURCE_KUBECONFIG}"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "global kubeconfig CA reference unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "refusing to read the global kubeconfig as certificate authority data"
  assert_not_contains "${FAKE_LOG}" "make:"
  assert_not_contains "${FAKE_LOG}" "kubectl:apply"
  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
}

test_runtime_root_inside_repo_is_rejected() {
  begin_case runtime-in-repo
  TMP_CASE_DIR="${REPO_DIR}/runtime"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "runtime root inside repository unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "runtime directory must be outside the repository"
  assert_not_contains "${FAKE_LOG}" "make:"
  assert_not_contains "${FAKE_LOG}" "kubectl:apply"
  assert_repo_unchanged "${before}"
  assert_path_absent "${TMP_CASE_DIR}"
}

test_runtime_root_case_alias_inside_repo_is_rejected() {
  begin_case runtime-case-alias
  case_alias_repo="/PRIVATE${REPO_DIR#/private}"
  if [[ "${case_alias_repo}" == "${REPO_DIR}" ]]; then
    return 0
  fi
  TMP_CASE_DIR="${case_alias_repo}/runtime"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "case-alias runtime root inside repository unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "runtime directory must be outside the repository"
  assert_repo_unchanged "${before}"
  assert_path_absent "${REPO_DIR}/runtime"
}

test_symlinked_image_lock_root_inside_repo_is_rejected() {
  begin_case image-lock-symlink
  mkdir -p "${REPO_DIR}/.image-lock-target"
  printf 'sentinel\n' >"${REPO_DIR}/.image-lock-target/sentinel"
  IMAGE_LOCK_ROOT="${CASE_DIR}/image-lock-link"
  ln -s "${REPO_DIR}/.image-lock-target" "${IMAGE_LOCK_ROOT}"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "symlinked image lock root inside repository unexpectedly accepted"
  fi

  assert_contains "${OUTPUT_FILE}" "image lock root must be outside the repository"
  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
  rm -f "${IMAGE_LOCK_ROOT}"
  rm -rf "${REPO_DIR}/.image-lock-target"
}

test_internal_mode_requires_real_helper_lock_ownership() {
  begin_case forged-internal
  local forged_runtime="${TMP_CASE_DIR}/forged-runtime"
  mkdir -p "${forged_runtime}/e2e-state"
  cp "${SOURCE_KUBECONFIG}" "${forged_runtime}/e2e-state/target.kubeconfig"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  if env \
    PATH="${BIN_DIR}:${PATH}" \
    HOME="${HOME_DIR}" \
    KIND="${BIN_DIR}/kind" \
    KUBECTL="${BIN_DIR}/kubectl" \
    MAKE="${BIN_DIR}/make" \
    KUSTOMIZE="${BIN_DIR}/kustomize" \
    CONTROLLER_GEN="${BIN_DIR}/controller-gen" \
    KUBECONFIG="${forged_runtime}/e2e-state/target.kubeconfig" \
    E2E_KIND_TARGET_READY=1 \
    E2E_KIND_OPERATION_TOKEN=forged-token \
    E2E_KIND_EXPECTED_CONTEXT=kind-target \
    E2E_KIND_EXPECTED_CLUSTER=kind-target \
    E2E_CLUSTER_LOCK_ROOT="${EXTERNAL_LOCK_ROOT}" \
    FAKE_REPO="${REPO_DIR}" \
    FAKE_LOG="${FAKE_LOG}" \
    "${deploy_script}" \
      --internal-deploy \
      --repo "${REPO_DIR}" \
      --cluster target \
      --runtime-dir "${forged_runtime}" \
      --expected-kind-identity forged-fingerprint \
      >"${OUTPUT_FILE}" 2>&1; then
    fail "forged internal deploy unexpectedly succeeded"
  fi

  assert_contains "${OUTPUT_FILE}" "does not own the held operation lock"
  assert_not_contains "${FAKE_LOG}" "kubectl-config:"
  assert_not_contains "${FAKE_LOG}" "make:"
  assert_repo_unchanged "${before}"
  rm -rf "${forged_runtime}"
}

test_owned_cleanup_residue_preserves_runtime_metadata() {
  begin_case owned-cleanup-residue
  cat >"${REPO_DIR}/scripts/e2e-kind-cluster.sh" <<'HELPER'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "${E2E_CLUSTER_STATE_DIR}"
printf 'owned state\n' >"${E2E_CLUSTER_STATE_DIR}/status"
mkdir -p \
  "${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.lease" \
  "${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation"
printf '%s\n' "${E2E_CLUSTER_STATE_DIR}" >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.lease/state_dir"
printf '%s\n' "${E2E_CLUSTER_STATE_DIR}" >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation/state_dir"
printf 'owned-token\n' >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation/token"
exit 45
HELPER
  chmod +x "${REPO_DIR}/scripts/e2e-kind-cluster.sh"
  local before preserved_runtime
  before="$(snapshot_tree "${REPO_DIR}")"

  if run_deploy; then
    fail "helper cleanup residue unexpectedly succeeded"
  fi

  assert_contains "${OUTPUT_FILE}" "E2E helper left persistent state"
  assert_contains "${OUTPUT_FILE}" "preserving runtime ownership state for recovery"
  preserved_runtime="$(find "${TMP_CASE_DIR}" -maxdepth 1 -type d -name 'orka-kind-deploy.*' -print -quit)"
  [[ -n "${preserved_runtime}" ]] || fail "owned cleanup residue did not preserve runtime metadata"
  [[ -e "${preserved_runtime}/e2e-state/status" ]] || fail "preserved runtime is missing E2E ownership state"
  assert_repo_unchanged "${before}"

  rm -rf \
    "${preserved_runtime}" \
    "${EXTERNAL_LOCK_ROOT}/kind-target.lease" \
    "${EXTERNAL_LOCK_ROOT}/kind-target.operation"
}

test_successor_locks_are_not_misattributed() {
  begin_case successor-locks
  cat >"${REPO_DIR}/scripts/e2e-kind-cluster.sh" <<'HELPER'
#!/usr/bin/env bash
set -euo pipefail
runtime_dir="$(dirname "${E2E_CLUSTER_STATE_DIR}")"
printf 'original-token\n' >"${runtime_dir}/e2e-operation-token"
mkdir -p \
  "${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.lease" \
  "${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation"
printf '/successor/state\n' >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.lease/state_dir"
printf '/successor/state\n' >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation/state_dir"
printf 'successor-token\n' >"${E2E_CLUSTER_LOCK_ROOT}/kind-${KIND_CLUSTER}.operation/token"
HELPER
  chmod +x "${REPO_DIR}/scripts/e2e-kind-cluster.sh"
  local before
  before="$(snapshot_tree "${REPO_DIR}")"

  run_deploy

  assert_not_contains "${OUTPUT_FILE}" "left this deployment's"
  assert_repo_unchanged "${before}"
  assert_path_absent "${TMP_CASE_DIR}/e2e-state"

  rm -rf \
    "${EXTERNAL_LOCK_ROOT}/kind-target.lease" \
    "${EXTERNAL_LOCK_ROOT}/kind-target.operation"
}

test_success_is_clean_complete_and_scoped() {
  begin_case success
  local before source_before
  before="$(snapshot_tree "${REPO_DIR}")"
  source_before="$(cat "${SOURCE_KUBECONFIG}")"

  run_deploy

  assert_repo_unchanged "${before}"
  [[ "$(cat "${SOURCE_KUBECONFIG}")" == "${source_before}" ]] || fail "source kubeconfig was modified"
  assert_no_e2e_residue

  assert_contains "${FAKE_LOG}" "make:-C ${REPO_DIR}"
  assert_contains "${FAKE_LOG}" "docker-build-all"
  assert_not_contains "${FAKE_LOG}" "test-e2e-setup-only"
  assert_not_contains "${FAKE_LOG}" "install deploy"
  assert_contains "${FAKE_LOG}" "kind:load docker-image controller:test --name target"
  assert_contains "${FAKE_LOG}" "kind:load docker-image registry.example/ai:test --name target"
  assert_contains "${FAKE_LOG}" "kind:load docker-image registry.example/general:test --name target"
  assert_contains "${FAKE_LOG}" "kind:load docker-image registry.example/harness:test --name target"

  assert_contains "${FAKE_LOG}" "rollout restart deployment/orka-controller-manager"
  assert_contains "${FAKE_LOG}" "rollout status deployment/orka-controller-manager"
  assert_contains "${FAKE_LOG}" "rollout restart deployment/orka-agent-harness-wrapper"
  assert_contains "${FAKE_LOG}" "rollout status deployment/orka-agent-harness-wrapper"
  assert_not_contains "${FAKE_LOG}" "kubeconfig=${AMBIENT_KUBECONFIG}"
  if grep -F 'kubectl:apply' "${FAKE_LOG}" | grep -Fq "kubeconfig=${SOURCE_KUBECONFIG}"; then
    fail "apply used the source kubeconfig instead of the isolated kind export"
  fi
  if grep -F 'kubectl:apply' "${FAKE_LOG}" | grep -Fvq 'server=https://target.example'; then
    fail "an apply command was not bound to the selected kind server identity"
  fi

  assert_contains "${APPLIED_MANIFEST}" "image: controller:test"
  assert_contains "${APPLIED_MANIFEST}" "--ai-worker-image=registry.example/ai:test"
  assert_contains "${APPLIED_MANIFEST}" "--general-worker-image=registry.example/general:test"
  assert_contains "${APPLIED_MANIFEST}" "image: registry.example/harness:test"
}

test_caller_owned_roots_keep_permissions() {
  begin_case caller-owned-root-permissions
  mkdir -p "${IMAGE_LOCK_ROOT}"
  chmod 755 "${TMP_CASE_DIR}" "${IMAGE_LOCK_ROOT}"
  local runtime_mode image_lock_mode
  runtime_mode="$(path_mode "${TMP_CASE_DIR}")"
  image_lock_mode="$(path_mode "${IMAGE_LOCK_ROOT}")"

  run_deploy

  [[ "$(path_mode "${TMP_CASE_DIR}")" == "${runtime_mode}" ]] || \
    fail "caller-owned runtime root permissions changed"
  [[ "$(path_mode "${IMAGE_LOCK_ROOT}")" == "${image_lock_mode}" ]] || \
    fail "caller-owned image lock root permissions changed"
  assert_no_e2e_residue
}

test_image_metacharacters_are_rejected_before_build() {
  begin_case image-metacharacters
  TEST_AI_WORKER_IMAGE="x;>${SECRET_MARKER}"

  if run_deploy; then
    fail "image reference with shell metacharacters unexpectedly succeeded"
  fi

  assert_contains "${OUTPUT_FILE}" "AI worker image contains unsupported characters"
  assert_not_contains "${FAKE_LOG}" "make:"
  assert_path_absent "${SECRET_MARKER}"
}

test_build_failure_is_clean_and_cleans_e2e_state() {
  begin_case build-failure
  local before
  before="$(snapshot_tree "${REPO_DIR}")"
  FAKE_MAKE_FAIL_MATCH="docker-build-all"

  if run_deploy; then
    fail "image build failure unexpectedly succeeded"
  fi

  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
  assert_contains "${FAKE_LOG}" "docker-build-all"
  assert_not_contains "${FAKE_LOG}" "kubectl:apply"
}

test_failure_is_clean_and_cleans_e2e_state() {
  begin_case rollout-failure
  local before
  before="$(snapshot_tree "${REPO_DIR}")"
  FAKE_KUBECTL_FAIL_MATCH="rollout status deployment/orka-agent-harness-wrapper"

  if run_deploy; then
    fail "harness-wrapper rollout failure unexpectedly succeeded"
  fi

  assert_repo_unchanged "${before}"
  assert_no_e2e_residue
  assert_contains "${FAKE_LOG}" "rollout restart deployment/orka-controller-manager"
  assert_contains "${FAKE_LOG}" "rollout restart deployment/orka-agent-harness-wrapper"
  assert_contains "${FAKE_LOG}" "rollout status deployment/orka-controller-manager"
  assert_contains "${FAKE_LOG}" "rollout status deployment/orka-agent-harness-wrapper"
}

test_server_identity_mismatch_stops_before_build_and_apply
test_ca_identity_mismatch_stops_before_build_and_apply
test_global_kubeconfig_is_rejected_before_cluster_access
test_global_kubeconfig_hardlink_is_rejected
test_ca_file_cannot_reference_global_kubeconfig
test_runtime_root_inside_repo_is_rejected
test_runtime_root_case_alias_inside_repo_is_rejected
test_symlinked_image_lock_root_inside_repo_is_rejected
test_internal_mode_requires_real_helper_lock_ownership
test_owned_cleanup_residue_preserves_runtime_metadata
test_successor_locks_are_not_misattributed
test_success_is_clean_complete_and_scoped
test_caller_owned_roots_keep_permissions
test_image_metacharacters_are_rejected_before_build
test_build_failure_is_clean_and_cleans_e2e_state
test_failure_is_clean_and_cleans_e2e_state

printf 'PASS: Orka kind deploy hardening tests\n'
