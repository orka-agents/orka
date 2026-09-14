#!/usr/bin/env bash
# Local command fixtures for workspace_lifecycle_test.go.

set -Eeuo pipefail
workspace=fixture-workspace
submit_task() { :; }
wait_fixture_request() { :; }
wait_field() {
  if [[ "$4" == true ]]; then
    printf 'settlement\n' >>"$TEST_DIR/calls"
    [[ "$SCENARIO" != unsettled ]]
  elif [[ "$1" == executionworkspace && "$4" == Suspended ]]; then
    printf 'workspace-suspended\n' >>"$TEST_DIR/calls"
    [[ "$SCENARIO" != not-suspended ]]
  fi
}
workspace_for_task() {
  [[ "$1" == native-cancel ]]
  printf '%s\n' fixture-workspace
}
kubectl_ate() {
  [[ "$*" == 'get actors --atespace orka-system -o json' ]]
  printf 'actors-checked\n' >>"$TEST_DIR/calls"
  if [[ "$SCENARIO" == active-actor ]]; then
    printf '%s\n' '{"actors":[{}]}'
  else
    printf '%s\n' '{"actors":[]}'
  fi
}
kubectl() {
  case "$*" in
    '-n orka-system delete task native-cancel --wait=false')
      printf 'cancellation-request\n' >>"$TEST_DIR/calls"
      ;;
    '-n orka-system delete executionworkspace fixture-workspace --wait=false')
      [[ -e "$TEST_DIR/archived" ]]
      printf 'workspace-delete\n' >>"$TEST_DIR/calls"
      ;;
    *) return 90 ;;
  esac
}
wait_fixture_disconnect() {
  printf 'provider-disconnect\n' >>"$TEST_DIR/calls"
  [[ "$SCENARIO" != no-disconnect ]]
}
delete_native_session() {
  printf 'archive:%s\n' "$1" >>"$TEST_DIR/calls"
  [[ "$SCENARIO" != archive-conflict ]] || return 1
  touch "$TEST_DIR/archived"
}
wait_absent() {
  [[ -e "$TEST_DIR/archived" ]]
  printf '%s-absence\n' "$1" >>"$TEST_DIR/calls"
}
