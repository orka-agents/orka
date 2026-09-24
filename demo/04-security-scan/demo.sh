#!/usr/bin/env bash
# Orka — findings that arrive as pull requests
# Priya registers an old app. Orka scans it, checks its own findings, and opens the fix Priya picks as a pull request.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
cd "$repo_root"

here=$demo_root/04-security-scan
repo=nodejs-goof
work=$demo_root/setup/state/04
mkdir -p "$work"
# Keep the registration in the selected installation, including isolated demos.
awk -v ns="$ORKA_NAMESPACE" '
  $0 == "  namespace: orka-system" {$0 = "  namespace: " ns}
  {print}' "$here/manifests/repository-scan.yaml" >"$work/repository-scan.yaml"
cd "$work"

ensure_port_forward
orka_connect
peq "orka security repo delete $repo"
peq "kubectl -n $ORKA_NAMESPACE delete repositoryscan $repo --wait=true"
# Keep previous pull requests for review. Each scan records its own findings
# and patch, so this recording does not need to close other proposed fixes.
# --- on-camera helpers ---------------------------------------------------
# registration — the fields of the RepositoryScan a viewer needs: the
# repository, the two agents, and the note about credentials.
registration() {
  awk '/^  repoURL:|^  branch:|^  analysisAgentRef:|^  patchAgentRef:/ {grab=2} /^  # Four credential/ {grab=2}
       grab > 0 {print; grab--}' repository-scan.yaml
}
scan_runs() { orka security scan list "$repo" -o json | jq -e '.items | length > 0'; }

banner "Orka — findings that arrive as pull requests" \
  "Priya registers an old app. Orka scans it, checks its own findings, and opens the fix Priya picks as a pull request."

say "Priya, on the security team, has an old Node.js app nobody wants to touch."
say "She wants findings that were checked before she reads them, and fixes as pull requests."
helpers_note registration

chapter "Priya registers the repository"

say "She registers the repository once. The record names the repository, the"
say "Agent that reviews, and the Agent that patches. Only Orka's Publisher"
say "ever holds the Git credentials; the agents never do."
pe "registration"
pe "orka security repo create -f repository-scan.yaml"
pe "orka security repo list"
ok "Registered. The first scan starts on its own."

chapter "Orka scans the app"

say "The reviewing Agent first writes a threat model: what the app does and"
say "where an attacker would push. Then it reviews the code slice by slice"
say "against that model. A separate validation Task checks likely findings."
say "Each row is a stage; the counts move as the scan works. The command"
say "returns when the scan ends. Quiet stretches are cut."
wait_for "the first scan run" scan_runs 300
pe "orka security scan status $repo --watch --interval 20s"
pe "orka security scan list $repo"
say "The threat model opens with what the app does and where an attacker would push."
pe "orka security threat-model get $repo | head -n 14"
ok "The scan finished with a threat model and a set of findings on record."

chapter "Findings come with evidence"

say "Every finding cites a file and a line. A validating agent then checks"
say "up to two likely findings: it reads the code path and tries a safe"
say "reproduction when it can. Orka keeps its decision. Here are the ones that passed."
wait_for "a validated recommended finding" \
  "orka security finding list $repo --recommended --validation-status validated -o json | jq -e '.items | length > 0'" 1800
pe "orka security finding list $repo --recommended --validation-status validated"
orka security finding list "$repo" --recommended --validation-status validated -o json >"$work/findings.json"
target=$(jq -r '[.items[] | select(.validationStatus == "validated")][0].id // empty' "$work/findings.json")
[[ -n $target ]] || { bad "no validated recommended finding"; exit 1; }
orka security finding get "$target" -o json >"$work/selected-finding.json"
jq -e '.validationStatus == "validated"' "$work/selected-finding.json" >/dev/null ||
  { bad "the selected finding is not validated"; exit 1; }
say "One finding in full."
pe "orka security finding get $target"
ok "The finding records its location, severity, and the validation decision."

chapter "Priya picks one to fix"

say "Nothing is patched by itself. Priya asks for a fix on this one finding."
say "Orka hands it to the coding Agent in a writable workspace, and the"
say "Publisher opens the pull request with credentials the agent never saw."
pe "orka security finding patch $target"
watch_until "orka task list -l orka.ai/security-finding-id=$target --watch" \
  "orka security finding patches $target -o json | jq -e '[.items[] | select(.status == \"pr_opened\" or (.status | test(\"failed|rejected\")))] | length > 0'" 20
if orka security finding patches "$target" -o json | jq -e '[.items[] | select(.status | test("failed|rejected"))] | length > 0' >/dev/null; then
  bad "the patch proposal did not reach pr_opened"; orka security finding patches "$target" -o json | jq '.items[] | {status,reason}' >&2; exit 1
fi
pe "orka security finding patches $target"
pr=$(orka security finding pr "$target" -o json | jq -r '.prURL // empty')
assert_pr "$pr"
say "Now check it somewhere Orka does not control."
pe "gh pr view $pr --json title,state --jq '.title, .state'"
pe "gh pr diff $pr --name-only"
ok "A checked finding and a proposed fix, with Priya deciding in the middle."

orka security scan list "$repo" -o json >"$work/scans.json"
orka security finding patches "$target" -o json >"$work/patches.json"
record=$(jq -r '.items | sort_by(.startedAt) | last | "\(.reviewedSliceCount)/\(.sliceCount) slices, \(.acceptedFindings) findings"' "$work/scans.json")
validated=$(jq '[.items[] | select(.validationStatus == "validated")] | length' "$work/findings.json")
patches=$(jq '.items | length' "$work/patches.json")
evidence \
  "Registered repository" "$repo" \
  "Scan" "$record" \
  "Validated findings listed" "$validated" \
  "Patch proposals for this finding" "$patches" \
  "Pull request" "$pr"
cta
