#!/usr/bin/env bash
# shellcheck disable=SC2034,SC2317,SC2329 # Extracted functions consume fixture state and mocks.
set -Eeuo pipefail

if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "error: this test suite requires bash >= 4; found ${BASH_VERSION}" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/scripts/live-acp-runtime-e2e.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/orka-acp-session-cleanup.XXXXXX")"
trap 'rm -rf "${test_root}"' EXIT

fail() { printf 'not ok - %s\n' "$*" >&2; exit 1; }
extract_function() {
  awk -v signature="$1() {" '$0 == signature { copying=1 } copying { print } copying && $0 == "}" { exit }' "${script}"
}
for name in archive_test_sessions settle_and_delete_test_tasks provider_pool_cleanup_owned runtimepool_mutations_allowed; do
  body="$(extract_function "${name}")"
  [[ -n "${body}" ]] || fail "missing ${name}"
  eval "${body}"
done

setup() {
  temp_root="${test_root}/$1"
  mkdir -p "${temp_root}"
  namespace="fixture-namespace"
  namespace_shared=1
  run_id="fixture-run"
  state_wait_seconds=37
  events="${temp_root}/events"
  selected="${temp_root}/selected.json"
  current="${temp_root}/current.json"
  session_payload="${temp_root}/session.json"
  conflicts=0
  session_absent=0
  : >"${events}"
  cat >"${current}" <<'JSON'
{"items":[
  {"metadata":{"name":"read","uid":"read-uid","creationTimestamp":"2026-09-09T10:00:00Z","labels":{"orka.ai/acp-e2e-run":"fixture-run"}},"spec":{"sessionRef":{"name":"owned-session","create":true}},"status":{"execution":{"runtimeSessionUID":"owned-session-uid","runtimePoolName":"owned-pool","runtimePoolUID":"owned-pool-uid"}}},
  {"metadata":{"name":"continue","uid":"continue-uid","creationTimestamp":"2026-09-09T10:01:00Z","labels":{"orka.ai/acp-e2e-run":"fixture-run"}},"spec":{"sessionRef":{"name":"owned-session","create":false}},"status":{"execution":{"runtimeSessionUID":"owned-session-uid","runtimePoolName":"owned-pool","runtimePoolUID":"owned-pool-uid"}}},
  {"metadata":{"name":"foreign","uid":"foreign-uid","labels":{"orka.ai/acp-e2e-run":"other-run"}},"spec":{"sessionRef":{"name":"foreign-session","create":true}},"status":{"execution":{"runtimePoolName":"foreign-pool","runtimePoolUID":"foreign-pool-uid"}}}
]}
JSON
  jq --arg run "${run_id}" '{items:[.items[] | select(.metadata.labels["orka.ai/acp-e2e-run"] == $run)]}' \
    "${current}" >"${selected}"
  cat >"${session_payload}" <<'JSON'
{"name":"owned-session","namespace":"fixture-namespace","createdAt":"2026-09-09T10:00:01Z","executionControl":{"sessionUID":"owned-session-uid"},"transcript":"must not be retained"}
JSON
}

log() { :; }
warn() { printf 'warning:%s\n' "$*" >>"${events}"; }
safe_task_summary() { :; }
sleep() { printf 'retry\n' >>"${events}"; }
k() {
  if [[ "$*" == "-n ${namespace} get task -l orka.ai/acp-e2e-run=${run_id} -o json" ]]; then
    cat "${selected}"
  elif [[ "$*" == "-n ${namespace} get task -o json" ]]; then
    cat "${current}"
  elif [[ "$1 $2 $3 $4" == "-n ${namespace} delete task" ]]; then
    [[ "$6" == "--wait=false" ]] || fail 'Task deletion waited before Session archival'
    printf 'delete-task:%s\n' "$5" >>"${events}"
    jq --arg name "$5" '(.items[] | select(.metadata.name == $name) | .metadata.deletionTimestamp) = "2026-09-09T10:02:00Z"' \
      "${current}" >"${temp_root}/next.json"
    mv "${temp_root}/next.json" "${current}"
  else
    fail "unexpected kubectl call: $*"
  fi
}
probe_task() {
  task_probe_file="${temp_root}/probe.json"
  jq --arg name "$1" '.items[] | select(.metadata.name == $name)' "${current}" >"${task_probe_file}"
  task_probe_state="absent"
  [[ ! -s "${task_probe_file}" ]] || task_probe_state="present"
}
api_request() {
  [[ "$2" == "/api/v1/sessions/owned-session?namespace=${namespace}" ]] || fail "unexpected Session target: $2"
  case "$1" in
    GET)
      api_response_status=200
      [[ "${session_absent}" -ne 1 ]] || api_response_status=404
      cp "${session_payload}" "${temp_root}/api-response.json"
      ;;
    DELETE)
      [[ "$(grep -c '^delete-task:' "${events}")" == 2 ]] || fail 'Session archival preceded Task cancellation requests'
      [[ ! -s "${temp_root}/api-response.json" ]] || fail 'Session transcript remained in the response file'
      if (( conflicts > 0 )); then
        conflicts=$((conflicts - 1))
        api_response_status=409
        printf 'archive-conflict\n' >>"${events}"
      else
        api_response_status=204
        printf 'archive-session\n' >>"${events}"
        jq '{items:[.items[] | select(.spec.sessionRef.name != "owned-session")]}' \
          "${current}" >"${temp_root}/next.json"
        mv "${temp_root}/next.json" "${current}"
      fi
      ;;
    *) fail "unexpected API method: $1" ;;
  esac
}
wait_until() {
  printf 'barrier:%s\n' "$1" >>"${events}"
  grep -q '^archive-session$' "${events}" || fail 'Task finalizer wait preceded Session archival'
  shift 2
  "$@"
}
task_deletion_barrier_complete() { probe_task "$1"; [[ "${task_probe_state}" == "absent" ]]; }
task_absent() { probe_task "$1"; [[ "${task_probe_state}" == "absent" ]]; }
release_task_observer_finalizer() { fail 'cleanup touched a controller finalizer'; }

