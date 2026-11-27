#!/usr/bin/env bash

if [[ "${__ORKA_DEMO_STYLE_LOADED:-0}" == "1" ]]; then
  return 0 2>/dev/null || true
fi
__ORKA_DEMO_STYLE_LOADED=1

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

demo_show_cmd() {
  local cmd="$*"
  printf '\n'
  if declare -F _demo_magic_render_prompt >/dev/null 2>&1; then
    _demo_magic_render_prompt
  else
    printf '%b' "${BOLD}${CYAN}> ${COLOR_RESET}"
  fi
  if command -v pv >/dev/null 2>&1 && [[ -n "${TYPE_SPEED:-}" && "${TYPE_SPEED}" != "0" ]] && demo_profile_is presenter; then
    printf '%s' "${cmd}" | pv -qL "${TYPE_SPEED}"
    printf '\n'
  else
    printf '%s\n' "${cmd}"
  fi
}

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
    docs)      head -n 20 "${path}"; printf '%b…%b\n' "${DIM}" "${COLOR_RESET}" ;;
    social)    head -n 8  "${path}"; printf '%b…%b\n' "${DIM}" "${COLOR_RESET}" ;;
    hero)      printf '%b…%b\n' "${DIM}" "${COLOR_RESET}" ;;
  esac
}

demo_show_full() {
  local path="$1"
  if [[ ! -f "${path}" ]]; then
    printf '%b[file not found: %s]%b\n' "${YELLOW}" "${path}" "${COLOR_RESET}"
    return 0
  fi
  cat "${path}"
}

