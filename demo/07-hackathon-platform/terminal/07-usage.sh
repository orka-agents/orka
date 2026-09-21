#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
verify_scene usage
orka usage other unassociated --from 2026-09-20T04:34:00Z --limit 100 -o json |
  python3 "$capture_repo/demo/06-hackathon/terminal/view.py" usage >/dev/null

chapter "Make agent work quantifiable"
say "Reported usage for one Teams request."
pe 'orka usage other unassociated --from 2026-09-20T04:34:00Z --limit 100 -o json |
  jq '\''.otherWork.tasks[] |
    select(.taskName == "gw-c08666cf65136687ed1e01e22591863e7fa3fb42") |
    .usage | {inputTokens, outputTokens, completeness, modelCost}'\'''
note "ACP scan token counts are unavailable. Missing usage stays visible."
nap 1.2
