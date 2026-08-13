#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/scripts/upgrade-orka-crds.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/upgrade-orka-crds-test.XXXXXX")"

cleanup() {
  if [[ "${KEEP_TEST_TMP:-0}" == "1" ]]; then
    echo "kept test fixtures at ${test_root}" >&2
  else
    rm -rf "${test_root}"
  fi
}
trap cleanup EXIT

pass_count=0
fail_count=0

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$1" | awk '{print tolower($1)}'
  fi
}


sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print tolower($1)}'
  else
    shasum -a 256 | awk '{print tolower($1)}'
  fi
}

fake_kubeconfig_json='{"clusters":[{"name":"test-cluster","cluster":{"server":"https://test.example.invalid","certificate-authority-data":"VEVTVA=="}}]}'
fake_cluster_uid='test-cluster-uid'
fake_api_server_identity_sha256="$(
  printf '%s\n' "${fake_kubeconfig_json}"     | jq -c '
        .clusters
        | .[0].cluster
        | {
            server: (.server // null),
            certificateAuthorityData: (.["certificate-authority-data"] // null),
            insecureSkipTLSVerify: (.["insecure-skip-tls-verify"] // null),
            tlsServerName: (.["tls-server-name"] // null),
            proxyURL: (.["proxy-url"] // null),
            disableCompression: (.["disable-compression"] // null)
          }
      '     | sha256_stdin
)"

write_marker() {
  local backup="$1"
  local marker="$2"
  local kind="$3"
  cat >"${marker}" <<MARKER
format=orka-backup-verification-v1
kind=${kind}
context=test-context
cluster_uid=${fake_cluster_uid}
api_server_identity_sha256=${fake_api_server_identity_sha256}
sha256=$(sha256_file "${backup}")
verified=true
verified_at=2026-07-25T08:00:00Z
MARKER
}

replace_marker_field() {
  local marker="$1"
  local key="$2"
  local value="$3"
  awk -F= -v key="${key}" -v value="${value}" '
    $1 == key { print key "=" value; next }
    { print }
  ' "${marker}" >"${marker}.next"
  mv "${marker}.next" "${marker}"
}

write_list() {
  local path="$1"
  local items="$2"
  cat >"${path}" <<JSON
{"apiVersion":"v1","kind":"List","items":${items}}
JSON
}

historical_items() {
  cat <<'JSON'
[
  {
    "apiVersion":"core.orka.ai/v1alpha1",
    "kind":"AgentRuntime",
    "metadata":{"namespace":"gateway-ns","name":"legacy-runtime"},
    "spec":{"contractVersion":"orka.harness.v1"}
  },
  {
    "apiVersion":"core.orka.ai/v1alpha1",
    "kind":"Agent",
    "metadata":{"namespace":"gateway-ns","name":"legacy-agent"},
    "spec":{"runtime":{"runtimeRef":{"name":"legacy-runtime"}}}
  },
  {
    "apiVersion":"gateway.orka.ai/v1alpha1",
    "kind":"GatewayBinding",
    "metadata":{"namespace":"gateway-ns","name":"legacy-binding"},
    "spec":{"agentRef":{"name":"legacy-agent"},"gatewayRef":{"name":"legacy-gateway"}}
  }
]
JSON
}

create_fake_kubectl() {
  local case_dir="$1"
  mkdir -p "${case_dir}/bin"
  cat >"${case_dir}/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
set -Eeuo pipefail

printf '%s\n' "$*" >>"${FAKE_KUBECTL_LOG}"

if [[ "${1:-}" == "--context" ]]; then
  shift 2
fi

command="${1:-}"
shift || true

case "${command}" in
  get)
    if [[ "${1:-}" == "--raw=/version" ]]; then
      printf '%s\n' '{"gitVersion":"v1.34.0"}'
      exit 0
    fi
    if [[ "${1:-}" == "namespace" && "${2:-}" == "kube-system" ]]; then
      printf '%s\n' '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kube-system","uid":"test-cluster-uid"}}'
      exit 0
    fi

    resource="${1:-}"
    shift || true
    if [[ " $* " == *" -A "* && " $* " == *" custom-columns="* ]]; then
      awk -F '\t' -v resource="${resource}" '$1 == resource { print $2, $3, $4 }' "${FAKE_WRAPPER_STATE}"
      exit 0
    fi

    case "${resource}" in
      agentruntimes.core.orka.ai) cat "${FAKE_RUNTIME_JSON}" ;;
      agents.core.orka.ai) cat "${FAKE_AGENT_JSON}" ;;
      tasks.core.orka.ai) cat "${FAKE_TASK_JSON}" ;;
      gatewaybindings.gateway.orka.ai) cat "${FAKE_BINDING_JSON}" ;;
      deployment.apps|statefulset.apps) cat "${FAKE_WORKLOAD_JSON}" ;;
      *)
        echo "unexpected fake kubectl get: ${resource} $*" >&2
        exit 1
        ;;
    esac
    ;;
  config)
    if [[ "${1:-}" == "view" ]]; then
      printf '%s\n' '{"clusters":[{"name":"test-cluster","cluster":{"server":"https://test.example.invalid","certificate-authority-data":"VEVTVA=="}}]}'
    else
      echo "unexpected fake kubectl config request: $*" >&2
      exit 1
    fi
    ;;
  api-resources)
    if [[ " $* " == *" --api-group=core.orka.ai "* ]]; then
      printf '%s\n' 'agentruntimes.core.orka.ai' 'agents.core.orka.ai' 'tasks.core.orka.ai'
    elif [[ " $* " == *" --api-group=gateway.orka.ai "* ]]; then
      printf '%s\n' 'gatewaybindings.gateway.orka.ai'
    else
      echo "unexpected api-resources request: $*" >&2
      exit 1
    fi
    ;;
  delete)
    resource="${1:-}"
    shift || true
    namespace=""
    name=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        -n)
          namespace="$2"
          shift 2
          ;;
        --wait=true)
          shift
          ;;
        *)
          name="$1"
          shift
          ;;
      esac
    done
    printf '%s\t%s\t%s\n' "${resource}" "${namespace}" "${name}" >>"${FAKE_DELETE_LOG}"
    awk -F '\t' -v resource="${resource}" -v namespace="${namespace}" -v name="${name}" \
      '!( $1 == resource && $2 == namespace && $3 == name )' "${FAKE_WRAPPER_STATE}" >"${FAKE_WRAPPER_STATE}.next"
    mv "${FAKE_WRAPPER_STATE}.next" "${FAKE_WRAPPER_STATE}"
    ;;
  apply)
    dry_run=false
    for argument in "$@"; do
      if [[ "${argument}" == "--dry-run=server" ]]; then
        dry_run=true
      fi
    done
    if [[ "${dry_run}" == "true" ]]; then
      printf '%s\n' dry-run >>"${FAKE_APPLY_LOG}"
      if [[ -n "${FAKE_POST_DRY_RUN_RUNTIME_JSON:-}" && -f "${FAKE_POST_DRY_RUN_RUNTIME_JSON}" ]]; then
        cp "${FAKE_POST_DRY_RUN_RUNTIME_JSON}" "${FAKE_RUNTIME_JSON}"
      fi
      if [[ -n "${FAKE_POST_DRY_RUN_AGENT_JSON:-}" && -f "${FAKE_POST_DRY_RUN_AGENT_JSON}" ]]; then
        cp "${FAKE_POST_DRY_RUN_AGENT_JSON}" "${FAKE_AGENT_JSON}"
      fi
      if [[ -n "${FAKE_POST_DRY_RUN_TASK_JSON:-}" && -f "${FAKE_POST_DRY_RUN_TASK_JSON}" ]]; then
        cp "${FAKE_POST_DRY_RUN_TASK_JSON}" "${FAKE_TASK_JSON}"
      fi
      printf '%s\n' 'dry-run-ok'
    else
      printf '%s\n' live >>"${FAKE_APPLY_LOG}"
      printf '%s\n' 'applied'
    fi
    ;;
  *)
    echo "unexpected fake kubectl command: ${command} $*" >&2
    exit 1
    ;;
