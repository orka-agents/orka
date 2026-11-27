#!/usr/bin/env bash

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

DEMO_RECORD_PROFILE=presenter
DEMO_CHAPTER_TOTAL=4

chapter "first"  "▸" > "${tmp}/c1"
chapter "second" "▸" > "${tmp}/c2"

assert_contains "$(cat "${tmp}/c1")" "Chapter 1/4 — first"
assert_contains "$(cat "${tmp}/c2")" "Chapter 2/4 — second"

unset DEMO_CHAPTER_TOTAL
chapter "third" "▸" > "${tmp}/c3"
assert_contains    "$(cat "${tmp}/c3")" "Chapter 3 — third"
assert_not_contains "$(cat "${tmp}/c3")" "/4"

printf 'PASS\n'
