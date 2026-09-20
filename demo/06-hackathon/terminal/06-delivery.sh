#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=engineering
view ready delivery

chapter "A delivered result reviewers can inspect"
say "Execution and publication have separate recorded outcomes."
pe "orka task status $fix_task -o json | view delivery"
note "The pull request stays open for human review."
nap 1.2
