#!/usr/bin/env bash

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

__field_from_args() {
  local a
  for a in "$@"; do
    case "${a}" in
      *executionWorkspace.*) printf '%s' "${a##*executionWorkspace.}" | tr -d '}' ;;
    esac
  done
}

make_kubectl() {
  local cp="$1" cph="$2" wp="$3" wph="$4" wr="$5"
  eval "kubectl() {
    local field task
    field=\"\$(__field_from_args \"\$@\")\"
    case \"\$*\" in
      *demo-substrate-cold*) task=cold ;;
      *demo-substrate-warm*) task=warm ;;
      *) task=unknown ;;
    esac
    case \"\${task}.\${field}\" in
      cold.provider) printf '%s' '${cp}'  ;;
      cold.phase)    printf '%s' '${cph}' ;;
      warm.provider) printf '%s' '${wp}'  ;;
      warm.phase)    printf '%s' '${wph}' ;;
      warm.reused)   printf '%s' '${wr}'  ;;
      *) printf '' ;;
    esac
  }"
}

cold="demo-substrate-cold"
warm="demo-substrate-warm"
pr="https://github.com/sozercan/vekil/pull/123"

make_kubectl substrate Retained substrate Retained true
set +e
out_ok="$(payoff_card_substrate "${cold}" "${warm}" "${pr}" 2>&1)"
rc_ok=$?
set -e
assert_status_zero "${rc_ok}" "payoff_card_substrate happy path must succeed"
assert_contains "${out_ok}" "Agent Substrate"
assert_contains "${out_ok}" "substrate"
assert_contains "${out_ok}" "reused"
assert_contains "${out_ok}" "${pr}"
assert_not_contains "${out_ok}" "—"
assert_not_contains "${out_ok}" "≠"
assert_not_contains "${out_ok}" "→"

make_kubectl agent-sandbox Retained substrate Retained true
set +e
out_provider="$(payoff_card_substrate "${cold}" "${warm}" "${pr}" 2>&1)"
rc_provider=$?
set -e
assert_status_nonzero "${rc_provider}" "non-substrate provider must fail"
assert_contains "${out_provider}" "provider=substrate"

make_kubectl substrate Retained substrate Retained false
set +e
out_reuse="$(payoff_card_substrate "${cold}" "${warm}" "${pr}" 2>&1)"
rc_reuse=$?
set -e
assert_status_nonzero "${rc_reuse}" "reuse failure (reused != true) must fail"
assert_contains "${out_reuse}" "reuse FAILED"

make_kubectl substrate Retained substrate Retained true
set +e
out_nopr="$(payoff_card_substrate "${cold}" "${warm}" "" 2>&1)"
rc_nopr=$?
set -e
assert_status_nonzero "${rc_nopr}" "missing PR URL must fail"
assert_contains "${out_nopr}" "no pull request"

printf 'PASS\n'
