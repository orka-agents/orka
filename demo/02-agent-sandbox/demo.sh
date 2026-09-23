#!/usr/bin/env bash
# Orka — a workspace that sleeps
# Maya asks an agent for a change today and looks at the same files tomorrow. In between, nothing runs and nothing is lost.
source "$(dirname "${BASH_SOURCE[0]}")/../lib/demo.sh"
cd "$repo_root"

here=$repo_root/demo/02-agent-sandbox
runtime_ns=${ORKA_RUNTIME_NAMESPACE:-orka-runtimes}
branch=orka/healthz-from-sandbox
export runtime_ns

# Session names are unique per run: a Session that is still archiving from
# the previous recording cannot be reused, and a fresh name reads better than
# a wait. The rendered manifests are what the viewer sees.
run_id=$(date -u +%H%M)
session=inventory-$run_id
rendered=$demo_root/setup/state/02-agent-sandbox
mkdir -p "$rendered"
for m in "$here"/manifests/*.yaml; do
  sed "s/SESSION_NAME/$session/" "$m" >"$rendered/$(basename "$m")"
done
# The viewer sees short file names, the way a developer keeps a manifest
# next to the code, not the recorder's state directory.
cd "$rendered"
ensure_port_forward
orka_connect
delete_demo_objects 02-agent-sandbox
peq "gh pr list --repo sozercan/orka-demo-inventory --head $branch --json number --jq '.[].number' | xargs -I{} gh pr close {} --repo sozercan/orka-demo-inventory --delete-branch"
peq "gh api -X DELETE repos/sozercan/orka-demo-inventory/git/refs/heads/$branch"

workspace_of() {
  kubectl -n "$ORKA_NAMESPACE" get task "$1" \
    -o jsonpath='{.metadata.labels.acp\.workspace\.orka\.ai/execution-workspace}' 2>/dev/null
}
# The Sandbox that belongs to this Session's runtime pool, not any that lingers.
sandbox_name() {
  local pool
  pool=$(kubectl -n "$ORKA_NAMESPACE" get task healthz-implement -o jsonpath='{.status.execution.runtimePoolName}' 2>/dev/null)
  [[ -n $pool ]] || return 0
  kubectl -n "$runtime_ns" get sandboxes.agents.x-k8s.io -o json 2>/dev/null |
    jq -r --arg pool "$pool" '[.items[] | select(.metadata.name | startswith($pool))][0].metadata.name // empty'
}
sandbox_mode() {
  kubectl -n "$runtime_ns" get sandboxes.agents.x-k8s.io "$1" -o jsonpath='{.spec.operatingMode}' 2>/dev/null
}

# --- on-camera helpers ---------------------------------------------------
# request FILE — the three lines of a Task that matter here: the Session, the
# workspace class, and the prompt. The rest is repository plumbing.
request() {
  awk '
    /^  sessionRef:/ {grab=2} /^      classRef:/ {grab=2} /^  prompt:/ {grab=99}
    grab > 0 {print; grab--}' "$1"
}
# host — what the Sandbox host is running for this team, in three short rows.
# Generated names are 55 characters; the viewer needs the kind, the state, and
# a stable identity.
host() {
  local out
  out=$(kubectl -n "$runtime_ns" get sandboxes.agents.x-k8s.io,pods,pvc -o json 2>/dev/null | jq -r '
    .items[] | select(.metadata.name | test("sandbox-claim")) |
    if .kind == "Sandbox" then
      "Sandbox   " + (.metadata.uid[0:8]) + "   " + (.spec.operatingMode // "-") + "   ready=" + (([.status.conditions[]? | select(.type=="Ready") | .status] | first) // "-")
    elif .kind == "Pod" then
      "Pod       " + (.metadata.uid[0:8]) + "   " + .status.phase
    else
      "Disk      " + (.metadata.uid[0:8]) + "   " + .status.phase + "   " + (.status.capacity.storage // "")
    end')
  [[ -n $out ]] && printf '%s\n' "$out" || echo "nothing running for this team"
}
# lifecycle CLASS — what the class does when the agent stops.
lifecycle() {
  kubectl -n "$ORKA_NAMESPACE" get executionworkspaceclass "$1" -o json |
    jq -r '"when the agent stops: " + .spec.lifecycle.defaultOnDetach + "   max lifetime: " + .spec.lifecycle.maxLifetime'
}
# pod_keys POD — check known credential variables without returning values.
pod_keys() {
  if ! kubectl -n "$runtime_ns" exec "$1" -- sh -c '
    test -z "${GH_TOKEN+x}${GITHUB_TOKEN+x}${OPENAI_API_KEY+x}${ANTHROPIC_API_KEY+x}"
  ' >/dev/null; then
    bad "credential check failed or the host could not be inspected"
    return 1
  fi
  printf 'none of the four checked model/Git credential variables is present\n'
}
# pr_view URL — the pull request as GitHub sees it.
pr_view() {
  gh pr view "$1" --json title,state,headRefName,url --jq '"title:  " + .title, "state:  " + .state, "branch: " + .headRefName, "url:    " + .url'
}

banner "Orka — a workspace that sleeps" \
  "Maya asks an agent for a change today and looks at the same files tomorrow. In between, nothing runs and nothing is lost."

say "Maya, on the inventory team, wants a health check added today and wants"
say "to look at the same files tomorrow. Nobody should pay for an idle agent."
helpers_note request, lifecycle, host, pod_keys, pr_view

chapter "The platform team chose the lifecycle"

say "A workspace class is a lifecycle the platform team saved. Maya asks for"
say "it by name. This one says: when the agent stops, put its host to sleep"
say "and keep the disk."
pe "kubectl -n orka-system get executionworkspaceclasses"
pe "lifecycle sandbox-session"
say "The host is kubernetes-sigs Agent Sandbox. Here is what it currently runs."
pe "host"

chapter "Maya makes the first request"

say "A Task is one request with a recorded outcome. It opens a Session, a"
say "conversation Orka remembers, and names the class and the repository."
pe "request first-request.yaml"
pe "orka task create -f first-request.yaml"
say "Orka asks the host for a Sandbox: one Pod for the agent and one small disk."
wait_for "the Sandbox to exist" "[[ -n \$(sandbox_name) ]]" 300
sb=$(sandbox_name)
pod=$sb
wait_for "the Sandbox Pod" "kubectl -n $runtime_ns get pod $pod --no-headers 2>/dev/null | grep -q Running" 300
pe "host"
sb_uid=$(kubectl -n "$runtime_ns" get sandboxes.agents.x-k8s.io "$sb" -o jsonpath='{.metadata.uid}')
kubectl -n "$runtime_ns" get pod "$pod" -o json >"$rendered/first-pod.json"
first_pod_uid=$(jq -r '.metadata.uid' "$rendered/first-pod.json")
pvc=$(jq -r '[.spec.volumes[]? | .persistentVolumeClaim.claimName // empty] | unique | if length == 1 then .[0] else empty end' "$rendered/first-pod.json")
[[ -n $pvc ]] || { bad "expected one saved workspace disk"; exit 1; }
first_disk_uid=$(kubectl -n "$runtime_ns" get pvc "$pvc" -o jsonpath='{.metadata.uid}')
say "We check the host environment for four common model and Git credential variables."
pe "pod_keys $pod"
say "Now the agent works. Quiet stretches are cut from the recording."
wait_task healthz-implement 1800
kubectl -n "$ORKA_NAMESPACE" get task healthz-implement -o json >"$rendered/first-task.json"
jq -e '.status.delivery.state == "VerifiedExact"' "$rendered/first-task.json" >/dev/null ||
  { bad "the first request has no VerifiedExact publication"; exit 1; }
pe "result healthz-implement"
say "The agent changed files but never pushed. Orka's Publisher, which alone"
say "holds a Git token, verified the files, published the branch, and opened"
say "the pull request. The Task has the receipt."
pe "task_summary healthz-implement"
pr=$(gh pr list --repo sozercan/orka-demo-inventory --head "$branch" --json url --jq '.[0].url')
assert_pr "$pr"
pe "pr_view $pr"
ok "The change is on GitHub. The agent that made it never held a Git token."

chapter "The workspace goes to sleep"

ws=$(workspace_of healthz-implement)
say "The agent is done and the Session is idle. The class said sleep, so Orka"
say "asks the host to suspend the Sandbox: the Pod goes away, the disk stays."
wait_for "the workspace to suspend" \
  "[[ \$(kubectl -n $ORKA_NAMESPACE get executionworkspace $ws -o jsonpath='{.status.state}') == Suspended ]]" 600
wait_for "the suspended Pod to disappear" \
  "[[ -z \$(kubectl -n $runtime_ns get pod $pod --ignore-not-found -o name) ]]" 300
kubectl -n "$runtime_ns" get pods -o json >"$rendered/suspended-pods.json"
suspended_pods=$(jq --arg name "$pod" '[.items[] | select(.metadata.name == $name)] | length' "$rendered/suspended-pods.json")
kubectl -n "$runtime_ns" get pvc "$pvc" -o json >"$rendered/suspended-disk.json"
jq -e --arg uid "$first_disk_uid" '.metadata.uid == $uid and .status.phase == "Bound"' \
  "$rendered/suspended-disk.json" >/dev/null || { bad "the original workspace disk is not Bound"; exit 1; }
((suspended_pods == 0)) || { bad "a Pod is still running for the sleeping workspace"; exit 1; }
suspended_disk=$(jq -r '.status.phase + ", " + .status.capacity.storage' "$rendered/suspended-disk.json")
pe "host"
ok "The Pod released its CPU and memory reservation. The saved disk and cluster still have costs."

chapter "Maya comes back tomorrow"

say "The follow-up names the same Session. That is the only thing tying it"
say "to yesterday's work."
pe "request follow-up-request.yaml"
pe "orka task create -f follow-up-request.yaml"
wait_for "the Sandbox to wake" "[[ \$(sandbox_mode $sb) == Running ]]" 600
wait_for "the replacement Pod to run" \
  "kubectl -n $runtime_ns get pod $pod --no-headers 2>/dev/null | grep -q Running" 600
pe "host"
[[ $(kubectl -n "$runtime_ns" get sandboxes.agents.x-k8s.io "$sb" -o jsonpath='{.metadata.uid}') == "$sb_uid" ]] ||
  { bad "the resumed Sandbox is not the one the first request used"; exit 1; }
kubectl -n "$runtime_ns" get pod "$pod" -o json >"$rendered/follow-up-pod.json"
second_pod_uid=$(jq -r '.metadata.uid' "$rendered/follow-up-pod.json")
[[ $second_pod_uid != "$first_pod_uid" ]] || { bad "the follow-up did not start a new Pod"; exit 1; }
jq -e --arg claim "$pvc" 'any(.spec.volumes[]?; .persistentVolumeClaim.claimName == $claim)' \
  "$rendered/follow-up-pod.json" >/dev/null || { bad "the new Pod does not mount the saved disk"; exit 1; }
[[ $(kubectl -n "$runtime_ns" get pvc "$pvc" -o jsonpath='{.metadata.uid}') == "$first_disk_uid" ]] ||
  { bad "the follow-up has a different workspace disk"; exit 1; }
ok "Same Sandbox, same identity. A new Pod started on the kept disk."
wait_task healthz-follow-up 1200
orka task result healthz-follow-up >"$rendered/follow-up-result.txt"
grep -Eq '^main\.go:[0-9]+:.*healthz' "$rendered/follow-up-result.txt" &&
  grep -Eq '^README\.md:[0-9]+:.*healthz' "$rendered/follow-up-result.txt" ||
  { bad "the follow-up did not report the saved endpoint and README entry"; exit 1; }
kubectl -n "$ORKA_NAMESPACE" get task healthz-follow-up -o json >"$rendered/follow-up-task.json"
follow_up_delivery=$(jq -r '.status.delivery.state' "$rendered/follow-up-task.json")
[[ $follow_up_delivery == NoChange ]] || { bad "the follow-up changed the published tree"; exit 1; }
pe "result healthz-follow-up"
pe "task_summary healthz-follow-up"
ok "The agent found yesterday's endpoint. Delivery NoChange confirms it had no new changes to publish."

chapter "Clean up"

say "When the Session is over, deleting the workspace removes the Sandbox and"
say "its disk. The Task records and the pull request stay."
pe "kubectl -n orka-system delete executionworkspace $ws --wait=false"
wait_for "Sandbox cleanup" "[[ -z \$(kubectl -n $runtime_ns get sandboxes.agents.x-k8s.io $sb --ignore-not-found -o name) ]]" 600
wait_for "workspace disk cleanup" "[[ -z \$(kubectl -n $runtime_ns get pvc $pvc --ignore-not-found -o name) ]]" 600
pe "host"
pr_state=$(gh pr view "$pr" --json state --jq .state)
[[ $pr_state == OPEN ]] || { bad "the pull request is no longer open"; exit 1; }
ok "This Sandbox and its disk are gone. The pull request is still open."

kubectl -n "$ORKA_NAMESPACE" get tasks -l demo.orka.ai/name=02-agent-sandbox -o json >"$rendered/tasks.json"
session_tasks=$(jq --arg session "$session" '[.items[] | select(.spec.sessionRef.name == $session)] | length' "$rendered/tasks.json")
((session_tasks == 2)) || { bad "expected two Tasks in this Session"; exit 1; }
evidence \
  "Requests in the Session" "$session_tasks" \
  "Sandbox identity, day one and day two" "${sb_uid:0:8} both times" \
  "Pods while asleep" "$suspended_pods" \
  "Disk while asleep" "$suspended_disk" \
  "Follow-up publication" "$follow_up_delivery" \
  "Pull request" "$pr ($pr_state)"
cta
