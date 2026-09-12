#!/usr/bin/env bash
# Exercise cleanup and preflight without a cluster or provider credentials.
set -Eeuo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-native-substrate-test.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT
source "${root}/scripts/agent-substrate-e2e.sh"

# Workspace classes are namespaced. Class setup must work even when the
# kubeconfig's default namespace has no copy of the source class.
(
  TMP_ROOT="${test_root}/lifetime-class"
  mkdir -p "${TMP_ROOT}"
  kubectl() {
    case "$*" in
      '-n orka-system get executionworkspaceclass native-substrate -o json')
        jq -n '{apiVersion:"workspace.orka.ai/v1alpha1",kind:"ExecutionWorkspaceClass",
          metadata:{name:"native-substrate",namespace:"orka-system",uid:"source-uid",resourceVersion:"41"},
          spec:{providerRef:{name:"native-substrate"},parametersRef:{name:"native-substrate"},
            lifecycle:{maxLifetime:"2h",defaultOnDetach:"Suspend",deletionPolicy:{providerResources:"Delete"}}}}'
        ;;
      '-n orka-system create -f -') cat >"${TMP_ROOT}/class.json" ;;
      '-n orka-system --request-timeout=15s get executionworkspaceclass native-lifetime -o json')
        jq '.status.conditions=[{type:"Ready",status:"True"}]' "${TMP_ROOT}/class.json"
        ;;
      *) printf 'Unexpected namespace or class operation: %s\n' "$*" >&2; return 9 ;;
    esac
  }
  create_lifetime_workspace_class
  jq -e '
    .metadata == {name:"native-lifetime",namespace:"orka-system"} and
    .spec.providerRef.name == "native-substrate" and
    .spec.parametersRef.name == "native-substrate" and
    .spec.lifecycle.maxLifetime == "120s" and
    .spec.lifecycle.defaultOnDetach == "Suspend" and
    .spec.lifecycle.deletionPolicy.providerResources == "Delete"
  ' "${TMP_ROOT}/class.json" >/dev/null
)

# The lifetime command needs access to the fixture in addition to the normal
# model proxy. Permit only this worker pool, fixture Pod, namespace, and port.
(
  TMP_ROOT="${test_root}/lifetime-network"
  mkdir -p "${TMP_ROOT}"
  policy_failure=""
  kubectl() {
    [[ "$*" == '-n ate-demo create -f -' ]] || return 9
    cat >"${TMP_ROOT}/policy.json"
    [[ -z "${policy_failure}" ]] || return 7
  }
  create_lifetime_fixture_access
  jq -e '
    .kind == "NetworkPolicy" and
    .metadata == {name:"native-lifetime-fixture",namespace:"ate-demo"} and
    .spec.podSelector == {matchLabels:{"ate.dev/worker-pool":"orka-native"}} and
    .spec.policyTypes == ["Egress"] and
    (.spec.egress | length) == 1 and
    .spec.egress[0].to == [{
      namespaceSelector:{matchLabels:{"kubernetes.io/metadata.name":"vekil-system"}},
      podSelector:{matchLabels:{"app.kubernetes.io/name":"vekil","app.kubernetes.io/component":"responses-fixture"}}
    }] and
    .spec.egress[0].ports == [{protocol:"TCP",port:1337}]
  ' "${TMP_ROOT}/policy.json" >/dev/null
  policy_failure=failed
  if create_lifetime_fixture_access; then
    echo 'lifetime setup continued after fixture access was denied' >&2
    exit 1
  fi
)

