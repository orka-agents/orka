#!/usr/bin/env bash
# validate-casts.sh — gate asciinema recordings before they are published.
#
# A recorded demo is the most-shared artifact of the whole suite, yet nothing
# today stops a broken take from shipping: the Jun-2026 batch shipped a Demo 20
# that ended on `exit 1` and a Demo 40 whose success card read "scanPhase Error".
# This validator is that missing gate. It parses the asciinema v3 cast (header +
# event stream) and FAILS a cast that exhibits a viewer-visible defect:
#
#   1. ends on a non-zero process exit (an "x" exit event with code != 0)
#   2. ends on raw/escaped JSON instead of a payoff card or a clean log line
#   3. has the wrong terminal geometry (cols/rows) for docs embedding
#   4. uses an idle_time_limit outside the allowed band
#   5. exceeds the per-profile playback duration budget
#   6. contains a viewer-visible hard error (a kubectl/Go panic / traceback)
#
# Usage:
#   hack/demos/validate-casts.sh <file-or-dir> [<file-or-dir> ...]
#   hack/demos/validate-casts.sh ~/Downloads/orka-demos-2026-06-08
#
# Env overrides (defaults target the docs profile at 110x30):
#   CAST_EXPECT_COLS=110 CAST_EXPECT_ROWS=30
#   CAST_IDLE_MIN=1.0 CAST_IDLE_MAX=2.0
#   CAST_MAX_SECONDS=180        # 0 disables the duration budget
#   CAST_REQUIRE_GEOMETRY=1     # 0 to skip cols/rows enforcement
#
# Exit code is the number of casts that failed (0 = all clean), capped at 250.

set -Eeuo pipefail

if ! command -v jq >/dev/null 2>&1; then
  printf 'validate-casts: jq is required\n' >&2
  exit 2
fi

CAST_EXPECT_COLS="${CAST_EXPECT_COLS:-110}"
CAST_EXPECT_ROWS="${CAST_EXPECT_ROWS:-30}"
CAST_IDLE_MIN="${CAST_IDLE_MIN:-1.0}"
CAST_IDLE_MAX="${CAST_IDLE_MAX:-2.0}"
CAST_MAX_SECONDS="${CAST_MAX_SECONDS:-180}"
CAST_REQUIRE_GEOMETRY="${CAST_REQUIRE_GEOMETRY:-1}"

# Strip ANSI escapes / control sequences so end-of-cast content checks see text.
__strip_ansi() {
  perl -pe 's/\e\[[0-9;?]*[A-Za-z]//g; s/\e\][^\a]*\a//g; s/\e[()][AB0]//g; s/\e[<>=]//g; s/[\r\a]//g'
}

# Print the last N non-blank visible lines of a cast's output stream.
__cast_tail_text() {
  local file="$1" n="${2:-12}"
  jq -j 'select(type=="array" and .[1]=="o") | .[2]' "${file}" 2>/dev/null \
    | __strip_ansi | grep -vE '^[[:space:]]*$' | tail -n "${n}"
}

# A "float <= float" helper using awk (avoids bc dependency).
__fle() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a<=b)}'; }
__fge() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a>=b)}'; }

