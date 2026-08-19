#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/scripts/sync-helm-crds.sh"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/sync-helm-crds-test.XXXXXX")"

cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

source_dir="${test_root}/config/crd/bases"
destination_dir="${test_root}/charts/orka/crds"
mkdir -p "${source_dir}" "${destination_dir}"

printf '%s\n' 'kind: First' >"${source_dir}/core.orka.ai_firsts.yaml"
printf '%s\n' 'kind: Second' >"${source_dir}/core.orka.ai_seconds.yaml"
cat >"${source_dir}/fake.workspace.orka.ai_fakeproviders.yaml" <<'EOF_FAKE_CRD'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: fakeproviders.fake.workspace.orka.ai
spec:
  group: fake.workspace.orka.ai
  names:
    kind: FakeProvider
EOF_FAKE_CRD
printf '%s\n' 'outdated' >"${destination_dir}/core.orka.ai_firsts.yaml"
printf '%s\n' 'stale' >"${destination_dir}/core.orka.ai_stale.yaml"
printf '%s\n' 'development CRD must be removed' >"${destination_dir}/fakeprovider-customresourcedefinition.yaml"
mkdir -p "${destination_dir}/legacy/nested"
printf '%s\n' 'stale yml' >"${destination_dir}/legacy/stale.yml"
printf '%s\n' '{"kind":"Stale"}' >"${destination_dir}/legacy/nested/stale.json"
printf '%s\n' 'preserve this documentation' >"${destination_dir}/README.md"
printf '%s\n' 'preserve this metadata' >"${destination_dir}/NOTES.txt"
printf '%s\n' 'preserve nested docs' >"${destination_dir}/legacy/README.md"

"${script}" --source "${source_dir}" --destination "${destination_dir}"

cmp -s "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"
cmp -s "${source_dir}/core.orka.ai_seconds.yaml" "${destination_dir}/core.orka.ai_seconds.yaml"
[[ ! -e "${destination_dir}/core.orka.ai_stale.yaml" ]]
[[ ! -e "${destination_dir}/fakeprovider-customresourcedefinition.yaml" ]]
[[ ! -e "${destination_dir}/legacy/stale.yml" ]]
[[ ! -e "${destination_dir}/legacy/nested/stale.json" ]]
grep -Fx 'preserve this documentation' "${destination_dir}/README.md" >/dev/null
grep -Fx 'preserve this metadata' "${destination_dir}/NOTES.txt" >/dev/null
grep -Fx 'preserve nested docs' "${destination_dir}/legacy/README.md" >/dev/null
"${script}" --check --source "${source_dir}" --destination "${destination_dir}" >/dev/null

sha256_file() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$1" | awk '{print tolower($1)}'
  fi
}

snapshot_destination() {
  local entry name

  while IFS= read -r entry; do
    name="${entry:${#destination_dir}+1}"
    if [[ -L "${entry}" ]]; then
      printf 'symlink\t%s\t%s\n' "${name}" "$(readlink "${entry}")"
    elif [[ -f "${entry}" ]]; then
      printf 'file\t%s\t%s\n' "${name}" "$(sha256_file "${entry}")"
    elif [[ -d "${entry}" ]]; then
      printf 'directory\t%s\n' "${name}"
    else
      printf 'other\t%s\n' "${name}"
    fi
  done < <(find "${destination_dir}" -mindepth 1 -print | LC_ALL=C sort)
}