# Dormant history uses a named read-only identity. Its token is passed through
# a private header file, never process arguments or unauthenticated fixture reads.
(
  TMP_ROOT="${test_root}/history"
  mkdir -p "${TMP_ROOT}"
  token_failure=""
  kubectl() {
    case "$*" in
      '-n orka-system apply -f -') cat >"${TMP_ROOT}/identity.json" ;;
      '-n orka-system create token native-history-client --duration=15m')
        [[ -z "${token_failure}" ]] || return 7
        printf 'fixture-history-token\n'
        ;;
      *'port-forward '*) ;;
      *) return 9 ;;
    esac
  }
  curl() {
    printf '%s\n' "$@" >"${TMP_ROOT}/curl-arguments"
    printf '{"messageCount":1,"transcript":"fixture history"}\n'
  }
  kill() { return 0; }
  create_history_api_identity
  jq -e '
    (.items | length) == 3 and
    all(.items[]; .metadata.namespace == "orka-system") and
    (.items[] | select(.kind == "Role") | .rules) == [{apiGroups:["core.orka.ai"],resources:["sessions"],resourceNames:["native-session"],verbs:["get"]}] and
    (.items[] | select(.kind == "RoleBinding") | .subjects) == [{kind:"ServiceAccount",name:"native-history-client",namespace:"orka-system"}]
  ' "${TMP_ROOT}/identity.json" >/dev/null
  python3 - "${TMP_ROOT}" <<'PY'
import pathlib, stat, sys
for name in ('native-history-token', 'native-history-header'):
    assert stat.S_IMODE((pathlib.Path(sys.argv[1]) / name).stat().st_mode) == 0o600
PY
  service_read orka-system orka-api 8080 '/api/v1/sessions/native-session?namespace=orka-system' "${TMP_ROOT}/native-history-header" >/dev/null
  grep -Fxq -- "@${TMP_ROOT}/native-history-header" "${TMP_ROOT}/curl-arguments"
  if grep -Fq 'fixture-history-token' "${TMP_ROOT}/curl-arguments"; then
    echo 'native history credential entered curl arguments' >&2
    exit 1
  fi
  fixture_read /fixture/marker-counts >/dev/null
  if grep -Fq -- '--header' "${TMP_ROOT}/curl-arguments"; then
    echo 'native history credential was sent to the model fixture' >&2
    exit 1
  fi
  token_failure=failed
  if create_history_api_identity; then
    echo 'native history read continued after token creation failed' >&2
    exit 1
  fi
  [[ ! -e "${TMP_ROOT}/native-history-header" ]]
)

# Cleanup uses a separate identity scoped to the one Session being archived.
(
  TMP_ROOT="${test_root}/session-identity"
  mkdir -p "${TMP_ROOT}"
  token_failure=""
  kubectl() {
    case "$*" in
      '-n orka-system apply -f -') cat >"${TMP_ROOT}/identity.json" ;;
      '-n orka-system create token native-cleanup-client --duration='*m)
        local minutes="${6#--duration=}"
        minutes="${minutes%m}"
        [[ "${minutes}" =~ ^[0-9]+$ ]] || return 9
        # Match the API server's minimum TokenRequest lifetime.
        if (( 10#${minutes} < 10 )); then
          echo 'cleanup TokenRequest duration is below the Kubernetes minimum' >&2
          return 8
        fi
        [[ -z "${token_failure}" ]] || return 7
        printf 'fixture-cleanup-token\n'
        ;;
      *'port-forward '*) ;;
      *) return 9 ;;
    esac
  }
  curl() {
    printf '%s\n' "$@" >"${TMP_ROOT}/curl-arguments"
    printf '204'
  }
  kill() { return 0; }
  create_cleanup_api_identity cancel-session
  jq -e '
    (.items | length) == 3 and
    all(.items[]; .metadata.namespace == "orka-system") and
    (.items[] | select(.kind == "Role") | .rules) == [{apiGroups:["core.orka.ai"],resources:["sessions"],resourceNames:["cancel-session"],verbs:["get","delete"]}] and
    (.items[] | select(.kind == "RoleBinding") | .subjects) == [{kind:"ServiceAccount",name:"native-cleanup-client",namespace:"orka-system"}]
  ' "${TMP_ROOT}/identity.json" >/dev/null
  python3 - "${TMP_ROOT}" <<'PY'
import pathlib, stat, sys
for name in ('native-cleanup-token', 'native-cleanup-header'):
    assert stat.S_IMODE((pathlib.Path(sys.argv[1]) / name).stat().st_mode) == 0o600
