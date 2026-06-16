#!/usr/bin/env bash
# validate-casts.sh correctly accepts a clean cast and rejects the defect
# classes the audit found (non-zero exit ending, raw-JSON ending, wrong
# geometry, over-budget). Uses self-contained synthetic v3 casts — no recorded
# artifacts or cluster required — so the gate's own logic is covered by
# `make demo-test`.

set -Eeuo pipefail

_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_validator="$(cd "${_here}/../.." && pwd)/validate-casts.sh"

if [[ ! -x "${_validator}" && ! -f "${_validator}" ]]; then
  printf 'FAIL validate-casts.sh not found at %s\n' "${_validator}" >&2
  exit 1
fi

_tmp="$(mktemp -d)"
trap 'rm -rf "${_tmp}"' EXIT

# v3 header: cols/rows match the validator defaults (110x30); idle within band.
_header='{"version":3,"term":{"cols":110,"rows":30},"idle_time_limit":1.5}'

write_cast() { printf '%s\n' "$1" > "${_tmp}/$2"; }

# A clean cast: short, ends on a payoff card line, exits 0.
{
  printf '%s\n' "${_header}"
  printf '[0.1,"o","starting demo\\r\\n"]\n'
  printf '[0.2,"o","working...\\r\\n"]\n'
  printf '[0.2,"o","\\u2570\\u2500\\u2500\\u2500 Pull request opened \\u2500\\u2500\\u2500\\u256f\\r\\n"]\n'
  printf '[0.2,"x","0"]\n'
} > "${_tmp}/good.cast"

# Ends on a non-zero exit event (the Demo-20 breakage).
{
  printf '%s\n' "${_header}"
  printf '[0.1,"o","running\\r\\n"]\n'
  printf '[0.2,"x","1"]\n'
} > "${_tmp}/badexit.cast"

# v3 cast with NO exit event — a truncated/failed recording.
{
  printf '%s\n' "${_header}"
  printf '[0.1,"o","\\u2570 Pull request opened \\u256f\\r\\n"]\n'
} > "${_tmp}/noexit.cast"

# Ends on a raw JSON bracket with no payoff card (the Demo-40/50/70 ending).
{
  printf '%s\n' "${_header}"
  printf '[0.1,"o","audit dump:\\r\\n"]\n'
  printf '[0.2,"o","[\\r\\n"]\n'
  printf '[0.2,"o","  {\\"k\\": 1}\\r\\n"]\n'
  printf '[0.2,"o","]\\r\\n"]\n'
  printf '[0.2,"x","0"]\n'
} > "${_tmp}/rawjson.cast"

# Wrong geometry (80x24 instead of 110x30).
{
  printf '%s\n' '{"version":3,"term":{"cols":80,"rows":24},"idle_time_limit":1.5}'
  printf '[0.1,"o","\\u2570 Pull request opened \\u256f\\r\\n"]\n'
  printf '[0.2,"x","0"]\n'
} > "${_tmp}/geometry.cast"

run_validator() {
  # Returns the validator exit code; geometry off unless a case needs it.
  local require_geo="$1"; shift
  CAST_REQUIRE_GEOMETRY="${require_geo}" CAST_MAX_SECONDS="${CAST_MAX_SECONDS:-180}" \
    bash "${_validator}" "$@" >/dev/null 2>&1
}

# good.cast passes (geometry enforced).
if run_validator 1 "${_tmp}/good.cast"; then :; else
  printf 'FAIL clean cast was rejected\n' >&2; exit 1
fi

# badexit.cast is rejected.
if run_validator 0 "${_tmp}/badexit.cast"; then
  printf 'FAIL non-zero-exit cast was accepted\n' >&2; exit 1
fi

# noexit.cast (v3 with no exit event) is rejected as a likely-failed recording.
if run_validator 0 "${_tmp}/noexit.cast"; then
  printf 'FAIL v3 cast with no exit event was accepted\n' >&2; exit 1
fi

# ...but the same cast passes when the exit-event requirement is opted out.
if CAST_REQUIRE_EXIT_EVENT=0 run_validator 0 "${_tmp}/noexit.cast"; then :; else
  printf 'FAIL CAST_REQUIRE_EXIT_EVENT=0 did not relax the missing-exit check\n' >&2; exit 1
fi

# rawjson.cast is rejected.
if run_validator 0 "${_tmp}/rawjson.cast"; then
  printf 'FAIL raw-JSON-ending cast was accepted\n' >&2; exit 1
fi

# geometry.cast is rejected when geometry is enforced.
if run_validator 1 "${_tmp}/geometry.cast"; then
  printf 'FAIL wrong-geometry cast was accepted under CAST_REQUIRE_GEOMETRY=1\n' >&2; exit 1
fi

# over-budget: good.cast fails an absurdly small budget.
if CAST_MAX_SECONDS=0.01 run_validator 1 "${_tmp}/good.cast"; then
  printf 'FAIL over-budget cast was accepted\n' >&2; exit 1
fi

# A mistyped/missing explicit target fails the gate even alongside a valid cast.
if run_validator 1 "${_tmp}/good.cast" "${_tmp}/does-not-exist.cast"; then
  printf 'FAIL missing explicit target was ignored when a valid cast was present\n' >&2; exit 1
fi

# Idle-trim: a long inactive gap plays back capped at idle_time_limit, so a cast
# with a 600s raw gap must still pass a budget it would only blow if measured
# raw. (idle=1.5; trimmed playback ~2.4s << 180s.)
{
  printf '%s\n' "${_header}"
  printf '[0.5,"o","start\\r\\n"]\n'
  printf '[600.0,"o","after a long k8s wait\\r\\n"]\n'
  printf '[0.3,"o","\\u2570 Pull request opened \\u256f\\r\\n"]\n'
  printf '[0.1,"x","0"]\n'
} > "${_tmp}/longgap.cast"
if CAST_MAX_SECONDS=180 run_validator 0 "${_tmp}/longgap.cast"; then :; else
  printf 'FAIL long-idle-gap cast was rejected; duration must be idle-trimmed, not raw\n' >&2; exit 1
fi

printf 'PASS\n'
