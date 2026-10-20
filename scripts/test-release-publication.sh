#!/usr/bin/env bash
# shellcheck disable=SC2016 # Report filters are jq programs.
set -Eeuo pipefail
umask 077

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
# shellcheck source=scripts/lib/release-qualification-report.sh
. "${script_dir}/lib/release-qualification-report.sh"
# shellcheck source=scripts/lib/redact.sh
. "${script_dir}/lib/redact.sh"

if ! acp_report_enabled; then
  echo 'RELEASE_GATE=1 and an initialized ACP_E2E_REPORT_FILE are required.' >&2
  exit 2
fi
checkout_sha="$(git -C "${repo_root}" rev-parse HEAD)"
jq -e --arg sha "${checkout_sha}" '
  .schemaVersion == 2 and .candidateSHA == $sha and .checkoutSHA == $sha
' "${ACP_E2E_REPORT_FILE}" >/dev/null
acp_report_update '.result = "not_qualified" | .stage = "publication_tests" | .publicationTests = {status:"running"}'

publication_test_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/release-publication-tests.XXXXXX")"
trap 'rm -rf "${publication_test_dir}"' EXIT
test_status=0
# These packages use temporary Git repositories and local HTTP test servers.
# Do not pass workflow, provider, or optional canary credentials to them.
(
  unset GH_TOKEN GITHUB_TOKEN COPILOT_GITHUB_TOKEN
  unset ACP_E2E_WRITE_READ_CREDENTIAL_TOKEN ACP_E2E_WRITE_TARGET_READ_CREDENTIAL_TOKEN
  unset ACP_E2E_WRITE_CREDENTIAL_TOKEN ACP_E2E_WRITE_FORGE_CREDENTIAL_TOKEN
  go -C "${repo_root}" test -json -count=1 -timeout=10m ./internal/publisher ./internal/publisher/service
) >"${publication_test_dir}/events.jsonl" 2>"${publication_test_dir}/stderr" || test_status=$?

if ! evidence="$(jq -sc --arg sha "${checkout_sha}" --argjson status "${test_status}" \
    --argjson required "$(acp_publication_test_requirements)" '
    . as $events
    | {candidateSHA:$sha, exitCode:$status, status:"passed",
        failedEvents:([$events[] | select(.Action == "fail")] | length),
        suites: (reduce ($required | keys[]) as $package ({};
          .[$package] = {
            passed: ([$events[] | select(.Package == $package and .Test == null and .Action == "pass")] | length == 1),
            testCount: ([$events[] | select(.Package == $package and .Action == "pass"
              and .Test != null and (.Test | contains("/") | not))] | length),
            passedTests: [$required[$package][] | . as $test
              | select(([$events[] | select(.Package == $package and .Test == $test and .Action == "run")] | length) == 1)
              | select(([$events[] | select(.Package == $package and .Test == $test and .Action == "pass")] | length) == 1)
              | select(([$events[] | select(.Package == $package and .Test == $test
                and (.Action == "skip" or .Action == "fail"))] | length) == 0)]
          }))}
  ' "${publication_test_dir}/events.jsonl")"; then
  acp_report_update '.publicationTests = {status:"failed", reason:"invalid_test_output", exitCode:$status}' \
    --argjson status "${test_status}"
  echo 'Publication test output was incomplete or invalid.' >&2
  exit 1
fi
acp_report_update '.publicationTests = $evidence' --argjson evidence "${evidence}"
if ! acp_report_publication_tests_qualified "${ACP_E2E_REPORT_FILE}"; then
  acp_report_update '.publicationTests.status = "failed"'
  # Keep diagnostics to test identities and redacted tool errors, never raw
  # fixture responses or operation credential contents.
  jq -r 'select(.Action == "fail") | "FAIL " + .Package + " " + (.Test // "[package]")' \
    "${publication_test_dir}/events.jsonl" >&2
  redact <"${publication_test_dir}/stderr" >&2
  echo 'Publication tests failed, skipped required cases, or did not finish both packages.' >&2
  exit 1
fi
echo 'Git publication and GitHub API fixture tests passed. Live GitHub publication was not exercised by these tests.'