esac
FAKE
  chmod +x "${case_dir}/bin/kubectl"
}

new_case() {
  local name="$1"
  local case_dir="${test_root}/${name}"
  mkdir -p "${case_dir}"
  create_fake_kubectl "${case_dir}"

  printf '%s\n' 'sqlite backup fixture' >"${case_dir}/sqlite.backup"
  write_list "${case_dir}/cr-backup.json" '[]'
  write_marker "${case_dir}/sqlite.backup" "${case_dir}/sqlite.backup.verified" sqlite-pvc
  write_marker "${case_dir}/cr-backup.json" "${case_dir}/cr-backup.json.verified" orka-crs
  write_list "${case_dir}/runtimes.json" '[]'
  write_list "${case_dir}/agents.json" '[]'
  write_list "${case_dir}/tasks.json" '[]'
  write_list "${case_dir}/bindings.json" '[]'
  cat >"${case_dir}/workload.json" <<'JSON'
{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":2},"spec":{"replicas":0},"status":{"observedGeneration":2,"replicas":0,"readyReplicas":0,"availableReplicas":0,"updatedReplicas":0,"terminatingReplicas":0}}
JSON
  : >"${case_dir}/wrappers.tsv"
  : >"${case_dir}/kubectl.log"
  : >"${case_dir}/apply.log"
  : >"${case_dir}/delete.log"
  printf '%s\n' "${case_dir}"
}

prepare_historical_case() {
  local case_dir="$1"
  write_list "${case_dir}/cr-backup.json" "$(historical_items)"
  write_marker "${case_dir}/cr-backup.json" "${case_dir}/cr-backup.json.verified" orka-crs
}

run_upgrade_with_backups() {
  local case_dir="$1"
  local sqlite_path="$2"
  local sqlite_marker_path="$3"
  local cr_path="$4"
  local cr_marker_path="$5"
  shift 5

  PATH="${case_dir}/bin:${PATH}" \
  FAKE_KUBECTL_LOG="${case_dir}/kubectl.log" \
  FAKE_APPLY_LOG="${case_dir}/apply.log" \
  FAKE_DELETE_LOG="${case_dir}/delete.log" \
  FAKE_RUNTIME_JSON="${case_dir}/runtimes.json" \
  FAKE_AGENT_JSON="${case_dir}/agents.json" \
  FAKE_TASK_JSON="${case_dir}/tasks.json" \
  FAKE_BINDING_JSON="${case_dir}/bindings.json" \
  FAKE_WORKLOAD_JSON="${case_dir}/workload.json" \
  FAKE_WRAPPER_STATE="${case_dir}/wrappers.tsv" \
  FAKE_POST_DRY_RUN_RUNTIME_JSON="${case_dir}/post-dry-run-runtimes.json" \
  FAKE_POST_DRY_RUN_AGENT_JSON="${case_dir}/post-dry-run-agents.json" \
  FAKE_POST_DRY_RUN_TASK_JSON="${case_dir}/post-dry-run-tasks.json" \
    /bin/bash "${script}" \
      --context test-context \
      --sqlite-backup "${sqlite_path}" \
      --sqlite-backup-marker "${sqlite_marker_path}" \
      --cr-backup "${cr_path}" \
      --cr-backup-marker "${cr_marker_path}" \
      "$@"
}

run_upgrade() {
  local case_dir="$1"
  shift
  run_upgrade_with_backups \
    "${case_dir}" \
    "${case_dir}/sqlite.backup" \
    "${case_dir}/sqlite.backup.verified" \
    "${case_dir}/cr-backup.json" \
    "${case_dir}/cr-backup.json.verified" \
    "$@"
}

assert_no_live_apply() {
  local case_dir="$1"
  ! grep -Fqx live "${case_dir}/apply.log"
}

assert_one_dry_run_no_live_apply() {
  local case_dir="$1"
  [[ "$(grep -Fxc dry-run "${case_dir}/apply.log" || true)" == "1" ]]
  assert_no_live_apply "${case_dir}"
}

assert_one_dry_run_and_one_live_apply() {
  local case_dir="$1"
  [[ "$(grep -Fxc dry-run "${case_dir}/apply.log" || true)" == "1" ]]
  [[ "$(grep -Fxc live "${case_dir}/apply.log" || true)" == "1" ]]
  [[ "$(wc -l <"${case_dir}/apply.log" | tr -d ' ')" == "2" ]]
}

assert_no_kubectl_calls() {
  local case_dir="$1"
  [[ ! -s "${case_dir}/kubectl.log" ]]
}

expect_failure() {
  local case_dir="$1"
  local expected="$2"
  shift 2
  local output="${case_dir}/output"
  if "$@" >"${output}" 2>&1; then
    echo "command unexpectedly succeeded for ${case_dir}" >&2
    return 1
  fi
  grep -F "${expected}" "${output}" >/dev/null
}

expect_preapply_failure() {
  local case_dir="$1"
  local expected="$2"
  shift 2
  expect_failure "${case_dir}" "${expected}" "$@"
  [[ ! -s "${case_dir}/apply.log" ]]
}

expect_workload_blocked() {
  local name="$1"
  local workload_json="$2"
  local mapping_kind="$3"
  local case_dir
  case_dir="$(new_case "${name}")"
  prepare_historical_case "${case_dir}"
  printf '%s\n' "${workload_json}" >"${case_dir}/workload.json"
  expect_failure "${case_dir}" 'is not fully scaled to zero' \
    run_upgrade "${case_dir}" \
    --gateway-workload "gateway-ns/legacy-gateway=${mapping_kind}/gateway-ns/gateway-adapter"
  assert_no_live_apply "${case_dir}"
}

run_test() {
  local name="$1"
  shift
  local result

  set +e
  (
    set -Eeuo pipefail
    "$@"
  )
  result=$?
  set -e

  if [[ "${result}" == "0" ]]; then
    echo "ok - ${name}"
    pass_count=$((pass_count + 1))
  else
    echo "not ok - ${name}" >&2
    fail_count=$((fail_count + 1))
  fi
}

test_controller_epoch_crd_uses_canonical_plural() {
  local generated chart
  generated="${root}/config/crd/bases/core.orka.ai_controllerepochs.yaml"
  chart="${root}/manifest_staging/charts/orka/crds/controllerepoch-customresourcedefinition.yaml"

  [[ -f "${generated}" ]]
  [[ -f "${chart}" ]]
  [[ ! -e "${root}/config/crd/bases/core.orka.ai_controllerepoches.yaml" ]]
  [[ ! -e "${root}/manifest_staging/charts/orka/crds/core.orka.ai_controllerepoches.yaml" ]]

  grep -F '  name: controllerepochs.core.orka.ai' "${generated}" >/dev/null
  grep -F '    plural: controllerepochs' "${generated}" >/dev/null
  grep -F '  name: controllerepochs.core.orka.ai' "${chart}" >/dev/null
  grep -F '    plural: controllerepochs' "${chart}" >/dev/null
  grep -Fx -- '- bases/core.orka.ai_controllerepochs.yaml' "${root}/config/crd/kustomization.yaml" >/dev/null
}

test_requires_backup_arguments() {
  local case_dir output
  case_dir="$(new_case requires-backups)"
  output="${case_dir}/output"
  if PATH="${case_dir}/bin:${PATH}" /bin/bash "${script}" --context test-context >"${output}" 2>&1; then
    return 1
  fi
  grep -F -- '--sqlite-backup is required' "${output}" >/dev/null
  assert_no_kubectl_calls "${case_dir}"
}

test_backup_attestation_rejections() {
  local case_dir

  case_dir="$(new_case cr-digest-mismatch)"
  printf '%s\n' tampered >>"${case_dir}/cr-backup.json"
  expect_preapply_failure "${case_dir}" 'CR backup digest does not match its verification marker' run_upgrade "${case_dir}"

  case_dir="$(new_case wrong-marker-context)"
  replace_marker_field "${case_dir}/sqlite.backup.verified" context wrong-context
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup marker context does not match --context' run_upgrade "${case_dir}"

  case_dir="$(new_case wrong-cluster-uid)"
  replace_marker_field "${case_dir}/sqlite.backup.verified" cluster_uid wrong-cluster-uid
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup marker cluster_uid does not match the target cluster' run_upgrade "${case_dir}"

  case_dir="$(new_case wrong-api-server-identity)"
  replace_marker_field "${case_dir}/sqlite.backup.verified" api_server_identity_sha256 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup marker api_server_identity_sha256 does not match the target cluster' run_upgrade "${case_dir}"

  case_dir="$(new_case wrong-marker-kind)"
  replace_marker_field "${case_dir}/cr-backup.json.verified" kind sqlite-pvc
  expect_preapply_failure "${case_dir}" 'CR backup marker kind must be orka-crs' run_upgrade "${case_dir}"

  case_dir="$(new_case unverified-marker)"
  replace_marker_field "${case_dir}/sqlite.backup.verified" verified false
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup marker must attest verified=true' run_upgrade "${case_dir}"

  case_dir="$(new_case duplicate-marker-field)"
  printf '%s\n' 'sha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' >>"${case_dir}/sqlite.backup.verified"
  expect_preapply_failure "${case_dir}" 'must contain exactly one sha256= field' run_upgrade "${case_dir}"

  case_dir="$(new_case relative-backup-path)"
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup path must be absolute' \
    run_upgrade_with_backups "${case_dir}" relative.backup "${case_dir}/sqlite.backup.verified" "${case_dir}/cr-backup.json" "${case_dir}/cr-backup.json.verified"

  case_dir="$(new_case symlink-backup-path)"
  ln -s "${case_dir}/sqlite.backup" "${case_dir}/sqlite-link.backup"
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup path must not be a symbolic link' \
    run_upgrade_with_backups "${case_dir}" "${case_dir}/sqlite-link.backup" "${case_dir}/sqlite.backup.verified" "${case_dir}/cr-backup.json" "${case_dir}/cr-backup.json.verified"

  case_dir="$(new_case empty-backup-file)"
  : >"${case_dir}/sqlite.backup"
  expect_preapply_failure "${case_dir}" 'SQLite/PVC backup file is empty' run_upgrade "${case_dir}"

  case_dir="$(new_case malformed-cr-backup)"
  printf '%s\n' '{}' >"${case_dir}/cr-backup.json"
  write_marker "${case_dir}/cr-backup.json" "${case_dir}/cr-backup.json.verified" orka-crs
  expect_preapply_failure "${case_dir}" 'CR backup must be a Kubernetes JSON List' run_upgrade "${case_dir}"
}

test_affected_gateway_requires_mapping() {
  local case_dir
  case_dir="$(new_case gateway-mapping-required)"
  prepare_historical_case "${case_dir}"
  expect_failure "${case_dir}" 'affected Gateway gateway-ns/legacy-gateway has no --gateway-workload mapping' run_upgrade "${case_dir}"
  assert_no_live_apply "${case_dir}"
}

test_live_reference_chain_blocks() {
  local case_dir
  case_dir="$(new_case live-reference-chain)"
  prepare_historical_case "${case_dir}"
  write_list "${case_dir}/runtimes.json" '[{"kind":"AgentRuntime","metadata":{"namespace":"gateway-ns","name":"legacy-runtime"},"spec":{"contractVersion":"orka.harness.v1"}}]'
  write_list "${case_dir}/agents.json" '[{"kind":"Agent","metadata":{"namespace":"gateway-ns","name":"legacy-agent"},"spec":{"runtime":{"runtimeRef":{"name":"legacy-runtime"}}}}]'
  write_list "${case_dir}/bindings.json" '[{"kind":"GatewayBinding","metadata":{"namespace":"gateway-ns","name":"legacy-binding"},"spec":{"agentRef":{"name":"legacy-agent"},"gatewayRef":{"name":"legacy-gateway"}}}]'

  expect_failure "${case_dir}" 'AgentRuntime gateway-ns/legacy-runtime still declares orka.harness.v1' \
    run_upgrade "${case_dir}" --gateway-workload gateway-ns/legacy-gateway=deployment/gateway-ns/gateway-adapter
  grep -F 'Agent gateway-ns/legacy-agent still references affected AgentRuntime legacy-runtime' "${case_dir}/output" >/dev/null
  grep -F 'GatewayBinding gateway-ns/legacy-binding still references affected Agent legacy-agent' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
}

test_backed_up_reference_blocks_after_runtime_deleted() {
  local case_dir
  case_dir="$(new_case backed-up-reference)"
  prepare_historical_case "${case_dir}"
  write_list "${case_dir}/agents.json" '[{"kind":"Agent","metadata":{"namespace":"gateway-ns","name":"legacy-agent"},"spec":{"runtime":{"runtimeRef":{"name":"legacy-runtime"}}}}]'
  write_list "${case_dir}/bindings.json" '[{"kind":"GatewayBinding","metadata":{"namespace":"gateway-ns","name":"legacy-binding"},"spec":{"agentRef":{"name":"legacy-agent"},"gatewayRef":{"name":"legacy-gateway"}}}]'

  expect_failure "${case_dir}" 'Agent gateway-ns/legacy-agent still references affected AgentRuntime legacy-runtime' \
    run_upgrade "${case_dir}" --gateway-workload gateway-ns/legacy-gateway=deployment/gateway-ns/gateway-adapter
  grep -F 'GatewayBinding gateway-ns/legacy-binding still references affected Agent legacy-agent' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
}

test_gateway_scale_to_zero_invariants() {
  expect_workload_blocked spec-nonzero \
    '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":3},"spec":{"replicas":1},"status":{"observedGeneration":3,"replicas":0}}' \
    deployment
  expect_workload_blocked status-nonzero \
    '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":3},"spec":{"replicas":0},"status":{"observedGeneration":3,"replicas":1}}' \
    deployment
  expect_workload_blocked stale-generation \
    '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":4},"spec":{"replicas":0},"status":{"observedGeneration":3,"replicas":0}}' \
    deployment
  expect_workload_blocked terminating-replica \
    '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":3},"spec":{"replicas":0},"status":{"observedGeneration":3,"replicas":0,"terminatingReplicas":1}}' \
    deployment
  expect_workload_blocked statefulset-current-replica \
    '{"apiVersion":"apps/v1","kind":"StatefulSet","metadata":{"namespace":"gateway-ns","name":"gateway-adapter","generation":3},"spec":{"replicas":0},"status":{"observedGeneration":3,"replicas":0,"currentReplicas":1}}' \
    statefulset
}

test_wrapper_requires_explicit_cleanup() {
  local case_dir
  case_dir="$(new_case wrapper-blocked)"
  printf '%s\t%s\t%s\t%s\n' \
    deployment.apps orka-system orka-agent-harness-wrapper agent-harness-wrapper \
    service orka-system orka-agent-harness-wrapper agent-harness-wrapper \
    secret orka-system harness-wrapper-auth '<none>' >"${case_dir}/wrappers.tsv"

  expect_failure "${case_dir}" 'legacy harness-wrapper Deployment orka-system/orka-agent-harness-wrapper still exists' run_upgrade "${case_dir}"
  grep -F 'cleanup: kubectl --context test-context' "${case_dir}/output" >/dev/null
  [[ ! -s "${case_dir}/delete.log" ]]
  assert_no_live_apply "${case_dir}"
}

test_explicit_wrapper_cleanup_targets_only_legacy_resources() {
  local case_dir expected actual
  case_dir="$(new_case wrapper-cleanup)"
  printf '%s\t%s\t%s\t%s\n' \
    deployment.apps orka-system orka-agent-harness-wrapper agent-harness-wrapper \
    service orka-system orka-agent-harness-wrapper agent-harness-wrapper \
    secret orka-system harness-wrapper-auth '<none>' \
    deployment.apps orka-system harness-wrapper-metrics '<none>' >"${case_dir}/wrappers.tsv"

  run_upgrade "${case_dir}" --delete-legacy-wrapper >"${case_dir}/output" 2>&1

  expected="${case_dir}/expected-deletes.tsv"
  actual="${case_dir}/actual-deletes.tsv"
  printf '%s\t%s\t%s\n' \
    deployment.apps orka-system orka-agent-harness-wrapper \
    service orka-system orka-agent-harness-wrapper \
    secret orka-system harness-wrapper-auth | LC_ALL=C sort >"${expected}"
  LC_ALL=C sort "${case_dir}/delete.log" >"${actual}"
  cmp -s "${expected}" "${actual}"
  grep -F $'deployment.apps\torka-system\tharness-wrapper-metrics\t<none>' "${case_dir}/wrappers.tsv" >/dev/null
  assert_one_dry_run_and_one_live_apply "${case_dir}"
}


test_legacy_builtin_opencode_agent_blocks_cutover() {
  local case_dir
  case_dir="$(new_case legacy-builtin-opencode-agent)"
  write_list "${case_dir}/agents.json" '[
    {"kind":"Agent","metadata":{"namespace":"work","name":"legacy-opencode"},"spec":{"runtime":{"type":"opencode"}}},
    {"kind":"Agent","metadata":{"namespace":"work","name":"current-codex"},"spec":{"runtime":{"type":"codex"}}},
    {"kind":"Agent","metadata":{"namespace":"work","name":"custom-runtime"},"spec":{"runtime":{"runtimeRef":{"name":"custom"}}}}
  ]'

  expect_failure "${case_dir}" 'Agent work/legacy-opencode must be removed before the OpenCode CRD cutover' run_upgrade "${case_dir}"
  grep -F 'export and delete the Agent before cutover, then recreate it after the new CRD is applied' "${case_dir}/output" >/dev/null
  grep -F 'migrate it to claude/codex/copilot or runtimeRef before retrying' "${case_dir}/output" >/dev/null
  ! grep -F 'Agent work/current-codex' "${case_dir}/output" >/dev/null
  ! grep -F 'Agent work/custom-runtime' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
  [[ ! -s "${case_dir}/apply.log" ]]
}

