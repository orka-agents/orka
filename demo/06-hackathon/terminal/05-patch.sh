#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
view ready patch

chapter "Engineering team: patch and checks"
say "The coding agent prepares changes. Orka handles publication."
pe "orka task result $fix_task -o json | view patch"
note "Agent report excerpt. The complete result is retained with the task."
nap 1.2
