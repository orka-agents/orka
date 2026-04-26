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

render_security_agents_manifest > "${DEMO_WORKDIR}/security-agents.yaml"
render_security_repository_scan_manifest > "${DEMO_WORKDIR}/security-repositoryscan.yaml"

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

clear

p "# Security scanning"
pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}?namespace=${DEMO_NAMESPACE}\" | jq '{name: .metadata.name, phase: .status.phase, findings: .status.findingCounts}'"
pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/security/repositories/${DEMO_SECURITY_SCAN_NAME}/findings?namespace=${DEMO_NAMESPACE}&state=open&limit=5\" | jq '.items | map({id, severity, title, validationStatus, state})'"
pe "curl -fsS -X POST -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/security/findings/${security_finding_id}/patch?namespace=${DEMO_NAMESPACE}\" | jq '{id, status, branch}'"

wait_for_patch_proposal_ready "${security_finding_id}" "${DEMO_SECURITY_PATCH_TIMEOUT:-1200}" || die "patch proposal failed for finding ${security_finding_id}"

pe "curl -fsS -H \"Authorization: Bearer \$ORKA_TOKEN\" \"\$ORKA_SERVER/api/v1/security/findings/${security_finding_id}/patches?namespace=${DEMO_NAMESPACE}\" | jq '.items | map({id, status, branch, taskName})'"
pe "wait_for_security_pull_request \"${security_finding_id}\" \"\${DEMO_SECURITY_PR_TIMEOUT:-180}\" | jq '{status, number: (.prNumber // .number), html_url: (.prURL // .html_url)}'"
