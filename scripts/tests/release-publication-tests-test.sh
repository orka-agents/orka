#!/usr/bin/env bash
# shellcheck disable=SC2016 # Fixture expressions are jq programs.
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/release-qualification-report.sh
. "${root}/scripts/lib/release-qualification-report.sh"
fixture="$(mktemp -d "${TMPDIR:-/tmp}/release-publication-tests-test.XXXXXX")"
trap 'rm -rf "${fixture}"' EXIT
export RELEASE_GATE=1 ACP_E2E_WRITE_CREATE_PR=0 ACP_E2E_REPORT_FILE="${fixture}/acceptance.json"
export ACP_E2E_REPO=https://github.com/orka-agents/orka.git
ACP_E2E_REF="$(git -C "${root}" rev-parse HEAD)"
export ACP_E2E_REF
export PUBLICATION_TEST_EVENTS="${fixture}/events.jsonl" PUBLICATION_TEST_EXIT=0
mkdir "${fixture}/bin"
cat >"${fixture}/bin/go" <<'STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
[[ "$1" == -C && "$3" == test && "$4" == -json && "$5" == -count=1 && "$6" == -timeout=10m ]]
[[ "$7" == ./internal/publisher && "$8" == ./internal/publisher/service && $# == 8 ]]
for name in GH_TOKEN GITHUB_TOKEN COPILOT_GITHUB_TOKEN ACP_E2E_WRITE_READ_CREDENTIAL_TOKEN \
  ACP_E2E_WRITE_TARGET_READ_CREDENTIAL_TOKEN ACP_E2E_WRITE_CREDENTIAL_TOKEN ACP_E2E_WRITE_FORGE_CREDENTIAL_TOKEN; do
  [[ -z "${!name:-}" ]] || exit 97
done
cat "${PUBLICATION_TEST_EVENTS}"
exit "${PUBLICATION_TEST_EXIT}"
STUB
chmod +x "${fixture}/bin/go"
jq -nc --argjson required "$(acp_publication_test_requirements)" '
  $required | to_entries[] as $suite
  | {Action:"start",Package:$suite.key},
    ($suite.value[] as $test | {Action:"run",Package:$suite.key,Test:$test},
      {Action:"pass",Package:$suite.key,Test:$test}),
    {Action:"output",Package:$suite.key,Output:"excluded-fixture-response"},
    {Action:"pass",Package:$suite.key}
' >"${fixture}/complete.jsonl"

run_suite() {
  PATH="${fixture}/bin:${PATH}" \
    GH_TOKEN=excluded-workflow-value GITHUB_TOKEN=excluded-workflow-value \
    COPILOT_GITHUB_TOKEN=excluded-provider-value \
    ACP_E2E_WRITE_READ_CREDENTIAL_TOKEN=excluded-source-value \
    ACP_E2E_WRITE_TARGET_READ_CREDENTIAL_TOKEN=excluded-target-value \
    ACP_E2E_WRITE_CREDENTIAL_TOKEN=excluded-write-value \
    ACP_E2E_WRITE_FORGE_CREDENTIAL_TOKEN=excluded-forge-value \
    bash "${root}/scripts/test-release-publication.sh" >"${fixture}/output" 2>&1
}

cp "${fixture}/complete.jsonl" "${PUBLICATION_TEST_EVENTS}"
acp_report_init "${root}"
run_suite
acp_report_publication_tests_qualified "${ACP_E2E_REPORT_FILE}"
jq -e '.result == "not_qualified" and .coverage.liveGitHub == "not_tested"' "${ACP_E2E_REPORT_FILE}" >/dev/null
if grep -F 'excluded-' "${ACP_E2E_REPORT_FILE}" "${fixture}/output" >/dev/null; then
  echo 'publication test evidence or output retained excluded content' >&2
  exit 1
fi
printf '%s\n' 'ok - publication tests run uncached without credentials and retain only candidate-bound test evidence'

while IFS= read -r mutation; do
  acp_report_init "${root}"
  jq -c "${mutation}" "${fixture}/complete.jsonl" >"${PUBLICATION_TEST_EVENTS}"
  if run_suite; then
    printf 'incomplete publication tests passed: %s\n' "${mutation}" >&2
    exit 1
  fi
  jq -e '.publicationTests.status == "failed" and .result == "not_qualified"' "${ACP_E2E_REPORT_FILE}" >/dev/null
done <<'MUTATIONS'
select(.Package != "github.com/orka-agents/orka/internal/publisher/service")
select(.Test != "TestPublishExactCASForkVerifyAndNoForcePush")
if .Test == "TestGitHubPRReconcilerCreatesExactPullRequest" and .Action == "pass" then .Action = "skip" else . end
if .Test == "TestPublishExactCASForkVerifyAndNoForcePush" and .Action == "pass" then ., . else . end
select(.Test != null or .Action != "pass")
if .Action == "output" then .Action = "fail" else . end
empty
MUTATIONS

acp_report_init "${root}"
cp "${fixture}/complete.jsonl" "${PUBLICATION_TEST_EVENTS}"
if PUBLICATION_TEST_EXIT=23 run_suite; then
  echo 'publication tests ignored the Go process exit status' >&2
  exit 1
fi
jq -e '.publicationTests.exitCode == 23 and .publicationTests.status == "failed"' "${ACP_E2E_REPORT_FILE}" >/dev/null

acp_report_init "${root}"
printf 'truncated output\n' >"${PUBLICATION_TEST_EVENTS}"
if run_suite; then
  echo 'publication tests accepted malformed output' >&2
  exit 1
fi
jq -e '.publicationTests.status == "failed" and .publicationTests.reason == "invalid_test_output"' \
  "${ACP_E2E_REPORT_FILE}" >/dev/null

acp_report_init "${root}"
acp_report_update '.candidateSHA = "0000000000000000000000000000000000000000"'
if run_suite; then
  echo 'publication tests accepted another candidate checkout' >&2
  exit 1
fi
printf '%s\n' 'ok - missing, skipped, duplicate, failed, interrupted and stale-candidate test runs fail closed'
