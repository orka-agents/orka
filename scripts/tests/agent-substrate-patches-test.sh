#!/usr/bin/env bash
set -Eeuo pipefail

# scripts/tests suites rely on 'set -e' stopping on failed (( )) arithmetic,
# which macOS's stock bash 3.2 does not honor; failures would be silently
# masked there. Require a modern bash (for example: brew install bash).
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "error: this test suite requires bash >= 4; found ${BASH_VERSION}" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
e2e="${root}/scripts/agent-substrate-e2e.sh"
installer="${root}/hack/demos/cluster/install-substrate.sh"
router_patch="${root}/hack/agent-substrate/atenet-router-authorization-redaction.patch"
ateom_patch="${root}/hack/agent-substrate/ateom-runsc-delete-recovery.patch"

bash -n "${e2e}" "${installer}"
for patch in "${router_patch}" "${ateom_patch}"; do
  [[ -s "${patch}" ]] || {
    echo "missing reviewed patch: ${patch}" >&2
    exit 1
  }
  git apply --numstat "${patch}" >/dev/null
done
router_paths="$(git apply --numstat "${router_patch}" | cut -f3- | LC_ALL=C sort)"
expected_router_paths="$(printf '%s\n' \
  cmd/servers/atenet/app/router/envoyrunner.go \
  cmd/servers/atenet/app/router/extproc_in.go \
  cmd/servers/atenet/app/router/extproc_in_test.go \
  cmd/servers/atenet/app/router/xds.go \
  cmd/servers/atenet/app/router/xds_test.go \
  manifests/ate-install/atenet-router.yaml | LC_ALL=C sort)"
[[ "${router_paths}" == "${expected_router_paths}" ]] || {
  echo "router patch paths differ from the reviewed scope" >&2
  exit 1
}
ateom_paths="$(git apply --numstat "${ateom_patch}" | cut -f3- | LC_ALL=C sort)"
expected_ateom_paths="$(printf '%s\n' \
  cmd/servers/ateom-gvisor/ateom-gvisor.go \
  cmd/servers/ateom-gvisor/runsc.go \
  cmd/servers/ateom-gvisor/runsc_test.go | LC_ALL=C sort)"
[[ "${ateom_paths}" == "${expected_ateom_paths}" ]] || {
  echo "ateom patch paths differ from the reviewed scope" >&2
  exit 1
}

grep -F 'SUBSTRATE_ATENET_EXTPROC_IN_BLOB="317511845fef40b7602861383f7664e915215a69"' "${e2e}" >/dev/null
grep -F 'SUBSTRATE_ATENET_ENVOY_RUNNER_BLOB="8d38be29f09a7ce23886b71a051586354c8413e5"' "${e2e}" >/dev/null
grep -F 'SUBSTRATE_ATENET_MANIFEST_BLOB="e309cad0a2e8435d1ed8dfd51ce347ab4f5a7521"' "${e2e}" >/dev/null
grep -F 'SUBSTRATE_ATENET_XDS_BLOB="20ce920de816c885e4614c4f723e75a6c2b74d8d"' "${e2e}" >/dev/null
grep -F 'SUBSTRATE_ATENET_XDS_TEST_BLOB="189914c245d13eeec19293d08258fcd8c27676e7"' "${e2e}" >/dev/null
grep -F 'SUBSTRATE_ATEOM_RUNSC_BLOB="6db499a549f2b6987a867b144e8d6b3828cad9ff"' "${e2e}" >/dev/null
grep -F 'apply_substrate_atenet_authorization_redaction_patch' "${e2e}" >/dev/null
grep -F 'apply_substrate_ateom_delete_recovery_patch' "${e2e}" >/dev/null
grep -F 'verify_router_request_metadata_allowlist' "${e2e}" >/dev/null
grep -F 'X-Request-ID: ${request_id}' "${e2e}" >/dev/null
grep -F 'case ":method", ":path", ":authority", "host", "x-request-id":' "${router_patch}" >/dev/null
grep -F 'if !isSafeRequestMetadataHeader(k) {' "${router_patch}" >/dev/null
grep -F 'url.ParseRequestURI(value)' "${router_patch}" >/dev/null
grep -F 'credential headers and query values are omitted' "${router_patch}" >/dev/null
grep -F 'TestRequestMetadataStringOnlyIncludesSafeRoutingMetadata' "${router_patch}" >/dev/null
grep -F 'func digestRequestID(value string) string {' "${router_patch}" >/dev/null
grep -F 'TestRequestIDDigestSupportsBothHeaderRepresentations' "${router_patch}" >/dev/null
grep -F 'headerFreeAccessLog  = ' "${router_patch}" >/dev/null
grep -F 'assertHeaderFreeAccessLogs(t, listenersMap)' "${router_patch}" >/dev/null
for covered_value in Authorization Proxy-Authorization Txn-Token Cookie X-API-Key RawValue test-token-placeholder; do
  grep -F "${covered_value}" "${router_patch}" >/dev/null || {
    echo "router patch lacks ${covered_value} coverage" >&2
    exit 1
  }
