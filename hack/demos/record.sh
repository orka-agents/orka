#!/usr/bin/env bash
# record.sh — single reproducible entrypoint for recording the demo casts.
#
# Replaces the hand-copied `asciinema rec ...` snippet that lived (only) in
# Demo 10's header, so every cast is captured with the SAME geometry, idle
# trim, and profile. Pin those here once and the published batch can't drift
# (the Jun-2026 batch shipped at idle 1.2 while the docs said 1.5).
#
# Usage:
#   hack/demos/record.sh <demo> [profile] [out-dir]
#   hack/demos/record.sh all  [profile] [out-dir]
#
#   <demo>    one of: 00 10 20 30 40 50 60 70  (or "all")
#   profile   presenter|docs|social|hero   (default: docs — the published cast)
#   out-dir   where .cast files land        (default: hack/demos/out)
#
# Env:
#   DEMO_RECORD_IDLE=1.5     # idle-time-limit (canonical; matches RECORDING.md)
#   DEMO_RECORD_COLS=110 DEMO_RECORD_ROWS=30
#   DEMO_RECORD_ENV=hack/demos/cluster/demo-env.sh   # sourced OFF-CAMERA
#   DEMO_RECORD_VALIDATE=1   # run validate-casts.sh on each cast after recording
#
# The env file and the API port-forward are brought up BEFORE `asciinema rec`
# so the opening frame is the demo banner, not `demo-env loaded:` / port-forward
# plumbing.

set -Eeuo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${here}/../.." && pwd)"

idle="${DEMO_RECORD_IDLE:-1.5}"
cols="${DEMO_RECORD_COLS:-110}"
rows="${DEMO_RECORD_ROWS:-30}"
env_file="${DEMO_RECORD_ENV:-${here}/cluster/demo-env.sh}"

demo="${1:-}"
profile="${2:-docs}"
out_dir="${3:-${here}/out}"

case "${profile}" in
  presenter|docs|social|hero) ;;
  *) printf 'record: invalid profile %q (presenter|docs|social|hero)\n' "${profile}" >&2; exit 2 ;;
esac

if ! command -v asciinema >/dev/null 2>&1; then
  printf 'record: asciinema is not on PATH\n' >&2
  exit 2
fi

# demo id -> "script-basename|title|cast-name"
__demo_meta() {
  case "$1" in
    00) printf '00-preflight.sh|Orka — 00 preflight|00-preflight' ;;
    10) printf '10-chat-pr.sh|Orka — 10 chat to PR|10' ;;
    20) printf '20-manual-workflow.sh|Orka — 20 manual YAML workflow|20' ;;
    30) printf '30-cron-workflow.sh|Orka — 30 scheduled workflow|30' ;;
    40) printf '40-security-scanning.sh|Orka — 40 security remediation|40' ;;
    50) printf '50-kontxt.sh|Orka — 50 kontxt tokens|50' ;;
    60) printf '60-agent-sandbox.sh|Orka — 60 agent sandbox|60' ;;
    70) printf '70-agent-substrate.sh|Orka — 70 agent substrate|70' ;;
    *)  return 1 ;;
  esac
}

