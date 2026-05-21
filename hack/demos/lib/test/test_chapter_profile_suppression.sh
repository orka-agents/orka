#!/usr/bin/env bash
# social profile suppresses chapters past index 3; hero suppresses all.
#
# Uses tmp-file capture instead of $() so __DEMO_CHAPTER_INDEX increments
# survive across calls in this test process.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

# social: chapters 1..3 print, 4+ are silent.
DEMO_RECORD_PROFILE=social
DEMO_CHAPTER_TOTAL=6
__DEMO_CHAPTER_INDEX=0
chapter "one"   "▸" > "${tmp}/s1"
chapter "two"   "▸" > "${tmp}/s2"
chapter "three" "▸" > "${tmp}/s3"
chapter "four"  "▸" > "${tmp}/s4"
chapter "five"  "▸" > "${tmp}/s5"

assert_contains "$(cat "${tmp}/s1")" "Chapter 1/6 — one"
assert_contains "$(cat "${tmp}/s3")" "Chapter 3/6 — three"
assert_eq       "$(cat "${tmp}/s4")" "" "social profile must suppress chapter 4"
assert_eq       "$(cat "${tmp}/s5")" "" "social profile must suppress chapter 5"

# hero: every chapter is silent.
DEMO_RECORD_PROFILE=hero
__DEMO_CHAPTER_INDEX=0
chapter "one" "▸" > "${tmp}/h1"
chapter "two" "▸" > "${tmp}/h2"
assert_eq "$(cat "${tmp}/h1")" "" "hero profile must suppress chapter 1"
assert_eq "$(cat "${tmp}/h2")" "" "hero profile must suppress chapter 2"

printf 'PASS\n'