PY
  [[ "$(service_status DELETE orka-system orka-api 8080 '/api/v1/sessions/cancel-session?namespace=orka-system' "${TMP_ROOT}/native-cleanup-header")" == 204 ]]
  grep -Fxq -- "@${TMP_ROOT}/native-cleanup-header" "${TMP_ROOT}/curl-arguments"
  grep -Fxq -- DELETE "${TMP_ROOT}/curl-arguments"
  grep -Fxq -- '%{http_code}' "${TMP_ROOT}/curl-arguments"
  if grep -Fq 'fixture-cleanup-token' "${TMP_ROOT}/curl-arguments"; then
    echo 'Session cleanup credential entered curl arguments' >&2
    exit 1
  fi
  token_failure=failed
  if create_cleanup_api_identity cancel-session; then
    echo 'Session cleanup continued after token creation failed' >&2
    exit 1
  fi
  [[ ! -e "${TMP_ROOT}/native-cleanup-header" ]]
)

# Archival retries unsettled work and proves absence with a GET. Neither an
# authorization failure nor a transport error can release the cleanup identity.
(
  TMP_ROOT="${test_root}/session-cleanup"
  mkdir -p "${TMP_ROOT}"
  create_cleanup_api_identity() {
    [[ "$1" == cancel-session ]]
    printf 'fixture-header\n' >"${TMP_ROOT}/native-cleanup-header"
  }
  service_status() {
    printf '%s\n' "$1" >>"${TMP_ROOT}/calls"
    local count=0
    if [[ -f "${TMP_ROOT}/$1-count" ]]; then count=$(cat "${TMP_ROOT}/$1-count"); fi
    printf '%s\n' "$((count + 1))" >"${TMP_ROOT}/$1-count"
    case "${scenario}:$1" in
      transport:DELETE) return 7 ;;
      denied:DELETE) printf '403' ;;
      unsettled:DELETE) printf '409' ;;
      missing:DELETE|missing:GET) printf '404' ;;
      bad-read:GET) printf '503' ;;
      readable:GET) printf '200' ;;
      *:DELETE) if (( count == 0 )); then printf '409'; else printf '204'; fi ;;
      *:GET) if (( count == 0 )); then printf '200'; else printf '404'; fi ;;
      *) return 9 ;;
    esac
  }
  kubectl() {
    [[ "$*" == '-n orka-system delete serviceaccount,role,rolebinding native-cleanup-client' ]] || return 9
    printf 'revoke\n' >>"${TMP_ROOT}/calls"
  }
  sleep() { :; }
  date() {
    local tick
    tick=$(cat "${TMP_ROOT}/clock")
    if [[ "${scenario}" == unsettled || "${scenario}" == readable ]]; then
      printf '%s\n' "$((tick + 130))" >"${TMP_ROOT}/clock"
    else
      printf '%s\n' "$((tick + 1))" >"${TMP_ROOT}/clock"
    fi
    printf '%s\n' "${tick}"
  }
  for scenario in success missing denied bad-read transport unsettled readable; do
    rm -f "${TMP_ROOT}/DELETE-count" "${TMP_ROOT}/GET-count"
    : >"${TMP_ROOT}/calls"
    printf '0\n' >"${TMP_ROOT}/clock"
    if delete_native_session cancel-session >"${TMP_ROOT}/output" 2>&1; then
      [[ "${scenario}" == success || "${scenario}" == missing ]]
      grep -Fxq GET "${TMP_ROOT}/calls"
      [[ "$(tail -n 1 "${TMP_ROOT}/calls")" == revoke ]]
      [[ ! -e "${TMP_ROOT}/native-cleanup-header" ]]
    else
      [[ "${scenario}" != success && "${scenario}" != missing ]]
      if grep -Fxq revoke "${TMP_ROOT}/calls"; then
        echo 'failed Session cleanup was treated as absence' >&2
        exit 1
      fi
    fi
  done
)

