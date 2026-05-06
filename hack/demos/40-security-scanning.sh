#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"
# shellcheck source=hack/demos/lib/manifests.sh
. "${script_dir}/lib/manifests.sh"

require_demo_base
require_security_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env
require_orka_api_reachable

render_security_agents_manifest > "${DEMO_WORKDIR}/security-agents.yaml"
render_security_repository_scan_manifest > "${DEMO_WORKDIR}/security-repositoryscan.yaml"

log "Preparing repository scan before the presenter view; the first run can take a while"
kubectl apply -f "${DEMO_WORKDIR}/security-agents.yaml" >/dev/null
kubectl apply -f "${DEMO_WORKDIR}/security-repositoryscan.yaml" >/dev/null

if [[ -n "${DEMO_SECURITY_FINDING_ID:-}" ]]; then
  security_finding_id="${DEMO_SECURITY_FINDING_ID}"
else
  security_finding_id="$(first_security_finding_id)"
  if [[ -z "${security_finding_id}" ]]; then
    security_finding_id="$(wait_for_first_security_finding "${DEMO_SECURITY_FINDING_TIMEOUT:-1800}")" || die "no open findings were discovered for ${DEMO_SECURITY_SCAN_NAME}"
  fi
fi
log "Prepared security finding ${security_finding_id}"

clear

p "# Demo 40: Security Scanning"

p "Brief: start with an open repository finding, generate a patch branch, and end with a human-reviewable PR."

p "Show: apply the analysis and remediation Agents, then the RepositoryScan."
pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"

p "Inspect: the scan status and first open findings define the work item."
pe "orka_api GET \"/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}?namespace=${DEMO_NAMESPACE}\" | jq '{name: .metadata.name, phase: .status.phase, findings: .status.findingCounts}'"
pe "orka_api GET \"/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}/findings?namespace=${DEMO_NAMESPACE}&state=open&limit=5\" | jq '.items | map({id, severity, title, validationStatus, state})'"

p "Run: ask Orka to patch one finding."
pe "orka_api POST \"/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}\" | jq '{id, status, branch}'"

p "Follow: wait for the remediation task to produce a patch proposal."
pe "wait_for_patch_proposal_ready \"${security_finding_id}\" \"\${DEMO_SECURITY_PATCH_TIMEOUT:-1200}\""

p "Inspect: the patch record points back to the remediation task and branch."
pe "orka_api GET \"/api/v1/security/findings/${security_finding_id}/patches?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({id, status, branch, taskName})'"

p "Run: open the remediation PR for human review."
pe "wait_for_security_pull_request \"${security_finding_id}\" \"\${DEMO_SECURITY_PR_TIMEOUT:-180}\" | tee ${DEMO_WORKDIR}/security-pr.json | jq '{status, number: (.prNumber // .number), html_url: (.prURL // .html_url)}'"

p "Summary: the finding moved from scan result to patch proposal to PR."
pe "summarize_security_run \"${security_finding_id}\" \"${DEMO_WORKDIR}/security-pr.json\""