done
if grep -Eq '^\+.*(redactedHeaderValue|sanitizeRequestHeaderValue)' "${router_patch}"; then
  echo 'router patch uses denylist-based credential logging' >&2
  exit 1
fi
grep -F 'upstream:info,router:info,ext_proc:info' "${router_patch}" >/dev/null
grep -F 'request_id_digest="sha256:$(printf' "${e2e}" >/dev/null
grep -F 'atenet-router leaked a raw request ID in its logs' "${e2e}" >/dev/null
grep -F 'request audit digest while omitting raw request metadata and credentials' "${e2e}" >/dev/null
grep -F 'Timeout: durationpb.New(0), // Disable route timeout for streaming responses.' "${router_patch}" >/dev/null
grep -F 'Expected streaming route timeout to be disabled' "${router_patch}" >/dev/null
if grep -Fq 'upstream:debug,router:debug,ext_proc:debug' <(grep '^+' "${router_patch}" || true); then
  echo 'router patch adds credential-bearing Envoy debug logging' >&2
  exit 1
fi
grep -F 'cmd/servers/atenet/app/router/envoyrunner.go' "${e2e}" >/dev/null
grep -F 'manifests/ate-install/atenet-router.yaml' "${e2e}" >/dev/null
grep -F 'runscDeleteAttempts   = 4' "${ateom_patch}" >/dev/null
grep -F 'failed closed while verifying container absence' "${ateom_patch}" >/dev/null
grep -F 'runsc_test.go' "${ateom_patch}" >/dev/null
grep -F 'Restore the worker Pod network before fallible runsc cleanup' "${ateom_patch}" >/dev/null
grep -F 'kind export kubeconfig --name "${KIND_CLUSTER}" --kubeconfig "${SUBSTRATE_KUBECONFIG}"' "${installer}" >/dev/null
grep -F 'chmod 0600 "${SUBSTRATE_KUBECONFIG}"' "${installer}" >/dev/null
grep -F 'direct_evaluation_hardening_matches' "${installer}" >/dev/null
grep -F 'DIRECT_EVAL_MARKER_SCHEMA_VERSION="4"' "${installer}" >/dev/null
grep -F 'DIRECT_EVAL_PATCH_SET_VERSION=' "${installer}" >/dev/null
grep -F '.data["substrate-commit"] == $substrate_commit' "${installer}" >/dev/null
grep -F '.data["reviewed-patch-set"] == $reviewed_patch_set' "${installer}" >/dev/null
grep -F 'git hash-object "${patch_path}"' "${installer}" >/dev/null
grep -F 'choose DEMO_CLUSTER_REUSE=recreate or cancel' "${installer}" >/dev/null
reuse_kubeconfig_line="$(grep -nF 'activate_scoped_kubeconfig' "${installer}" | sed -n '2p' | cut -d: -f1)"
cluster_health_line="$(grep -nF 'health_summary="$(cluster_health)"' "${installer}" | cut -d: -f1)"
[[ "${reuse_kubeconfig_line}" =~ ^[1-9][0-9]*$ && "${cluster_health_line}" =~ ^[1-9][0-9]*$ ]]
(( reuse_kubeconfig_line < cluster_health_line )) || {
  echo 'installer checks retained cluster health before activating its scoped kubeconfig' >&2
  exit 1
}

# Exercise retained-cluster marker matching with command stubs. A marker written
# before the current router hardening carried only the old pinned-hardening string;
# it must fail closed rather than silently accepting stale router logging.
marker_fixture="$(mktemp -d "${TMPDIR:-/tmp}/agent-substrate-marker-test.XXXXXX")"
fake_bin="${marker_fixture}/bin"
mkdir -p "${fake_bin}" "${marker_fixture}/gopath/bin" "${marker_fixture}/home"
cat >"${fake_bin}/go" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == "env" && "${2:-}" == "GOPATH" ]]; then
  printf '%s\n' "${FAKE_GOPATH}"
  exit 0
fi
exit 2
EOF
cat >"${fake_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "${1:-}" == "info" ]]
EOF
cat >"${fake_bin}/kind" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
if [[ "${1:-}" == "get" && "${2:-}" == "clusters" ]]; then
  printf '%s\n' "${KIND_CLUSTER}"
  exit 0
