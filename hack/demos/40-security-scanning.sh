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
DEMO_CHAPTER_TOTAL=6
clear
banner "Security Scanning"

# Chapter 1 ------------------------------------------------------------------
narrate "A RepositoryScan finds real CVEs in a real app — pre-warmed off-camera."
chapter "Apply the scan + remediation Agents" "🔍"
log_info "Target: ${DEMO_SECURITY_GIT_REPO} (${DEMO_SECURITY_GIT_BRANCH})"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"

# Chapter 2 ------------------------------------------------------------------
narrate "Findings are severity-ranked; the top one becomes the work item."
chapter "Inspect the open findings" "📋"
demo_pe "orka_api GET \"/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}?namespace=${DEMO_NAMESPACE}\" | jq '{name: .metadata.name, phase: .status.phase, findings: .status.findingCounts}'"
demo_pe "orka_api GET \"/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}/findings?namespace=${DEMO_NAMESPACE}&state=open&limit=5\" | jq '.items | map({id, severity, title, validationStatus, state})'"
log_success "selected finding: ${security_finding_id}"

# Chapter 3 ------------------------------------------------------------------
narrate "One POST asks Orka to patch the finding — a remediation Task is born."
chapter "Request a patch" "🛠️"
demo_pe "orka_api POST \"/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}\" | jq '{id, status, branch}'"

# Chapter 4 ------------------------------------------------------------------
narrate "Remediation runs; we wait silently for the patch proposal to land."
chapter "Wait for the patch proposal" "⏳"
log_info "Waiting for patch proposal (timeout ${DEMO_SECURITY_PATCH_TIMEOUT:-1200}s)..."
wait_for_patch_proposal_ready "${security_finding_id}" "${DEMO_SECURITY_PATCH_TIMEOUT:-1200}" \
  || die "patch proposal did not become ready for finding ${security_finding_id}"
log_success "patch proposal ready"
demo_pe "orka_api GET \"/api/v1/security/findings/${security_finding_id}/patches?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({id, status, branch, taskName})'"

# Chapter 5 ------------------------------------------------------------------
narrate "The patch becomes a real branch and a real PR for human review."
chapter "Open the remediation pull request" "🚢"
log_info "Waiting for pull request to open (timeout ${DEMO_SECURITY_PR_TIMEOUT:-180}s)..."
wait_for_security_pull_request "${security_finding_id}" "${DEMO_SECURITY_PR_TIMEOUT:-180}" \
  > "${DEMO_WORKDIR}/security-pr.json" \
  || die "pull request did not open for finding ${security_finding_id}"
demo_pe "jq '{status, number: (.prNumber // .number), html_url: (.prURL // .html_url)}' ${DEMO_WORKDIR}/security-pr.json"

# Chapter 6 ------------------------------------------------------------------
narrate "Scan → finding → patch → branch → PR — every step replayable from the API."
chapter "Remediation handoff" "✅"
payoff_card_security "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_security_run "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"
fi