expect_check_failure() {
  local expected_message="$1"
  local before after output

  before="$(snapshot_destination)"
  if output="$("${script}" --check --source "${source_dir}" --destination "${destination_dir}" 2>&1)"; then
    echo "expected synchronization check to fail: ${expected_message}" >&2
    exit 1
  fi
  after="$(snapshot_destination)"
  [[ "${before}" == "${after}" ]] || {
    echo "synchronization check modified the destination" >&2
    diff -u <(printf '%s\n' "${before}") <(printf '%s\n' "${after}") >&2 || true
    exit 1
  }
  grep -F "${expected_message}" <<<"${output}" >/dev/null || {
    echo "expected failure message not found: ${expected_message}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  }
}

expect_script_failure() {
  local expected_message="$1"
  local output
  shift

  if output="$("${script}" "$@" 2>&1)"; then
    echo "expected synchronization command to fail: ${expected_message}" >&2
    exit 1
  fi
  grep -F "${expected_message}" <<<"${output}" >/dev/null || {
    echo "expected failure message not found: ${expected_message}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  }
}

printf '%s\n' 'drifted' >"${destination_dir}/core.orka.ai_firsts.yaml"
expect_check_failure 'out-of-sync Helm CRD: core.orka.ai_firsts.yaml'
cp "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"

rm "${destination_dir}/core.orka.ai_seconds.yaml"
expect_check_failure 'missing Helm CRD: core.orka.ai_seconds.yaml'
cp "${source_dir}/core.orka.ai_seconds.yaml" "${destination_dir}/core.orka.ai_seconds.yaml"

printf '%s\n' 'stale' >"${destination_dir}/core.orka.ai_stale.yaml"
expect_check_failure 'stale Helm CRD: core.orka.ai_stale.yaml'
rm "${destination_dir}/core.orka.ai_stale.yaml"

printf '%s\n' 'stale nested yaml' >"${destination_dir}/legacy/stale.yml"
expect_check_failure 'stale Helm CRD: legacy/stale.yml'
rm "${destination_dir}/legacy/stale.yml"

printf '%s\n' '{"kind":"Stale"}' >"${destination_dir}/legacy/nested/stale.json"
expect_check_failure 'stale Helm CRD: legacy/nested/stale.json'
rm "${destination_dir}/legacy/nested/stale.json"

rm "${destination_dir}/core.orka.ai_firsts.yaml"
ln -s "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"
expect_check_failure 'symlinked Helm CRD: core.orka.ai_firsts.yaml'

rm "${destination_dir}/core.orka.ai_firsts.yaml"
ln -s README.md "${destination_dir}/core.orka.ai_firsts.yaml"
readme_before="$(sha256_file "${destination_dir}/README.md")"
"${script}" --source "${source_dir}" --destination "${destination_dir}" >/dev/null
[[ ! -L "${destination_dir}/core.orka.ai_firsts.yaml" ]]
cmp -s "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"
[[ "${readme_before}" == "$(sha256_file "${destination_dir}/README.md")" ]]

outside_dir="${test_root}/outside"
mkdir -p "${outside_dir}"
printf '%s\n' 'outside sentinel' >"${outside_dir}/core.orka.ai_firsts.yaml"
rm "${destination_dir}/core.orka.ai_firsts.yaml"
ln -s "${outside_dir}" "${destination_dir}/core.orka.ai_firsts.yaml"
"${script}" --source "${source_dir}" --destination "${destination_dir}" >/dev/null
[[ ! -L "${destination_dir}/core.orka.ai_firsts.yaml" ]]
cmp -s "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"
grep -Fx 'outside sentinel' "${outside_dir}/core.orka.ai_firsts.yaml" >/dev/null

printf '%s\n' 'leave this drift in place' >"${destination_dir}/core.orka.ai_firsts.yaml"
rm "${destination_dir}/core.orka.ai_seconds.yaml"
mkdir "${destination_dir}/core.orka.ai_seconds.yaml"
before="$(snapshot_destination)"
expect_script_failure \
  'Helm CRD destination is not a regular file or symlink:' \
  --source "${source_dir}" --destination "${destination_dir}"
after="$(snapshot_destination)"
[[ "${before}" == "${after}" ]] || {
  echo 'failed synchronization partially modified the destination' >&2
  diff -u <(printf '%s\n' "${before}") <(printf '%s\n' "${after}") >&2 || true
  exit 1
}
rmdir "${destination_dir}/core.orka.ai_seconds.yaml"
cp "${source_dir}/core.orka.ai_firsts.yaml" "${destination_dir}/core.orka.ai_firsts.yaml"
cp "${source_dir}/core.orka.ai_seconds.yaml" "${destination_dir}/core.orka.ai_seconds.yaml"

source_first_before="$(sha256_file "${source_dir}/core.orka.ai_firsts.yaml")"
source_second_before="$(sha256_file "${source_dir}/core.orka.ai_seconds.yaml")"
for operation in check sync; do
  args=(--source "${source_dir}" --destination "${source_dir}")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  expect_script_failure 'generated and Helm CRD directories must differ:' "${args[@]}"
done
[[ "${source_first_before}" == "$(sha256_file "${source_dir}/core.orka.ai_firsts.yaml")" ]]
[[ "${source_second_before}" == "$(sha256_file "${source_dir}/core.orka.ai_seconds.yaml")" ]]

find_failure_bin="${test_root}/find-failure-bin"
mkdir -p "${find_failure_bin}"
cat >"${find_failure_bin}/find" <<'EOF'
#!/usr/bin/env bash
exit 23
EOF
chmod +x "${find_failure_bin}/find"
for operation in check sync; do
  args=(--source "${source_dir}" --destination "${destination_dir}")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  before="$(snapshot_destination)"
  if output="$(PATH="${find_failure_bin}:${PATH}" "${script}" "${args[@]}" 2>&1)"; then
    echo "expected symlink scan failure during ${operation}" >&2
    exit 1
  fi
  grep -F 'cannot scan Helm CRD destination for symlinks:' <<<"${output}" >/dev/null
  after="$(snapshot_destination)"
  [[ "${before}" == "${after}" ]]
done

find_second_failure_bin="${test_root}/find-second-failure-bin"
find_counter="${test_root}/find-counter"
mkdir -p "${find_second_failure_bin}"
cat >"${find_second_failure_bin}/find" <<EOF
#!/usr/bin/env bash
count=0
if [[ -f "${find_counter}" ]]; then
  count="\$(cat "${find_counter}")"
fi
count=\$((count + 1))
printf '%s\n' "\${count}" >"${find_counter}"
if [[ "\${count}" -eq 1 ]]; then
  exit 0
fi
exit 23
EOF
chmod +x "${find_second_failure_bin}/find"
for operation in check sync; do
  printf '%s\n' 0 >"${find_counter}"
  args=(--source "${source_dir}" --destination "${destination_dir}")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  before="$(snapshot_destination)"
  if output="$(PATH="${find_second_failure_bin}:${PATH}" "${script}" "${args[@]}" 2>&1)"; then
    echo "expected manifest scan failure during ${operation}" >&2
    exit 1
  fi
  grep -F 'cannot scan Helm CRD destination for manifests:' <<<"${output}" >/dev/null
  after="$(snapshot_destination)"
  [[ "${before}" == "${after}" ]]
done

source_inside_destination="${test_root}/overlap-destination/source"
mkdir -p "${source_inside_destination}"
printf '%s\n' 'kind: SourceInsideDestination' >"${source_inside_destination}/source.yaml"
source_inside_hash="$(sha256_file "${source_inside_destination}/source.yaml")"
for operation in check sync; do
  args=(--source "${source_inside_destination}" --destination "${test_root}/overlap-destination")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  expect_script_failure 'generated and Helm CRD directories must not overlap:' "${args[@]}"
done
[[ "${source_inside_hash}" == "$(sha256_file "${source_inside_destination}/source.yaml")" ]]

source_parent="${test_root}/overlap-source"
destination_inside_source="${source_parent}/destination"
mkdir -p "${destination_inside_source}"
printf '%s\n' 'kind: DestinationInsideSource' >"${source_parent}/source.yaml"
printf '%s\n' 'destination sentinel' >"${destination_inside_source}/README.md"
source_parent_hash="$(sha256_file "${source_parent}/source.yaml")"
for operation in check sync; do
  args=(--source "${source_parent}" --destination "${destination_inside_source}")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  expect_script_failure 'generated and Helm CRD directories must not overlap:' "${args[@]}"
done
[[ "${source_parent_hash}" == "$(sha256_file "${source_parent}/source.yaml")" ]]
grep -Fx 'destination sentinel' "${destination_inside_source}/README.md" >/dev/null

for candidate in / /.; do
  for operation in check sync; do
    args=(--source "${source_dir}" --destination "${candidate}")
    if [[ "${operation}" == "check" ]]; then
      args=(--check "${args[@]}")
    fi
    expect_script_failure 'refusing to manage filesystem root as Helm CRD directory:' "${args[@]}"
  done
done

nested_symlink_target="${test_root}/nested-symlink-target"
mkdir -p "${nested_symlink_target}"
printf '%s\n' 'kind: Escaped' >"${nested_symlink_target}/escaped.yaml"
ln -s "${nested_symlink_target}" "${destination_dir}/linked-manifests"
for operation in check sync; do
  args=(--source "${source_dir}" --destination "${destination_dir}")
  if [[ "${operation}" == "check" ]]; then
    args=(--check "${args[@]}")
  fi
  expect_script_failure 'Helm CRD directory must not contain unmanaged symlink:' "${args[@]}"
done
rm "${destination_dir}/linked-manifests"
grep -Fx 'kind: Escaped' "${nested_symlink_target}/escaped.yaml" >/dev/null

linked_destination="${test_root}/linked-crds"
ln -s "${destination_dir}" "${linked_destination}"
for candidate in "${linked_destination}" "${linked_destination}/" "${linked_destination}/."; do
  for operation in check sync; do
    if [[ "${operation}" == "check" ]]; then
      if output="$("${script}" --check --source "${source_dir}" --destination "${candidate}" 2>&1)"; then
        echo "expected symlinked destination check to fail: ${candidate}" >&2
        exit 1
      fi
    elif output="$("${script}" --source "${source_dir}" --destination "${candidate}" 2>&1)"; then
      echo "expected symlinked destination sync to fail: ${candidate}" >&2
      exit 1
    fi
    grep -F 'Helm CRD directory must not be a symlink:' <<<"${output}" >/dev/null
  done
done

printf '%s\n' 'ok - Helm CRD sync copies generated YAML, fails before partial updates, safely replaces manifest symlinks, removes stale nested YAML/YML/JSON, preserves non-manifest files, propagates scan failures, rejects symlink escapes, overlapping trees, and unsafe destinations, and detects drift read-only'
