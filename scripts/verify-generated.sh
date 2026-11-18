#!/usr/bin/env bash
# Regenerates every checked-in generated file and fails when the result differs
# from the commit. Each stale file is printed with the command that regenerates
# it. Run it through `make verify-generated` on a clean working tree.
set -euo pipefail

make_cmd=${MAKE:-make}

changed_files() {
  {
    git diff --name-only HEAD --
    git ls-files --others --exclude-standard
  } | sort -u
}

regenerate_command() {
  case $1 in
  *zz_generated.*.go | *envelope_gen.go | internal/storageclient/storagefault/wrappers_gen.go)
    echo "make generate"
    ;;
  config/crd/bases/* | config/rbac/role.yaml | config/webhook/manifests.yaml | manifest_staging/*)
    echo "make manifests"
    ;;
  *)
    echo "make generate manifests"
    ;;
  esac
}

if [[ -n "$(changed_files)" ]]; then
  echo "verify-generated needs a clean working tree to tell generator output from your edits." >&2
  echo "Commit your changes (or run it in a fresh clone), then rerun: make verify-generated" >&2
  changed_files | sed 's/^/  /' >&2
  exit 2
fi

"${make_cmd}" generate manifests

stale=$(changed_files)
if [[ -z "${stale}" ]]; then
  echo "Generated files are up to date."
  exit 0
fi

{
  echo
  echo "Generated files are stale. These files differ from what their generators produce:"
  while IFS= read -r file; do
    printf '  %-90s fix: %s\n' "${file}" "$(regenerate_command "${file}")"
  done <<<"${stale}"
  echo
  echo "Never edit generated files by hand. Change the source (*_types.go, kubebuilder"
  echo "markers, config/, cmd/build/helmify/, or the storage provider interfaces), run"
  echo "'make generate manifests', and commit every file it changes."
  echo
  git --no-pager diff --stat HEAD --
} >&2
exit 1