(
  setup ordering
  conflicts=1
  settle_and_delete_test_tasks "${temp_root}/owners"
  [[ "$(grep -c '^archive-session$' "${events}")" == 1 ]] || fail 'shared Session was archived more than once'
  grep -q '^archive-conflict$' "${events}" || fail 'fixture did not exercise unsettled Session retry'
  grep -q '^retry$' "${events}" || fail 'unsettled Session was not retried'
  jq -e '.items | length == 1 and .[0].metadata.uid == "foreign-uid"' "${current}" >/dev/null
  grep -q '^owned-session-uid$' "${temp_root}/owners" || fail 'Session UID was lost before BranchClaim cleanup'
)
printf '%s\n' 'ok - run-owned Session is archived once after cancellation and before Task finalizer waits; foreign Session is preserved'

for scenario in foreign-reference changed-task-uid changed-task-label changed-session-uid borrowed-session old-session; do
  (
    setup "${scenario}"
    case "${scenario}" in
      foreign-reference) filter='(.items[] | select(.metadata.name == "foreign") | .spec.sessionRef.name) = "owned-session"' ;;
      changed-task-uid) filter='(.items[] | select(.metadata.name == "read") | .metadata.uid) = "replacement-uid"' ;;
      changed-task-label) filter='(.items[] | select(.metadata.name == "read") | .metadata.labels["orka.ai/acp-e2e-run"]) = "other-run"' ;;
      changed-session-uid)
        jq '.executionControl.sessionUID = "replacement-session-uid"' "${session_payload}" >"${temp_root}/next.json"
        mv "${temp_root}/next.json" "${session_payload}"
        filter='.' ;;
      borrowed-session)
        jq '(.items[].spec.sessionRef.create) = false' "${selected}" >"${temp_root}/next.json"
        mv "${temp_root}/next.json" "${selected}"
        filter='.' ;;
      old-session)
        jq '.createdAt = "2026-09-09T09:59:00Z"' "${session_payload}" >"${temp_root}/next.json"
        mv "${temp_root}/next.json" "${session_payload}"
        filter='.' ;;
    esac
    jq "${filter}" "${current}" >"${temp_root}/next.json"
    mv "${temp_root}/next.json" "${current}"
    if settle_and_delete_test_tasks "${temp_root}/owners"; then
      fail "cleanup accepted ${scenario}"
    fi
    if grep -Eq '^archive-session$|^archive-conflict$|^barrier:' "${events}"; then
      fail "unsafe Session cleanup progressed for ${scenario}"
    fi
  )
done
printf '%s\n' 'ok - Session cleanup rejects foreign references, replacement identities, and Sessions not created by the run'

(
  setup absent
  session_absent=1
  archive_test_sessions "${selected}"
  [[ ! -s "${events}" ]] || fail 'already archived Session caused a mutation'
)
printf '%s\n' 'ok - an already absent Session needs no repeated DELETE'