# Direct egress changes exactly the supported deployment argument, even when
# containers or arguments move. Ambiguous state and failed updates stop setup.
(
  deployment='{"metadata":{"uid":"api-uid","resourceVersion":"42"},"spec":{"template":{"spec":{"containers":[{"name":"sidecar","args":["--unrelated"]},{"name":"ate-api-server","args":["--unrelated","--egress-gateway-address=atenet-egress.ate-system.svc:443","--other"]}]}}}}'
  egress_failure=""
  kubectl() {
    case "$*" in
      '-n ate-system get deployment ate-api-server -o json')
        [[ "${egress_failure}" != read ]] || return 7
        printf '%s\n' "${deployment}"
        ;;
      '-n ate-system patch deployment ate-api-server --type=json -p '*)
        [[ "${egress_failure}" != patch ]] || return 7
        printf '%s\n' "${@: -1}" >"${test_root}/egress-patch.json"
        ;;
      '-n ate-system rollout status deployment/ate-api-server --timeout=5m')
        [[ "${egress_failure}" != rollout ]] || return 7
        ;;
      *) return 9 ;;
    esac
  }
  substrate_configure_direct_egress
  jq -e '. == [
    {op:"test",path:"/metadata/uid",value:"api-uid"},
    {op:"test",path:"/metadata/resourceVersion",value:"42"},
    {op:"replace",path:"/spec/template/spec/containers/1/args/1",value:"--egress-gateway-address="}
  ]' "${test_root}/egress-patch.json" >/dev/null
  for egress_failure in read patch rollout; do
    if substrate_configure_direct_egress >"${test_root}/egress-error" 2>&1; then
      echo 'Substrate setup ignored a failed direct egress configuration' >&2
      exit 1
    fi
  done
  egress_failure=""
  deployment="$(jq '.spec.template.spec.containers[1].args += ["--egress-gateway-address=another:443"]' <<<"${deployment}")"
  if substrate_configure_direct_egress >"${test_root}/egress-error" 2>&1; then
    echo 'Substrate setup accepted ambiguous egress configuration' >&2
    exit 1
  fi
)

# The local installer must bind the same limited worker namespace access as
# Helm, then test the installed controller identity before submitting Tasks.
(
  ORKA_NAMESPACE=isolated-controller
  rbac_failure=""
  kubectl() {
    printf '%s\n' "$*" >>"${test_root}/rbac-calls"
    case "$*" in
      '-n ate-demo apply -f -') cat >"${test_root}/worker-rbac.json" ;;
      *'auth can-i '*) ;;
      *) return 9 ;;
    esac
    [[ -z "${rbac_failure}" || "$*" != *"${rbac_failure}"* ]]
  }
  grant_substrate_worker_access
  jq -e '
    .items as $items |
    ($items | map(select(.kind == "Role")) | .[0]) as $role |
    ($items | map(select(.kind == "RoleBinding")) | .[0]) as $binding |
    ($items | length) == 2 and
    all($items[]; .metadata.namespace == "ate-demo") and
    ($role.rules | length) == 2 and
    any($role.rules[]; .apiGroups == [""] and .resources == ["pods"] and (.verbs | sort) == ["delete", "get", "list"]) and
    any($role.rules[]; .apiGroups == ["networking.k8s.io"] and .resources == ["networkpolicies"] and (.verbs | sort) == ["create", "delete", "get", "list", "patch", "update", "watch"]) and
    $binding.roleRef == {apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:$role.metadata.name} and
    $binding.subjects == [{kind:"ServiceAccount",name:"orka-controller-manager",namespace:"isolated-controller"}]
  ' "${test_root}/worker-rbac.json" >/dev/null
  grep -Fxq -- 'auth can-i list workerpools.ate.dev --all-namespaces --as=system:serviceaccount:isolated-controller:orka-controller-manager --quiet' "${test_root}/rbac-calls"
  grep -Fxq -- '-n ate-demo auth can-i delete pods --as=system:serviceaccount:isolated-controller:orka-controller-manager --quiet' "${test_root}/rbac-calls"
  for rbac_failure in 'apply -f -' 'list workerpools.ate.dev' 'delete pods' 'create networkpolicies'; do
    if grant_substrate_worker_access >"${test_root}/rbac-error" 2>&1; then
      echo 'Substrate setup ignored missing controller permissions' >&2
      exit 1
    fi
  done
)

