#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
e2e="${root}/scripts/agent-substrate-e2e.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/agent-substrate-e2e-hardening.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

bootstrap_value="$(printf '%s' 'override-bootstrap:@[]{}*?+/=,;&%$()!' '\path with arbitrary punctuation')"
export SUBSTRATE_BOOTSTRAP_TOKEN="${bootstrap_value}"
# shellcheck source=scripts/agent-substrate-e2e.sh
KEEP_CLUSTER=1 source "${e2e}"
trap - EXIT ERR
trap 'rm -rf "${test_root}"' EXIT

fail() {
  echo "not ok - $*" >&2
  exit 1
}

assert_redacted() {
  local description="$1"
  local sensitive_value="$2"
  local input="$3"
  local output
  output="$(printf '%s\n' "${input}" | redact)"
  if grep -Fq -- "${sensitive_value}" <<<"${output}"; then
    fail "${description} leaked its credential value"
  fi
  grep -Fq '[REDACTED]' <<<"${output}" || fail "${description} omitted the redaction marker"
}

basic_value="$(printf '%s' 'Basic ' 'fixture:@[]{}()!?+/=,;:\path')"
custom_value="$(printf '%s' 'Custom-Scheme ' 'value with spaces @[]{}*?+/=,;:\and-more')"
txn_value="$(printf '%s' 'txn:@[]{}*?+/=,;:' ' value with spaces')"
proxy_value="$(printf '%s' 'Digest username="tester", response="@[]{}*?+/=,;:\\"')"
api_key_value="$(printf '%s' 'key:@[]{}*?+/=,;:' ' with spaces')"
cookie_value="$(printf '%s' 'session=@[]{}*?+/=,;:' '; preference=still-sensitive')"
compound_token_value="$(printf '%s' 'custom-scheme ' '@[]{}*?+/=,;&%$()! with spaces')"

assert_redacted 'Basic Authorization header' "${basic_value}" "Authorization: ${basic_value}"
assert_redacted 'custom Authorization scheme' "${custom_value}" "authorization: ${custom_value}"
assert_redacted 'Proxy-Authorization header' "${proxy_value}" "Proxy-Authorization: ${proxy_value}"
assert_redacted 'transaction token header' "${txn_value}" "Txn-Token: ${txn_value}"
assert_redacted 'API key header' "${api_key_value}" "X-API-Key: ${api_key_value}"
assert_redacted 'cookie header' "${cookie_value}" "Cookie: ${cookie_value}"
assert_redacted 'snake-case token JSON field' "${compound_token_value}" "{\"access_token\":\"${compound_token_value}\"}"
assert_redacted 'snake-case token assignment' "${compound_token_value}" "refresh_token=${compound_token_value}"
assert_redacted 'camel-case token assignment' "${compound_token_value}" "idToken=${compound_token_value}"
assert_redacted \
  'JSON Authorization field' \
  "${custom_value}" \
  "{\"Authorization\":\"${custom_value}\",\"diagnostic\":\"public\"}"
assert_redacted \
  'unstructured overridden bootstrap value' \
  "${bootstrap_value}" \
  "diagnostic bootstrap value=${bootstrap_value} after-value"
assert_redacted \
  'bootstrap environment assignment' \
  "${bootstrap_value}" \
  "ORKA_WORKSPACE_BOOTSTRAP_TOKEN=${bootstrap_value} after-value"

if grep -Fq -- "${bootstrap_value}" <(
  printf 'prefix %s suffix\n' "${bootstrap_value}" | redact
); then
  fail 'literal bootstrap redaction treated punctuation as a pattern'
fi

