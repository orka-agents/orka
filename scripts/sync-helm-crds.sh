#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source_dir="${root}/config/crd/bases"
destination_dir="${root}/charts/orka/crds"
mode="sync"

usage() {
  cat <<'USAGE'
Usage: scripts/sync-helm-crds.sh [--check] [--source DIR] [--destination DIR]

Synchronize production generated CRD YAML files into the Helm chart. The
development-only fake.workspace.orka.ai API group is excluded. Every
Helm-recognized manifest below the destination (*.yaml, *.yml, and *.json) is
managed; README.md and other non-manifest files are preserved.

Options:
  --check             Verify that source and destination CRDs are identical.
  --source DIR        Read generated CRDs from DIR.
  --destination DIR   Manage Helm CRDs in DIR.
  -h, --help          Show this help.
USAGE
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --check)
      mode="check"
      shift
      ;;
    --source)
      [[ $# -ge 2 ]] || {
        echo "--source requires a directory" >&2
        exit 2
      }
      source_dir="$2"
      shift 2
      ;;
    --destination)
      [[ $# -ge 2 ]] || {
        echo "--destination requires a directory" >&2
        exit 2
      }
      destination_dir="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

while :; do
  case "${destination_dir}" in
    /)
      break
      ;;
    */)
      destination_dir="${destination_dir%/}"
      ;;
    */.)
      destination_dir="${destination_dir%/.}"
      [[ -n "${destination_dir}" ]] || destination_dir="/"
      ;;
    *)
      break
      ;;
  esac
done

[[ -d "${source_dir}" ]] || {
  echo "generated CRD directory does not exist: ${source_dir}" >&2
  exit 1
}

shopt -s nullglob
source_candidates=("${source_dir}"/*.yaml)
if [[ ${#source_candidates[@]} -eq 0 ]]; then
  echo "generated CRD directory contains no YAML files: ${source_dir}" >&2
  exit 1
fi

source_files=()
for source_file in "${source_candidates[@]}"; do
  if [[ ! -f "${source_file}" || -L "${source_file}" ]]; then
    echo "generated CRD is not a regular file: ${source_file}" >&2
    exit 1
  fi
  group="$(awk '$1 == "group:" { print $2; exit }' "${source_file}")"
  if [[ "${group}" == "fake.workspace.orka.ai" ]]; then
    continue
  fi
  source_files+=("${source_file}")
done
if [[ ${#source_files[@]} -eq 0 ]]; then
  echo "generated CRD directory contains no production YAML files: ${source_dir}" >&2
  exit 1
fi


destination_name_for_source() {
  local source_file="$1"
  local kind
  kind="$(awk '''$1 == "names:" { in_names = 1; next } in_names && $1 == "kind:" { print $2; exit }''' "${source_file}")"
  if [[ -n "${kind}" ]]; then
    printf '%s-customresourcedefinition.yaml\n' "$(printf '%s' "${kind}" | tr '''[:upper:]''' '''[:lower:]''')"
    return
  fi
  # Synthetic unit-test fixtures without a CRD spec retain their basename.
  printf '%s\n' "${source_file##*/}"
}

source_for_destination_name() {
  local wanted="$1"
  local source_file
  for source_file in "${source_files[@]}"; do
    if [[ "$(destination_name_for_source "${source_file}")" == "${wanted}" ]]; then
      printf '%s\n' "${source_file}"
      return 0
    fi
  done
  return 1
}

source_physical="$(cd -- "${source_dir}" && pwd -P)"

validate_destination_directory() {
  local destination_physical

  if [[ -L "${destination_dir}" ]]; then
    echo "Helm CRD directory must not be a symlink: ${destination_dir}" >&2
    return 1
  fi
  if [[ ! -d "${destination_dir}" ]]; then
    echo "Helm CRD directory does not exist: ${destination_dir}" >&2
    return 1
  fi
  if ! destination_physical="$(cd -- "${destination_dir}" && pwd -P)"; then
    echo "cannot resolve Helm CRD directory: ${destination_dir}" >&2
    return 1
  fi
  if [[ "${destination_physical}" == "/" ]]; then
    echo "refusing to manage filesystem root as Helm CRD directory: ${destination_dir}" >&2
    return 1
  fi
  if [[ "${destination_physical}" == "${source_physical}" ]]; then
    echo "generated and Helm CRD directories must differ: ${destination_dir}" >&2
    return 1
  fi
  if [[ "${source_physical}" == "${destination_physical}/"* ||
    "${destination_physical}" == "${source_physical}/"* ]]; then
    echo "generated and Helm CRD directories must not overlap: ${source_dir} ${destination_dir}" >&2
    return 1
  fi
}

