#!/usr/bin/env bash
# Shared test helpers. Sourced by every test_*.sh file.
#
# Loads demo-magic (with -n) + style.sh in a NO_COLOR shell so assertions
# compare against plain text. Provides assert_* primitives.

set -Eeuo pipefail

export NO_COLOR=1

_test_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_demo_lib_dir="$(cd "${_test_lib_dir}/.." && pwd)"

# shellcheck source=hack/demos/lib/demo-magic.sh
. "${_demo_lib_dir}/demo-magic.sh" -n
# shellcheck source=hack/demos/lib/style.sh
. "${_demo_lib_dir}/style.sh"

assert_eq() {
  local got="$1"
  local want="$2"
  local msg="${3:-assertion}"
  if [[ "${got}" != "${want}" ]]; then
    printf 'FAIL %s\n  want: %q\n  got:  %q\n' "${msg}" "${want}" "${got}" >&2
    exit 1
  fi
}

assert_contains() {
  local hay="$1"
  local needle="$2"
  local msg="${3:-assertion}"
  if [[ "${hay}" != *"${needle}"* ]]; then
    printf 'FAIL %s\n  needle: %q\n  hay:    %q\n' "${msg}" "${needle}" "${hay}" >&2
    exit 1
  fi
}

assert_not_contains() {
  local hay="$1"
  local needle="$2"
  local msg="${3:-assertion}"
  if [[ "${hay}" == *"${needle}"* ]]; then
    printf 'FAIL %s\n  forbidden: %q\n  hay:       %q\n' "${msg}" "${needle}" "${hay}" >&2
    exit 1
  fi
}

assert_status_zero() {
  local rc="$1"
  local msg="${2:-expected status 0}"
  if [[ "${rc}" -ne 0 ]]; then
    printf 'FAIL %s (got rc=%d)\n' "${msg}" "${rc}" >&2
    exit 1
  fi
}

assert_status_nonzero() {
  local rc="$1"
  local msg="${2:-expected non-zero status}"
  if [[ "${rc}" -eq 0 ]]; then
    printf 'FAIL %s (got rc=0)\n' "${msg}" >&2
    exit 1
  fi
}
