#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v go >/dev/null
fixture_dir="$(mktemp -d)"
trap 'rm -rf "${fixture_dir}"' EXIT
commit="$(git -C "${root}" rev-parse HEAD)"

assert_contains() {
  if ! grep -Fq -- "$2" "$1"; then
    printf 'FAIL: expected %s in %s\n' "$2" "$1" >&2
    exit 1
  fi
}

assert_not_contains() {
  if grep -Fq -- "$2" "$1"; then
    printf 'FAIL: unexpected %s in %s\n' "$2" "$1" >&2
    exit 1
  fi
}

assert_file_exists() {
  if [[ ! -f "$1" ]]; then
    printf 'FAIL: expected file %s\n' "$1" >&2
    exit 1
  fi
}

assert_file_missing() {
  if [[ -e "$1" ]]; then
    printf 'FAIL: unexpected file %s\n' "$1" >&2
    exit 1
  fi
}

cat > "${fixture_dir}/go.mod" <<'EOF'
module example.com/coveragefixture

go 1.20
EOF

cat > "${fixture_dir}/fixture.go" <<'EOF'
package coveragefixture

func Covered() int {
    return 1
}

func Uncovered() int {
    value := 1
    value++
    return value
}
EOF

cat > "${fixture_dir}/valid.out" <<'EOF'
mode: set
example.com/coveragefixture/fixture.go:3.20,5.2 1 1
example.com/coveragefixture/fixture.go:7.22,11.2 3 0
EOF

touch "${fixture_dir}/empty.out"
printf 'mode: set\n' > "${fixture_dir}/header-only.out"
printf 'not a Go coverage profile\n' > "${fixture_dir}/invalid.out"

for outcome in success failure skipped cancelled error; do
  for profile_case in valid missing empty header-only invalid; do
    summary="${fixture_dir}/${profile_case}-${outcome}.md"
    summary_report="${fixture_dir}/${profile_case}-${outcome}-report.md"
    details="${fixture_dir}/${profile_case}-${outcome}.txt"
    (
      cd "${fixture_dir}"
      TEST_OUTCOME="${outcome}" GITHUB_STEP_SUMMARY="${summary}" \
        GITHUB_SERVER_URL=https://github.example GITHUB_REPOSITORY=example/orka GITHUB_RUN_ID=1234 \
        bash "${root}/scripts/go-coverage-summary.sh" "${fixture_dir}/${profile_case}.out" "${details}" "${summary_report}"
    )

    assert_file_exists "${summary_report}"
    if ! cmp -s "${summary}" "${summary_report}"; then
      printf 'FAIL: job summary and report differ for %s / %s\n' "${profile_case}" "${outcome}" >&2
      exit 1
    fi
    expected_intro=$'## Results for Go Tests\n\nThis report shows whether the Go tests passed and how much Go code they covered.'
    if [[ "$(head -n 3 "${summary}")" != "${expected_intro}" ]]; then
      printf 'FAIL: expected title and introduction before results for %s / %s\n' "${profile_case}" "${outcome}" >&2
      exit 1
    fi
    assert_not_contains "${summary}" 'Go Test Coverage'
    assert_not_contains "${summary}" 'Go test outcome:'
    assert_not_contains "${summary}" 'Report: Available'
    assert_not_contains "${summary}" 'incomplete diagnostic output'
    assert_contains "${summary}" "https://github.example/example/orka/commit/${commit}"
    assert_contains "${summary}" '**Scope:** `make test`, excluding E2E packages'
    assert_contains "${summary}" '<summary>How is coverage calculated?</summary>'

    case "${outcome}" in
      success) assert_contains "${summary}" '**Test result:** ✅ Passed' ;;
      failure) assert_contains "${summary}" '**Test result:** ❌ Failed' ;;
      skipped) assert_contains "${summary}" '**Test result:** ⏭️ Skipped' ;;
      cancelled) assert_contains "${summary}" '**Test result:** ⚠️ Cancelled' ;;
      error) assert_contains "${summary}" '**Test result:** ❌ Error' ;;
    esac

    case "${profile_case}" in
      valid)
        assert_contains "${summary}" '**Coverage profile:** ✅ Valid'
        assert_contains "${summary}" '**Statement coverage:** 25.0%'
        assert_contains "${summary}" '[Download the coverage evidence](https://github.example/example/orka/actions/runs/1234#artifacts)'
        assert_file_exists "${details}"
        assert_contains "${details}" 'Covered'
        assert_contains "${details}" 'Uncovered'
        assert_contains "${details}" 'total:'
        ;;
      missing) assert_contains "${summary}" '**Coverage profile:** ⚠️ Missing:' ;;
      empty|header-only) assert_contains "${summary}" '**Coverage profile:** ⚠️ Empty:' ;;
      invalid) assert_contains "${summary}" '**Coverage profile:** ❌ Invalid:' ;;
    esac

    if [[ "${profile_case}" != valid ]]; then
      assert_not_contains "${summary}" '%'
      assert_not_contains "${summary}" 'Statement coverage:'
      assert_not_contains "${summary}" 'Download the coverage evidence'
      assert_file_missing "${details}"
    fi
    if [[ "${outcome}" == success ]]; then
      assert_not_contains "${summary}" "This run didn't pass"
      assert_not_contains "${summary}" 'not as a coverage baseline'
    else
      assert_contains "${summary}" "This run didn't pass, so any coverage shown may be incomplete."
      assert_contains "${summary}" 'Use it to debug the issue, **not as a coverage baseline**.'
    fi
    printf 'ok - %s profile / %s outcome\n' "${profile_case}" "${outcome}"
  done
done