KUBECTL_ATE_MODE=''
kubectl_ate() {
  case "${KUBECTL_ATE_MODE}" in
    actor-not-found)
      printf 'Error: failed to get actor: rpc error: code = NotFound desc = Actor focused-actor not found\n' >&2
      return 1
      ;;
    unrelated-not-found)
      printf 'Error: rpc error: code = NotFound desc = Worker focused-actor not found\n' >&2
      return 1
      ;;
    wrong-actor-not-found)
      printf 'Error: rpc error: code = NotFound desc = Actor different-actor not found\n' >&2
      return 1
      ;;
    generic-api-failure)
      printf 'Error: failed to connect to ate-api-server\n' >&2
      return 7
      ;;
    empty-success)
      printf '{"actors":[]}\n'
      ;;
    empty-object-success)
      printf '{}\n'
      ;;
    present-success)
      printf '{"actors":[{"actorId":"focused-actor"}]}\n'
      ;;
    malformed-success)
      printf 'temporarily unavailable\n'
      ;;
    *)
      return 99
      ;;
  esac
}

expect_absent_success() {
  local mode="$1"
  local output
  KUBECTL_ATE_MODE="${mode}"
  if ! output="$(wait_actor_absent focused-actor -1 2>&1)"; then
    fail "wait_actor_absent rejected ${mode}: ${output}"
  fi
  grep -Fq 'actor/focused-actor: absent' <<<"${output}" || fail "${mode} did not report absence"
}

expect_absent_failure() {
  local mode="$1"
  local expected="$2"
  local output
  KUBECTL_ATE_MODE="${mode}"
  if output="$(wait_actor_absent focused-actor -1 2>&1)"; then
    fail "wait_actor_absent accepted ${mode} as absence"
  fi
  grep -Fq -- "${expected}" <<<"${output}" || fail "${mode} did not retain its failure classification"
}

expect_absent_success actor-not-found
expect_absent_success empty-success
expect_absent_success empty-object-success
expect_absent_failure unrelated-not-found 'without an actor NotFound response'
expect_absent_failure wrong-actor-not-found 'without an actor NotFound response'
expect_absent_failure generic-api-failure 'kubectl-ate failed with exit 7'
expect_absent_failure present-success 'actor query succeeded with 1 result(s)'
expect_absent_failure malformed-success 'actor query succeeded with an invalid response'

# Actor STATUS_RUNNING may race the router's upstream readiness after worker
# loss or runsc recovery. The handoff write must retry transient HTTP failures
# without presenting response bodies or credentials.
handoff_curl_calls=0
handoff_saw_connect_timeout=0
handoff_saw_max_time=0
curl() {
  handoff_curl_calls=$((handoff_curl_calls + 1))
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --connect-timeout)
        handoff_saw_connect_timeout=1
        shift
        ;;
      --max-time)
        handoff_saw_max_time=1
        shift
        ;;
    esac
    shift
  done
  [[ "${handoff_curl_calls}" -ge 2 ]]
}
sleep() {
  :
}
if ! write_workspace_handoff_token 'http://127.0.0.1:18082/v1/files' 'fixture.actors.resources.substrate.ate.dev' 'Zml4dHVyZQ==' 2; then
  fail 'workspace handoff token write did not retry a transient router failure'
fi
[[ "${handoff_curl_calls}" == '2' ]] || fail "workspace handoff retry count = ${handoff_curl_calls}, want 2"
[[ "${handoff_saw_connect_timeout}" == '1' ]] || fail 'workspace handoff retry omitted a connect timeout'
[[ "${handoff_saw_max_time}" == '1' ]] || fail 'workspace handoff retry omitted an overall request timeout'
unset -f curl sleep

exec_counter_file="${test_root}/exec-curl-count"
exec_connect_file="${test_root}/exec-connect-timeout"
exec_max_file="${test_root}/exec-max-time"
curl() {
  local count=0
  if [[ -f "${exec_counter_file}" ]]; then
    count="$(cat "${exec_counter_file}")"
  fi
  count=$((count + 1))
  printf '%s\n' "${count}" >"${exec_counter_file}"
  while [[ "$#" -gt 0 ]]; do
    case "$1" in
      --connect-timeout)
        : >"${exec_connect_file}"
        shift
        ;;
      --max-time)
        : >"${exec_max_file}"
        shift
        ;;
    esac
    shift
  done
  if [[ "${count}" -lt 2 ]]; then
    return 22
  fi
  printf '%s\n' '{"exitCode":0,"stdout":"direct-ok","stderr":""}'
}
sleep() {
  :
}
exec_response="$(run_idempotent_workspace_exec \
  'http://127.0.0.1:18082/v1/exec' 'fixture.actors.resources.substrate.ate.dev' \
  'fixture-handoff-token' 'fixture-request-id' 2)" || fail 'idempotent workspace exec did not retry a transient router failure'