test_post_upgrade_shaped_opencode_agent_still_blocks_cutover() {
  local case_dir
  case_dir="$(new_case post-upgrade-shaped-opencode-agent)"
  write_list "${case_dir}/agents.json" '[
    {
      "kind":"Agent",
      "metadata":{"namespace":"work","name":"post-upgrade-shaped-opencode"},
      "spec":{
        "model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
        "runtime":{"type":"opencode","defaultAllowedTools":["Read","Write","Edit","Bash","Glob","Grep"],"defaultAllowBash":true}
      }
    }
  ]'

  expect_failure "${case_dir}" 'Agent work/post-upgrade-shaped-opencode must be removed before the OpenCode CRD cutover' run_upgrade "${case_dir}"
  grep -F 'export and delete the Agent before cutover, then recreate it after the new CRD is applied' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
}

test_opencode_agent_provider_ref_blocks_cutover() {
  local case_dir
  case_dir="$(new_case opencode-agent-provider-ref)"
  write_list "${case_dir}/agents.json" '[
    {
      "kind":"Agent",
      "metadata":{"namespace":"work","name":"provider-ref-opencode"},
      "spec":{
        "model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
        "providerRef":{"name":"legacy-provider"},
        "runtime":{"type":"opencode"}
      }
    },
    {
      "kind":"Agent",
      "metadata":{"namespace":"work","name":"null-provider-ref-opencode"},
      "spec":{
        "model":{"name":"openai/gpt-5.4","contextWindow":32768,"maxTokens":4096},
        "providerRef":null,
        "runtime":{"type":"opencode"}
      }
    }
  ]'

  expect_failure "${case_dir}" 'Agent work/provider-ref-opencode must be removed before the OpenCode CRD cutover' run_upgrade "${case_dir}"
  grep -F 'Agent work/null-provider-ref-opencode must be removed before the OpenCode CRD cutover' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
}

