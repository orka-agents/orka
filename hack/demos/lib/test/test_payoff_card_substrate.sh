#!/usr/bin/env bash
# payoff_card_substrate asserts provider=substrate on both agentic tasks,
# reused=true on the warm task, and a non-empty PR URL. We stub kubectl to
# return canned status.executionWorkspace fields and exercise the pass + each
# failure branch without a cluster.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# The card reads status.executionWorkspace.<field> via:
#   kubectl get task <name> -n <ns> -o jsonpath={.status.executionWorkspace.<field>}
__field_from_args() {
  local a
  for a in "$@"; do
    case "${a}" in
      *executionWorkspace.*) printf '%s' "${a##*executionWorkspace.}" | tr -d '}' ;;
    esac
  done
}

make_kubectl() {
  # $1=cold provider, $2=cold phase
  # $3=warm provider, $4=warm phase, $5=warm reused
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

# --- Happy path: both substrate, warm reattached, PR present -> exit 0 ------
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
# Card bodies must stay ASCII — multi-byte glyphs break byte-width alignment.
assert_not_contains "${out_ok}" "—"
assert_not_contains "${out_ok}" "≠"
assert_not_contains "${out_ok}" "→"

# --- Wrong provider -> non-zero --------------------------------------------
make_kubectl agent-sandbox Retained substrate Retained true
set +e
out_provider="$(payoff_card_substrate "${cold}" "${warm}" "${pr}" 2>&1)"
rc_provider=$?
set -e
assert_status_nonzero "${rc_provider}" "non-substrate provider must fail"
assert_contains "${out_provider}" "provider=substrate"

# --- Warm not reattached -> non-zero ---------------------------------------
make_kubectl substrate Retained substrate Retained false
set +e
out_reuse="$(payoff_card_substrate "${cold}" "${warm}" "${pr}" 2>&1)"
rc_reuse=$?
set -e
assert_status_nonzero "${rc_reuse}" "reuse failure (reused != true) must fail"
assert_contains "${out_reuse}" "reuse FAILED"

# --- Missing PR URL -> non-zero --------------------------------------------
make_kubectl substrate Retained substrate Retained true
set +e
out_nopr="$(payoff_card_substrate "${cold}" "${warm}" "" 2>&1)"
rc_nopr=$?
set -e
assert_status_nonzero "${rc_nopr}" "missing PR URL must fail"
assert_contains "${out_nopr}" "no pull request"

printf 'PASS\n'
