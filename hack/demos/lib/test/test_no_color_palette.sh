#!/usr/bin/env bash
# Color codes degrade to empty strings under NO_COLOR (the test helper sets it).
# Also verify that with NO_COLOR there are no ANSI escape sequences in
# banner/chapter/log output.

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

# After sourcing under NO_COLOR=1, every palette variable must be empty.
for var in COLOR_RESET GREEN CYAN BOLD RED YELLOW BLUE MAGENTA DIM; do
  if [[ -n "${!var}" ]]; then
    printf 'FAIL %s expected empty under NO_COLOR, got %q\n' "${var}" "${!var}" >&2
    exit 1
  fi
done

# Banner and chapter output must not contain ESC sequences.
DEMO_CHAPTER_TOTAL=2
out_banner="$(banner "Smoke")"
case "${out_banner}" in
  *$'\033'*) printf 'FAIL banner contains ESC under NO_COLOR\n' >&2; exit 1 ;;
esac

out_chapter="$(chapter "one" "▸")"
case "${out_chapter}" in
  *$'\033'*) printf 'FAIL chapter contains ESC under NO_COLOR\n' >&2; exit 1 ;;
esac

out_log="$(log_info "hello")"
case "${out_log}" in
  *$'\033'*) printf 'FAIL log_info contains ESC under NO_COLOR\n' >&2; exit 1 ;;
esac

printf 'PASS\n'