# A failed API read is not proof of deletion.
kubectl() { return 7; }
if wait_absent task gone; then
  echo 'cleanup treated an API error as absence' >&2
  exit 1
fi
kubectl() { return 0; }
wait_absent task gone
unset -f kubectl

# Terminal failure must not wait out the success deadline. Unknown or unreadable
# status must not pass. These checks complete without sleeping or a cluster.
job_status='{"status":{"conditions":[{"type":"Complete","status":"True"}]}}'
kubectl() { printf '%s\n' "${job_status}"; }
wait_job complete 0
for condition in Failed FailureTarget; do
  job_status="{\"status\":{\"conditions\":[{\"type\":\"${condition}\",\"status\":\"True\"}]}}"
  if wait_job failed 600 2>"${test_root}/error"; then
    echo 'failed conformance job passed' >&2
    exit 1
  fi
  grep -Fq 'job/failed failed' "${test_root}/error"
done
job_status='{"status":{}}'
if wait_job incomplete 0 2>"${test_root}/error"; then
  echo 'incomplete conformance job passed' >&2
  exit 1
fi
grep -Fq 'Timed out' "${test_root}/error"
kubectl() { return 7; }
if wait_job unreadable 0; then
  echo 'unreadable conformance job passed' >&2
  exit 1
fi

# Terminal Tasks must stop an incompatible wait before garbage collection
# removes the pool's diagnostic status, while an expected settlement still passes.
runtime_diagnostics() { printf 'runtime status captured\n' >&2; }
kubectl() { printf '%s\n' "${job_status}"; }
for task_phase in Failed Succeeded Cancelled; do
  job_status="$(jq -cn --arg phase "${task_phase}" '{status:{phase:$phase}}')"
  wait_field task settled '.status.phase' "${task_phase}"
  if wait_field task settled '.status.phase' Running 600 2>"${test_root}/error"; then
    echo 'terminal ACP task passed its running wait' >&2
    exit 1
  fi
  grep -Fq 'Task/settled settled' "${test_root}/error"
  grep -Fq 'runtime status captured' "${test_root}/error"
done
job_status='{"status":{"state":"Failed"}}'
wait_field executionworkspace failed '.status.state' Failed
if wait_field executionworkspace failed '.status.state' Suspended 0 2>"${test_root}/error"; then
  echo 'failed workspace passed its suspension wait' >&2
  exit 1
fi
grep -Fq 'ExecutionWorkspace/failed failed' "${test_root}/error"
grep -Fq 'runtime status captured' "${test_root}/error"
job_status='{"status":{}}'
if wait_field task incomplete '.status.phase' Running 0 2>"${test_root}/error"; then
  echo 'incomplete ACP task passed its running wait' >&2
  exit 1
fi
grep -Fq 'Timed out' "${test_root}/error"
kubectl() { return 7; }
if wait_field task unreadable '.status.phase' Running 0; then
  echo 'unreadable ACP task passed its running wait' >&2
  exit 1
fi

# A prompt ID alone cannot prove that inference reached the fixture. Keep the
# one-request limit and reject failure, missing delivery, and duplicate calls.
fixture_key_value="$(fixture_key ORKA_NATIVE_FIRST_OK)"
fixture_count=1
fixture_read() { jq -cn --arg key "${fixture_key_value}" --argjson count "${fixture_count}" '{($key):$count}'; }
wait_fixture_request first ORKA_NATIVE_FIRST_OK 0
for fixture_count in 2 '"invalid"' -1; do
  if wait_fixture_request duplicate ORKA_NATIVE_FIRST_OK 0; then
    echo 'invalid fixture count passed the restart barrier' >&2
    exit 1
  fi
