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

# ---------------------------------------------------------------------------
# Stage-aware status hook — keeps viewers oriented during the multi-minute
# scan by narrating each stage transition persistently AND showing a
# rewriting heartbeat with the live stage counts.
#
# Stages (from internal/security and the orka.ai/security-stage label):
#   threat-model  — first task; produces the canonical repo threat model
#   discovery     — 5 parallel scope-focused tasks; use threat model as input
#   validation    — per-finding; suppresses false positives
#   patch         — per /patch request; opens branch + PR
# ---------------------------------------------------------------------------
_security_stage_status() {
  local scan_name="$1"
  local elapsed="$2"
  # One kubectl query → counts for all stages.
  local data
  data="$(kubectl -n "${DEMO_NAMESPACE}" get task \
            -l "orka.ai/security-target=${scan_name}" \
            -o custom-columns=STAGE:.metadata.labels.orka\\.ai/security-stage,PHASE:.status.phase \
            --no-headers 2>/dev/null || true)"
  local th_total th_done di_total di_done val_total val_done patch_total patch_done
  th_total=$(printf '%s\n'   "${data}" | awk '$1=="threat-model"{n++} END{print n+0}')
  th_done=$(printf '%s\n'    "${data}" | awk '$1=="threat-model" && $2=="Succeeded"{n++} END{print n+0}')
  di_total=$(printf '%s\n'   "${data}" | awk '$1=="discovery"{n++} END{print n+0}')
  di_done=$(printf '%s\n'    "${data}" | awk '$1=="discovery" && $2=="Succeeded"{n++} END{print n+0}')
  val_total=$(printf '%s\n'  "${data}" | awk '$1=="validation"{n++} END{print n+0}')
  val_done=$(printf '%s\n'   "${data}" | awk '$1=="validation" && $2=="Succeeded"{n++} END{print n+0}')
  patch_total=$(printf '%s\n' "${data}" | awk '$1=="patch"{n++} END{print n+0}')
  patch_done=$(printf '%s\n' "${data}" | awk '$1=="patch" && $2=="Succeeded"{n++} END{print n+0}')

  # Persistent stage-transition events (once each).
  (( th_total  >= 1 )) && demo_announce_once "scan-tm-started"  "🧠" "Threat-model task started — building canonical repo context (longest single stage)"
  (( th_done   >= 1 )) && demo_announce_once "scan-tm-done"     "✅" "Threat model complete — discovery agents will use it as canonical input"
  (( di_total  >= 1 )) && demo_announce_once "scan-disc-start"  "🔍" "Discovery fan-out: parallel scope-focused passes across auth, data exposure, supply chain, app logic, recent commits"
  (( di_total  >= 5 && di_done >= 5 )) && demo_announce_once "scan-disc-done" "✅" "Discovery complete — 5/5 scopes finished, validators will rank findings"
  (( val_total >= 1 )) && demo_announce_once "scan-val-start"  "🧪" "Per-finding validators started — suppressing false positives before surfacing in the API"
  (( patch_total >= 1 )) && demo_announce_once "scan-patch-start" "🛠️ " "Remediation Task started — drafting the fix on a new branch"
  (( patch_done  >= 1 )) && demo_announce_once "scan-patch-done"  "✅" "Patch complete — branch + PR opened against ${DEMO_SECURITY_GIT_REPO}"

  # Live heartbeat with counts.
  __demo_heartbeat 'scan/%s stages: threat-model %d/%d • discovery %d/%d • validation %d/%d • patch %d/%d • elapsed %ss' \
    "${scan_name}" "${th_done}" "${th_total:-1}" "${di_done}" "${di_total:-0}" \
    "${val_done}" "${val_total:-0}" "${patch_done}" "${patch_total:-0}" "${elapsed}"
}

DEMO_WAIT_STATUS_HOOK=_security_stage_status

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
narrate "Behind the scenes, the scan ran a 4-stage pipeline: first one threat-model task analyzed the repo (what does this app DO, where are the trust boundaries), then 5 parallel discovery passes — each handed that same threat model as canonical context — scanned scope by scope (auth, data exposure, supply chain, app logic, recent commits), then per-finding validators suppressed false positives. The findings you're about to see are ranked using the repo's own threat model, not just a generic linter ruleset."
chapter "Apply the scan + remediation Agents" "🔍"
demo_event "📥" "Applying analysis + remediation Agents and the RepositoryScan CR — three Kubernetes objects, no other infrastructure."
log_info "Target: ${DEMO_SECURITY_GIT_REPO} (${DEMO_SECURITY_GIT_BRANCH})"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
demo_pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"
demo_event "📊" "RepositoryScan is the control object. Phase transitions: Pending → Scanning → Ready. findingCounts is updated as discovery + validation complete."
demo_pe "kubectl get repositoryscan ${DEMO_SECURITY_SCAN_NAME} -n ${DEMO_NAMESPACE}"

# Chapter 3 ------------------------------------------------------------------
narrate "Findings are severity-ranked; the top one becomes the work item."
chapter "Inspect the open findings" "📋"
demo_event "🔎" "Each Finding is a first-class object: severity, ID, title, source location, and the threat-model context that justified its ranking."
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

# Chapter 4 ------------------------------------------------------------------
narrate "One POST asks Orka to patch the finding — a remediation Task is born."
chapter "Request a patch" "🛠️"
demo_event "📮" "POST /api/v1/security/findings/<id>/patch — this single REST call creates a Task object whose owner reference points back to the Finding."
log_info "Requesting patch for finding ${security_finding_id}..."
demo_pe "orka_api POST \"/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}\" | jq '{id, status, branch}'"
demo_event "🤖" "The remediation Agent now runs: read source, draft minimal fix, run tests, open branch, open PR. Provenance is preserved end-to-end."

# Chapter 5 ------------------------------------------------------------------
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
demo_event "✅" "Patch proposal is ready — the remediation Task wrote a branch with the fix."
log_info "Patch proposals for ${security_finding_id}:"
demo_pe "orka_api GET \"/api/v1/security/findings/${security_finding_id}/patches?namespace=${DEMO_NAMESPACE}\" | jq -r '.items[] | \"\\(.status)\\t\\(.branch)\\t\\(.taskName)\"' | column -t -s\$'\\t'"

# Chapter 6 ------------------------------------------------------------------
narrate "The patch becomes a real branch and a real PR for human review."
chapter "Open the remediation pull request" "🚢"
demo_event "🚀" "The branch is pushed and a PR is opened automatically. From here it's a normal code review — humans approve, GitHub merges."
log_info "Waiting for pull request to open (timeout ${DEMO_SECURITY_PR_TIMEOUT:-180}s)..."
wait_for_security_pull_request "${security_finding_id}" "${DEMO_SECURITY_PR_TIMEOUT:-180}" \
  > "${DEMO_WORKDIR}/security-pr.json" \
  || die "pull request did not open for finding ${security_finding_id}"
demo_event "🎉" "Pull request opened. The Finding object now has the PR URL stamped on its status — full audit chain from scan → finding → patch → branch → PR."

# Chapter 7 ------------------------------------------------------------------
narrate "Scan → finding → patch → branch → PR — every step replayable from the API."
chapter "Remediation handoff" "✅"
payoff_card_security "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"

if demo_profile_is presenter; then
  printf '\n%bAudit JSON%b\n' "${DIM}" "${COLOR_RESET}"
  summarize_security_run "${security_finding_id}" "${DEMO_WORKDIR}/security-pr.json"
fi
