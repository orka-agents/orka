#!/usr/bin/env bash
# Orka — a save point for an agent
# Priya starts a security audit. The workspace sleeps when nobody works, and a checkpoint brings the audit back after the workspace is deleted.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
cd "$repo_root"

here=$repo_root/demo/03-agent-substrate
atespace=orka-system
pool_ns=${SUBSTRATE_POOL_NAMESPACE:-ate-demo}
: "${ATE_BIN:=$demo_root/setup/state/kubectl-ate}"
[[ -x $ATE_BIN ]] || ATE_BIN=$repo_root/bin/substrate-eval/kubectl-ate
export ATE_BIN
# `kubectl ate` is Substrate's kubectl plugin. The demo types it that way; the
# function resolves the plugin binary the setup built.
kubectl() {
  if [[ ${1:-} == ate ]]; then
    shift
    "$ATE_BIN" "$@"
  else
    command kubectl "$@"
  fi
}

branch=orka/security-audit
# Session names are unique per run: a Session that is still archiving from
# the previous recording cannot be reused, and a fresh name reads better than
# a wait. The rendered manifests are what the viewer sees.
run_id=$(date -u +%H%M)
session=audit-$run_id
rendered=$demo_root/setup/state/03-agent-substrate
mkdir -p "$rendered"
for m in "$here"/manifests/*.yaml; do
  sed "s/SESSION_NAME/$session/" "$m" >"$rendered/$(basename "$m")"
done
cd "$rendered"
ensure_port_forward
orka_connect
delete_demo_objects 03-agent-substrate
peq "gh api -X DELETE repos/sozercan/orka-demo-inventory/git/refs/heads/$branch"
peq "gh api -X DELETE repos/sozercan/orka-demo-inventory/git/refs/heads/$branch-restored"

workspace_of() {
  kubectl -n "$ORKA_NAMESPACE" get task "$1" \
    -o jsonpath='{.metadata.labels.acp\.workspace\.orka\.ai/execution-workspace}' 2>/dev/null
}
actor_count() {
  kubectl ate get actors -a "$atespace" -o json 2>/dev/null | jq '.actors | length'
}
ws_state() {
  kubectl -n "$ORKA_NAMESPACE" get executionworkspace "$1" -o jsonpath='{.status.state}' 2>/dev/null
}
free_workers() {
  kubectl ate get workers | awk 'NR > 1 && /(^|[[:space:]])FREE([[:space:]]|$)/ {n++} END {print n+0}'
}

# Read the file from the exact commit Orka verified, not from the agent's
# description of it. Keep both copies so the restore claim can be checked.
published_audit() {
  local task=$1 branch=$2 output=$3 sha
  kubectl -n "$ORKA_NAMESPACE" get task "$task" -o json >"$rendered/$task.json"
  jq -e --arg branch "$branch" '
    .status.phase == "Succeeded" and .status.delivery.state == "VerifiedExact" and
    .status.delivery.branch == $branch and (.status.delivery.verifiedRemoteSHA | length > 0)
  ' "$rendered/$task.json" >/dev/null || { bad "$task has no matching verified publication"; return 1; }
  sha=$(jq -r '.status.delivery.verifiedRemoteSHA' "$rendered/$task.json")
  gh api -H 'Accept: application/vnd.github.raw+json' \
    "repos/sozercan/orka-demo-inventory/contents/AUDIT.md?ref=$sha" >"$output"
  [[ -s $output ]] || { bad "$task published an empty audit"; return 1; }
}

# --- on-camera helpers ---------------------------------------------------
# actors — one row per Actor: a short name, its state, and the worker hosting it.
actors() {
  local record rows
  record=$(kubectl ate get actors -a "$atespace" -o json) || return 1
  rows=$(jq -r '
    .actors[]? | [(.metadata.name | sub("^acp-ws-session-[0-9a-f]+-actor-"; "actor-") | .[0:14]),
                  (.status.state | ltrimstr("ACTOR_STATE_")),
                  (.status.workerAssignment.workerPod // "-")] | @tsv' <<<"$record") || return 1
  if [[ -n $rows ]]; then printf '%s\n' "$rows" | column -t; else printf 'no Actors\n'; fi
}
# workers — the shared worker pool and what each worker is doing.
workers() {
  kubectl ate get workers | awk 'NR == 1 || $1 != ""' | cut -c1-96
}
# request FILE — the lines of a Task that matter here.
request() {
  awk '
    /^  sessionRef:/ {grab=2} /^      classRef:/ {grab=2} /^      restoreFrom:/ {grab=4} /^  prompt:/ {grab=99}
    grab > 0 {print; grab--}' "$1"
}
# checkpoint_status — phase and a short digest.
checkpoint_status() {
  kubectl -n "$ORKA_NAMESPACE" get executionworkspacecheckpoint audit-checkpoint -o json |
    jq -r '"phase:  " + (.status.phase // "-"), "digest: " + ((.status.digest // "-") | .[0:26]) + "…"'
}

banner "Orka — a save point for an agent" \
  "Priya starts a security audit. The workspace sleeps when nobody works, and a checkpoint brings the audit back after the workspace is deleted."

say "Priya, on the security team, is auditing the inventory service. Nothing"
say "should run while nobody works, and the findings must survive anything."
helpers_note actors, workers, request, checkpoint_status

chapter "Substrate keeps a pool of workers"

say "This host is Agent Substrate. It keeps a few worker Pods ready all the"
say "time and runs each agent inside an Actor, an isolated environment with"
say "its own kernel, hosted by whichever worker is free."
pe "workers"
pe "actors"
say "The platform team's class for this host says: sleep when the agent stops,"
say "keep the data, and boot a fresh Actor from that data next time."
pe "kubectl -n orka-system get executionworkspaceclass substrate-session"
initial_actors=$(actor_count)
initial_free_workers=$(free_workers)
((initial_actors == 0)) || { bad "the security workspace host is not idle"; exit 1; }
ok "$initial_free_workers workers free, $initial_actors Actors. Nothing is running for the security team."

chapter "Priya starts the audit"

say "The request is a Task. It opens a Session, names the class, and asks"
say "for at most six findings written to a file."
pe "request first-request.yaml"
pe "orka task create -f first-request.yaml"
say "Orka creates an Actor for the Session; Substrate places it on a worker."
wait_for "an Actor to boot" "(( \$(actor_count) >= 1 ))" 600
pe "actors"
say "That Actor is the agent's whole world. No Git credential rides along;"
say "Orka's Publisher holds it, outside the sandbox."
wait_task audit-start 1200
pe "result audit-start"
say "The Publisher verified the files and published the audit as a branch."
pe "orka task status audit-start"
pe "git ls-remote $DEMO_REPO refs/heads/$branch | cut -c1-12"
published_audit audit-start "$branch" "$rendered/original-AUDIT.md"
ok "Findings written by an agent with no Git token, published by Orka as a branch."

chapter "The workspace goes to sleep"

ws=$(workspace_of audit-start)
say "The agent is done. Substrate captures the Actor's data to storage, then"
say "removes the Actor and retires the worker that hosted it. The pool gets a"
say "clean replacement, so the next agent never inherits a used worker."
wait_for "the workspace to suspend" "[[ \$(ws_state $ws) == Suspended ]]" 600
pe "kubectl -n orka-system get executionworkspace $ws"
pe "actors"
pe "workers"
suspended_actors=$(actor_count)
suspended_free_workers=$(free_workers)
((suspended_actors == 0)) || { bad "an Actor remains after the workspace suspended"; exit 1; }
ok "$suspended_actors Actors, $suspended_free_workers workers free. The audit's files are in storage."

chapter "Priya saves a checkpoint"

ws_uid=$(kubectl -n "$ORKA_NAMESPACE" get executionworkspace "$ws" -o jsonpath='{.metadata.uid}')
sed "s/WORKSPACE_NAME/$ws/; s/WORKSPACE_UID/$ws_uid/" "$here/manifests/checkpoint.yaml" | grep -v '^#' >checkpoint.yaml
say "A checkpoint is a copy of the workspace's data that Orka keeps as an"
say "object of its own, with a digest. It survives the workspace being deleted."
pe "cat checkpoint.yaml"
pe "kubectl apply -f checkpoint.yaml"
wait_for "the checkpoint to be Ready" \
  "[[ \$(kubectl -n $ORKA_NAMESPACE get executionworkspacecheckpoint audit-checkpoint -o jsonpath='{.status.phase}') == Ready ]]" 600
pe "checkpoint_status"
ok "The audit has a save point with a digest."

chapter "Delete the workspace, restore the copy"

say "Now the worst case: the workspace is deleted, and with it the data"
say "Substrate kept for it."
pe "kubectl -n orka-system delete executionworkspace $ws --wait=false"
wait_for "the workspace to disappear" "! kubectl -n $ORKA_NAMESPACE get executionworkspace $ws >/dev/null 2>&1" 600
pe "kubectl -n orka-system get executionworkspaces"
say "A brand-new Task, outside the old Session, restores from the checkpoint."
say "It names the checkpoint's identity and digest, so a swapped copy is refused."
cp_uid=$(kubectl -n "$ORKA_NAMESPACE" get executionworkspacecheckpoint audit-checkpoint -o jsonpath='{.metadata.uid}')
cp_digest=$(kubectl -n "$ORKA_NAMESPACE" get executionworkspacecheckpoint audit-checkpoint -o jsonpath='{.status.digest}')
sed "s/CHECKPOINT_UID/$cp_uid/; s/CHECKPOINT_DIGEST/$cp_digest/" "$here/manifests/restore-request.yaml" >restore-request.yaml
pe "request restore-request.yaml"
pe "orka task create -f restore-request.yaml"
wait_task audit-restore 1200
pe "result audit-restore"
published_audit audit-restore "$branch-restored" "$rendered/restored-AUDIT.md"
if ! cmp -s "$rendered/original-AUDIT.md" "$rendered/restored-AUDIT.md"; then
  bad "the restored audit does not match the original"; exit 1
fi
audit_digest=$(shasum -a 256 "$rendered/original-AUDIT.md" | awk '{print $1}')
match="identical file bytes (SHA-256 ${audit_digest:0:12})"
ok "The audit came back from a deleted workspace, written by an Actor that is long gone."
pe "orka task status audit-restore"


chapter "Clean up"

restored_ws=$(workspace_of audit-restore)
[[ -n $restored_ws ]] || { bad "the restored Task has no workspace identity"; exit 1; }
say "The restored Task asked Orka to delete its workspace when it finished."
wait_for "the restored workspace to be collected" \
  "[[ -z \$(kubectl -n $ORKA_NAMESPACE get executionworkspace $restored_ws --ignore-not-found -o name) ]]" 600
say "Once that cleanup finishes, Priya removes the saved checkpoint too."
pe "kubectl -n orka-system delete executionworkspacecheckpoint audit-checkpoint"
peq "gh api -X DELETE repos/sozercan/orka-demo-inventory/git/refs/heads/$branch"
peq "gh api -X DELETE repos/sozercan/orka-demo-inventory/git/refs/heads/$branch-restored"
pe "actors"
pe "workers"
end_actors=$(actor_count)
end_free_workers=$(free_workers)
((end_actors == 0)) || { bad "an Actor remains after cleanup"; exit 1; }
ok "$end_actors Actors, $end_free_workers workers free. The Task records stay."

evidence \
  "Actors while asleep" "$suspended_actors" \
  "Workers free at the end" "$end_free_workers" \
  "Checkpoint digest" "${cp_digest:0:26}…" \
  "Original workspace" "deleted" \
  "Restored audit matches" "$match" \
  "Branches published" "$branch, $branch-restored"
cta
