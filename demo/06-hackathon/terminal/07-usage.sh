#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
view ready usage
started=2026-09-20T04:34:00Z

chapter "Recorded work and model usage"
say "A measured Teams request. Missing measurements remain visible."
pe 'orka usage other unassociated --from "$started" --limit 100 -o json | view usage'
nap 0.8