validate_one() {
  local file="$1"
  local problems=()

  # --- Header parse ---------------------------------------------------------
  local header
  header="$(head -n 1 "${file}" 2>/dev/null || true)"
  if [[ -z "${header}" ]] || ! printf '%s' "${header}" | jq -e . >/dev/null 2>&1; then
    printf 'FAIL %s\n  - first line is not a valid asciinema header\n' "${file}" >&2
    return 1
  fi
  local version cols rows idle
  version="$(printf '%s' "${header}" | jq -r '.version // 0')"
  cols="$(printf '%s' "${header}" | jq -r '.term.cols // .width // 0')"
  rows="$(printf '%s' "${header}" | jq -r '.term.rows // .height // 0')"
  idle="$(printf '%s' "${header}" | jq -r '.idle_time_limit // 0')"

  # --- Geometry -------------------------------------------------------------
  if [[ "${CAST_REQUIRE_GEOMETRY}" == "1" ]]; then
    [[ "${cols}" == "${CAST_EXPECT_COLS}" ]] || problems+=("cols=${cols}, want ${CAST_EXPECT_COLS}")
    [[ "${rows}" == "${CAST_EXPECT_ROWS}" ]] || problems+=("rows=${rows}, want ${CAST_EXPECT_ROWS}")
  fi

  # --- idle_time_limit band -------------------------------------------------
  if [[ "${idle}" == "0" || "${idle}" == "null" ]]; then
    problems+=("idle_time_limit not set (recommend ${CAST_IDLE_MIN}-${CAST_IDLE_MAX})")
  elif ! __fge "${idle}" "${CAST_IDLE_MIN}" || ! __fle "${idle}" "${CAST_IDLE_MAX}"; then
    problems+=("idle_time_limit=${idle} outside ${CAST_IDLE_MIN}-${CAST_IDLE_MAX}")
  fi

  # --- Duration budget ------------------------------------------------------
  if [[ "${CAST_MAX_SECONDS}" != "0" ]]; then
    local dur cap
    # Budget the IDLE-TRIMMED PLAYBACK duration, not raw recording time. The
    # player caps any inactive gap at idle_time_limit (that is the whole point
    # of recording with --idle-time-limit), so a demo that waits minutes on
    # Kubernetes/model work plays back in seconds. Measuring raw intervals would
    # falsely fail such casts. Cap each interval at idle_time_limit before
    # summing; fall back to a sane cap when the header omits it.
    #   v3: event time fields are INTER-EVENT DELTAS -> cap each, then SUM.
    #   v2: ABSOLUTE timestamps -> convert to deltas, cap each, then SUM.
    cap="${idle}"
    if [[ "${cap}" == "0" || "${cap}" == "null" ]]; then
      cap="${CAST_IDLE_MAX}"
    fi
    if [[ "${version}" == "2" ]]; then
      dur="$(jq -s --argjson cap "${cap}" '
        [.[] | select(type=="array") | .[0]]
        | . as $ts
        | [range(0; length) | $ts[.] - (if . == 0 then 0 else $ts[.-1] end)]
        | map(if . > $cap then $cap else . end) | add // 0' "${file}" 2>/dev/null || echo 0)"
    else
      dur="$(jq -s --argjson cap "${cap}" '
        [.[] | select(type=="array") | .[0]]
        | map(if . > $cap then $cap else . end) | add // 0' "${file}" 2>/dev/null || echo 0)"
    fi
    if __fge "${dur}" "${CAST_MAX_SECONDS}"; then
      problems+=("idle-trimmed playback ${dur}s exceeds budget ${CAST_MAX_SECONDS}s")
    fi
  fi

  # --- Exit event (catch failed recordings) ---------------------------------
  # asciinema v3 emits an "x" (exit) event carrying the recorded command's exit
  # code; a non-zero code means the demo ended on a failing process (the Demo-20
  # breakage). A *missing* exit event is also suspect: every v3 cast in this
  # suite carries one, so its absence usually means a truncated/aborted
  # recording — fail closed rather than silently pass. Allow opting out for
  # formats/recorders that legitimately omit exit events (e.g. v2) via
  # CAST_REQUIRE_EXIT_EVENT=0.
  local exit_codes bad_exit require_exit
  exit_codes="$(jq -r 'select(type=="array" and .[1]=="x") | .[2]' "${file}" 2>/dev/null || true)"
  bad_exit="$(printf '%s\n' "${exit_codes}" | grep -vE '^0?$' | head -n 1 || true)"
  # v2 casts have no exit-event concept; only require one for v3+.
  require_exit="${CAST_REQUIRE_EXIT_EVENT:-1}"
  [[ "${version}" == "2" ]] && require_exit=0
  if [[ -n "${bad_exit}" ]]; then
    problems+=("ends on non-zero exit event (code=${bad_exit})")
  elif [[ "${require_exit}" == "1" && -z "$(printf '%s' "${exit_codes}" | tr -d '[:space:]')" ]]; then
    problems+=("no exit (x) event present — likely a truncated/failed recording (set CAST_REQUIRE_EXIT_EVENT=0 to allow)")
  fi

  # --- Final-frame content --------------------------------------------------
  local tail_text last_line
  tail_text="$(__cast_tail_text "${file}" 14)"
  last_line="$(printf '%s\n' "${tail_text}" | tail -n 1)"

  # Ending on a bare JSON bracket / escaped-JSON blob reads as "raw dump", not a
  # payoff. Allow it only if a payoff card or a clean success log is visible in
  # the final frame.
  if printf '%s' "${last_line}" | grep -qE '^[[:space:]]*[]}][[:space:]]*,?[[:space:]]*$'; then
    if ! printf '%s\n' "${tail_text}" | grep -qE '╰|╯|✅|🎉|🚀|⚡|Pull request|PR '; then
      problems+=("ends on raw JSON bracket with no payoff card in final frame")
    fi
  fi
  # An escaped newline blob in the last line is a tostring'd JSON dump.
  if printf '%s' "${last_line}" | grep -qE '\\n.*\\n'; then
    problems+=("ends on escaped-JSON blob (\\n-laden line)")
  fi

  # --- Viewer-visible hard errors anywhere ----------------------------------
  local hard_err
  hard_err="$(jq -j 'select(type=="array" and .[1]=="o") | .[2]' "${file}" 2>/dev/null \
    | __strip_ansi \
    | grep -nE 'panic:|goroutine [0-9]+ \[|Traceback \(most recent call last\)|standard_init_linux|level=fatal' \
    | head -n 1 || true)"
  if [[ -n "${hard_err}" ]]; then
    problems+=("viewer-visible hard error: ${hard_err}")
  fi

  if (( ${#problems[@]} > 0 )); then
    printf 'FAIL %s\n' "${file}" >&2
    local p
    for p in "${problems[@]}"; do
      printf '  - %s\n' "${p}" >&2
    done
    return 1
  fi
  printf 'ok   %s\n' "${file}"
  return 0
}

# --- Collect targets --------------------------------------------------------
declare -a targets=()
if (( $# == 0 )); then
  printf 'usage: %s <file-or-dir> [...]\n' "$0" >&2
  exit 2
fi
missing=0
for arg in "$@"; do
  if [[ -d "${arg}" ]]; then
    while IFS= read -r f; do targets+=("${f}"); done \
      < <(find "${arg}" -maxdepth 1 -type f -name '*.cast' | sort)
  elif [[ -f "${arg}" ]]; then
    targets+=("${arg}")
  else
    # A mistyped/missing explicit target must FAIL the gate, not just warn —
    # otherwise `validate-casts a.cast typo.cast` can exit 0 on the one that
    # happened to exist, silently skipping a recording that was requested.
    printf 'validate-casts: no such file or directory: %s\n' "${arg}" >&2
    missing=$(( missing + 1 ))
  fi
done

if (( ${#targets[@]} == 0 )); then
  printf 'validate-casts: no .cast files found\n' >&2
  exit 2
fi

fail=0
for f in "${targets[@]}"; do
  if ! validate_one "${f}"; then
    fail=$(( fail + 1 ))
  fi
done

printf '\n%d/%d casts passed' "$(( ${#targets[@]} - fail ))" "${#targets[@]}"
if (( missing > 0 )); then
  printf ' (%d requested target(s) missing)' "${missing}"
fi
printf '\n'
fail=$(( fail + missing ))
(( fail > 250 )) && fail=250
exit "${fail}"