done
fixture_count=0
job_status='{"status":{"phase":"Running"}}'
kubectl() { printf '%s\n' "${job_status}"; }
if wait_fixture_request missing ORKA_NATIVE_FIRST_OK 0 2>"${test_root}/error"; then
  echo 'missing inference passed the restart barrier' >&2
  exit 1
fi
grep -Fq 'never reached the provider fixture' "${test_root}/error"
for task_phase in Failed Succeeded Cancelled; do
  job_status="$(jq -cn --arg phase "${task_phase}" '{status:{phase:$phase}}')"
  if wait_fixture_request settled ORKA_NATIVE_FIRST_OK 600 2>"${test_root}/error"; then
    echo 'terminal Task without inference passed the fixture barrier' >&2
    exit 1
  fi
  grep -Fq 'settled before reaching the provider fixture' "${test_root}/error"
done
fixture_read() { return 7; }
if wait_fixture_request unreadable ORKA_NATIVE_FIRST_OK 0; then
  echo 'unreadable fixture passed the restart barrier' >&2
  exit 1
fi

# A terminal Task alone cannot prove its provider request was cancelled. The
# fixture must observe one disconnect and one request, including after cleanup.
fixture_count=1
fixture_disconnects=1
fixture_read() {
  case "$1" in
    /fixture/marker-counts) jq -cn --arg key "${fixture_key_value}" --argjson count "${fixture_count}" '{($key):$count}' ;;
    /fixture/marker-observations) jq -cn --arg key "${fixture_key_value}" --argjson count "${fixture_disconnects}" '{($key):{disconnects:$count}}' ;;
    *) return 9 ;;
  esac
}
wait_fixture_disconnect ORKA_NATIVE_FIRST_OK 0
for fixture_disconnects in 0 2 '"invalid"' -1; do
  if wait_fixture_disconnect ORKA_NATIVE_FIRST_OK 0 2>"${test_root}/error"; then
    echo 'invalid disconnect evidence passed cancellation' >&2
    exit 1
  fi
done
fixture_disconnects=1
for fixture_count in 0 2; do
  if wait_fixture_disconnect ORKA_NATIVE_FIRST_OK 0; then
    echo 'missing or replayed request passed cancellation' >&2
    exit 1
  fi
done
fixture_read() { return 7; }
if wait_fixture_disconnect ORKA_NATIVE_FIRST_OK 0; then
  echo 'unreadable fixture passed cancellation' >&2
  exit 1
fi
unset -f fixture_read
unset -f runtime_diagnostics
source "${root}/scripts/agent-substrate-e2e.sh"

# Job logs pass through redaction before cleanup prints them. Never dump a Pod
# spec, even when a container fails before it can produce logs.
saved_run_dir="${TMP_ROOT}"
TMP_ROOT="${test_root}/diagnostics"
mkdir -p "${TMP_ROOT}"
printf 'fixture-bootstrap-value\n' >"${TMP_ROOT}/bootstrap-token"
kubectl() {
  case "$*" in
    *'get pods'*) printf '{"items":[{"metadata":{"name":"failed-pod"},"spec":{"env":"spec-must-not-be-printed"},"status":{"phase":"Failed"}}]}\n' ;;
    *'get tasks,runtimepools,'*) printf '{"items":[{"kind":"RuntimePool","metadata":{"name":"blocked-pool"},"spec":{"env":"spec-must-not-be-printed"},"status":{"lifecycle":"Degraded","message":"native provisioning failed fixture-bootstrap-value","result":"result-must-not-be-printed"}}]}\n' ;;
    *'logs '*) printf 'native boot failed\nAuthorization: Bearer fixture-header-value\nfixture-bootstrap-value\n' ;;
    *) return 9 ;;
  esac
}
job_diagnostics failed >"${test_root}/diagnostics.log" 2>&1
runtime_diagnostics >>"${test_root}/diagnostics.log" 2>&1
grep -Fq 'native boot failed' "${test_root}/diagnostics.log"
grep -Fq 'failed-pod' "${test_root}/diagnostics.log"
grep -Fq 'blocked-pool' "${test_root}/diagnostics.log"
grep -Fq 'native provisioning failed' "${test_root}/diagnostics.log"
if grep -Eq 'fixture-header-value|fixture-bootstrap-value|spec-must-not-be-printed|result-must-not-be-printed' "${test_root}/diagnostics.log"; then
  echo 'conformance diagnostics exposed credentials or Pod specs' >&2
  exit 1
