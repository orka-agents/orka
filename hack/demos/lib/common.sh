#!/usr/bin/env bash

set -Eeuo pipefail

demo_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
demo_dir="$(cd "${demo_lib_dir}/.." && pwd)"
repo_root="$(cd "${demo_dir}/../.." && pwd)"
default_orka_bin="${repo_root}/bin/orka"
if [[ ! -x "${default_orka_bin}" && -x "${repo_root}/../orka/bin/orka" ]]; then
  default_orka_bin="${repo_root}/../orka/bin/orka"
fi

: "${ORKA_NAMESPACE:=orka-system}"
: "${DEMO_NAMESPACE:=demo-magic}"
: "${ORKA_TOKEN_NAMESPACE:=${DEMO_NAMESPACE}}"
: "${ORKA_API_BASE:=http://127.0.0.1:8080}"
: "${ORKA_BIN:=${default_orka_bin}}"
: "${ORKA_TOKEN_COMMAND:=kubectl create token orka-client -n ${ORKA_TOKEN_NAMESPACE}}"
: "${DEMO_WORKDIR:=/tmp/orka-demo}"

: "${DEMO_LABEL_VALUE:=demo-magic}"

: "${DEMO_AGENT_CPU_REQUEST:=100m}"
: "${DEMO_AGENT_CPU_LIMIT:=1}"
: "${DEMO_AGENT_MEMORY_REQUEST:=256Mi}"
: "${DEMO_AGENT_MEMORY_LIMIT:=1Gi}"

: "${DEMO_PR_COORDINATOR_NAME:=demo-pr-coordinator}"
: "${DEMO_CODER_AGENT_NAME:=demo-coder}"
: "${DEMO_SECURITY_REVIEWER_NAME:=demo-reviewer-security}"
: "${DEMO_QUALITY_REVIEWER_NAME:=demo-reviewer-quality}"

: "${DEMO_RUN_ID:=$(date +%Y%m%d%H%M%S)}"

: "${DEMO_MANUAL_TASK_NAME:=demo-manual-pr}"
: "${DEMO_CHAT_SESSION_PREFIX:=demo-magic-chat-pr-}"
: "${DEMO_CHAT_SESSION:=${DEMO_CHAT_SESSION_PREFIX}${DEMO_RUN_ID}}"
: "${DEMO_CHAT_PUSH_BRANCH:=demo/chat-pr-${DEMO_RUN_ID}}"
: "${DEMO_MANUAL_PUSH_BRANCH:=demo/manual-workflow-${DEMO_RUN_ID}}"

: "${DEMO_CRON_AGENT_NAME:=demo-cron-reporter}"
: "${DEMO_CRON_TASK_NAME:=demo-cron-report}"
: "${DEMO_CRON_SCHEDULE:=*/2 * * * *}"

: "${DEMO_SECURITY_ANALYSIS_AGENT_NAME:=demo-security-analysis}"
: "${DEMO_SECURITY_PATCH_AGENT_NAME:=demo-security-patch}"
: "${DEMO_SECURITY_SCAN_PREFIX:=demo-security-repository}"
: "${DEMO_SECURITY_SCAN_NAME:=${DEMO_SECURITY_SCAN_PREFIX}-${DEMO_RUN_ID}}"
: "${DEMO_SECURITY_SCHEDULE:=}"

: "${DEMO_SECURITY_GIT_REPO:=https://github.com/sozercan/actions-test.git}"
: "${DEMO_SECURITY_GIT_BRANCH:=demo/security-python-command-injection}"
: "${DEMO_SECURITY_GIT_SECRET_REF:=${DEMO_GIT_SECRET_REF:-}}"
: "${DEMO_SECURITY_GIT_FORK_REPO:=${DEMO_GIT_FORK_REPO:-}}"
: "${DEMO_SECURITY_PR_BASE_BRANCH:=${DEMO_SECURITY_GIT_BRANCH}}"
: "${DEMO_SECURITY_GIT_SUB_PATH:=${DEMO_GIT_SUB_PATH:-}}"

: "${DEMO_CHAT_REQUEST:=In README.md, add a short CONTRIBUTING.md link sentence if one is not already present, then immediately after that sentence add a short subsection titled \"Maintainer Demo Workflow Examples\". Keep it to one intro sentence, four short bullets, and one guardrail sentence. The intro sentence must explicitly say these are demo workflow examples for maintainers, not end-user product features. The bullets must be exactly: chat orchestration, parallel review, scheduled workflows, and security scanning. The guardrail sentence must say demos must use placeholder or sample data only, never real secrets, tokens, or cookies, and require human approval for privileged actions. Keep the diff documentation-only and easy to review.}"
: "${DEMO_MANUAL_REQUEST:=In CONTRIBUTING.md, add a short subsection titled \"Maintainer Demo Checklist\" near the contribution workflow guidance. Keep it to four one-line checklist bullets covering: use test-only credentials, work on a demo branch, inspect the diff before push, and open a PR only after validation. Add one closing sentence that says demo workflows are for maintainers, not end-user product features. Keep the diff documentation-only and easy to review.}"
: "${DEMO_CRON_REQUEST:=Produce a short repository heartbeat report with the current HEAD commit, a count of Markdown files, and a brief summary of the repo purpose. Do not modify files, commit, or push.}"

