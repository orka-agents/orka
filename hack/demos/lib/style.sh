#!/usr/bin/env bash
# Visual style kit for hack/demos scripts.
#
# Provides:
#   banner, chapter, narrate, log_info/success/warning/error
#   demo_profile, demo_profile_is, demo_pe, demo_show
#   payoff_card_pr / _security / _cron / _kontxt / _sandbox
#
# Pure bash + ANSI — no gum/glow/bat. Designed to be sourced from
# lib/common.sh so every script picks it up without explicit sourcing.
#
# Recording profiles (DEMO_RECORD_PROFILE):
#   presenter (default) — full transparency, typewriter on, all chapters
#   docs                — typewriter off, narration cues printed, all chapters
#   social              — typewriter off, only chapters 1..3
#   hero                — typewriter off, no chapters at all (≤60s cuts)
#
# This file is sourced — do not `set -euo pipefail` here.

# Idempotent: don't re-source if already loaded.
if [[ "${__ORKA_DEMO_STYLE_LOADED:-0}" == "1" ]]; then
  return 0 2>/dev/null || true
fi
__ORKA_DEMO_STYLE_LOADED=1

# ---------------------------------------------------------------------------
# Color palette — extends demo-magic.sh's GREEN/CYAN/BOLD/COLOR_RESET.
# tput-based with NO_COLOR + non-tty fallbacks identical to demo-magic.sh.
# ---------------------------------------------------------------------------
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  COLOR_RESET="${COLOR_RESET:-$(tput sgr0 2>/dev/null || printf '')}"
  GREEN="${GREEN:-$(tput setaf 2 2>/dev/null || printf '')}"
  CYAN="${CYAN:-$(tput setaf 6 2>/dev/null || printf '')}"
  BOLD="${BOLD:-$(tput bold 2>/dev/null || printf '')}"
  RED="${RED:-$(tput setaf 1 2>/dev/null || printf '')}"
  YELLOW="${YELLOW:-$(tput setaf 3 2>/dev/null || printf '')}"
  BLUE="${BLUE:-$(tput setaf 4 2>/dev/null || printf '')}"
  MAGENTA="${MAGENTA:-$(tput setaf 5 2>/dev/null || printf '')}"
  DIM="${DIM:-$(tput dim 2>/dev/null || printf '')}"
else
  COLOR_RESET="${COLOR_RESET:-}"
  GREEN="${GREEN:-}"
  CYAN="${CYAN:-}"
  BOLD="${BOLD:-}"
  RED="${RED:-}"
  YELLOW="${YELLOW:-}"
  BLUE="${BLUE:-}"
  MAGENTA="${MAGENTA:-}"
  DIM="${DIM:-}"
fi

# ---------------------------------------------------------------------------
# Profile dispatcher.
# ---------------------------------------------------------------------------

demo_profile() {
  local profile="${DEMO_RECORD_PROFILE:-presenter}"
  case "${profile}" in
    presenter|docs|hero|social) printf '%s' "${profile}" ;;
    *) printf 'presenter' ;;
  esac
}

demo_profile_is() {
  [[ "$(demo_profile)" == "$1" ]]
}

# Wraps demo-magic's pe(). In presenter, behaves like pe (typewriter on).
# In docs/social/hero, runs the command with TYPE_SPEED=0 so the prompt
# renders instantly. Legacy `pe` calls keep working unchanged.
demo_pe() {
  local cmd="$*"
  if demo_profile_is presenter; then
    pe "${cmd}"
  else
    local _saved_speed="${TYPE_SPEED:-}"
    TYPE_SPEED=0
    pe "${cmd}"
    TYPE_SPEED="${_saved_speed}"
  fi
}

# demo_show <path> — render a file with profile-appropriate verbosity.
#   presenter: full file via cat (audit transparency)
#   docs:      first 20 lines + footer with path
#   social:    first 8 lines + footer with path
#   hero:      footer with path only (no body)
demo_show() {
  local path="$1"
  if [[ ! -f "${path}" ]]; then
    printf '%b[file not found: %s]%b\n' "${YELLOW}" "${path}" "${COLOR_RESET}"
    return 0
  fi
  local profile
  profile="$(demo_profile)"
  case "${profile}" in
    presenter) cat "${path}" ;;
    docs)      head -n 20 "${path}"; printf '%b… (%s)%b\n' "${DIM}" "${path}" "${COLOR_RESET}" ;;
    social)    head -n 8  "${path}"; printf '%b… (%s)%b\n' "${DIM}" "${path}" "${COLOR_RESET}" ;;
    hero)      printf '%b(%s)%b\n' "${DIM}" "${path}" "${COLOR_RESET}" ;;
  esac
}