fi
TMP_ROOT="${saved_run_dir}"
unset -f kubectl

mkdir -p "${test_root}/tools"
cat >"${test_root}/tools/docker" <<'SH'
#!/usr/bin/env bash
exit 1
SH
cat >"${test_root}/tools/git" <<'SH'
#!/usr/bin/env bash
printf 'unexpected provider access\n' >>"${ORKA_SUBSTRATE_TEST_CALLS}"
exit 99
SH
chmod +x "${test_root}/tools/docker" "${test_root}/tools/git"
export ORKA_SUBSTRATE_TEST_CALLS="${test_root}/unexpected-calls"
original_path="${PATH}"
export PATH="${test_root}/tools:${PATH}"
# Calling in a conditional disables errexit inside a Bash function. The
# preflight must still return failure before creating or installing anything.
if substrate_prepare_upstream "${root}" "${test_root}/run" guarded-cluster 2>"${test_root}/error"; then
  echo 'preflight ignored an unavailable Docker engine' >&2
  exit 1
fi
[[ ! -e "${ORKA_SUBSTRATE_TEST_CALLS}" ]]
[[ ! -e "${test_root}/run" ]]
grep -Fq 'Docker engine is unavailable' "${test_root}/error"
# A working Docker stub cannot authorize a provider fork or a movable ref.
printf '#!/usr/bin/env bash\nexit 0\n' >"${test_root}/tools/docker"
for selection in fork branch; do
  if [[ "${selection}" == fork ]]; then
    export SUBSTRATE_REPO=https://example.invalid/provider-fork.git
    unset SUBSTRATE_REF
  else
    unset SUBSTRATE_REPO
    export SUBSTRATE_REF=main
  fi
  if substrate_prepare_upstream "${root}" "${test_root}/run" guarded-cluster 2>"${test_root}/error"; then
    echo 'preflight accepted an unsupported provider source' >&2
    exit 1
  fi
  [[ ! -e "${ORKA_SUBSTRATE_TEST_CALLS}" ]]
  grep -Fq 'provider forks and patches are unsupported' "${test_root}/error"
done
unset SUBSTRATE_REPO SUBSTRATE_REF
export PATH="${original_path}"

# A reusable provider checkout must reject source files Git does not track,
# as well as staged and unstaged changes to tracked files.
provider_dir="${test_root}/provider"
git init -q "${provider_dir}"
printf 'package fixture\n' >"${provider_dir}/fixture.go"
git -C "${provider_dir}" add fixture.go
git -C "${provider_dir}" -c user.name=Fixture -c user.email=fixture@example.invalid -c commit.gpgsign=false commit -qs -m 'fixture'
substrate_require_clean_upstream "${provider_dir}"
printf 'package fixture\n' >"${provider_dir}/unexpected.go"
if substrate_require_clean_upstream "${provider_dir}" 2>"${test_root}/error"; then
  echo 'preflight accepted an untracked provider source file' >&2
  exit 1
fi
rm "${provider_dir}/unexpected.go"
printf '// changed\n' >>"${provider_dir}/fixture.go"
for state in unstaged staged; do
  if [[ "${state}" == staged ]]; then git -C "${provider_dir}" add fixture.go; fi
  if substrate_require_clean_upstream "${provider_dir}" 2>"${test_root}/error"; then
    echo "preflight accepted ${state} provider changes" >&2
    exit 1
  fi
done

source "${root}/hack/agent-substrate/upstream.env"
[[ "$(shasum -a 256 "${root}/internal/substratepb/ateapi.proto" | awk '{print $1}')" == "${SUBSTRATE_UPSTREAM_PROTO_SHA256}" ]]
printf 'Native Substrate source, preflight, and absence checks passed\n'