test_post_dry_run_legacy_builtin_opencode_agent_blocks_live_apply() {
  local case_dir
  case_dir="$(new_case post-dry-run-legacy-builtin-opencode-agent)"
  write_list "${case_dir}/post-dry-run-agents.json" '[{"kind":"Agent","metadata":{"namespace":"work","name":"late-opencode"},"spec":{"runtime":{"type":"opencode"}}}]'

  expect_failure "${case_dir}" 'hard-cutover state changed after dry-run; no CRDs were applied' run_upgrade "${case_dir}"
  grep -F 'Agent work/late-opencode must be removed before the OpenCode CRD cutover' "${case_dir}/output" >/dev/null
  assert_one_dry_run_no_live_apply "${case_dir}"
  [[ "$(grep -Fc 'get agents.core.orka.ai -A -o json' "${case_dir}/kubectl.log")" == "2" ]]
}

test_legacy_task_workspace_credentials_block_cutover() {
  local case_dir
  case_dir="$(new_case legacy-task-workspace-credential)"
  write_list "${case_dir}/tasks.json" '[
    {"kind":"Task","metadata":{"namespace":"work","name":"top-level"},"spec":{"type":"container","workspace":{"gitRepo":"https://github.com/example/private.git","gitSecretRef":{"name":"git-creds"}}}},
    {"kind":"Task","metadata":{"namespace":"work","name":"nested-agent"},"spec":{"type":"agent","agentRuntime":{"workspace":{"gitRepo":"https://github.com/example/private.git","gitSecretRef":{"name":"git-creds"}}}}},
    {"kind":"Task","metadata":{"namespace":"work","name":"current"},"spec":{"type":"container","workspace":{"gitRepo":"https://github.com/example/private.git","readCredentialRef":{"name":"git-read"}}}}
  ]'

  expect_failure "${case_dir}" 'Task work/top-level still uses legacy spec.workspace.gitSecretRef' run_upgrade "${case_dir}"
  grep -F 'Task work/nested-agent still uses legacy spec.agentRuntime.workspace.gitSecretRef' "${case_dir}/output" >/dev/null
  grep -F 'recreate it with spec.workspace.readCredentialRef after the new CRD is applied' "${case_dir}/output" >/dev/null
  ! grep -F 'Task work/current' "${case_dir}/output" >/dev/null
  assert_no_live_apply "${case_dir}"
  [[ ! -s "${case_dir}/apply.log" ]]
}

