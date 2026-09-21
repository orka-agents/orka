#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
verify_scene schedule

chapter "Security: a scan starts on a schedule"
say "Find undiscovered security vulnerabilities in source code."
pe "orka task get hackathon-scheduled-scan |
  jq '{schedule: .spec.schedule, paused: .spec.suspend,
       lastTick: .status.lastScheduleTime}'"
pe "orka task logs hackathon-scheduled-scan-1789878840"
note "Future ticks paused after the scan."
nap 0.7
