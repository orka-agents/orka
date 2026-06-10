#!/usr/bin/env bash
# Run all hack/demos/lib smoke tests. Pure bash, no test framework.
#
# Usage: bash hack/demos/lib/test/run-all.sh

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

pass=0
fail=0
failures=()

for t in "${script_dir}"/test_*.sh; do
  name="$(basename "${t}")"
  if bash "${t}" >/dev/null 2>&1; then
    printf '  ok  %s\n' "${name}"
    pass=$(( pass + 1 ))
  else
    printf '  FAIL %s\n' "${name}"
    fail=$(( fail + 1 ))
    failures+=("${name}")
  fi
done

printf '\n%d passed, %d failed\n' "${pass}" "${fail}"
if (( fail > 0 )); then
  printf '\nfailed tests (re-run for detail):\n'
  for f in "${failures[@]}"; do
    printf '  bash %s/%s\n' "${script_dir}" "${f}"
  done
  exit 1
fi
