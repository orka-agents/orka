#!/bin/sh
# Deterministic, model-free fake agent CLI for the coexistence live E2E.
#
# The harness-v1 wrapper runs this binary through its generic adapter
# (ORKA_HARNESS_WRAPPER_RUNTIME=generic + ORKA_HARNESS_WRAPPER_COMMAND):
# the turn prompt arrives on stdin and everything written to stdout becomes
# the terminal turn result. No network, no credentials, no model access.
#
# Prompts containing COEXISTENCE_HOLD_TURN keep the turn running long enough
# for the E2E to restart the v1 controller while the attempt is active. Before
# holding, the fake agent drops a marker file into the wrapper Pod's writable
# /tmp. The wrapper only starts this child process after it has durably
# recorded TurnAccepted in its admission ledger (StartTurn in
# workers/harness/cliwrapper/server.go admits, accepts, then runs the turn),
# so the marker's existence proves the wrapper durably admitted the turn.
set -eu

prompt="$(cat)"

case "${prompt}" in
  *COEXISTENCE_HOLD_TURN*)
    : >/tmp/coexistence-hold-turn-active
    sleep 45
    ;;
esac

printf 'coexistence-fake-agent-result: %s\n' "${prompt}"
