#!/usr/bin/env bash
# demo_profile defaults to presenter; demo_profile_is matches/fails correctly.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

unset DEMO_RECORD_PROFILE
got_default="$(demo_profile)"
assert_eq "${got_default}" "presenter" "default profile is presenter"

if demo_profile_is presenter; then :; else
  printf 'FAIL demo_profile_is presenter should succeed when unset\n' >&2
  exit 1
fi
if demo_profile_is docs; then
  printf 'FAIL demo_profile_is docs should not succeed when default is presenter\n' >&2
  exit 1
fi

DEMO_RECORD_PROFILE=docs
got_docs="$(demo_profile)"
assert_eq "${got_docs}" "docs"
demo_profile_is docs || { printf 'FAIL demo_profile_is docs when DOCS\n' >&2; exit 1; }

# Unknown value falls back to presenter (defensive).
DEMO_RECORD_PROFILE=bogus
got_fallback="$(demo_profile)"
assert_eq "${got_fallback}" "presenter" "unknown profile falls back to presenter inside style.sh"

printf 'PASS\n'