test_post_dry_run_legacy_task_blocks_live_apply() {
  local case_dir
  case_dir="$(new_case post-dry-run-legacy-task)"
  write_list "${case_dir}/post-dry-run-tasks.json" '[{"kind":"Task","metadata":{"namespace":"work","name":"late-legacy"},"spec":{"type":"ai","workspace":{"gitSecretRef":{"name":"git-creds"}}}}]'

  expect_failure "${case_dir}" 'hard-cutover state changed after dry-run; no CRDs were applied' run_upgrade "${case_dir}"
  grep -F 'Task work/late-legacy still uses legacy spec.workspace.gitSecretRef' "${case_dir}/output" >/dev/null
  assert_one_dry_run_no_live_apply "${case_dir}"
  [[ "$(grep -Fc 'get tasks.core.orka.ai -A -o json' "${case_dir}/kubectl.log")" == "2" ]]
}

test_post_dry_run_state_change_blocks_live_apply() {
  local case_dir
  case_dir="$(new_case post-dry-run-change)"
  prepare_historical_case "${case_dir}"
  write_list "${case_dir}/post-dry-run-runtimes.json" '[{"kind":"AgentRuntime","metadata":{"namespace":"gateway-ns","name":"late-v1-runtime"},"spec":{"contractVersion":"orka.harness.v1"}}]'

  expect_failure "${case_dir}" 'AgentRuntime gateway-ns/late-v1-runtime still declares orka.harness.v1' \
    run_upgrade "${case_dir}" --gateway-workload gateway-ns/legacy-gateway=deployment/gateway-ns/gateway-adapter
  assert_one_dry_run_no_live_apply "${case_dir}"
  [[ "$(grep -Fc 'get agentruntimes.core.orka.ai -A -o json' "${case_dir}/kubectl.log")" == "2" ]]
}