ORKA_TOKEN_CACHE=""

log() {
  printf '==> %s\n' "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

require_file() {
  [[ -f "$1" ]] || die "missing required file: $1"
}

require_exec() {
  [[ -x "$1" ]] || die "missing executable: $1"
}

require_vars() {
  local name
  for name in "$@"; do
    [[ -n "${!name:-}" ]] || die "missing required environment variable: $name"
  done
}

resolve_demo_magic_path() {
  if [[ -n "${DEMO_MAGIC_PATH:-}" ]]; then
    printf '%s\n' "${DEMO_MAGIC_PATH}"
    return 0
  fi

  local candidate
  for candidate in \
    "${repo_root}/bin/demo-magic.sh" \
    "${repo_root}/demo-magic.sh" \
    "${HOME}/demo-magic.sh" \
    "${HOME}/demo-magic/demo-magic.sh" \
    "${HOME}/src/demo-magic/demo-magic.sh"; do
    if [[ -f "${candidate}" ]]; then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done

  return 1
}

source_demo_magic() {
  local path
  local args=("$@")
  local restore_nounset=0
  local has_pv=1
  path="$(resolve_demo_magic_path)" || die "demo-magic.sh not found; set DEMO_MAGIC_PATH=/path/to/demo-magic.sh"
  if ! command -v pv >/dev/null 2>&1; then
    has_pv=0
    if ((${#args[@]})); then
      args=("-d" "${args[@]}")
    else
      args=("-d")
    fi
  fi
  if [[ $- == *u* ]]; then
    restore_nounset=1
    set +u
  fi
  # shellcheck disable=SC1090
  . "${path}" "${args[@]}"
  if [[ ! -t 0 || ! -t 1 ]]; then
    run_cmd() {
      eval "$@"
    }
  fi
  if (( ! has_pv )); then
    TYPE_SPEED=""
  fi
  if (( restore_nounset )); then
    set -u
  fi
}

configure_demo_magic() {
  if command -v pv >/dev/null 2>&1; then
    TYPE_SPEED="${TYPE_SPEED:-40}"
  else
    TYPE_SPEED=""
  fi
  DEMO_PROMPT="${DEMO_PROMPT:-${GREEN}orka-demo ${CYAN}\W ${COLOR_RESET}}"
  PROMPT_TIMEOUT="${PROMPT_TIMEOUT:-0}"
}

ensure_demo_workdir() {
  mkdir -p "${DEMO_WORKDIR}"
}

prepare_api_env() {
  export ORKA_SERVER="${ORKA_API_BASE}"
  export ORKA_TOKEN="$(get_orka_token)"
}

get_orka_token() {
  if [[ -n "${ORKA_TOKEN:-}" ]]; then
    printf '%s' "${ORKA_TOKEN}"
    return 0
  fi
  if [[ -z "${ORKA_TOKEN_CACHE}" ]]; then
    ORKA_TOKEN_CACHE="$(eval "${ORKA_TOKEN_COMMAND}")"
  fi
  printf '%s' "${ORKA_TOKEN_CACHE}"
}

orka_cli() {
  local args=("${ORKA_BIN}" --server "${ORKA_API_BASE}" --namespace "${DEMO_NAMESPACE}")
  if [[ -n "${ORKA_TOKEN:-}" ]]; then
    args+=(--token "${ORKA_TOKEN}")
  fi
  "${args[@]}" "$@"
}

orka_api() {
  local method="$1"
  local path="$2"
  local body_file="${3:-}"
  local token
  token="$(get_orka_token)"

  local args=(
    -fsS
    -X "${method}"
    -H "Authorization: Bearer ${token}"
    -H "Accept: application/json"
  )
  if [[ -n "${body_file}" ]]; then
    args+=(-H "Content-Type: application/json" --data-binary "@${body_file}")
  fi

  curl "${args[@]}" "${ORKA_API_BASE}${path}"
}

demo_label_selector() {
  printf 'demo.orka.ai/name=%s' "${DEMO_LABEL_VALUE}"
}

task_exists() {
  kubectl get task "$1" -n "${DEMO_NAMESPACE}" >/dev/null 2>&1
}

agent_exists() {
  kubectl get agent "$1" -n "${DEMO_NAMESPACE}" >/dev/null 2>&1
}

repository_scan_exists() {
  kubectl get repositoryscan "$1" -n "${DEMO_NAMESPACE}" >/dev/null 2>&1
}

delete_task_if_exists() {
  kubectl delete task "$1" -n "${DEMO_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
}

delete_agent_if_exists() {
  kubectl delete agent "$1" -n "${DEMO_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
}

delete_repository_scan_if_exists() {
  kubectl delete repositoryscan "$1" -n "${DEMO_NAMESPACE}" --ignore-not-found >/dev/null 2>&1 || true
}

delete_repository_scans_by_name_prefix() {
  local prefix="$1"
  local names
  names="$(
    kubectl get repositoryscans -n "${DEMO_NAMESPACE}" -o json 2>/dev/null \
      | jq -r --arg prefix "${prefix}" '.items[] | select(.metadata.name | startswith($prefix)) | .metadata.name'
  )"
  if [[ -n "${names}" ]]; then
    while IFS= read -r name; do
      [[ -n "${name}" ]] || continue
      kubectl delete repositoryscan "${name}" -n "${DEMO_NAMESPACE}" >/dev/null 2>&1 || true
    done <<< "${names}"
  fi
}

delete_tasks_by_selector() {
  local selector="$1"
  local names
  names="$(kubectl get tasks -n "${DEMO_NAMESPACE}" -l "${selector}" -o name 2>/dev/null || true)"
  if [[ -n "${names}" ]]; then
    kubectl delete -n "${DEMO_NAMESPACE}" ${names} >/dev/null 2>&1 || true
  fi
}

delete_tasks_by_name_prefix() {
  local prefix="$1"
  local names
  names="$(
    kubectl get tasks -n "${DEMO_NAMESPACE}" -o json 2>/dev/null \
      | jq -r --arg prefix "${prefix}" '.items[] | select(.metadata.name | startswith($prefix)) | .metadata.name'
  )"
  if [[ -n "${names}" ]]; then
    while IFS= read -r name; do
      [[ -n "${name}" ]] || continue
      kubectl delete task "${name}" -n "${DEMO_NAMESPACE}" >/dev/null 2>&1 || true
    done <<< "${names}"
  fi
}

latest_task_by_selector() {
  local selector="$1"
  kubectl get tasks -n "${DEMO_NAMESPACE}" -l "${selector}" -o json 2>/dev/null \
    | jq -r '.items | sort_by(.metadata.creationTimestamp) | last? | .metadata.name // empty'
}

latest_chat_parent_task() {
  latest_task_by_selector "orka.ai/chat-session=${DEMO_CHAT_SESSION}"
}

wait_for_chat_parent_task() {
  local timeout_seconds="${1:-120}"
  local deadline parent
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    parent="$(latest_chat_parent_task)"
    if [[ -n "${parent}" ]]; then
      printf '%s\n' "${parent}"
      return 0
    fi
    sleep 2
  done

  return 1
}

latest_scheduled_child_task() {
  latest_task_by_selector "orka.ai/parent-task=${DEMO_CRON_TASK_NAME},orka.ai/scheduled-run=true"
}

wait_for_task_terminal() {
  local task_name="$1"
  local timeout_seconds="${2:-900}"
  local deadline phase
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    phase="$(kubectl get task "${task_name}" -n "${DEMO_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "${phase}" in
      Succeeded|Failed|Cancelled)
        printf '%s\n' "${phase}"
        return 0
        ;;
    esac
    sleep 5
  done

  return 1
}

wait_for_first_scheduled_child() {
  local timeout_seconds="${1:-180}"
  local deadline child
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    child="$(latest_scheduled_child_task)"
    if [[ -n "${child}" ]]; then
      printf '%s\n' "${child}"
      return 0
    fi
    sleep 5
  done

  return 1
}

follow_task_logs_when_ready() {
  local task_name="$1"
  local timeout_seconds="${2:-180}"
  local deadline job_name pod_phase status
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    job_name="$(kubectl get task "${task_name}" -n "${DEMO_NAMESPACE}" -o jsonpath='{.status.jobName}' 2>/dev/null || true)"
    status="$(kubectl get task "${task_name}" -n "${DEMO_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"

    if [[ -n "${job_name}" ]]; then
      pod_phase="$(kubectl get pods -n "${DEMO_NAMESPACE}" -l "job-name=${job_name}" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || true)"
      case "${pod_phase}" in
        Running|Succeeded|Failed)
          orka_cli task logs -f "${task_name}"
          return $?
          ;;
      esac
    fi

    if [[ "${status}" == "Failed" || "${status}" == "Cancelled" ]]; then
      orka_cli task logs "${task_name}" >/dev/null 2>&1 || true
      return 1
    fi

    sleep 2
  done

  printf '%s\n' "timed out waiting to stream logs for task ${task_name}" >&2
  return 1
}

wait_for_task_result_available() {
  local task_name="$1"
  local timeout_seconds="${2:-120}"
  local deadline
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    if orka_api GET "/api/v1/tasks/${task_name}/result?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done

  return 1
}

wait_for_repository_scan_ready() {
  local scan_name="$1"
  local timeout_seconds="${2:-1800}"
  local deadline phase
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    phase="$(kubectl get repositoryscan "${scan_name}" -n "${DEMO_NAMESPACE}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "${phase}" in
      Ready)
        return 0
        ;;
      Error)
        return 1
        ;;
    esac
    sleep 10
  done

  return 1
}

