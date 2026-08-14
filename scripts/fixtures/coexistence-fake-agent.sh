#!/bin/sh
# Deterministic, model-free fake agent CLI for the coexistence live E2E.
#
# The harness-v1 wrapper runs this binary through its generic adapter
# (ORKA_HARNESS_WRAPPER_RUNTIME=generic + ORKA_HARNESS_WRAPPER_COMMAND):
# the turn prompt arrives on stdin and everything written to stdout becomes
# the terminal turn result. No network, no credentials, no model access.
#
# Prompts containing COEXISTENCE_HOLD_TURN keep the turn running long enough
# for the E2E to restart the v1 controller while the attempt is active.
set -eu

prompt="$(cat)"

case "${prompt}" in
  *COEXISTENCE_HOLD_TURN*)
    sleep 45
    ;;
esac

printf 'coexistence-fake-agent-result: %s\n' "${prompt}"
