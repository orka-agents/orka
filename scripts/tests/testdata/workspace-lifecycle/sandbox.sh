#!/usr/bin/env bash
# Local command fixtures for workspace_lifecycle_test.go.

set -Eeuo pipefail
acp_task_namespace=fixture
ORKA_NAMESPACE=fixture
ambiguous_session=fixture-ambiguous
restart_session=fixture-restart
reset_lc_sessions="fixture-first fixture-timeout fixture-cancel fixture-ambiguous fixture-restart"
log() { :; }
die() { printf '%s\n' "$*" >&2; exit 1; }
run() { "$@"; }
sleep() { :; }
date() {
  local tick
  tick=$(cat "$TEST_DIR/clock")
  printf '%s\n' "$((tick + 150))" >"$TEST_DIR/clock"
  printf '%s\n' "$tick"
}
fixture_marker_count() {
  printf '%s\n' fixture-count >>"$TEST_DIR/calls"
  if [[ "$SCENARIO" == replay ]]; then printf '2\n'; else printf '1\n'; fi
}
fixture_marker_disconnects() {
  printf '%s\n' fixture-disconnect >>"$TEST_DIR/calls"
  if [[ "$SCENARIO" == no-disconnect ]]; then printf '0\n'; else printf '1\n'; fi
}
delete_fixed_session() {
  if [[ "$1" == orka-ws-lc-cancel-session ]]; then
    [[ $(grep -c '^fixture-count$' "$TEST_DIR/calls") == 2 ]]
    grep -q '^fixture-disconnect$' "$TEST_DIR/calls"
  fi
  printf 'archive:%s\n' "$1" >>"$TEST_DIR/calls"
  [[ "$SCENARIO" != archive-conflict ]] || return 1
  touch "$TEST_DIR/archived"
}
kubectl() {
  [[ "$1" == -n && "$2" == fixture ]]
  shift 2
  case "$1 $2" in
    'get task')
      [[ ! -e "$TEST_DIR/released" ]] || return 1
      if [[ -e "$TEST_DIR/archived" && "$SCENARIO" != finalizer-retained ]]; then
        printf '%s\n' '{"metadata":{"finalizers":["acp-e2e.orka.ai/lifecycle-observer"]}}'
      else
        printf '%s\n' '{"metadata":{"finalizers":["orka.ai/cleanup","acp-e2e.orka.ai/lifecycle-observer"]}}'
      fi
      ;;
    'patch task')
      [[ -e "$TEST_DIR/archived" && "$SCENARIO" != finalizer-retained ]]
      printf '%s\n' observer-release >>"$TEST_DIR/calls"
      touch "$TEST_DIR/released"
      ;;
    'delete task')
      if [[ " $* " == *' --wait=false '* ]]; then
        printf '%s\n' cancellation-request >>"$TEST_DIR/calls"
      else
        [[ -e "$TEST_DIR/archived" ]]
        printf '%s\n' task-absence >>"$TEST_DIR/calls"
      fi
      ;;
    *) return 90 ;;
  esac
}
wait_resource_absent() {
  [[ -e "$TEST_DIR/released" ]]
  printf '%s\n' task-absence >>"$TEST_DIR/calls"
}
