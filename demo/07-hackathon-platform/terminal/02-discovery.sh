#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
verify_scene discovery

chapter "Security: from source code to evidence"
pe "orka security scan list hackathon-nodejs-goof -o json |
  jq '.items[0] | {reviewedSliceCount, sliceCount, acceptedFindings}'"
pe "orka security finding get fnd_0afb30da8140 |
  jq '{title, severity, validationStatus, filePath, line}'"
note "An image URL reaches a shell command."
nap 1.2