banner() {
  local title="${1:-Orka demo}"
  local inner_width=66
  local label="🤖 Orka — ${title}"
  local visible_len
  visible_len=$(( ${#label} + 1 ))
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

  if [[ "${profile}" == "hero" ]]; then
    __DEMO_NARRATE_PENDING=""
    return 0
  fi
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

demo_scenario() {
  local title="${1:-Demo scenario}"
  shift || true
  local body="$*"
  local bar_width=68
  local bar
  bar="$(printf '━%.0s' $(seq 1 "${bar_width}"))"
  printf '\n%b%s%b\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  printf '%b🎬  Scenario — %s%b\n' "${BOLD}" "${title}" "${COLOR_RESET}"
  printf '%b%s%b\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
  if [[ -n "${body}" ]]; then
    if command -v fold >/dev/null 2>&1; then
      printf '%s\n' "${body}" | fold -s -w 72 | sed 's/^/    /'
    else
      printf '    %s\n' "${body}"
    fi
  fi
  printf '%b%s%b\n\n' "${CYAN}" "${bar}" "${COLOR_RESET}"
}

demo_event() {
  local emoji="${1:-▸}"
  shift || true
  printf '%b[%s]%b %s  %s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${emoji}" "$*"
}

demo_phase() {
  local emoji="${1:-▸}"
  local title="${2:-}"
  local desc="${3:-}"
  if demo_profile_is hero; then
    return 0
  fi
  if demo_profile_is social && (( ${__DEMO_CHAPTER_INDEX:-0} > 3 )); then
    return 0
  fi
  local bar_width=64
  local bar
  bar="$(printf '─%.0s' $(seq 1 "${bar_width}"))"
  printf '\n%b%s%b\n' "${DIM}" "${bar}" "${COLOR_RESET}"
  printf '%s  %b%s%b\n' "${emoji}" "${BOLD}" "${title}" "${COLOR_RESET}"
  if [[ -n "${desc}" ]]; then
    printf '   %s\n' "${desc}"
  fi
  printf '%b%s%b\n\n' "${DIM}" "${bar}" "${COLOR_RESET}"
}

demo_state() {
  local key="$1"
  shift || true
  printf '%b[%s]%b 🔄  %s%s%s\n' \
    "${DIM}" "$(__demo_log_ts)" "${COLOR_RESET}" \
    "${key}" \
    "${key:+: }" \
    "$*"
}

__DEMO_ANNOUNCED_KEYS=""
demo_announce_once() {
  local key="$1"
  local emoji="${2:-▸}"
  shift 2 || true
  case " ${__DEMO_ANNOUNCED_KEYS} " in
    *" ${key} "*) return 0 ;;
  esac
  if [[ -t 2 ]] && ! demo_profile_is hero; then
    printf '\r\033[2K' >&2
  fi
  demo_event "${emoji}" "$*"
  __DEMO_ANNOUNCED_KEYS="${__DEMO_ANNOUNCED_KEYS} ${key}"
}

demo_announce_reset() {
  if (( $# == 0 )); then
    __DEMO_ANNOUNCED_KEYS=""
    return 0
  fi
  local prefix="$1"
  local new="" k
  for k in ${__DEMO_ANNOUNCED_KEYS}; do
    case "${k}" in
      ${prefix}*) ;;
      *) new="${new} ${k}" ;;
    esac
  done
  __DEMO_ANNOUNCED_KEYS="${new}"
}

__demo_heartbeat() {
  local fmt="$1"
  shift || true
  if [[ "${DEMO_WAIT_QUIET:-0}" == "1" ]] || demo_profile_is hero; then
    return 0
  fi
  local body line
  body="$(printf "${fmt}" "$@")"
  line="$(printf '[%s] ⏳  %s' "$(__demo_log_ts)" "${body}")"
  if [[ -t 2 ]]; then
    printf '\r\033[2K%b%s%b' "${DIM}" "${line}" "${COLOR_RESET}" >&2
  else
    printf '%s\n' "${line}" >&2
  fi
}

__CARD_INNER=58
__CARD_BAR_WIDTH=60

__card_top() {
  local title="${1:-}"
  local bar_width="${__CARD_BAR_WIDTH}"
  local left="╭─"
  local right="─╮"
  if [[ -n "${title}" ]]; then
    local label=" ${title} "
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

__card_kv() {
  local key="$1"
  local val="$2"
  local key_field
  key_field="$(printf '%-12s' "${key}")"
  local combined="${key_field}${val}"
  __card_line "${combined}"
}

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

payoff_card_cron() {
  local child_task="$1"
  local schedule="${DEMO_CRON_SCHEDULE:-*/2 * * * *}"
  local phase="-"
  if [[ -n "${child_task}" ]] && command -v kubectl >/dev/null 2>&1; then
    phase="$(kubectl get task "${child_task}" -n "${DEMO_NAMESPACE}" \
              -o jsonpath='{.status.phase}' 2>/dev/null || printf 'Unknown')"
    [[ -z "${phase}" ]] && phase="Unknown"
  fi

  local next_hint=""
  local minute_field
  minute_field="$(printf '%s' "${schedule}" | awk '{print $1}')"
  if [[ "${minute_field}" =~ ^\*/([0-9]+)$ ]]; then
    local interval="${BASH_REMATCH[1]}"
    local now_min secs_now next_min secs_to_next
    now_min="$(date +%M)"
    secs_now="$(date +%S)"
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

payoff_card_substrate() {
  local cold_task="$1"
  local warm_task="$2"
  local pr_url="${3:-}"
  local ns="${DEMO_SUBSTRATE_NAMESPACE:-default}"

  __substrate_ws_field() {
    local task="$1"
    local field="$2"
    local val="-"
    if [[ -n "${task}" ]] && command -v kubectl >/dev/null 2>&1; then
      val="$(kubectl get task "${task}" -n "${ns}" \
        -o "jsonpath={.status.executionWorkspace.${field}}" 2>/dev/null || true)"
    fi
    [[ -z "${val}" ]] && val="-"
    printf '%s' "${val}"
  }

  local cold_provider cold_phase
  local warm_provider warm_phase warm_reused
  cold_provider="$(__substrate_ws_field "${cold_task}" provider)"
  cold_phase="$(__substrate_ws_field "${cold_task}" phase)"
  warm_provider="$(__substrate_ws_field "${warm_task}" provider)"
  warm_phase="$(__substrate_ws_field "${warm_task}" phase)"
  warm_reused="$(__substrate_ws_field "${warm_task}" reused)"

  __card_top "Agent Substrate - real agent, warm reuse"
  __card_blank
  __card_line "COLD (fresh gVisor workspace)"
  __card_kv "  task"      "${cold_task:-(none)}"
  __card_kv "  provider"  "${cold_provider}"
  __card_kv "  phase"     "${cold_phase}"
  __card_blank
  __card_line "WARM (sessionRef -> reattached)"
  __card_kv "  task"      "${warm_task:-(none)}"
  __card_kv "  provider"  "${warm_provider}"
  __card_kv "  phase"     "${warm_phase}"
  __card_kv "  reused"    "${warm_reused}"
  __card_blank
  __card_line "Pull request"
  __card_line "  ${pr_url:-(none)}"
  __card_blank
  __card_line "One Task API, gVisor isolation, warm reuse."
  __card_bottom

  if [[ "${cold_provider}" != "substrate" || "${warm_provider}" != "substrate" ]]; then
    log_error "payoff_card_substrate: expected provider=substrate on both tasks"
    return 1
  fi
  if [[ "${warm_reused}" != "true" ]]; then
    log_error "payoff_card_substrate: workspace reuse FAILED — warm task did not reattach (reused != true)"
    return 1
  fi
  if [[ -z "${pr_url}" ]]; then
    log_error "payoff_card_substrate: no pull request URL — the agentic run did not open a PR"
    return 1
  fi
  log_success "substrate: real agent opened ${pr_url}; warm task reattached the cold workspace (reused=true)"
}
