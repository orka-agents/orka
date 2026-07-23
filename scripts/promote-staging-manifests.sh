#!/usr/bin/env bash
set -euo pipefail

LC_ALL=C
export LC_ALL

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
STAGING_DIR="${ROOT_DIR}/manifest_staging"
DEPLOY_DIR="${ROOT_DIR}/deploy"
CHARTS_DIR="${ROOT_DIR}/charts"

fail() {
  printf 'promote-staging-manifests: %s\n' "$*" >&2
  exit 1
}

[[ $# -eq 0 ]] || fail "this command takes no arguments"
[[ -f "${STAGING_DIR}/deploy/orka.yaml" ]] || fail "missing staging deploy manifest"
[[ -f "${STAGING_DIR}/charts/orka/Chart.yaml" ]] || fail "missing staging Helm chart"

work_dir=$(mktemp -d "${ROOT_DIR}/.manifest-promotion.XXXXXX")
deploy_backup=""
charts_backup=""
deploy_installed=false
charts_installed=false

cleanup() {
  local status=$?
  trap - EXIT

  if [[ $status -ne 0 ]]; then
    if [[ "$charts_installed" == true ]]; then rm -rf "$CHARTS_DIR"; fi
    if [[ -n "$charts_backup" && -e "$charts_backup" ]]; then mv "$charts_backup" "$CHARTS_DIR" || true; charts_backup=""; fi
    if [[ "$deploy_installed" == true ]]; then rm -rf "$DEPLOY_DIR"; fi
    if [[ -n "$deploy_backup" && -e "$deploy_backup" ]]; then mv "$deploy_backup" "$DEPLOY_DIR" || true; deploy_backup=""; fi
  fi
  if [[ -n "$deploy_backup" && -e "$deploy_backup" ]]; then rm -rf "$deploy_backup"; fi
  if [[ -n "$charts_backup" && -e "$charts_backup" ]]; then rm -rf "$charts_backup"; fi
  rm -rf "$work_dir"
  exit "$status"
}
trap cleanup EXIT

cp -R "${STAGING_DIR}/deploy" "${work_dir}/deploy"
cp -R "${STAGING_DIR}/charts" "${work_dir}/charts"

if [[ -e "$DEPLOY_DIR" || -L "$DEPLOY_DIR" ]]; then
  [[ -d "$DEPLOY_DIR" && ! -L "$DEPLOY_DIR" ]] || fail "deploy path is not a regular directory"
  deploy_backup="${ROOT_DIR}/.deploy.backup.$$"
  [[ ! -e "$deploy_backup" && ! -L "$deploy_backup" ]] || fail "deploy backup already exists"
  mv "$DEPLOY_DIR" "$deploy_backup"
fi
if [[ -e "$CHARTS_DIR" || -L "$CHARTS_DIR" ]]; then
  [[ -d "$CHARTS_DIR" && ! -L "$CHARTS_DIR" ]] || fail "charts path is not a regular directory"
  charts_backup="${ROOT_DIR}/.charts.backup.$$"
  [[ ! -e "$charts_backup" && ! -L "$charts_backup" ]] || fail "charts backup already exists"
  mv "$CHARTS_DIR" "$charts_backup"
fi

mv "${work_dir}/deploy" "$DEPLOY_DIR"
deploy_installed=true
mv "${work_dir}/charts" "$CHARTS_DIR"
charts_installed=true

if ! diff -ruN "${STAGING_DIR}/deploy" "$DEPLOY_DIR"; then fail "promoted deploy tree differs from staging"; fi
if ! diff -ruN "${STAGING_DIR}/charts" "$CHARTS_DIR"; then fail "promoted chart tree differs from staging"; fi

deploy_installed=false
charts_installed=false
trap - EXIT
cleanup_failed=false
if [[ -n "$deploy_backup" ]] && ! rm -rf "$deploy_backup"; then
  printf 'promote-staging-manifests: warning: could not remove backup %s\n' "$deploy_backup" >&2
  cleanup_failed=true
fi
if [[ -n "$charts_backup" ]] && ! rm -rf "$charts_backup"; then
  printf 'promote-staging-manifests: warning: could not remove backup %s\n' "$charts_backup" >&2
  cleanup_failed=true
fi
if ! rm -rf "$work_dir"; then
  printf 'promote-staging-manifests: warning: could not remove work directory %s\n' "$work_dir" >&2
  cleanup_failed=true
fi
if [[ "$cleanup_failed" == true ]]; then
  exit 1
fi
printf 'Promoted manifest_staging/deploy and manifest_staging/charts into release snapshots.\n'
