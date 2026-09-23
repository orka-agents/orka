#!/usr/bin/env bash
# Orka — findings that arrive as pull requests
# Priya registers an old app. Orka scans it, checks its own findings, and opens the fix Priya picks as a pull request.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
cd "$repo_root"

here=demo/04-security-scan
repo=nodejs-goof
work=$demo_root/setup/state/04
mkdir -p "$work"

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
       grab > 0 {print; grab--}' "$here/manifests/repository-scan.yaml"
}
latest_scan() { orka security scan list "$repo" -o json | jq -r '.items | sort_by(.startedAt) | last | .phase // empty'; }
# stages — one row per stage of the scan with a count of its Tasks by phase.
# A scan fans out into a dozen or more slice Tasks; one row each would fill
# the terminal on every refresh and hide the shape of the work.
stages() {
  kubectl -n "$ORKA_NAMESPACE" get tasks -l "orka.ai/security-target=$repo" -o json | jq -r '
    def stage: .metadata.name |
      if test("-threat-model-") then "1 threat model"
      elif test("-mapper-scan-") then "2 map the code"
      elif test("-initial-review-|-review-slice-") then "3 review slices"
      elif test("-auto-validation-") then "4 validate findings"
      elif test("-patch-") then "5 patch"
      else "other" end;
    ["STAGE","TASKS","PENDING","RUNNING","SUCCEEDED","FAILED"],
    (.items | group_by(stage) | .[] | [
      (.[0] | stage), length,
      ([.[] | select((.status.phase // "Pending") == "Pending")] | length),
      ([.[] | select(.status.phase == "Running")] | length),
      ([.[] | select(.status.phase == "Succeeded")] | length),
      ([.[] | select(.status.phase == "Failed")] | length)
    ]) | map(tostring) | @tsv' | column -t -s $'\t'
}
watch_stages() {
  local until=$1 interval=${2:-20} last="" now
  while true; do
    now=$(stages 2>/dev/null || true)
    if [[ $now != "$last" ]]; then
      printf '%s── %s ──%s\n' "$C_DIM" "$(date -u +%H:%M:%S)" "$C_RESET"
      printf '%s\n' "$now"
      last=$now
    fi
    if eval "$until" >/dev/null 2>&1; then return 0; fi
    sleep "$interval"
  done
}
# scan_record — what the scan reviewed and what it kept.
scan_record() {
  orka security scan list "$repo" -o json |
    jq -r '.items | sort_by(.startedAt) | last | "slices reviewed:   \(.reviewedSliceCount) / \(.sliceCount)", "findings kept:     \(.acceptedFindings)", "findings dropped:  \(.droppedFindings)"'
}
# threat_model — the opening of the threat model the scan wrote first.
threat_model() {
  orka security threat-model get "$repo" -o json | jq -r .content | grep -v '^\s*$' | sed -n '1,6p' | cut -c1-96
}
# findings — the validated findings: severity, id, title. The CLI filters by
# validation status; the table view has no severity or title column yet, so
# the columns come from JSON.
findings() {
  orka security finding list "$repo" --recommended --validation-status validated -o json |
    jq -r '.items[] | [.severity, .id, .title] | @tsv' | cut -c1-96 | sed -n '1,8p'
}
# finding ID — one finding in full.
finding() {
  orka security finding get "$1" -o json |
    jq -r '"title:     " + .title, "severity:  " + .severity, "validated: " + .validationStatus, "where:     " + .filePath + ":" + (.line | tostring), "summary:   " + .summary' | fold -s -w 96
}
# start_patch ID — ask for a fix and show the patch record Orka opened.
start_patch() {
  orka security finding patch "$1" | jq -r '"patch: " + .id, "task:  " + .taskName, "status: " + .status'
}
# patch_status ID — the patch record for a finding.
patch_status() {
  orka security finding patches "$1" -o json | jq -r '.items[0] | "status: " + .status, "branch: " + .branch, "pr:     " + (.prURL // "-")'
}

banner "Orka — findings that arrive as pull requests" \
  "Priya registers an old app. Orka scans it, checks its own findings, and opens the fix Priya picks as a pull request."

say "Priya, on the security team, has an old Node.js app nobody wants to touch."
say "She wants findings that were checked before she reads them, and fixes as pull requests."
helpers_note registration, stages, scan_record, findings, finding, start_patch, patch_status

chapter "Priya registers the repository"

say "She registers the repository once. The record names the repository, the"
say "Agent that reviews, and the Agent that patches. Only Orka's Publisher"
say "ever holds the Git credentials; the agents never do."
pe "registration"
pe "orka security repo create -f $here/manifests/repository-scan.yaml"
pe "orka security repo list"
ok "Registered. The first scan starts on its own."

chapter "Orka scans the app"

say "The reviewing Agent first writes a threat model: what the app does and"
say "where an attacker would push. Then it reviews the code slice by slice"
say "against that model. A separate validation Task checks likely findings."
say "Each row is a stage; the counts move as the scan works. Quiet stretches are cut."
watch_stages "[[ \$(latest_scan) =~ ^(succeeded|failed)$ ]]" 20
[[ $(latest_scan) == succeeded ]] || { bad "the scan run failed"; exit 1; }
pe "scan_record"
pe "threat_model"
ok "The scan finished with a threat model and a set of findings on record."

chapter "Findings come with evidence"

say "Every finding cites a file and a line. The validating agent checks the"
say "code path and tries a safe reproduction when it can. Orka keeps its"
say "decision on record. These findings passed that check."
wait_for "a validated recommended finding" \
  "orka security finding list $repo --recommended --validation-status validated -o json | jq -e '.items | length > 0'" 1800
pe "findings"
orka security finding list "$repo" --recommended --validation-status validated -o json >"$work/findings.json"
target=$(jq -r '[.items[] | select(.validationStatus == "validated")][0].id // empty' "$work/findings.json")
[[ -n $target ]] || { bad "no validated recommended finding"; exit 1; }
orka security finding get "$target" -o json >"$work/selected-finding.json"
jq -e '.validationStatus == "validated"' "$work/selected-finding.json" >/dev/null ||
  { bad "the selected finding is not validated"; exit 1; }
say "One finding in full."
pe "finding $target"
ok "The finding records its location, severity, and the validation decision."

chapter "Priya picks one to fix"

say "Nothing is patched by itself. Priya asks for a fix on this one finding."
say "Orka hands it to the coding Agent in a writable workspace, and the"
say "Publisher opens the pull request with credentials the agent never saw."
pe "start_patch $target"
watch_tasks "orka security finding patches $target -o json | jq -e '[.items[] | select(.status == \"pr_opened\" or (.status | test(\"failed|rejected\")))] | length > 0'" 20 orka.ai/security-finding-id=$target
if orka security finding patches "$target" -o json | jq -e '[.items[] | select(.status | test("failed|rejected"))] | length > 0' >/dev/null; then
  bad "the patch proposal did not reach pr_opened"; orka security finding patches "$target" -o json | jq '.items[] | {status,reason}' >&2; exit 1
fi
pe "patch_status $target"
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