exec_curl_calls="$(cat "${exec_counter_file}")"
[[ "${exec_curl_calls}" == '2' ]] || fail "workspace exec retry count = ${exec_curl_calls}, want 2"
[[ -f "${exec_connect_file}" ]] || fail 'workspace exec retry omitted a connect timeout'
[[ -f "${exec_max_file}" ]] || fail 'workspace exec retry omitted an overall request timeout'
[[ "$(jq -r '.stdout' <<<"${exec_response}")" == 'direct-ok' ]] || fail 'workspace exec retry returned the wrong response'
unset -f curl sleep

# The live router assertion must inspect private raw logs before presentation
# redaction so conventional Authorization leaks cannot be erased before grep.
grep -F 'raw_log_file="${TMP_ROOT}/atenet-router-raw-${request_id}.log"' "${e2e}" >/dev/null
grep -F 'grep -Fq -- "${handoff_token}" "${raw_log_file}"' "${e2e}" >/dev/null
router_check="$(awk '/^verify_router_request_metadata_allowlist\(\)/,/^}/' "${e2e}")"
if grep -Fq 'run_redacted' <<<"${router_check}"; then
  fail 'router leak assertion redacts logs before checking raw credentials'
fi

# The direct Substrate deployment intentionally omits Workspace/Publisher. Its
# strategic merge patch must also remove the inherited endpoint environment
# variable or controller startup fails closed while negotiating capabilities
# against a Service that does not exist in this cluster.
publisher_disable_patch="$(grep -A1 -F 'name: "ORKA_WORKSPACE_PUBLISHER_URL",' "${e2e}" || true)"
grep -Fq 'name: "ORKA_WORKSPACE_PUBLISHER_URL",' <<<"${publisher_disable_patch}" || \
  fail 'Substrate controller patch does not target the inherited Publisher URL'
grep -Fq '"$patch": "delete"' <<<"${publisher_disable_patch}" || \
  fail 'Substrate controller patch does not disable the omitted Publisher client'

# Substrate is a harness-v2 workspace-provider evaluation, not a third
# controller mode. Claim the namespace before applying the statically configured
# v2 workload and keep every required controller identity flag in the final
# strategic patch.
namespace_identity_line="$(grep -nF 'scripts/lib/ensure-static-mode-namespace.sh' "${e2e}" | head -n1 | cut -d: -f1 || true)"
controller_apply_line="$(grep -nF '"${ROOT_DIR}/bin/kustomize" build "${tmp_config}/config/acp-workload" | kubectl apply -f -' "${e2e}" | head -n1 | cut -d: -f1 || true)"
[[ "${namespace_identity_line}" =~ ^[0-9]+$ ]] || fail 'Substrate deploy does not establish the fail-closed Orka namespace identity'
[[ "${controller_apply_line}" =~ ^[0-9]+$ ]] || fail 'Substrate deploy does not apply the controller workload'
(( namespace_identity_line < controller_apply_line )) || \
  fail 'Substrate deploy must establish the namespace identity before the harness-v2 controller workload'
for required_arg in \
  '"--agent-execution-snapshot-key-file=/var/run/orka/agent-execution-snapshot/key"' \
  '"--controller-mode=harness-v2"' \
  '"--watch-namespace=orka-system"' \
  '"--enforce-namespace-isolation=true"' \
  '"--execution-mode-controller-usernames=system:serviceaccount:orka-system:orka-controller-manager"'; do
  grep -Fq -- "${required_arg}" "${e2e}" || fail "Substrate controller patch omits ${required_arg}"
