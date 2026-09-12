#!/usr/bin/env bash
# Shared logging and precondition helpers for E2E and release scripts.
# Source this file; do not execute it. Scripts that need a different log
# format (for example timestamped output) may redefine log after sourcing.

log() {
  printf '==> %s\n' "$*" >&2
}

warn() {
  printf 'warning: %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}