record_one() {
  local id="$1" meta script title cast_name out
  if ! meta="$(__demo_meta "${id}")"; then
    printf 'record: unknown demo %q\n' "${id}" >&2
    return 2
  fi
  IFS='|' read -r script title cast_name <<<"${meta}"
  out="${out_dir}/${cast_name}.cast"
  mkdir -p "${out_dir}"

  printf '==> recording demo %s (%s) in %s profile -> %s\n' "${id}" "${script}" "${profile}" "${out}"

  # Off-camera: source env (selects the intended kind context — the safety
  # layer for demos that create cluster resources and open real PRs) and bring
  # the API port-forward up so the cast opens on the banner, not the plumbing.
  # Fail LOUD if the env file is missing or errors: recording against the wrong
  # Kubernetes context is exactly the footgun demo-env.sh's kind-gate guards.
  if [[ -n "${env_file}" ]]; then
    if [[ ! -f "${env_file}" ]]; then
      printf 'record: DEMO_RECORD_ENV %q not found — refusing to record without a known context\n' "${env_file}" >&2
      return 1
    fi
    # shellcheck disable=SC1090
    if ! source "${env_file}" >/dev/null 2>&1; then
      printf 'record: failed to source %q — refusing to record against an unknown context\n' "${env_file}" >&2
      return 1
    fi
    # Fail closed when the env file could not select its intended kind context.
    # These demos create cluster resources and open real PRs, so recording
    # against an arbitrary current context is unsafe. demo-env.sh advertises this
    # via DEMO_ENV_KIND_READY (it is sourced and cannot exit on its own).
    # Override with DEMO_RECORD_ALLOW_ANY_CONTEXT=1 for a non-kind target (e.g. a
    # real cluster whose context the caller selected deliberately).
    if [[ "${DEMO_RECORD_ALLOW_ANY_CONTEXT:-0}" != "1" \
          && "${DEMO_ENV_KIND_READY:-0}" != "1" ]]; then
      printf 'record: %s did not select its intended kind context (DEMO_ENV_KIND_READY=%s); refusing to record. Start the cluster, or set DEMO_RECORD_ALLOW_ANY_CONTEXT=1 to override.\n' \
        "${env_file}" "${DEMO_ENV_KIND_READY:-unset}" >&2
      return 1
    fi
  fi

  # Check the recorder status explicitly: record_one is always called as
  # `record_one ... || rc=1`, which disables errexit INSIDE this function, so a
  # failed recording would otherwise fall through to validation (and could
  # "pass" a stale cast left at the target path, or return success outright when
  # DEMO_RECORD_VALIDATE=0). `--return` is load-bearing here: by default
  # `asciinema rec` exits 0 regardless of the recorded command's status, so
  # without it the explicit check below could never see a failed demo.
  #
  # The inner --command chains with `&&` (not `;`) so the demo runs ONLY if its
  # env loaded: a broken env file must abort the take, not silently record
  # against whatever context happens to be active. With an empty DEMO_RECORD_ENV
  # (caller sourced env themselves) the source clause is omitted entirely.
  local inner="cd '${repo_root}'"
  if [[ -n "${env_file}" ]]; then
    inner+=" && source '${env_file}' >/dev/null 2>&1"
  fi
  inner+=" && DEMO_RECORD_PROFILE='${profile}' ./hack/demos/${script}"
  if ! asciinema rec \
    --overwrite \
    --return \
    --idle-time-limit "${idle}" \
    --cols "${cols}" --rows "${rows}" \
    --title "${title}" \
    --command "${inner}" \
    "${out}"; then
    printf 'record: asciinema rec failed for demo %s — not publishing %s\n' "${id}" "${out}" >&2
    return 1
  fi

  if [[ "${DEMO_RECORD_VALIDATE:-1}" == "1" ]]; then
    # Per-demo idle-trimmed-playback budget. The flagship full-SDLC demos
    # (10 chat→PR, 20 manual→PR) drive a real coordinator + many child Tasks +
    # review + CI + a real PR, so their legitimate length exceeds the default
    # docs budget. Give them more headroom; everything else keeps the tight
    # default. Override per run with CAST_MAX_SECONDS.
    local demo_budget="${CAST_MAX_SECONDS:-}"
    if [[ -z "${demo_budget}" ]]; then
      case "${id}" in
        10|20) demo_budget=240 ;;
        *)     demo_budget=180 ;;
      esac
    fi
    CAST_REQUIRE_GEOMETRY=1 CAST_EXPECT_COLS="${cols}" CAST_EXPECT_ROWS="${rows}" \
    CAST_MAX_SECONDS="${demo_budget}" \
      bash "${here}/validate-casts.sh" "${out}" || {
        printf 'record: validation FAILED for %s — do not publish this take\n' "${out}" >&2
        return 1
      }
  fi
}

rc=0
if [[ "${demo}" == "all" ]]; then
  for id in 00 10 20 30 40 50 60 70; do
    record_one "${id}" || rc=1
  done
elif [[ -n "${demo}" ]]; then
  record_one "${demo}" || rc=1
else
  printf 'usage: %s <demo|all> [profile] [out-dir]\n' "$0" >&2
  exit 2
fi

exit "${rc}"