first_security_finding_id() {
  orka_api GET "/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}/findings?namespace=${DEMO_NAMESPACE}&state=open&limit=100" \
    | jq -r '
        def severity_rank:
          (. // "" | ascii_downcase) as $severity
          | if $severity == "critical" then 4
            elif $severity == "high" then 3
            elif $severity == "medium" then 2
            elif $severity == "low" then 1
            else 0
            end;
        (.items // [])
        | sort_by([(.severity | severity_rank), .id])
        | (last? | .id) // empty
      '
}

wait_for_first_security_finding() {
  local timeout_seconds="${1:-1800}"
  local deadline finding_id
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    finding_id="$(first_security_finding_id)"
    if [[ -n "${finding_id}" ]]; then
      printf '%s\n' "${finding_id}"
      return 0
    fi
    sleep 10
  done

  return 1
}

wait_for_patch_proposal_ready() {
  local finding_id="$1"
  local timeout_seconds="${2:-1200}"
  local deadline status
  deadline=$((SECONDS + timeout_seconds))

  while (( SECONDS < deadline )); do
    status="$(orka_api GET "/api/v1/security/findings/${finding_id}/patches?namespace=${DEMO_NAMESPACE}" \
      | jq -r '.items[0].status // empty')"
    case "${status}" in
      succeeded|pr_opened)
        return 0
        ;;
      failed)
        return 1
        ;;
    esac
    sleep 10
  done

  return 1
}

