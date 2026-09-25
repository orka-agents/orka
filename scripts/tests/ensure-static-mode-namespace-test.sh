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
helper="${root}/scripts/lib/ensure-static-mode-namespace.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/ensure-static-mode-namespace-test.XXXXXX")"
cleanup() {
  rm -rf -- "${test_root}"
}
trap cleanup EXIT

fail() {
  echo "$*" >&2
  exit 1
}

[[ -x "${helper}" ]] || fail "static-mode namespace helper is not executable: ${helper}"

fake_kubectl="${test_root}/kubectl"
cat >"${fake_kubectl}" <<'EOF_FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail

: "${FAKE_NAMESPACE_STATE:?}"
: "${FAKE_CALL_LOG:?}"
: "${FAKE_WRITE_LOG:?}"
: "${FAKE_EXPECTED_NAMESPACE:?}"
: "${FAKE_EXPECTED_MODE:?}"

printf '%s\n' "$*" >>"${FAKE_CALL_LOG}"

namespace_json() {
  local mode="$1"
  jq -n \
    --arg namespace "${FAKE_EXPECTED_NAMESPACE}" \
    --arg mode "${mode}" '{
      apiVersion: "v1",
      kind: "Namespace",
      metadata: {
        name: $namespace,
        labels: {"orka.ai/controller-mode": $mode}
      }
    }'
}

if [[ "$1" == "get" && "$2" == "namespace" && "$3" == "${FAKE_EXPECTED_NAMESPACE}" ]]; then
  [[ " $* " == *" --ignore-not-found "* && " $* " == *" -o json "* ]] || {
    echo "namespace GET is not fail-closed and machine-readable: $*" >&2
    exit 2
  }
  [[ "${FAKE_GET_ERROR:-false}" != "true" ]] || exit 17
  [[ ! -e "${FAKE_NAMESPACE_STATE}" ]] || cat "${FAKE_NAMESPACE_STATE}"
  exit 0
fi

if [[ "$1" == "create" ]]; then
  manifest=""
  args=("$@")
  for ((i = 0; i < ${#args[@]}; i++)); do
    if [[ "${args[$i]}" == "-f" && $((i + 1)) -lt ${#args[@]} ]]; then
      manifest="${args[$((i + 1))]}"
      break
    fi
  done
  [[ -n "${manifest}" && "${manifest}" != "-" && -f "${manifest}" ]] || {
    echo "namespace create must use a concrete manifest: $*" >&2
    exit 2
  }
  jq -e \
    --arg namespace "${FAKE_EXPECTED_NAMESPACE}" \
    --arg mode "${FAKE_EXPECTED_MODE}" '
      .apiVersion == "v1" and
      .kind == "Namespace" and
      .metadata.name == $namespace and
      .metadata.labels["orka.ai/controller-mode"] == $mode
    ' "${manifest}" >/dev/null || {
      echo "namespace was not created with its static identity atomically" >&2
      exit 2
    }

  case "${FAKE_CREATE_BEHAVIOR:-success}" in
    success)
      cp "${manifest}" "${FAKE_NAMESPACE_STATE}"
      printf 'create\n' >>"${FAKE_WRITE_LOG}"
      cat "${FAKE_NAMESPACE_STATE}"
      exit 0
      ;;
    race-exact)
      namespace_json "${FAKE_EXPECTED_MODE}" >"${FAKE_NAMESPACE_STATE}"
      exit 1
      ;;
    race-opposite)
      if [[ "${FAKE_EXPECTED_MODE}" == "harness-v1" ]]; then
        namespace_json harness-v2 >"${FAKE_NAMESPACE_STATE}"
      else
        namespace_json harness-v1 >"${FAKE_NAMESPACE_STATE}"
      fi
      exit 1
      ;;
    failure)
      exit 1
      ;;
    *)
      echo "unknown FAKE_CREATE_BEHAVIOR: ${FAKE_CREATE_BEHAVIOR}" >&2
      exit 2
      ;;
  esac
fi

echo "unexpected fake kubectl invocation: $*" >&2
exit 2
EOF_FAKE_KUBECTL
chmod +x "${fake_kubectl}"

scenario_dir=""
namespace_state=""
call_log=""
write_log=""

reset_scenario() {
  local name="$1"
  scenario_dir="${test_root}/${name}"
  mkdir -p "${scenario_dir}"
  namespace_state="${scenario_dir}/namespace.json"
  call_log="${scenario_dir}/calls.log"
  write_log="${scenario_dir}/writes.log"
  : >"${call_log}"
  : >"${write_log}"
}

write_namespace() {
  local namespace="$1"
  local mode="$2"
  jq -n --arg namespace "${namespace}" --arg mode "${mode}" '{
    apiVersion: "v1",
    kind: "Namespace",
    metadata: {
      name: $namespace,
      labels: {
        "orka.ai/controller-mode": $mode,
        "unrelated.example/retained": "true"
      }
    }
  }' >"${namespace_state}"
}

