#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
verify_scene handoff

chapter "Engineering receives the selected finding"
pe "orka task get $fix_task |
  jq '.metadata | {name, namespace}'"
note "Custom demo bridge. Engineering's own permissions."
nap 0.7
