#!/usr/bin/env bash
# Demo 40 — Security Scanning
#
# A RepositoryScan finds a real vulnerability in a real (forked) app, Orka
# ranks the findings, requests a patch for the top one, the patch becomes
# a branch, and the branch becomes a human-reviewable pull request.
#
# Pacing is controlled by DEMO_RECORD_PROFILE (presenter|docs|social|hero).

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

# ---------------------------------------------------------------------------
# Setup (silent — uses legacy log() so the pre-warm phase doesn't bleed
# into the cast).
# ---------------------------------------------------------------------------
require_demo_base
require_security_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

render_security_agents_manifest          > "${DEMO_WORKDIR}/security-agents.yaml"
render_security_repository_scan_manifest > "${DEMO_WORKDIR}/security-repositoryscan.yaml"
render_security_story_file               > "${DEMO_WORKDIR}/security-story.txt"

log "Applying analysis + remediation agents and the RepositoryScan"
kubectl apply -f "${DEMO_WORKDIR}/security-agents.yaml"          >/dev/null
kubectl apply -f "${DEMO_WORKDIR}/security-repositoryscan.yaml"  >/dev/null

log "Discovering the top-ranked open finding (this can take a few minutes on the first run)"
if [[ -n "${DEMO_SECURITY_FINDING_ID:-}" ]]; then
  security_finding_id="${DEMO_SECURITY_FINDING_ID}"
else
  security_finding_id="$(first_security_finding_id || true)"
  if [[ -z "${security_finding_id}" ]]; then
    security_finding_id="$(wait_for_first_security_finding "${DEMO_SECURITY_FINDING_TIMEOUT:-1800}")" \
      || die "no open findings were discovered for ${DEMO_SECURITY_SCAN_NAME}"
  fi
fi
log "Top-ranked finding: ${security_finding_id}"

# ---------------------------------------------------------------------------
# Narrated walkthrough.
# ---------------------------------------------------------------------------
DEMO_CHAPTER_TOTAL=7
clear
banner "Security Scanning"

# Chapter 1 ------------------------------------------------------------------
narrate "Security findings become first-class Kubernetes objects. One POST turns a finding into a real PR — scan -> finding -> patch -> branch -> PR, all in K8s."
chapter "What this demo is doing" "🧑"
demo_show_full "${DEMO_WORKDIR}/security-story.txt"

# Chapter 2 ------------------------------------------------------------------
narrate "Behind the scenes, the scan ran a 4-stage pipeline: 5 parallel discovery passes (auth, data exposure, supply chain, app logic, recent commits) → one threat-model synthesis that gives the repo context → per-finding validators that suppress false positives. The findings you're about to see are ranked by REAL risk, not by line count."
chapter "Apply the scan + remediation Agents" "🔍"
log_info "Target: ${DEMO_SECURITY_GIT_REPO} (${DEMO_SECURITY_GIT_BRANCH})"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"
demo_pe "kubectl get repositoryscan ${DEMO_SECURITY_SCAN_NAME} -n ${DEMO_NAMESPACE}"

# Chapter 2 ------------------------------------------------------------------
narrate "Findings are severity-ranked; the top one becomes the work item."
chapter "Inspect the open findings" "📋"
log_info "Top 5 open findings ranked by severity:"
demo_pe "orka_api GET \"/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}/findings?namespace=${DEMO_NAMESPACE}&state=open&limit=5\" | jq -r '.items[] | \"\\(.severity)\\t\\(.id)\\t\\(.title)\"' | column -t -s\$'\\t'"
# Print the selected finding's row by itself so viewers can match the
# `selected finding:` ID against a real row even if the scan added new
# findings between selection time and now. Failure to look it up is
# non-fatal — the patch flow still uses ${security_finding_id} either way.
selected_row="$(orka_api GET "/api/v1/security/findings/${security_finding_id}?namespace=${DEMO_NAMESPACE}" 2>/dev/null \
  | jq -r '[.severity, .id, .title] | @tsv' 2>/dev/null || true)"
if [[ -n "${selected_row}" ]]; then
  log_info "Selected for remediation:"
  printf '%s\n' "${selected_row}" | column -t -s$'\t'
fi
log_success "selected finding: ${security_finding_id}"

# Chapter 3 ------------------------------------------------------------------
narrate "One POST asks Orka to patch the finding — a remediation Task is born."
chapter "Request a patch" "🛠️"
log_info "Requesting patch for finding ${security_finding_id}..."
demo_pe "orka_api POST \"/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}\" | jq '{id, status, branch}'"

# Chapter 4 ------------------------------------------------------------------
narrate "Remediation runs; we wait for the patch proposal to land."
chapter "Wait for the patch proposal" "⏳"
log_info "Waiting for patch proposal (timeout ${DEMO_SECURITY_PATCH_TIMEOUT:-1200}s)..."
# Retry on transient failures (e.g. upstream LLM 429). Each attempt POSTs
# a fresh patch request; the wait helper polls until the latest patch
# row reaches ready or failed.
patch_attempts=0
max_attempts="${DEMO_SECURITY_PATCH_MAX_ATTEMPTS:-3}"
while (( patch_attempts < max_attempts )); do
  if wait_for_patch_proposal_ready "${security_finding_id}" "${DEMO_SECURITY_PATCH_TIMEOUT:-1200}"; then
    break
  fi
  patch_attempts=$(( patch_attempts + 1 ))
  if (( patch_attempts >= max_attempts )); then
    die "patch proposal did not become ready for finding ${security_finding_id} after ${max_attempts} attempts"
  fi
  log_warning "patch attempt ${patch_attempts} did not finish ready (often upstream LLM 429); retrying in 60s"
  sleep 60
  orka_api POST "/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}" >/dev/null 2>&1 || true
done
log_success "patch proposal ready"
log_info "Patch proposals for ${security_finding_id}:"
demo_pe "orka_api GET \"/api/v1/security/findings/${security_finding_id}/patches?namespace=${DEMO_NAMESPACE}\" | jq -r '.items[] | \"\\(.status)\\t\\(.branch)\\t\\(.taskName)\"' | column -t -s\$'\\t'"

# Chapter 5 ------------------------------------------------------------------
narrate "The patch becomes a real branch and a real PR for human review."
chapter "Open the remediation pull request" "🚢"
log_info "Waiting for pull request to open (timeout ${DEMO_SECURITY_PR_TIMEOUT:-180}s)..."
wait_for_security_pull_request "${security_finding_id}" "${DEMO_SECURITY_PR_TIMEOUT:-180}" \
  > "${DEMO_WORKDIR}/security-pr.json" \
  || die "pull request did not open for finding ${security_finding_id}"
log_success "pull request opened"

# Chapter 6 ------------------------------------------------------------------
narrate "Scan → finding → patch → branch → PR — every step replayable from the API."
chapter "Remediation handoff" "✅"
payoff_card_security "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_security_run "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"
fi
