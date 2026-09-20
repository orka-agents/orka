#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
view ready schedule

chapter "Reliability team: automatic start"
say "This scan began on a schedule, without a new prompt."
pe "orka task get hackathon-scheduled-scan | view schedule"
pe "orka task logs hackathon-scheduled-scan-1789878840"
pe "orka security scan list hackathon-nodejs-goof -o json | view scan"
note "Known vulnerable demo application. Execution time is condensed."
nap 0.7