collect_helm_manifest_entries() {
  helm_manifest_entries=()
  local entry scan_file
  scan_file="$(mktemp "${TMPDIR:-/tmp}/sync-helm-crds-manifests.XXXXXX")"
  if ! find "${destination_dir}" -mindepth 1 \
    \( -name '*.yaml' -o -name '*.yml' -o -name '*.json' \) -print0 >"${scan_file}"; then
    echo "cannot scan Helm CRD destination for manifests: ${destination_dir}" >&2
    rm -f -- "${scan_file}"
    return 1
  fi
  while IFS= read -r -d '' entry; do
    helm_manifest_entries+=("${entry}")
  done <"${scan_file}"
  rm -f -- "${scan_file}"
}

validate_destination_manifest_entries() {
  local destination_entry symlink_entry symlink_scan_file

  symlink_scan_file="$(mktemp "${TMPDIR:-/tmp}/sync-helm-crds-symlinks.XXXXXX")"
  if ! find "${destination_dir}" -mindepth 1 -type l -print0 >"${symlink_scan_file}"; then
    echo "cannot scan Helm CRD destination for symlinks: ${destination_dir}" >&2
    rm -f -- "${symlink_scan_file}"
    return 1
  fi
  while IFS= read -r -d '' symlink_entry; do
    case "${symlink_entry}" in
      *.yaml|*.yml|*.json)
        ;;
      *)
        echo "Helm CRD directory must not contain unmanaged symlink: ${symlink_entry}" >&2
        rm -f -- "${symlink_scan_file}"
        return 1
        ;;
    esac
  done <"${symlink_scan_file}"
  rm -f -- "${symlink_scan_file}"

  collect_helm_manifest_entries || return 1
  for destination_entry in "${helm_manifest_entries[@]}"; do
    if [[ ! -f "${destination_entry}" && ! -L "${destination_entry}" ]]; then
      echo "Helm CRD destination is not a regular file or symlink: ${destination_entry}" >&2
      return 1
    fi
  done
}

is_generated_crd_destination() {
  local destination_entry="$1"
  local relative="${destination_entry:$(( ${#destination_dir} + 1 ))}"
  [[ "${relative}" != */* ]] || return 1
  source_for_destination_name "${relative}" >/dev/null
}

check_sync() {
  local drift=0
  local source_file destination_file name destination_entry relative

  validate_destination_directory || return 1
  validate_destination_manifest_entries || return 1

  for source_file in "${source_files[@]}"; do
    name="$(destination_name_for_source "${source_file}")"
    destination_file="${destination_dir}/${name}"
    if [[ -L "${destination_file}" ]]; then
      echo "symlinked Helm CRD: ${name}" >&2
      drift=1
    elif [[ ! -f "${destination_file}" ]]; then
      echo "missing Helm CRD: ${name}" >&2
      drift=1
    elif ! cmp -s "${source_file}" "${destination_file}"; then
      echo "out-of-sync Helm CRD: ${name}" >&2
      drift=1
    fi
  done

  collect_helm_manifest_entries
  for destination_entry in "${helm_manifest_entries[@]}"; do
    if ! is_generated_crd_destination "${destination_entry}"; then
      relative="${destination_entry:$(( ${#destination_dir} + 1 ))}"
      echo "stale Helm CRD: ${relative}" >&2
      drift=1
    fi
  done

  if [[ ${drift} -ne 0 ]]; then
    echo "Helm CRDs differ from config/crd/bases; run 'make sync-helm-crds'" >&2
    return 1
  fi
}

if [[ "${mode}" == "check" ]]; then
  check_sync
  printf 'Helm CRDs are synchronized (%d files)\n' "${#source_files[@]}"
  exit 0
fi

if [[ -L "${destination_dir}" ]]; then
  echo "Helm CRD directory must not be a symlink: ${destination_dir}" >&2
  exit 1
fi
if [[ -e "${destination_dir}" && ! -d "${destination_dir}" ]]; then
  echo "Helm CRD destination is not a directory: ${destination_dir}" >&2
  exit 1
fi
mkdir -p "${destination_dir}"
validate_destination_directory
validate_destination_manifest_entries

staging_dir="$(mktemp -d "${destination_dir}/.sync-helm-crds.XXXXXX")"
cleanup_staging() {
  rm -rf -- "${staging_dir}"
}
trap cleanup_staging EXIT

for source_file in "${source_files[@]}"; do
  name="$(destination_name_for_source "${source_file}")"
  cp -- "${source_file}" "${staging_dir}/${name}"
done

for source_file in "${source_files[@]}"; do
  name="$(destination_name_for_source "${source_file}")"
  destination_file="${destination_dir}/${name}"
  if [[ -L "${destination_file}" ]]; then
    rm -f -- "${destination_file}"
  elif [[ -e "${destination_file}" && ! -f "${destination_file}" ]]; then
    echo "Helm CRD destination is not a regular file: ${destination_file}" >&2
    exit 1
  fi
  mv -f -- "${staging_dir}/${name}" "${destination_file}"
done

collect_helm_manifest_entries
for destination_file in "${helm_manifest_entries[@]}"; do
  if ! is_generated_crd_destination "${destination_file}"; then
    rm -f -- "${destination_file}"
  fi
done

check_sync
printf 'Synchronized %d CRDs into %s\n' "${#source_files[@]}" "${destination_dir}"
