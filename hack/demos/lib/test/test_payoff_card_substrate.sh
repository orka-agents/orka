#!/usr/bin/env bash
# payoff_card_substrate asserts provider=substrate on all three tasks and
# reused=true on the reuse task. We stub kubectl to return canned
# status.executionWorkspace fields and exercise the pass + both failure
# branches without a cluster.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# The card reads status.executionWorkspace.<field> via:
#   kubectl get task <name> -n <ns> -o jsonpath={.status.executionWorkspace.<field>}
# Our stub branches on the task name in "$@" and the requested jsonpath field.
__field_from_args() {
  # Echo the trailing jsonpath field name (provider|phase|reused|...) from the
  # kubectl args. The -o value looks like: jsonpath={.status.executionWorkspace.provider}
  local a
  for a in "$@"; do
    case "${a}" in
      *executionWorkspace.*) printf '%s' "${a##*executionWorkspace.}" | tr -d '}' ;;
    esac
  done
}

make_kubectl() {
  # $1=lifecycle provider, $2=lifecycle phase
  # $3=retain provider,    $4=retain phase
  # $5=reuse provider,     $6=reuse phase,  $7=reuse reused
  local lp="$1" lph="$2" rp="$3" rph="$4" up="$5" uph="$6" ur="$7"
  eval "kubectl() {
    local field task
    field=\"\$(__field_from_args \"\$@\")\"
    case \"\$*\" in
      *demo-substrate-lifecycle*) task=life ;;
      *demo-substrate-retain*)    task=retain ;;
      *demo-substrate-reuse*)     task=reuse ;;
      *) task=unknown ;;
    esac
    case \"\${task}.\${field}\" in
      life.provider)   printf '%s' '${lp}'  ;;
      life.phase)      printf '%s' '${lph}' ;;
      retain.provider) printf '%s' '${rp}'  ;;
      retain.phase)    printf '%s' '${rph}' ;;
      reuse.provider)  printf '%s' '${up}'  ;;
      reuse.phase)     printf '%s' '${uph}' ;;
      reuse.reused)    printf '%s' '${ur}'  ;;
      *) printf '' ;;
    esac
  }"
}

life="demo-substrate-lifecycle"
retain="demo-substrate-retain"
reuse="demo-substrate-reuse"

# --- Happy path: all substrate, reuse reattached -> exit 0 -----------------
make_kubectl substrate Deleted substrate Retained substrate Deleted true
set +e
out_ok="$(payoff_card_substrate "${life}" "${retain}" "${reuse}" 2>&1)"
rc_ok=$?
set -e
assert_status_zero "${rc_ok}" "payoff_card_substrate happy path must succeed"
assert_contains "${out_ok}" "Agent Substrate workspaces"
assert_contains "${out_ok}" "substrate"
assert_contains "${out_ok}" "Retained"
assert_contains "${out_ok}" "reused"
# Card bodies must stay ASCII — multi-byte glyphs break byte-width alignment.
assert_not_contains "${out_ok}" "—"
assert_not_contains "${out_ok}" "≠"
assert_not_contains "${out_ok}" "→"

# --- Wrong provider -> non-zero --------------------------------------------
make_kubectl agent-sandbox Deleted substrate Retained substrate Deleted true
set +e
out_provider="$(payoff_card_substrate "${life}" "${retain}" "${reuse}" 2>&1)"
rc_provider=$?
set -e
assert_status_nonzero "${rc_provider}" "non-substrate provider must fail"
assert_contains "${out_provider}" "provider=substrate"

# --- Reuse not reattached -> non-zero --------------------------------------
make_kubectl substrate Deleted substrate Retained substrate Deleted false
set +e
out_reuse="$(payoff_card_substrate "${life}" "${retain}" "${reuse}" 2>&1)"
rc_reuse=$?
set -e
assert_status_nonzero "${rc_reuse}" "reuse failure (reused != true) must fail"
assert_contains "${out_reuse}" "reuse FAILED"

printf 'PASS\n'
