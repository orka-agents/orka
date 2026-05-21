#!/usr/bin/env bash
# payoff_card_pr returns non-zero when the task result has no PR URL,
# and zero when it does. We stub summarize_task_run with two different
# JSON payloads to exercise both branches without a cluster.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# No PR URL → non-zero exit.
summarize_task_run() {
  jq -n '{
    task: "demo-x",
    phase: "Succeeded",
    agent: "coord",
    childTasks: 0,
    result: "implementation failed: no pull request"
  }'
}

set +e
out_missing="$(payoff_card_pr "demo-x" 2>&1)"
rc_missing=$?
set -e
assert_status_nonzero "${rc_missing}" "payoff_card_pr without PR URL must fail"
assert_contains "${out_missing}" "no PR URL"

# Real PR URL → exit 0, card body contains the URL.
summarize_task_run() {
  jq -n '{
    task: "demo-y",
    phase: "Succeeded",
    agent: "coord",
    childTasks: 4,
    result: "Final status:\n- Validation: PASSED\nPR: https://github.com/sozercan/vekil/pull/77"
  }'
}

set +e
out_ok="$(payoff_card_pr "demo-y")"
rc_ok=$?
set -e
assert_status_zero "${rc_ok}" "payoff_card_pr with PR URL must succeed"
assert_contains "${out_ok}" "Pull request opened"
assert_contains "${out_ok}" "demo-y"
assert_contains "${out_ok}" "https://github.com/sozercan/vekil/pull/77"

printf 'PASS\n'
