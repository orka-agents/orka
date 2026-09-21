#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
verify_scene patch

chapter "Engineering: a patch with focused tests"
say "The agent removes shell execution and checks URL handling."
pe "orka task result $fix_task -o json |
  jq -r '.result | split(\"Tests:\")[1] | split(\"\n\nLimitations:\")[0]' |
  fold -s -w 88"
note "Agent report excerpt. Full app and database flows were not tested."
nap 1.2