fi
if [[ "${1:-}" == "export" && "${2:-}" == "kubeconfig" ]]; then
  kubeconfig=""
  while (($#)); do
    if [[ "$1" == "--kubeconfig" ]]; then
      shift
      kubeconfig="$1"
      break
    fi
    shift
  done
  [[ -n "${kubeconfig}" ]]
  mkdir -p "$(dirname "${kubeconfig}")"
  : >"${kubeconfig}"
  exit 0
fi
exit 2
EOF
cat >"${fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -Eeuo pipefail
args=" $* "
case "${args}" in
  *" get nodes "*)
    printf 'True'
    ;;
  *" get deploy orka-controller-manager "*)
    printf '1/1'
    ;;
  *" get pods --no-headers "*)
    printf 'ate-worker-0 1/1 Running 0 1m\n'
    ;;
  *" get configmap "*" -o json "*)
    cat "${FAKE_MARKER_JSON_FILE}"
    ;;
  *" get configmap "*)
    ;;
  *" create configmap "*)
    printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: marker\n'
    ;;
  *" apply -f - "*)
    cat >/dev/null
    ;;
  *)
    printf 'unexpected fake kubectl invocation: %s\n' "$*" >&2
    exit 2
    ;;
esac
EOF
for command_name in ko curl; do
  cat >"${fake_bin}/${command_name}" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
