#!/usr/bin/env bash
# narrate is silent in presenter/social/hero, printed as "> ..." in docs.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

DEMO_CHAPTER_TOTAL=4

# docs: narration cue prints under the chapter title.
DEMO_RECORD_PROFILE=docs
__DEMO_CHAPTER_INDEX=0
narrate "one-line cue"
out_docs="$(chapter "with narration" "▸")"
assert_contains "${out_docs}" "> one-line cue"

# presenter: narration cue does NOT print (silent).
DEMO_RECORD_PROFILE=presenter
__DEMO_CHAPTER_INDEX=0
narrate "should be silent"
out_presenter="$(chapter "no narration shown" "▸")"
assert_not_contains "${out_presenter}" "should be silent"
assert_not_contains "${out_presenter}" "> "

# Pending cue must be cleared after the chapter consumes it.
DEMO_RECORD_PROFILE=docs
__DEMO_CHAPTER_INDEX=0
narrate "first cue"
chapter "first"  "▸" >/dev/null
out_second="$(chapter "second" "▸")"
assert_not_contains "${out_second}" "first cue"

printf 'PASS\n'
