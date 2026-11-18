#!/usr/bin/env bash
# Exercises scripts/verify-generated.sh against a scratch repository whose
# "generators" are a stub make.
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fixture_dir="$(mktemp -d)"
trap 'rm -rf "${fixture_dir}"' EXIT

assert_contains() {
  if ! grep -Fq -- "$2" "$1"; then
    printf 'FAIL: expected %s in %s\n' "$2" "$1" >&2
    cat "$1" >&2
    exit 1
  fi
}

repo="${fixture_dir}/repo"
mkdir -p "${repo}/api/v1alpha1" "${repo}/config/crd/bases"
git_cmd() {
  git -C "${repo}" -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false "$@"
}
git_cmd init -q
echo 'package v1alpha1' > "${repo}/api/v1alpha1/zz_generated.deepcopy.go"
echo 'kind: CustomResourceDefinition' > "${repo}/config/crd/bases/core.orka.ai_tasks.yaml"
git_cmd add -A
git_cmd commit -q -m fixture

# The stub make runs whatever STUB_GENERATOR says and records its arguments.
cat > "${fixture_dir}/make" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "$*" > "${STUB_ARGS}"
eval "${STUB_GENERATOR:-true}"
STUB
chmod +x "${fixture_dir}/make"
export STUB_ARGS="${fixture_dir}/args"

run_verify() {
  (cd "${repo}" && MAKE="${fixture_dir}/make" bash "${root}/scripts/verify-generated.sh") >"${fixture_dir}/out" 2>&1
}

# Up-to-date generated files pass.
STUB_GENERATOR=true run_verify
assert_contains "${fixture_dir}/out" "Generated files are up to date."
assert_contains "${STUB_ARGS}" "generate manifests"

# A generator that changes a file fails and names the file and the fix.
status=0
STUB_GENERATOR='echo "// regenerated" >> api/v1alpha1/zz_generated.deepcopy.go; echo new > config/crd/bases/core.orka.ai_new.yaml' run_verify || status=$?
[[ ${status} -eq 1 ]] || { echo "FAIL: stale files exited ${status}, want 1" >&2; exit 1; }
assert_contains "${fixture_dir}/out" "Generated files are stale."
assert_contains "${fixture_dir}/out" "api/v1alpha1/zz_generated.deepcopy.go"
assert_contains "${fixture_dir}/out" "(fix: make generate)"
assert_contains "${fixture_dir}/out" "config/crd/bases/core.orka.ai_new.yaml"
assert_contains "${fixture_dir}/out" "(fix: make manifests)"
assert_contains "${fixture_dir}/out" "Never edit generated files by hand."

# A dirty tree is refused before any generator runs.
rm -f "${STUB_ARGS}"
status=0
STUB_GENERATOR=true run_verify || status=$?
[[ ${status} -eq 2 ]] || { echo "FAIL: dirty tree exited ${status}, want 2" >&2; exit 1; }
assert_contains "${fixture_dir}/out" "needs a clean working tree"
[[ ! -e "${STUB_ARGS}" ]] || { echo "FAIL: generators ran on a dirty tree" >&2; exit 1; }

echo "verify-generated tests passed"