wait_for_security_pull_request() {
  local finding_id="$1"
  local timeout_seconds="${2:-300}"
  local deadline http_code token url body_file
  deadline=$((SECONDS + timeout_seconds))
  token="$(get_orka_token)"
  url="${ORKA_API_BASE}/api/v1/security/findings/${finding_id}/pull-request?namespace=${DEMO_NAMESPACE}"
  body_file="$(mktemp)"
  trap 'rm -f "${body_file}"' RETURN

  while (( SECONDS < deadline )); do
    http_code="$(
      curl -sS \
        -o "${body_file}" \
        -w '%{http_code}' \
        -X POST \
        -H "Authorization: Bearer ${token}" \
        -H "Accept: application/json" \
        "${url}" \
        || true
    )"

    case "${http_code}" in
      200|201)
        cat "${body_file}"
        return 0
        ;;
      409|422|500)
        ;;
      *)
        if [[ -s "${body_file}" ]]; then
          cat "${body_file}" >&2
        fi
        return 1
        ;;
    esac

    sleep 5
  done

  if [[ -s "${body_file}" ]]; then
    cat "${body_file}" >&2
  fi
  return 1
}

require_demo_base() {
  require_cmd kubectl
  require_cmd curl
  require_cmd jq
  require_exec "${ORKA_BIN}"
}

require_pr_demo_env() {
  require_vars \
    DEMO_PROVIDER_REF \
    DEMO_AI_MODEL \
    DEMO_RUNTIME_TYPE \
    DEMO_RUNTIME_MODEL \
    DEMO_RUNTIME_SECRET_REF \
    DEMO_GIT_REPO \
    DEMO_GIT_BRANCH \
    DEMO_GIT_SECRET_REF
}

require_cron_demo_env() {
  require_vars \
    DEMO_RUNTIME_TYPE \
    DEMO_RUNTIME_MODEL \
    DEMO_RUNTIME_SECRET_REF \
    DEMO_GIT_REPO \
    DEMO_GIT_BRANCH
}

require_security_demo_env() {
  require_vars \
    DEMO_RUNTIME_TYPE \
    DEMO_RUNTIME_MODEL \
    DEMO_RUNTIME_SECRET_REF \
    DEMO_SECURITY_GIT_REPO \
    DEMO_SECURITY_GIT_BRANCH \
    DEMO_SECURITY_GIT_SECRET_REF
}

open_url() {
  local url="$1"
  if command -v open >/dev/null 2>&1; then
    open "${url}" >/dev/null 2>&1 || true
    return 0
  fi
  if command -v xdg-open >/dev/null 2>&1; then
    xdg-open "${url}" >/dev/null 2>&1 || true
    return 0
  fi
  return 1
}
