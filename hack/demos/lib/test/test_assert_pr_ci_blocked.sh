#!/usr/bin/env bash
# assert_real_pr_result accepts CI_BLOCKED ONLY when Validation AND Review both
# pass (an environmental/secret-gated CI block, not a code failure), and still
# rejects CI_BLOCKED without that evidence, VALIDATION_BLOCKED/REVIEW_BLOCKED,
# and bare FAILED tokens. This is the fix for Demo 20 ending on `exit 1`.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# assert_real_pr_result lives in common.sh; load it on top of the style.sh the
# helpers already sourced. common.sh sets defaults under `set -u`; it is safe to
# source in a NO_COLOR test shell.
export DEMO_NAMESPACE="demo-test"
# shellcheck source=hack/demos/lib/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

# Stub the cluster/API calls. orka_api dispatches on URL; RESULT_TEXT is the
# fixture result body. Task always reports Succeeded; no failed children.
orka_api() {
  local _method="$1" url="$2"
  case "${url}" in
    *"/result"*)    jq -n --arg r "${RESULT_TEXT}" '{result: $r}' ;;
    *"/children"*)  printf '%s' '{"items":[]}' ;;
    *"?namespace"*) printf '%s' '{"status":{"phase":"Succeeded"}}' ;;
    *)              printf '%s' '{}' ;;
  esac
}
kubectl() { printf '%s' '{"items":[]}'; }

PR="PR: https://github.com/sozercan/vekil/pull/201"

expect_result() {
  local name="$1" want="$2"; RESULT_TEXT="$3"
  local got
  if assert_real_pr_result "demo-task" >/dev/null 2>&1; then got=ACCEPT; else got=REJECT; fi
  assert_eq "${got}" "${want}" "${name}"
}

expect_result "CI_BLOCKED + Validation PASS + Review APPROVED -> accept" ACCEPT \
  "$(printf 'Final status:\n- Validation: PASSED\n- Review: APPROVED\n- CI: CI_BLOCKED\n%s' "${PR}")"

expect_result "CI_BLOCKED alone -> reject" REJECT \
  "$(printf 'CI: CI_BLOCKED\n%s' "${PR}")"

expect_result "CI_BLOCKED + Validation only (no review) -> reject" REJECT \
  "$(printf 'Final status:\n- Validation: PASSED\n- CI: CI_BLOCKED\n%s' "${PR}")"

expect_result "VALIDATION_BLOCKED -> reject" REJECT \
  "$(printf 'Validation: VALIDATION_BLOCKED\n%s' "${PR}")"

expect_result "clean success -> accept" ACCEPT \
  "$(printf 'Final status:\n- Validation: PASSED\n- Review: APPROVED\n- CI: PASSED\n%s' "${PR}")"

expect_result "bare FAILED token -> reject" REJECT \
  "$(printf 'Final status:\n- Validation: PASSED\n- Review: APPROVED\n- CI: PASSED\nstep FAILED\n%s' "${PR}")"

printf 'PASS\n'
