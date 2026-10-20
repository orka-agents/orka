#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
profile="${1:-cover.out}"
details="${2:-coverage-functions.txt}"
: "${TEST_OUTCOME:?TEST_OUTCOME must be set}"
: "${GITHUB_STEP_SUMMARY:?GITHUB_STEP_SUMMARY must be set}"

commit="$(git -C "${root}" rev-parse HEAD)"
short_commit="$(git -C "${root}" rev-parse --short=8 HEAD)"
coverage_status='❌ Invalid: `go tool cover` could not process the profile.'
total=''
rm -f "${details}"

case "${TEST_OUTCOME}" in
  success) test_status='✅ Passed' ;;
  failure) test_status='❌ Failed' ;;
  cancelled) test_status='⚠️ Cancelled' ;;
  skipped) test_status='⏭️ Skipped' ;;
  error) test_status='❌ Error' ;;
  *) test_status='❓ Unknown' ;;
esac

if [[ ! -f "${profile}" ]]; then
  coverage_status='⚠️ Missing: no coverage profile was produced.'
elif [[ ! -s "${profile}" ]]; then
  coverage_status='⚠️ Empty: the coverage profile contains no data.'
elif report="$(go tool cover -func="${profile}" 2>/dev/null)"; then
  total="$(awk '$1 == "total:" && $2 == "(statements)" { print $3 }' <<< "${report}")"
  if [[ "${report}" == total:* ]]; then
    coverage_status='⚠️ Empty: the coverage profile contains no function data.'
    total=''
  elif [[ "${total}" =~ ^[0-9]+([.][0-9]+)?%$ ]]; then
    coverage_status='✅ Valid'
    printf '%s\n' "${report}" > "${details}"
  else
    total=''
  fi
fi

commit_display="\`${short_commit}\`"
artifact_url=''
if [[ -n "${GITHUB_SERVER_URL:-}" && -n "${GITHUB_REPOSITORY:-}" && -n "${GITHUB_RUN_ID:-}" ]]; then
  commit_display="[\`${short_commit}\`](${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/commit/${commit})"
  artifact_url="${GITHUB_SERVER_URL}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID}#artifacts"
fi

render_summary() {
  printf '## Results for Go Tests\n\n'
  printf 'This report shows whether the Go tests passed and how much Go code they covered.\n\n'
  printf -- '- **Test result:** %s\n' "${test_status}"
  printf -- '- **Coverage profile:** %s\n' "${coverage_status}"
  if [[ -n "${total}" ]]; then
    printf -- '- **Statement coverage:** %s\n' "${total}"
  fi
  printf -- '- **Commit:** %s\n' "${commit_display}"
  printf -- '- **Scope:** `make test`, excluding E2E packages\n'
  if [[ "${TEST_OUTCOME}" != success ]]; then
    printf "\nThis run didn't pass, so any coverage shown may be incomplete. Use it to debug the issue, **not as a coverage baseline**.\n"
  fi
  printf '\n<details>\n<summary>How is coverage calculated?</summary>\n\n'
  printf '`make test` runs the non-E2E Go packages with `go test -coverprofile cover.out`. '
  printf 'The percentage is the number of statements marked as executed divided by the total statements in that profile.\n\n'
  if [[ -n "${total}" && -n "${artifact_url}" ]]; then
    printf '[Download the coverage evidence](%s) for the raw profile and function-by-function breakdown.\n\n' "${artifact_url}"
  elif [[ -n "${total}" ]]; then
    printf 'The coverage artifact contains the raw profile and function-by-function breakdown.\n\n'
  fi
  printf '</details>\n'
}

render_summary >> "${GITHUB_STEP_SUMMARY}"