test_clean_historical_cutover_applies() {
  local case_dir
  case_dir="$(new_case clean-cutover)"
  prepare_historical_case "${case_dir}"

  run_upgrade "${case_dir}" --gateway-workload gateway-ns/legacy-gateway=deployment/gateway-ns/gateway-adapter >"${case_dir}/output" 2>&1
  grep -F 'ACP v2 CRD hard cutover applied' "${case_dir}/output" >/dev/null
  assert_one_dry_run_and_one_live_apply "${case_dir}"
  [[ "$(grep -Fc 'get agentruntimes.core.orka.ai -A -o json' "${case_dir}/kubectl.log")" == "2" ]]
}

run_test 'uses canonical ControllerEpoch CRD plural' test_controller_epoch_crd_uses_canonical_plural
run_test 'requires explicit backup paths' test_requires_backup_arguments
run_test 'rejects invalid backup paths and attestations' test_backup_attestation_rejections
run_test 'requires a mapping for each affected Gateway' test_affected_gateway_requires_mapping
run_test 'blocks live v1 runtime and reference chain' test_live_reference_chain_blocks
run_test 'blocks backed-up v1 references after runtime deletion' test_backed_up_reference_blocks_after_runtime_deleted
run_test 'enforces every scale-to-zero status invariant' test_gateway_scale_to_zero_invariants
run_test 'never deletes wrapper without opt-in' test_wrapper_requires_explicit_cleanup
run_test 'deletes only exact legacy wrapper targets with opt-in' test_explicit_wrapper_cleanup_targets_only_legacy_resources
run_test 'blocks legacy built-in OpenCode Agents before cutover' test_legacy_builtin_opencode_agent_blocks_cutover
run_test 'requires even post-upgrade-shaped OpenCode Agents to be recreated after cutover' test_post_upgrade_shaped_opencode_agent_still_blocks_cutover
run_test 'blocks built-in OpenCode Agents with providerRef before cutover' test_opencode_agent_provider_ref_blocks_cutover
run_test 'rechecks legacy built-in OpenCode Agents after server dry-run' test_post_dry_run_legacy_builtin_opencode_agent_blocks_live_apply
run_test 'blocks legacy Task workspace credentials before cutover' test_legacy_task_workspace_credentials_block_cutover
run_test 'rechecks legacy Tasks after server dry-run' test_post_dry_run_legacy_task_blocks_live_apply
run_test 'rechecks cluster state after server dry-run' test_post_dry_run_state_change_blocks_live_apply
run_test 'clean verified historical cutover applies' test_clean_historical_cutover_applies

printf '%s\n' "${pass_count} passed, ${fail_count} failed"
[[ "${fail_count}" == "0" ]]