# ---------------------------------------------------------------------------
# Banner — `╔══╗ ║ 🤖 Orka — <title> ║ ╚══╝` (68 cols wide, cyan border).
# Call once per script, after `clear`. Idempotent within a single shell
# (does not refuse a second call — scripts may print multiple banners
# intentionally during long pre-warm sequences).
# ---------------------------------------------------------------------------
banner() {
  local title="${1:-Orka demo}"
  local inner_width=66
  local label="🤖 Orka — ${title}"
  # Account for the emoji glyph rendering as 2 cells in most terminals.
  # ${#label} counts codepoints; emoji adds +1 cell over its codepoint count.
  local visible_len
  visible_len=$(( ${#label} + 1 ))
  # Title row layout inside the box: "  <label><spaces>  " — 4 fixed cells
  # of padding plus the variable spaces, must total inner_width.
  local pad=$(( inner_width - visible_len - 4 ))
  (( pad < 0 )) && pad=0
  local spaces
  spaces="$(printf '%*s' "${pad}" '')"
  local bar
  bar="$(printf '═%.0s' $(seq 1 "${inner_width}"))"
  printf '\n'
  printf '%b╔%s╗%b\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  printf '%b║%b  %b%s%b%s  %b║%b\n' \
    "${CYAN}" "${COLOR_RESET}" \
    "${BOLD}" "${label}" "${COLOR_RESET}" "${spaces}" \
    "${CYAN}" "${COLOR_RESET}"
  printf '%b╚%s╝%b\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  printf '\n'
}

# ---------------------------------------------------------------------------
# Chapters + narrate.
#
# Auto-numbered via __DEMO_CHAPTER_INDEX (1-based). Total comes from
# DEMO_CHAPTER_TOTAL set by the script; if unset, just shows "Chapter <n>".
#
# narrate() records a one-line cue for the *next* chapter call. The cue is
# printed under the chapter title in docs profile, silent in others.
# ---------------------------------------------------------------------------
__DEMO_CHAPTER_INDEX=0
__DEMO_NARRATE_PENDING=""

narrate() {
  __DEMO_NARRATE_PENDING="$*"
}

chapter() {
  local title="${1:-}"
  local emoji="${2:-▸}"
  __DEMO_CHAPTER_INDEX=$(( __DEMO_CHAPTER_INDEX + 1 ))
  local idx="${__DEMO_CHAPTER_INDEX}"
  local total="${DEMO_CHAPTER_TOTAL:-}"
  local profile
  profile="$(demo_profile)"

  # hero: suppress all chapters.
  if [[ "${profile}" == "hero" ]]; then
    __DEMO_NARRATE_PENDING=""
    return 0
  fi
  # social: suppress chapters past index 3.
  if [[ "${profile}" == "social" ]] && (( idx > 3 )); then
    __DEMO_NARRATE_PENDING=""
    return 0
  fi

  local label
  if [[ -n "${total}" ]]; then
    label="Chapter ${idx}/${total} — ${title}"
  else
    label="Chapter ${idx} — ${title}"
  fi

  local bar_width=68
  local bar
  bar="$(printf '━%.0s' $(seq 1 "${bar_width}"))"
  printf '\n%b%s%b\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  printf '%b%s%b  %b%s%b\n' \
    "${BOLD}" "${emoji}" "${COLOR_RESET}" \
    "${BOLD}" "${label}" "${COLOR_RESET}"
  if [[ "${profile}" == "docs" && -n "${__DEMO_NARRATE_PENDING}" ]]; then
    printf '%b> %s%b\n' "${DIM}" "${__DEMO_NARRATE_PENDING}" "${COLOR_RESET}"
  fi
  printf '%b%s%b\n\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  __DEMO_NARRATE_PENDING=""
}

# ---------------------------------------------------------------------------
# log_info / success / warning / error.
# Format: [HH:MM:SS] <emoji>  <msg>
# info/success → stdout (so asciinema captures them in the cast).
# warning/error → stderr.
# ---------------------------------------------------------------------------

__demo_log_ts() {
  date +'%H:%M:%S'
}

log_info() {
  printf '%b[%s]%b %bℹ️ %b  %s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${BLUE}" "${COLOR_RESET}" "$*"
}

log_success() {
  printf '%b[%s]%b %b✅%b  %s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${GREEN}" "${COLOR_RESET}" "$*"
}

log_warning() {
  printf '%b[%s]%b %b⚠️ %b  %s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${YELLOW}" "${COLOR_RESET}" "$*" >&2
}

log_error() {
  printf '%b[%s]%b %b❌%b  %s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${RED}" "${COLOR_RESET}" "$*" >&2
}

# ---------------------------------------------------------------------------
# Payoff card primitives.
#
# Cards are 62 inner cols wide (64 outer with the box edges). Hand-drawn
# with `╭ ╮ ╰ ╯ │ ─` and `printf '│ %-58s │'` for body lines.
# ---------------------------------------------------------------------------

# Internal: print a single card body line with proper padding. Truncates
# overlong lines to fit. Color codes inside `$content` are NOT counted
# toward width, so callers should keep content plain. Use __card_line_styled
# for already-padded styled text.
__CARD_INNER=58
__CARD_BAR_WIDTH=60

__card_top() {
  local title="${1:-}"
  local bar_width="${__CARD_BAR_WIDTH}"
  local left="╭─"
  local right="─╮"
  if [[ -n "${title}" ]]; then
    local label=" ${title} "
    # Layout: ╭─<label><fill>─╮ where each corner contributes 1 ─.
    # Total ─ characters between ╭ and ╮ is bar_width.
    local rest=$(( bar_width - ${#label} - 2 ))
    (( rest < 2 )) && rest=2
    local fill
    fill="$(printf '─%.0s' $(seq 1 "${rest}"))"
    printf '%b%s%s%s%s%b\n' "${CYAN}" "${left}" "${label}" "${fill}" "${right}" "${COLOR_RESET}"
  else
    local fill
    fill="$(printf '─%.0s' $(seq 1 $(( bar_width - 2 ))))"
    printf '%b╭─%s─╮%b\n' "${CYAN}" "${fill}" "${COLOR_RESET}"
  fi
}

__card_bottom() {
  local bar_width="${__CARD_BAR_WIDTH}"
  local fill
  fill="$(printf '─%.0s' $(seq 1 "${bar_width}"))"
  printf '%b╰%s╯%b\n' "${CYAN}" "${fill}" "${COLOR_RESET}"
}

__card_blank() {
  local pad
  pad="$(printf '%*s' "${__CARD_INNER}" '')"
  printf '%b│%b %s %b│%b\n' "${CYAN}" "${COLOR_RESET}" "${pad}" "${CYAN}" "${COLOR_RESET}"
}

# __card_line "<plain text>"
# Pads to inner width; truncates with ellipsis if too long.
__card_line() {
  local text="$1"
  local max="${__CARD_INNER}"
  if (( ${#text} > max )); then
    text="${text:0:$(( max - 1 ))}…"
  fi
  printf '%b│%b %-*s %b│%b\n' \
    "${CYAN}" "${COLOR_RESET}" \
    "${max}" "${text}" \
    "${CYAN}" "${COLOR_RESET}"
}

# __card_kv "<key>" "<value>"
__card_kv() {
  local key="$1"
  local val="$2"
  local key_field
  key_field="$(printf '%-12s' "${key}")"
  local combined="${key_field}${val}"
  __card_line "${combined}"
}

# ---------------------------------------------------------------------------
# payoff_card_pr <task-name>
#
# Reads summarize_task_run JSON, extracts task name, agent, child count,
# and the PR URL from the result text. Exits 1 (returns non-zero) if no
# PR URL is present so callers can fail loudly.
# ---------------------------------------------------------------------------
payoff_card_pr() {
  local task_name="$1"
  if ! command -v jq >/dev/null 2>&1; then
    log_error "payoff_card_pr requires jq"
    return 1
  fi
  local json
  json="$(summarize_task_run "${task_name}" chat-to-pr 2>/dev/null || printf '{}')"
  local task agent children result pr_url phase
  task="$(printf '%s' "${json}"   | jq -r '.task    // "-"')"
  agent="$(printf '%s' "${json}"  | jq -r '.agent   // "-"')"
  phase="$(printf '%s' "${json}"  | jq -r '.phase   // "-"')"
  children="$(printf '%s' "${json}" | jq -r '.childTasks // 0')"
  result="$(printf '%s' "${json}" | jq -r '.result  // ""')"
  pr_url="$(printf '%s\n' "${result}" \
    | grep -Eo 'https://github[.]com/[^[:space:])"<>'\'']+/pull/[0-9]+' \
    | head -n 1 || true)"

  if [[ -z "${pr_url}" ]]; then
    log_error "payoff_card_pr: no PR URL in task ${task_name} result"
    return 1
  fi

  __card_top "Pull request opened"
  __card_kv "task"     "${task}"
  __card_kv "phase"    "${phase}"
  __card_kv "agent"    "${agent}"
  __card_kv "children" "${children}"
  __card_blank
  __card_kv "PR"       "${pr_url}"
  __card_bottom
}

# ---------------------------------------------------------------------------
# payoff_card_security <finding-id> <pr-json-file>
# ---------------------------------------------------------------------------
payoff_card_security() {
  local finding_id="$1"
  local pr_file="${2:-}"
  if ! command -v jq >/dev/null 2>&1; then
    log_error "payoff_card_security requires jq"
    return 1
  fi
  local json
  json="$(summarize_security_run "${finding_id}" "${pr_file}" 2>/dev/null || printf '{}')"
  local scan phase patches pr_url pr_status
  scan="$(printf '%s'      "${json}" | jq -r '.repositoryScan // "-"')"
  phase="$(printf '%s'     "${json}" | jq -r '.scanPhase      // "-"')"
  patches="$(printf '%s'   "${json}" | jq -r '(.patches // []) | length')"
  pr_url="$(printf '%s'    "${json}" | jq -r '.pullRequest.url    // ""')"
  pr_status="$(printf '%s' "${json}" | jq -r '.pullRequest.status // "-"')"

  __card_top "Security finding remediated"
  __card_kv "finding"   "${finding_id}"
  __card_kv "scan"      "${scan}"
  __card_kv "scanPhase" "${phase}"
  __card_kv "patches"   "${patches}"
  __card_kv "prStatus"  "${pr_status}"
  __card_blank
  if [[ -n "${pr_url}" ]]; then
    __card_kv "PR"      "${pr_url}"
  else
    __card_kv "PR"      "(no pull request URL yet)"
  fi
  __card_bottom
}

# ---------------------------------------------------------------------------
# payoff_card_cron <child-task-name>
#
# Pulls the cron schedule from DEMO_CRON_SCHEDULE, computes a rough
# "next run in Xs" hint, prints the last child task name + phase.
# ---------------------------------------------------------------------------
payoff_card_cron() {
  local child_task="$1"
  local schedule="${DEMO_CRON_SCHEDULE:-*/2 * * * *}"
  local phase="-"
  if [[ -n "${child_task}" ]] && command -v kubectl >/dev/null 2>&1; then
    phase="$(kubectl get task "${child_task}" -n "${DEMO_NAMESPACE}" \
              -o jsonpath='{.status.phase}' 2>/dev/null || printf 'Unknown')"
    [[ -z "${phase}" ]] && phase="Unknown"
  fi

  # Best-effort "next run" hint from a `*/N * * * *` schedule.
  local next_hint=""
  local minute_field
  minute_field="$(printf '%s' "${schedule}" | awk '{print $1}')"
  if [[ "${minute_field}" =~ ^\*/([0-9]+)$ ]]; then
    local interval="${BASH_REMATCH[1]}"
    local now_min secs_now next_min secs_to_next
    now_min="$(date +%M)"
    secs_now="$(date +%S)"
    # strip leading zeros
    now_min="$(( 10#${now_min} ))"
    secs_now="$(( 10#${secs_now} ))"
    next_min=$(( (now_min / interval + 1) * interval ))
    secs_to_next=$(( (next_min - now_min) * 60 - secs_now ))
    (( secs_to_next < 0 )) && secs_to_next=$(( interval * 60 + secs_to_next ))
    next_hint="${secs_to_next}s"
  fi

  __card_top "Scheduled task"
  __card_kv "schedule"  "${schedule}"
  if [[ -n "${next_hint}" ]]; then
    __card_kv "nextRun"  "in ${next_hint}"
  fi
  __card_kv "lastChild" "${child_task:-(none)}"
  __card_kv "phase"     "${phase}"
  __card_bottom
}

# ---------------------------------------------------------------------------
# payoff_card_kontxt <ok-task> <denied-job>
#
# Renders the kontxt "one identity, two outcomes" card.
# Reads the safe orka.ai/transaction-id annotation digest only — NEVER the
# raw Txn-Token. The denied side just reports the Job's failed status.
# ---------------------------------------------------------------------------
payoff_card_kontxt() {
  local ok_task="${1:-}"
  local denied_job="${2:-}"
  local ok_ns="${DEMO_NAMESPACE:-demo-magic}"
  local job_ns="${DEMO_KONTXT_NAMESPACE:-${ORKA_TOKEN_NAMESPACE:-default}}"

  local ok_txn="-"
  local ok_phase="-"
  if [[ -n "${ok_task}" ]] && command -v kubectl >/dev/null 2>&1; then
    ok_phase="$(kubectl get task "${ok_task}" -n "${ok_ns}" \
                 -o jsonpath='{.status.phase}' 2>/dev/null || printf 'Unknown')"
    ok_txn="$(kubectl get task "${ok_task}" -n "${ok_ns}" \
               -o jsonpath='{.metadata.annotations.orka\.ai/transaction-id}' \
               2>/dev/null || printf '')"
    [[ -z "${ok_txn}"  ]] && ok_txn="(no transaction-id annotation)"
    [[ -z "${ok_phase}" ]] && ok_phase="Unknown"
  fi

  local denied_status="-"
  if [[ -n "${denied_job}" ]] && command -v kubectl >/dev/null 2>&1; then
    denied_status="$(kubectl get job "${denied_job}" -n "${job_ns}" \
                      -o jsonpath='{.status.conditions[?(@.type=="Failed")].reason}' \
                      2>/dev/null || printf 'Unknown')"
    [[ -z "${denied_status}" ]] && denied_status="BackoffLimitExceeded"
  fi

  __card_top "One identity, two outcomes"
  __card_kv "ok task"   "${ok_task:-(none)}"
  __card_kv "ok phase"  "${ok_phase}"
  __card_kv "ok txn"    "${ok_txn}"
  __card_blank
  __card_kv "denied"    "${denied_job:-(none)}"
  __card_kv "denied"    "${denied_status}"
  __card_bottom
}

# ---------------------------------------------------------------------------
# payoff_card_sandbox <session> <turn1-task> <turn2-task> <turn3-task>
#
# Hard-asserts all three turns reattached the same workspace claim name by
# grepping worker pod logs for the literal line emitted by
# workers/common/agent_runtime.go:424:
#     Task <ns>/<name> completed in sandbox workspace <claim>
# Exits 1 with stderr message on mismatch.
# ---------------------------------------------------------------------------
payoff_card_sandbox() {
  local session="$1"
  local t1="$2"
  local t2="$3"
  local t3="$4"
  local ns="${DEMO_NAMESPACE:-demo-magic}"

  __sandbox_claim_for() {
    local task="$1"
    local pod
    pod="$(kubectl get pods -n "${ns}" -l "orka.ai/task=${task}" \
             -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
    if [[ -z "${pod}" ]]; then
      printf ''
      return 0
    fi
    kubectl logs -n "${ns}" "${pod}" --all-containers=true 2>/dev/null \
      | grep -Eo 'completed in sandbox workspace [a-z0-9-]+' \
      | tail -n 1 \
      | awk '{print $NF}'
  }

  local c1 c2 c3
  c1="$(__sandbox_claim_for "${t1}")"
  c2="$(__sandbox_claim_for "${t2}")"
  c3="$(__sandbox_claim_for "${t3}")"

  __card_top "Agent sandbox session"
  __card_kv "session" "${session}"
  __card_kv "turn 1"  "${t1} → ${c1:-(no claim)}"
  __card_kv "turn 2"  "${t2} → ${c2:-(no claim)}"
  __card_kv "turn 3"  "${t3} → ${c3:-(no claim)}"
  __card_bottom

  if [[ -z "${c1}" || -z "${c2}" || -z "${c3}" ]]; then
    log_error "payoff_card_sandbox: could not extract claim name from one or more turns"
    return 1
  fi
  if [[ "${c1}" != "${c2}" || "${c2}" != "${c3}" ]]; then
    log_error "payoff_card_sandbox: sandbox claim reuse FAILED — turns landed on different claims"
    return 1
  fi
  log_success "sandbox claim reused across all three turns: ${c1}"
}
