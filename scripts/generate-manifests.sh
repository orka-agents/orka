#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGING_DIR="${ROOT_DIR}/manifest_staging"
KUSTOMIZE="${ROOT_DIR}/bin/kustomize"

usage() {
  cat <<'USAGE'
Usage:
  scripts/generate-manifests.sh sync [--kustomize <path>]
  scripts/generate-manifests.sh check [--kustomize <path>]

Commands:
  sync   Generate deploy and Helm staging manifests and replace
         manifest_staging atomically.
  check  Generate into a temporary directory and verify that the committed
         manifest_staging tree is current.
USAGE
}

fail() {
  printf 'generate-manifests: %s\n' "$*" >&2
  exit 1
}

mode=${1:-}
[[ "$mode" == "sync" || "$mode" == "check" ]] || { usage >&2; exit 2; }
shift

while [[ $# -gt 0 ]]; do
  case "$1" in
    --kustomize)
      [[ $# -ge 2 ]] || fail "--kustomize requires a path"
      KUSTOMIZE=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      usage >&2
      exit 2
      ;;
  esac
done

[[ -x "$KUSTOMIZE" ]] || fail "kustomize is not executable: ${KUSTOMIZE}"
command -v go >/dev/null 2>&1 || fail "required command not found: go"

work_dir=$(mktemp -d "${ROOT_DIR}/.manifest-staging.generate.XXXXXX")
backup_dir=""
installed_new=false

cleanup() {
  local status=$?
  trap - EXIT

  if [[ $status -ne 0 ]]; then
    if [[ "$installed_new" == true && -e "$STAGING_DIR" ]]; then
      rm -rf "$STAGING_DIR"
    fi
    if [[ -n "$backup_dir" && -e "$backup_dir" ]]; then
      if [[ -e "$STAGING_DIR" || -L "$STAGING_DIR" ]]; then rm -rf "$STAGING_DIR"; fi
      mv "$backup_dir" "$STAGING_DIR" || true
      backup_dir=""
    fi
  fi
  if [[ -n "$backup_dir" && -e "$backup_dir" ]]; then
    rm -rf "$backup_dir"
  fi
  if [[ -n "$work_dir" && -e "$work_dir" ]]; then
    rm -rf "$work_dir"
  fi
  exit "$status"
}
trap cleanup EXIT

mkdir -p "${work_dir}/deploy" "${work_dir}/charts/orka"

cd "$ROOT_DIR"
"$KUSTOMIZE" build config/default > "${work_dir}/deploy/orka.yaml"
"$KUSTOMIZE" build \
  --load-restrictor LoadRestrictionsNone \
  third_party/open-policy-agent/gatekeeper/helmify | \
  go run ./third_party/open-policy-agent/gatekeeper/helmify \
    --output-dir "${work_dir}/charts/orka"

./scripts/helm-crds.sh sync "${work_dir}/charts/orka"
./scripts/helm-crds.sh check "${work_dir}/charts/orka"

[[ -s "${work_dir}/deploy/orka.yaml" ]] || fail "generated deploy manifest is empty"
[[ -f "${work_dir}/charts/orka/Chart.yaml" ]] || fail "generated chart is missing Chart.yaml"
[[ -f "${work_dir}/charts/orka/values.yaml" ]] || fail "generated chart is missing values.yaml"

if [[ "$mode" == "check" ]]; then
  [[ -d "$STAGING_DIR" ]] || fail "missing committed staging directory: ${STAGING_DIR}"
  if ! diff -ruN "$STAGING_DIR" "$work_dir"; then
    fail "committed staging manifests are stale; run make manifests"
  fi
  printf 'Verified committed staging manifests are current.\n'
  exit 0
fi

if [[ -e "$STAGING_DIR" || -L "$STAGING_DIR" ]]; then
  [[ -d "$STAGING_DIR" && ! -L "$STAGING_DIR" ]] || fail "staging path is not a regular directory: ${STAGING_DIR}"
  backup_dir="${ROOT_DIR}/.manifest-staging.backup.$$"
  [[ ! -e "$backup_dir" && ! -L "$backup_dir" ]] || fail "temporary backup already exists: ${backup_dir}"
  mv "$STAGING_DIR" "$backup_dir"
fi

mv "$work_dir" "$STAGING_DIR"
work_dir=""
installed_new=true

if [[ -n "$backup_dir" ]]; then
  rm -rf "$backup_dir"
  backup_dir=""
fi
installed_new=false
trap - EXIT
printf 'Generated staging deploy manifest and Helm chart under %s.\n' "$STAGING_DIR"
