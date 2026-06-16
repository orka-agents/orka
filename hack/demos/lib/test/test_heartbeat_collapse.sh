#!/usr/bin/env bash
# __demo_heartbeat collapses consecutive same-state frames on the non-tty
# (recording) path: only the first occurrence of each distinct state prints,
# and the monotonic elapsed= counter does not by itself force a new line.
# This is the fix for the cast "elapsed=Ns" progress-spam that dominated 84-96%
# of playback in the Jun-2026 batch.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# Force the non-tty render path regardless of where the test runs: redirect fd2
# to a file so [[ -t 2 ]] is false inside __demo_heartbeat.
out_file="$(mktemp)"
trap 'rm -f "${out_file}"' EXIT

export DEMO_RECORD_PROFILE=docs
unset DEMO_WAIT_QUIET

{
  __demo_heartbeat_reset
  __demo_heartbeat 'task=t phase=Pending elapsed=%ss' 0
  __demo_heartbeat 'task=t phase=Pending elapsed=%ss' 5
  __demo_heartbeat 'task=t phase=Running elapsed=%ss' 10
  __demo_heartbeat 'task=t phase=Running elapsed=%ss' 15
  __demo_heartbeat 'task=t phase=Running elapsed=%ss' 20
  __demo_heartbeat 'task=t phase=Succeeded elapsed=%ss' 25
} 2>"${out_file}"

line_count="$(grep -c . "${out_file}" || true)"
assert_eq "${line_count}" "3" "6 heartbeats across 3 states must collapse to 3 lines"

# Each distinct state appears exactly once.
assert_contains "$(cat "${out_file}")" "phase=Pending"
assert_contains "$(cat "${out_file}")" "phase=Running"
assert_contains "$(cat "${out_file}")" "phase=Succeeded"
pending_n="$(grep -c 'phase=Pending' "${out_file}" || true)"
running_n="$(grep -c 'phase=Running' "${out_file}" || true)"
assert_eq "${pending_n}" "1" "Pending state printed once"
assert_eq "${running_n}" "1" "Running state printed once (not 3x)"

# A reset re-arms emission even when the next state matches the last one.
{
  __demo_heartbeat 'task=t phase=Succeeded elapsed=%ss' 30
  __demo_heartbeat_reset
  __demo_heartbeat 'task=t phase=Succeeded elapsed=%ss' 0
} 2>>"${out_file}"
succeeded_n="$(grep -c 'phase=Succeeded' "${out_file}" || true)"
assert_eq "${succeeded_n}" "2" "post-reset same-state heartbeat emits again"

# hero profile and DEMO_WAIT_QUIET suppress entirely.
quiet_file="$(mktemp)"
DEMO_WAIT_QUIET=1 __demo_heartbeat 'task=t phase=Running elapsed=%ss' 1 2>"${quiet_file}"
assert_eq "$(grep -c . "${quiet_file}" || true)" "0" "DEMO_WAIT_QUIET suppresses heartbeat"
rm -f "${quiet_file}"

printf 'PASS\n'
