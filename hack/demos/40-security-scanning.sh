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

clear

p "# Demo 40: Security Scanning"
p "Brief: start with an open repository finding, generate a patch branch, end with a human-reviewable PR."

p "Tip: re-running this demo against the same DEMO_SECURITY_SCAN_NAME reuses cached findings."
p "    Set DEMO_SECURITY_FINDING_ID=<id> to skip directly to a known finding."

p "Setup: render the analysis and remediation Agents and the RepositoryScan; clean prior runs."
pe "render_security_agents_manifest          > ${DEMO_WORKDIR}/security-agents.yaml"
pe "render_security_repository_scan_manifest > ${DEMO_WORKDIR}/security-repositoryscan.yaml"
pe "demo_security_reset"

p "Run: apply the Agents and the RepositoryScan."
pe "require_orka_api_reachable"
pe "kubectl apply -f ${DEMO_WORKDIR}/security-agents.yaml"
pe "kubectl apply -f ${DEMO_WORKDIR}/security-repositoryscan.yaml"

p "Follow: wait for the first open finding to appear."
pe "demo_security_discover_first_finding"

p "Inspect: the scan status and the first open findings define the work item."
pe "repository_scan_overview"
pe "repository_findings_summary"

p "Run: ask Orka to patch this finding."
pe "request_finding_patch"

p "Follow: wait for the remediation task to produce a patch proposal."
pe "wait_for_patch_proposal_ready \$DEMO_FINDING_ID ${DEMO_SECURITY_PATCH_TIMEOUT:-1200}"

p "Inspect: the patch record points back to the remediation task and branch."
pe "patch_summary"

p "Run: open the remediation PR for human review."
pe "open_security_pull_request"

p "Summary."
pe "summarize_security_run \$DEMO_FINDING_ID ${DEMO_WORKDIR}/security-pr.json"
