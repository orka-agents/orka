#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
view ready handoff

chapter "Engineering receives the selected finding"
say "A small demo bridge connects the separate teams."
pe "orka task get $fix_task | view handoff"
nap 0.7