done
if grep -Fq -- '"--acp-runtime-enabled=false"' "${e2e}"; then
  fail 'Substrate deploy still passes the removed dynamic ACP mode flag'
fi

# The harness-v2 ACP dispatcher requires the encrypted execution-snapshot key.
# Provision it before applying the workload so the Substrate-only rollout can
# activate its immutable snapshot store.
grep -F 'dd if=/dev/urandom bs=32 count=1' "${e2e}" | \
  grep -F '>"${capability_dir}/snapshot-key"' >/dev/null || \
  fail 'Substrate deploy does not generate a 32-byte execution-snapshot key'
grep -F 'create secret generic agent-execution-snapshot-key' "${e2e}" >/dev/null || \
  fail 'Substrate deploy does not provision the execution-snapshot Secret'
grep -F -- '--from-file="${snapshot_key_field}=${capability_dir}/snapshot-key"' "${e2e}" >/dev/null || \
  fail 'Substrate execution-snapshot Secret does not use the required key field'

# The extended path now installs a fail-once executable on the assigned worker,
# requires the patched verified-presence retry log, and restores the real runsc.
grep -F 'install_runsc_delete_failure_injector "${worker_name}"' "${e2e}" >/dev/null
grep -F 'node_injector_path="/root/orka-runsc-delete-failure-injector-$$-${RANDOM}"' "${e2e}" >/dev/null
grep -F 'cp "${incoming}" "${path}"' "${e2e}" >/dev/null
grep -F 'runsc delete did not remove the container; retrying' "${e2e}" >/dev/null
grep -F 'exercise_runsc_delete_retry_recovery' "${e2e}" >/dev/null

TMP_ROOT="${test_root}"
injector="${test_root}/runsc-delete-failure-injector"
build_runsc_delete_failure_injector "${injector}" "$(go env GOARCH)" "$(go env GOOS)"
[[ -x "${injector}" ]] || fail 'runsc delete failure injector did not compile as an executable'
runsc_real_log="${test_root}/runsc-real.log"
cat >"${injector}.orka-real" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"${RUNSC_REAL_LOG:?}"
STUB
chmod +x "${injector}.orka-real"
if RUNSC_REAL_LOG="${runsc_real_log}" "${injector}" -root fixture delete -force workspace >/dev/null 2>&1; then
  fail 'runsc delete failure injector did not fail the first delete call'
else
  injector_rc=$?
fi
[[ "${injector_rc}" == "86" ]] || fail "first injected delete exited ${injector_rc}, want 86"
[[ -f "${injector}.orka-delete-failure-observed" ]] || fail 'runsc delete failure injector omitted its marker'
[[ ! -s "${runsc_real_log}" ]] || fail 'first injected delete reached the real runsc executable'
RUNSC_REAL_LOG="${runsc_real_log}" "${injector}" -root fixture list --quiet
RUNSC_REAL_LOG="${runsc_real_log}" "${injector}" -root fixture delete -force workspace
grep -Fx -- '-root fixture list --quiet' "${runsc_real_log}" >/dev/null || fail 'injector did not delegate runsc list'
grep -Fx -- '-root fixture delete -force workspace' "${runsc_real_log}" >/dev/null || fail 'injector did not delegate the retry'

docker() {
  return 42
}
RUNSC_DELETE_INJECTION_NODE='fixture-node'
RUNSC_DELETE_INJECTION_PATH='/fixture/runsc'
if restore_runsc_delete_injector; then
  fail 'runsc restoration reported success after docker failed'
fi
[[ "${RUNSC_DELETE_INJECTION_NODE}" == 'fixture-node' ]] || fail 'failed restoration forgot its target node'
[[ "${RUNSC_DELETE_INJECTION_PATH}" == '/fixture/runsc' ]] || fail 'failed restoration forgot its target path'
unset -f docker
RUNSC_DELETE_INJECTION_NODE=''
RUNSC_DELETE_INJECTION_PATH=''

printf '%s\n' 'ok - Agent Substrate E2E absence checks, diagnostic redaction, and live retry injection are hardened'
