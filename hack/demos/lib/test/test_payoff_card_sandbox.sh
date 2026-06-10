#!/usr/bin/env bash
# payoff_card_sandbox asserts all three turns reattached the SAME sandbox claim.
# It resolves each turn's claim from an eager-capture file written at turn
# success (primary, survives pod GC) and falls back to live pod logs. These
# tests stub the filesystem + kubectl to exercise every branch without a
# cluster — and lock in the regression that motivated the rewrite:
#
#   Under `set -euo pipefail`, a missed `grep` in the extraction pipeline used
#   to return 1, which propagated through `c=$(...)` and ABORTED the demo
#   before the card box rendered. The card MUST always render its box and
#   return a clean non-zero on a real failure — never crash the run.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

session="vekil-metrics-77"
t1="demo-sandbox-turn-1-scout"
t2="demo-sandbox-turn-2-builder"
t3="demo-sandbox-turn-3-fixup"

workdir="$(mktemp -d -t orka-sandbox-card.XXXXXX)"
trap 'rm -rf "${workdir}"' EXIT
export DEMO_WORKDIR="${workdir}"
export DEMO_NAMESPACE="demo-magic"

write_capture() { printf '%s\n' "$2" > "${workdir}/sandbox-claim-$1.txt"; }
clear_captures() { rm -f "${workdir}"/sandbox-claim-*.txt; }

# Default kubectl stub: no pods exist (simulates pod garbage collection by the
# time the end-of-demo card runs). Tests that exercise the log fallback
# override this.
kubectl() { printf ''; }

# --- Happy path: capture files agree -> exit 0, box renders -----------------
clear_captures
write_capture "${t1}" "orka-session-abc123"
write_capture "${t2}" "orka-session-abc123"
write_capture "${t3}" "orka-session-abc123"
set +e
out_ok="$(payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}" 2>&1)"
rc_ok=$?
set -e
assert_status_zero "${rc_ok}" "payoff_card_sandbox happy path must succeed"
assert_contains "${out_ok}" "Agent sandbox session"
assert_contains "${out_ok}" "${session}"
assert_contains "${out_ok}" "orka-session-abc123"
assert_contains "${out_ok}" "reused across all three turns"

# --- REGRESSION: no captures + no pods -> box STILL renders, clean rc=1 ------
# This is the exact 60b failure: pods GC'd, no capture file. The old code
# aborted before the box printed. The box must render and the card must return
# 1 (not crash) so the demo reports a clean, explained failure.
clear_captures
set +e
out_empty="$(payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}" 2>&1)"
rc_empty=$?
set -e
assert_status_nonzero "${rc_empty}" "missing claim must fail cleanly (not crash)"
assert_contains "${out_empty}" "Agent sandbox session"   # box rendered
assert_contains "${out_empty}" "(no claim)"               # placeholder shown
assert_contains "${out_empty}" "could not extract claim name"

# --- Mismatch: captures disagree -> non-zero, reuse-failed message ----------
clear_captures
write_capture "${t1}" "orka-session-aaa"
write_capture "${t2}" "orka-session-bbb"
write_capture "${t3}" "orka-session-aaa"
set +e
out_mismatch="$(payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}" 2>&1)"
rc_mismatch=$?
set -e
assert_status_nonzero "${rc_mismatch}" "differing claims must fail"
assert_contains "${out_mismatch}" "claim reuse FAILED"

# --- Fallback: no capture files, but pod logs carry the claim line ----------
# Exercises the log path + the (agent-)?sandbox regex for BOTH worker formats
# (generic "agent-sandbox workspace" and sandbox-specific "sandbox workspace").
clear_captures
kubectl() {
  case "$*" in
    *"get pods"*)
      # jsonpath asks for the pod name; return a deterministic one per task.
      case "$*" in
        *"${t1}"*) printf 'pod-1' ;;
        *"${t2}"*) printf 'pod-2' ;;
        *"${t3}"*) printf 'pod-3' ;;
        *) printf '' ;;
      esac ;;
    *"logs"*"pod-1"*) printf 'Task demo-magic/%s completed in agent-sandbox workspace orka-session-xyz\n' "${t1}" ;;
    *"logs"*"pod-2"*) printf 'Task demo-magic/%s completed in sandbox workspace orka-session-xyz\n' "${t2}" ;;
    *"logs"*"pod-3"*) printf 'Task demo-magic/%s completed in agent-sandbox workspace orka-session-xyz\n' "${t3}" ;;
    *) printf '' ;;
  esac
}
set +e
out_fallback="$(payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}" 2>&1)"
rc_fallback=$?
set -e
assert_status_zero "${rc_fallback}" "pod-log fallback with matching claims must succeed"
assert_contains "${out_fallback}" "orka-session-xyz"
assert_contains "${out_fallback}" "reused across all three turns"

# --- Capture file takes precedence over pod logs ---------------------------
# Even with the log-bearing kubectl above, a capture file must win (it is the
# authoritative success-time snapshot).
clear_captures
write_capture "${t1}" "orka-session-fromfile"
write_capture "${t2}" "orka-session-fromfile"
write_capture "${t3}" "orka-session-fromfile"
set +e
out_prec="$(payoff_card_sandbox "${session}" "${t1}" "${t2}" "${t3}" 2>&1)"
rc_prec=$?
set -e
assert_status_zero "${rc_prec}" "capture-file precedence path must succeed"
assert_contains "${out_prec}" "orka-session-fromfile"
assert_not_contains "${out_prec}" "orka-session-xyz"

printf 'PASS\n'
