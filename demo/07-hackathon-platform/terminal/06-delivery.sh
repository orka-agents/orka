#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
verify_scene delivery
# Require the retained receipt and current open PR to match before recording.
orka task status "$fix_task" -o json |
  python3 "$capture_repo/demo/06-hackathon/terminal/view.py" delivery >/dev/null

chapter "A real pull request, ready for review"
pe "orka task status $fix_task -o json |
  jq '{execution: .execution.state, delivery: .delivery.state,
       pullRequest: .delivery.prReceipt.url}'"
note "VerifiedExact: the published commit matches the task."
nap 1.2