run_helper() {
  local namespace="$1"
  local mode="$2"
  shift 2
  env \
    FAKE_NAMESPACE_STATE="${namespace_state}" \
    FAKE_CALL_LOG="${call_log}" \
    FAKE_WRITE_LOG="${write_log}" \
    FAKE_EXPECTED_NAMESPACE="${namespace}" \
    FAKE_EXPECTED_MODE="${mode}" \
    "$@" \
    "${helper}" "${fake_kubectl}" "${namespace}" "${mode}"
}

expect_failure() {
  local expected="$1"
  shift
  local output
  if output="$("$@" 2>&1)"; then
    fail "helper unexpectedly succeeded: ${expected}"
  fi
  grep -F "${expected}" <<<"${output}" >/dev/null || {
    printf 'expected failure not found: %s\n%s\n' "${expected}" "${output}" >&2
    exit 1
  }
}

reset_scenario absent
run_helper orka-v2-system harness-v2 >/dev/null
[[ "$(<"${write_log}")" == "create" ]] || fail "fresh namespace was not created exactly once"
jq -e '.metadata.labels["orka.ai/controller-mode"] == "harness-v2"' "${namespace_state}" >/dev/null
run_helper orka-v2-system harness-v2 >/dev/null
[[ "$(wc -l <"${write_log}" | tr -d '[:space:]')" == "1" ]] || fail "same-mode retry rewrote the namespace"

reset_scenario absent-v1
run_helper orka-v1-system harness-v1 >/dev/null
jq -e '.metadata.labels["orka.ai/controller-mode"] == "harness-v1"' "${namespace_state}" >/dev/null

reset_scenario exact-existing
write_namespace orka-v2-system harness-v2
run_helper orka-v2-system harness-v2 >/dev/null
[[ ! -s "${write_log}" ]] || fail "exact existing namespace was rewritten"

for existing_mode in unlabeled opposite; do
  reset_scenario "existing-${existing_mode}"
  if [[ "${existing_mode}" == "unlabeled" ]]; then
    jq -n '{apiVersion:"v1",kind:"Namespace",metadata:{name:"orka-v2-system",labels:{}}}' >"${namespace_state}"
  else
    write_namespace orka-v2-system harness-v1
  fi
  expect_failure "cannot be adopted in place" run_helper orka-v2-system harness-v2
  [[ ! -s "${write_log}" ]] || fail "${existing_mode} namespace caused a write"
done

reset_scenario malformed
printf '%s\n' '{not-json' >"${namespace_state}"
expect_failure "cannot be adopted in place" run_helper orka-v2-system harness-v2
[[ ! -s "${write_log}" ]] || fail "malformed namespace response caused a write"

reset_scenario wrong-name
write_namespace another-system harness-v2
expect_failure "cannot be adopted in place" run_helper orka-v2-system harness-v2
[[ ! -s "${write_log}" ]] || fail "wrong-name namespace response caused a write"

reset_scenario duplicate-response
write_namespace orka-v2-system harness-v2
cp "${namespace_state}" "${scenario_dir}/one.json"
printf '\n' >>"${namespace_state}"
cat "${scenario_dir}/one.json" >>"${namespace_state}"
expect_failure "cannot be adopted in place" run_helper orka-v2-system harness-v2
[[ ! -s "${write_log}" ]] || fail "duplicate namespace response caused a write"

reset_scenario get-error
expect_failure "unable to inspect namespace orka-v2-system" \
  run_helper orka-v2-system harness-v2 FAKE_GET_ERROR=true
[[ ! -s "${write_log}" ]] || fail "namespace GET failure caused a write"

reset_scenario create-race-exact
run_helper orka-v2-system harness-v2 FAKE_CREATE_BEHAVIOR=race-exact >/dev/null
[[ ! -s "${write_log}" ]] || fail "exact create race recorded a helper write"
[[ "$(grep -c '^create ' "${call_log}")" == "1" ]] || fail "exact create race did not attempt one create"
[[ "$(grep -c '^get namespace ' "${call_log}")" == "2" ]] || fail "exact create race was not reread"

reset_scenario create-race-opposite
expect_failure "cannot be adopted in place" \
  run_helper orka-v2-system harness-v2 FAKE_CREATE_BEHAVIOR=race-opposite
[[ ! -s "${write_log}" ]] || fail "opposite create race recorded a helper write"

reset_scenario create-failure
expect_failure "unable to create namespace orka-v2-system" \
  run_helper orka-v2-system harness-v2 FAKE_CREATE_BEHAVIOR=failure
[[ ! -s "${write_log}" ]] || fail "failed namespace create recorded a write"

reset_scenario invalid-mode
expect_failure "controller mode must be harness-v1 or harness-v2" \
  run_helper orka-v2-system dual
[[ ! -s "${call_log}" ]] || fail "invalid controller mode reached kubectl"

reset_scenario empty-namespace
expect_failure "namespace must be non-empty" run_helper "" harness-v2
[[ ! -s "${call_log}" ]] || fail "empty namespace reached kubectl"

printf '%s\n' 'ok - static-mode namespace identity is atomic, immutable, idempotent, and create-race safe'