(
  setup pool-guards
  k() {
    if [[ "$*" == "-n ${namespace} get runtimepool owned-pool -o json" ]]; then
      jq -n --arg uid "${pool_uid}" --arg ns "${namespace}" '{metadata:{uid:$uid},spec:{trustDomain:{namespace:$ns},runtime:{profile:{providerKind:"codex"}}}}'
    elif [[ "$*" == "-n ${namespace} get task -o json" ]]; then
      cat "${current}"
    else
      fail "unexpected pool guard call: $*"
    fi
  }
  pool_uid="owned-pool-uid"
  if provider_pool_cleanup_owned owned-pool owned-pool-uid codex; then
    fail 'pool cleanup accepted remaining Task references'
  fi
  printf '{"items":[]}\n' >"${current}"
  provider_pool_cleanup_owned owned-pool owned-pool-uid codex
  pool_uid="replacement-uid"
  if provider_pool_cleanup_owned owned-pool owned-pool-uid codex; then
    fail 'pool cleanup accepted a replacement UID'
  fi
)
printf '%s\n' 'ok - provider pool retirement rejects live Task references and replacement pool UIDs'

for allowed in 0 1; do
  (
    setup "shared-handoff-${allowed}"
    shared_pool_mutation_allowed="${allowed}"
    eval "$(extract_function remove_provider_resources)"
    assert_all_tasks_validated() { :; }
    settle_and_delete_test_tasks() {
      cp "${selected}" "${temp_root}/cleanup-tasks.json"
      printf 'read-uid\ncontinue-uid\nowned-session-uid\n' >"$1"
      jq '{items:[.items[] | select(.metadata.name == "foreign")]}' "${current}" >"${temp_root}/next.json"
      mv "${temp_root}/next.json" "${current}"
    }
    record_runtime_namespace() { :; }
    pool_stopped() { :; }
    wait_until() { shift 2; "$@"; }
    delete_test_branchclaims() { :; }
    die() { fail "$*"; }
    cat >"${temp_root}/pools.json" <<'JSON'
{"items":[
  {"metadata":{"name":"owned-pool","uid":"owned-pool-uid"},"spec":{"trustDomain":{"namespace":"fixture-namespace"},"runtime":{"profile":{"providerKind":"codex"}},"runtimeNamespace":"fixture-runtimes"}},
  {"metadata":{"name":"foreign-pool","uid":"foreign-pool-uid"},"spec":{"trustDomain":{"namespace":"fixture-namespace"},"runtime":{"profile":{"providerKind":"codex"}},"runtimeNamespace":"fixture-runtimes"}}
]}
JSON
    k() {
      case "$*" in
        "-n ${namespace} get runtimepool -o json") cat "${temp_root}/pools.json" ;;
        "-n ${namespace} get runtimepool owned-pool -o json") jq '.items[0]' "${temp_root}/pools.json" ;;
        "-n ${namespace} get task -o json") cat "${current}" ;;
        "-n ${namespace} patch runtimepool owned-pool --type=json -p "*)
          jq -e '.[0] == {op:"test",path:"/metadata/uid",value:"owned-pool-uid"}
            and .[1] == {op:"add",path:"/spec/desiredReplicas",value:0}' <<<"$8" >/dev/null
          printf 'park:owned-pool\n' >>"${events}" ;;
        "-n ${namespace} delete runtimepool owned-pool --wait=true --timeout=5m")
          printf 'delete:owned-pool\n' >>"${events}" ;;
        "-n ${namespace} delete agent owned-agent --ignore-not-found=true --wait=true --timeout=2m") : ;;
        *) fail "unexpected handoff operation: $*" ;;
      esac
    }
    remove_provider_resources codex owned-agent
    if [[ "${allowed}" -eq 1 ]]; then
      [[ "$(cat "${events}")" == $'park:owned-pool\ndelete:owned-pool' ]] || fail 'opt-in handoff omitted its owned pool'
    else
      [[ ! -s "${events}" ]] || fail 'shared handoff mutated a pool without opt-in'
    fi
  )
done
printf '%s\n' 'ok - shared handoff honors opt-in and retires only pools identified by run-owned Task UIDs'

for allowed in 0 1; do
  (
    setup "opencode-profile-${allowed}"
    shared_pool_mutation_allowed="${allowed}"
    opencode_model="fixture-model"
    opencode_agent="fixture-agent"
    opencode_task="fixture-task"
    opencode_session="fixture-session"
    opencode_nonce="fixture-nonce"
    expected_license_line="fixture-license"
    run_read_smoke() { read_smoke_pool="owned-pool"; }
    park_runtimepool() { printf 'park:%s\n' "$1" >>"${events}"; }
    phase="$(awk '/^run_read_smoke opencode / { copying=1 } copying && /^opencode_policy_agent=/ { exit } copying { print }' "${script}")"
    [[ -n "${phase}" ]] || fail 'OpenCode profile transition is missing'
    eval "${phase}"
    if [[ "${allowed}" -eq 1 ]]; then
      [[ "$(cat "${events}")" == 'park:owned-pool' ]] || fail 'OpenCode read pool was not parked before its policy profile'
    else
      [[ ! -s "${events}" ]] || fail 'OpenCode profile transition bypassed the shared mutation guard'
    fi
  )
