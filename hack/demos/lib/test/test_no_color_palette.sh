#!/usr/bin/env bash

set -Eeuo pipefail

. "$(dirname "${BASH_SOURCE[0]}")/_test_helpers.sh"

for var in COLOR_RESET GREEN CYAN BOLD RED YELLOW BLUE MAGENTA DIM; do
  if [[ -n "${!var}" ]]; then
    printf 'FAIL %s expected empty under NO_COLOR, got %q\n' "${var}" "${!var}" >&2
    exit 1
  fi
done

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
