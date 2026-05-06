#!/usr/bin/env bash
# Lightweight, vendored fallback for the subset of demo-magic used by the Orka
# demo scripts. It provides the p/pe/run_cmd functions expected by
# hack/demos/lib/common.sh without requiring a separate checkout under ~/src.
#
# If you prefer the upstream demo-magic behavior, set DEMO_MAGIC_PATH to that
# demo-magic.sh file before running the demo scripts.

# This file is sourced, so avoid `set -euo pipefail` here.

DEMO_MAGIC_DEBUG=0
DEMO_MAGIC_NO_WAIT="${DEMO_MAGIC_NO_WAIT:-0}"

while (($#)); do
  case "$1" in
    -d|--debug|--no-wait)
      # Keep demo runs non-blocking by default for this vendored fallback.
      DEMO_MAGIC_DEBUG=1
      DEMO_MAGIC_NO_WAIT=1
      ;;
    -n|--no-wait)
      DEMO_MAGIC_NO_WAIT=1
      ;;
  esac
  shift || break
done

if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  COLOR_RESET="${COLOR_RESET:-$(tput sgr0 2>/dev/null || printf '')}"
  GREEN="${GREEN:-$(tput setaf 2 2>/dev/null || printf '')}"
  CYAN="${CYAN:-$(tput setaf 6 2>/dev/null || printf '')}"
  BOLD="${BOLD:-$(tput bold 2>/dev/null || printf '')}"
else
  COLOR_RESET="${COLOR_RESET:-}"
  GREEN="${GREEN:-}"
  CYAN="${CYAN:-}"
  BOLD="${BOLD:-}"
fi

TYPE_SPEED="${TYPE_SPEED:-}"
PROMPT_TIMEOUT="${PROMPT_TIMEOUT:-0}"
DEMO_PROMPT="${DEMO_PROMPT:-${GREEN}demo ${CYAN}\W ${COLOR_RESET}}"

_demo_magic_render_prompt() {
  local prompt="${DEMO_PROMPT}"
  prompt="${prompt//\\W/$(basename "${PWD}")}"
  printf '%b' "${prompt}"
}

_demo_magic_wait() {
  if [[ "${DEMO_MAGIC_NO_WAIT}" == "1" || ! -t 0 || ! -t 1 ]]; then
    return 0
  fi

  if [[ -n "${PROMPT_TIMEOUT:-}" && "${PROMPT_TIMEOUT}" != "0" ]]; then
    read -r -t "${PROMPT_TIMEOUT}" -p "" _demo_magic_unused || true
  else
    read -r -p "" _demo_magic_unused || true
  fi
}

p() {
  printf '\n%b\n' "$*"
  _demo_magic_wait
}

run_cmd() {
  eval "$@"
}

pe() {
  local cmd="$*"
  printf '\n'
  _demo_magic_render_prompt
  printf '%s\n' "${cmd}"
  _demo_magic_wait
  run_cmd "${cmd}"
}