done
printf '%s\n' 'ok - OpenCode parks its completed read profile before policy validation only within the allowed mutation scope'

(
  setup identity
  for name in sanitize_name create_api_identity_resource create_api_identity delete_api_identity; do
    eval "$(extract_function "${name}")"
  done
  api_token_file="${temp_root}/token"
  api_auth_header_file="${temp_root}/header"
  api_identity_inventory="${temp_root}/identity.tsv"
  identity_collision=0
  identity_replaced=0
  k() {
    if [[ "$*" == 'create -f - -o json' ]]; then
      [[ "${identity_collision}" -eq 0 ]] || return 1
      jq -c '.metadata.uid = (.kind + "-uid")' >"${temp_root}/created.json"
      cat "${temp_root}/created.json" >>"${temp_root}/identity.jsonl"
      cat "${temp_root}/created.json"
    elif [[ "$*" == "-n ${namespace} create token acp-api-fixture-run --duration=2h" ]]; then
      printf 'fixture\n'
    elif [[ "$1 $2 $3" == "-n ${namespace} get" ]]; then
      jq -s --arg kind "$4" --argjson replaced "${identity_replaced}" '
        .[] | select((.kind | ascii_downcase) == $kind)
        | if $replaced == 1 then .metadata.uid = "replacement-uid" else . end
      ' "${temp_root}/identity.jsonl"
    elif [[ "$1 $2 $3" == "-n ${namespace} delete" ]]; then
      printf 'delete-identity:%s:%s\n' "$4" "$5" >>"${events}"
    else
      fail "unexpected identity call: $*"
    fi
  }
  create_api_identity
  jq -s -e '[.[] | select(.kind == "Role") | .rules[] | select(.resources == ["sessions"])] ==
    [{apiGroups:["core.orka.ai"],resources:["sessions"],verbs:["get","delete"]}]' "${temp_root}/identity.jsonl" >/dev/null
  jq -s -e 'length == 3 and all(.[]; .metadata.name == "acp-api-fixture-run"
    and .metadata.labels["orka.ai/acp-e2e-run"] == "fixture-run")' "${temp_root}/identity.jsonl" >/dev/null
  identity_replaced=1
  if delete_api_identity; then
    fail 'API identity cleanup accepted a replacement UID'
  fi
  if grep -q '^delete-identity:' "${events}"; then
    fail 'API identity cleanup removed a replacement object'
  fi
  identity_replaced=0
  delete_api_identity
  [[ "$(grep -c '^delete-identity:' "${events}")" == 3 ]] || fail 'run-owned API resources were not removed'
  identity_collision=1
  if create_api_identity; then
    fail 'API setup accepted a pre-existing identity'
  fi
  [[ "$(wc -l <"${api_identity_inventory}" | tr -d ' ')" == 3 ]] || fail 'API setup recorded a foreign identity'
  : >"${events}"
  setup_body="$(awk '/^# Smoke cleanup also / { copying=1 } copying && /^session_nonce=/ { exit } copying { print }' "${script}")"
  [[ -n "${setup_body}" ]] || fail 'common API setup is missing'
  create_api_identity() { printf 'identity\n' >>"${events}"; }
  start_api_forward() { printf 'forward\n' >>"${events}"; }
  release_gate=0
  eval "${setup_body}"
  [[ "$(cat "${events}")" == $'identity\nforward' ]] || fail 'smoke mode omitted authenticated API setup'
)
printf '%s\n' 'ok - smoke initializes a create-only API identity with narrow Session permissions and UID-checked cleanup'

(
  setup api-status
  eval "$(extract_function api_request)"
  api_local_port=18080
  api_auth_header_file="${temp_root}/unused-fixture-header"
  ensure_api_forward() { :; }
  redact() { cat; }
  curl() {
    local output=""
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == "--output" ]]; then
        output="$2"
        shift
      fi
      shift
    done
    printf 'fixture response\n' >"${output}"
    printf '%s' "${http_status}"
  }
  http_status=204
  api_request DELETE /fixture >/dev/null
  http_status=404
  if api_request DELETE /fixture >/dev/null 2>&1; then
    fail 'ordinary API requests silently accepted NotFound'
  fi
  for http_status in 404 409; do
    api_request DELETE /fixture "" '^(204|404|409)$' >/dev/null
    [[ "${api_response_status}" == "${http_status}" ]] || fail 'cleanup lost the HTTP status'
  done
  http_status=403
  if api_request DELETE /fixture "" '^(204|404|409)$' >/dev/null 2>&1; then
    fail 'Session cleanup accepted authorization failure'
  fi
)
printf '%s\n' 'ok - cleanup handles only explicit absent/unsettled statuses and rejects authorization errors'
