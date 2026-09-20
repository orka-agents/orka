#!/usr/bin/env bash
source "$(dirname -- "$0")/common.sh"
export DEMO_TEAM=reliability
view ready discovery

chapter "Reliability team: discover and validate"
say "Agents review the source and retain findings with evidence."
pe "orka security scan list hackathon-nodejs-goof -o json | view progress"
say "One selected finding for Engineering:"
pe "orka security finding get fnd_0afb30da8140 | view finding"
note "Untrusted image input reaches a shell command."
nap 1.2