done
chmod +x "${fake_bin}"/*

marker_schema="$(sed -n 's/^DIRECT_EVAL_MARKER_SCHEMA_VERSION="\([^"]*\)"/\1/p' "${installer}")"
patch_set_version="$(sed -n 's/^DIRECT_EVAL_PATCH_SET_VERSION="\([^"]*\)"/\1/p' "${installer}")"
pinned_ref="$(sed -n 's/^SUBSTRATE_REF="${SUBSTRATE_REF:-\([^"]*\)}"/\1/p' "${installer}")"
hardening="$(sed -n 's/^DIRECT_EVAL_HARDENING="\([^"]*\)"/\1/p' "${installer}")"
[[ -n "${marker_schema}" && -n "${patch_set_version}" && -n "${pinned_ref}" && -n "${hardening}" ]]
reviewed_patch_set="$({
  for relative_path in \
    hack/agent-substrate/atelet-root-supervisor-capabilities.patch \
    hack/agent-substrate/atenet-router-authorization-redaction.patch \
    hack/agent-substrate/ateom-runsc-delete-recovery.patch; do
    printf '%s git-blob:%s\n' "${relative_path}" "$(git hash-object "${root}/${relative_path}")"
  done
})"

legacy_marker="${marker_fixture}/legacy.json"
wrong_ref_marker="${marker_fixture}/wrong-ref.json"
stale_version_marker="${marker_fixture}/stale-version.json"
stale_marker="${marker_fixture}/stale.json"
current_marker="${marker_fixture}/current.json"
jq -n '{data: {"pinned-hardening": "authorization-redaction,runsc-delete-recovery"}}' >"${legacy_marker}"
jq -n \
  --arg marker_schema "${marker_schema}" \
  --arg substrate_commit "0000000000000000000000000000000000000000" \
  --arg patch_set_version "${patch_set_version}" \
  --arg reviewed_patch_set "${reviewed_patch_set}" \
  --arg hardening "${hardening}" \
  '{data: {
    "marker-schema-version": $marker_schema,
    "substrate-commit": $substrate_commit,
    "reviewed-patch-set-version": $patch_set_version,
    "reviewed-patch-set": $reviewed_patch_set,
    "pinned-hardening": $hardening
  }}' >"${wrong_ref_marker}"
jq -n \
  --arg marker_schema "${marker_schema}" \
  --arg substrate_commit "${pinned_ref}" \
  --arg patch_set_version "stale-version" \
  --arg reviewed_patch_set "${reviewed_patch_set}" \
  --arg hardening "${hardening}" \
  '{data: {
    "marker-schema-version": $marker_schema,
    "substrate-commit": $substrate_commit,
    "reviewed-patch-set-version": $patch_set_version,
    "reviewed-patch-set": $reviewed_patch_set,
    "pinned-hardening": $hardening
  }}' >"${stale_version_marker}"
jq -n \
  --arg marker_schema "${marker_schema}" \
  --arg substrate_commit "${pinned_ref}" \
  --arg patch_set_version "${patch_set_version}" \
  --arg reviewed_patch_set "${reviewed_patch_set}-stale" \
  --arg hardening "${hardening}" \
  '{data: {
    "marker-schema-version": $marker_schema,
    "substrate-commit": $substrate_commit,
    "reviewed-patch-set-version": $patch_set_version,
    "reviewed-patch-set": $reviewed_patch_set,
    "pinned-hardening": $hardening
  }}' >"${stale_marker}"
jq -n \
  --arg marker_schema "${marker_schema}" \
  --arg substrate_commit "${pinned_ref}" \
  --arg patch_set_version "${patch_set_version}" \
  --arg reviewed_patch_set "${reviewed_patch_set}" \
  --arg hardening "${hardening}" \
  '{data: {
    "marker-schema-version": $marker_schema,
    "substrate-commit": $substrate_commit,
    "reviewed-patch-set-version": $patch_set_version,
    "reviewed-patch-set": $reviewed_patch_set,
    "pinned-hardening": $hardening
  }}' >"${current_marker}"

run_installer_reuse() {
  local marker_file="$1"
  local substrate_ref="${2:-${pinned_ref}}"
  env \
    PATH="${fake_bin}:${PATH}" \
    HOME="${marker_fixture}/home" \
    FAKE_GOPATH="${marker_fixture}/gopath" \
    FAKE_MARKER_JSON_FILE="${marker_file}" \
    SUBSTRATE_REF="${substrate_ref}" \
    KIND_CLUSTER=marker-test-cluster \
    SUBSTRATE_KUBECONFIG="${marker_fixture}/kubeconfig" \
    DEMO_CLUSTER_REUSE=reuse \
    AGENTIC=0 \
    bash "${installer}"
}

if mutable_ref_output="$(run_installer_reuse "${current_marker}" main 2>&1)"; then
  echo 'installer accepted a mutable Substrate branch for retained-cluster identity' >&2
  exit 1
fi
grep -F 'must be an immutable full 40-hex commit SHA' <<<"${mutable_ref_output}" >/dev/null

if legacy_output="$(run_installer_reuse "${legacy_marker}" 2>&1)"; then
  echo 'installer reused a marker from before the Envoy info-level fix' >&2
  exit 1
fi
grep -F 'does not match marker schema' <<<"${legacy_output}" >/dev/null

if wrong_ref_output="$(run_installer_reuse "${wrong_ref_marker}" 2>&1)"; then
  echo 'installer reused a cluster created from a different Substrate commit' >&2
  exit 1
fi
grep -F 'does not match marker schema' <<<"${wrong_ref_output}" >/dev/null

if stale_version_output="$(run_installer_reuse "${stale_version_marker}" 2>&1)"; then
  echo 'installer reused a cluster with a stale reviewed patch-set version' >&2
  exit 1
fi
grep -F 'does not match marker schema' <<<"${stale_version_output}" >/dev/null

if stale_output="$(run_installer_reuse "${stale_marker}" 2>&1)"; then
  echo 'installer reused a cluster with a stale reviewed patch fingerprint' >&2
  exit 1
fi
grep -F 'does not match marker schema' <<<"${stale_output}" >/dev/null

current_output="$(run_installer_reuse "${current_marker}" 2>&1)"
grep -F 'Reusing existing marked cluster' <<<"${current_output}" >/dev/null
uppercase_output="$(run_installer_reuse "${current_marker}" "$(printf '%s' "${pinned_ref}" | tr '[:lower:]' '[:upper:]')" 2>&1)"
grep -F 'Reusing existing marked cluster' <<<"${uppercase_output}" >/dev/null
rm -rf "${marker_fixture}"

# Exercise the generic blob and patch guards without a network checkout.
# shellcheck source=scripts/agent-substrate-e2e.sh
KEEP_CLUSTER=1 source "${e2e}"
trap - EXIT ERR
fixture="$(mktemp -d "${TMPDIR:-/tmp}/agent-substrate-patch-test.XXXXXX")"
git -C "${fixture}" init -q
git -C "${fixture}" config user.name test
git -C "${fixture}" config user.email test@example.invalid
printf 'before\n' >"${fixture}/source.txt"
git -C "${fixture}" add source.txt
git -C "${fixture}" commit -q -m fixture
blob="$(git -C "${fixture}" rev-parse HEAD:source.txt)"
# shellcheck disable=SC2034 # Consumed by functions sourced from the E2E script.
SUBSTRATE_DIR="${fixture}"
# shellcheck disable=SC2034 # Consumed by functions sourced from the E2E script.
SUBSTRATE_REF="fixture"
verify_substrate_source_blob source.txt "${blob}" fixture
if blob_error="$(verify_substrate_source_blob source.txt 0000000000000000000000000000000000000000 fixture 2>&1)"; then
  echo 'source-blob guard accepted an unreviewed object' >&2
  exit 1
fi
grep -F 'has unreviewed fixture context' <<<"${blob_error}" >/dev/null

printf 'after\n' >"${fixture}/source.txt"
git -C "${fixture}" diff -- source.txt >"${fixture}/fixture.patch"
git -C "${fixture}" checkout -- source.txt
apply_reviewed_substrate_patch fixture "${fixture}/fixture.patch" source.txt
grep -Fx 'after' "${fixture}/source.txt" >/dev/null

printf '%s\n' 'ok - Agent Substrate patches are source-pinned, scope-bounded, and fail closed'
