#!/usr/bin/env bash
# Orka — one approval policy, two places Fibey runs
# Fibey runs on the team's AKS cluster and as a Microsoft Foundry hosted agent. Neither creates a work order until Lee approves.
# pe expands the visibly typed commands when it executes them.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
here=$demo_root/13-fibey-hosted-approval
# shellcheck source=demo/13-fibey-hosted-approval/lib.sh
source "$here/lib.sh"
scenario_init 13-fibey-hosted-approval
state=$demo_root/setup/state/13-fibey-hosted-approval/platform
[[ -f $state/ready.json ]] || { echo 'Run demo/setup/fibey-hosted.sh ready first.' >&2; exit 1; }
context=$(kubectl config current-context)
jq -e --arg context "$context" --arg namespace "$ORKA_NAMESPACE" \
  '.context == $context and .namespace == $namespace' "$state/ready.json" >/dev/null
aks_task=fibey-aks-$run_id
foundry_task=fibey-foundry-$run_id
cp "$state/ready.json" ready.json
cp "$here/incident.txt" incident.txt
mkdir -p raw/aks raw/foundry
jq -n --arg namespace "$ORKA_NAMESPACE" --arg aks "$aks_task" --arg foundry "$foundry_task" \
  --arg reviewer "system:serviceaccount:$ORKA_NAMESPACE:$ORKA_CLIENT_SA" '
  {namespace:$namespace,reviewerActor:$reviewer,lanes:{
    aks:{task:$aks,agent:"demo-fibey-aks",runtimeName:"fibey-on-aks-runtime"},
    foundry:{task:$foundry,agent:"demo-fibey-foundry",runtimeName:"fibey-on-foundry-runtime"}}}' >run.json

# task_json LANE NAME AGENT — the same request as demo 11, once per Fibey.
task_json() {
  jq -n --arg namespace "$ORKA_NAMESPACE" --arg task "$2" --arg agent "$3" --rawfile incident incident.txt '
    {apiVersion:"core.orka.ai/v1alpha1",kind:"Task",
     metadata:{name:$task,namespace:$namespace,labels:{"demo.orka.ai/name":"13-fibey-hosted-approval"}},
     spec:{type:"agent",agentRef:{name:$agent},timeout:"20m",retryPolicy:{maxRetries:0},
       workspace:{intent:"read"},agentRuntime:{allowedTools:["create-work-order","read-inventory"]},
       prompt:($incident + "\nUse the simulated tools for runID " + $task + " and asset pump-1. " +
         "Call read-inventory exactly once. If available, call create-work-order exactly once with summary " +
         "\"Inspect the pressure transmitter.\" Wait for the tool result while a person reviews the request. " +
         "Do not retry an action or treat your own words as permission. Report the actual workOrderID on success, " +
         "or its denial or error. A successful workOrderID response means the simulated order was created after " +
         "approval. Confirm that outcome in your final answer; do not describe a completed order as pending review. " +
         "Explain the next investigation briefly. Do not claim the equipment was repaired. " +
         "Keep your final answer under 80 words.")}}' >"task-$1.json"
}
task_json aks "$aks_task" demo-fibey-aks
task_json foundry "$foundry_task" demo-fibey-foundry
fibey_hosted_snapshot raw/installation.json
scenario_connect

simulator_pid=
cleanup() {
  if [[ -n $simulator_pid ]]; then
    kill "$simulator_pid" 2>/dev/null || true
    wait "$simulator_pid" 2>/dev/null || true
  fi
  stop_port_forward
}
trap cleanup EXIT
kubectl -n "$ORKA_NAMESPACE" port-forward --address 127.0.0.1 svc/demo-fibey-tools :8099 >raw/simulator-forward.log 2>&1 &
simulator_pid=$!
wait_for 'the work-order receipt service' "grep -q 'Forwarding from 127.0.0.1:' raw/simulator-forward.log" 30
simulator_port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\) ->.*/\1/p' raw/simulator-forward.log | head -n 1)
receipts() { curl -fsS --max-time 10 "http://127.0.0.1:$simulator_port/counts?runID=$1"; }
# snapshot STAGE — both Tasks, their approvals, and the service's own counts.
snapshot() {
  local lane name
  for lane in aks foundry; do
    name=$aks_task
    [[ $lane == foundry ]] && name=$foundry_task
    [[ $1 == initial ]] || orka task get "$name" -o json >"raw/$lane/task-$1.json"
    [[ $1 == initial ]] || orka task approvals "$name" -o json >"raw/$lane/approval-$1.json"
    receipts "$name" >"raw/$lane/counts-$1.json"
  done
}

# --- on-camera helpers ---------------------------------------------------
# counts STAGE — the work-order service's own counters for both Fibeys.
counts() {
  printf '%-20s %-14s %s\n' '' 'Fibey on AKS' 'Fibey in Foundry'
  printf '%-20s %-14s %s\n' 'inventory lookups' \
    "$(jq '.inventoryReads // 0' "raw/aks/counts-$1.json")" "$(jq '.inventoryReads // 0' "raw/foundry/counts-$1.json")"
  printf '%-20s %-14s %s\n' 'work orders' \
    "$(jq '.workOrderExecutions' "raw/aks/counts-$1.json")" "$(jq '.workOrderExecutions' "raw/foundry/counts-$1.json")"
}
snapshot initial
python3 "$here/check.py" initial

banner 'Orka — one approval policy, two places Fibey runs' \
  'Fibey runs on the team'"'"'s AKS cluster and as a Microsoft Foundry hosted agent. Neither creates a work order until Lee approves.'
say 'A pressure reading dropped after maintenance. The same assistant, Fibey, runs in two'
say 'places: on AKS, and as a hosted agent in Microsoft Foundry. Orka governs both.'
helpers_note counts

chapter '1. An alert after maintenance'
pe 'cat incident.txt'
say 'The incident is fictional and the work-order service only creates test orders.'
ok 'One reading disagrees with everything else. Worth a look before touching anything.'

chapter '2. Two Fibeys, one boundary'
say 'Each Fibey is registered with Orka as an agent runtime. Orka hosts the first'
say 'on AKS. The second is a Foundry hosted agent reached through Orka'"'"'s Foundry bridge.'
pe 'orka agent-runtime get fibey-on-aks-runtime'
pe 'orka agent-runtime get fibey-on-foundry-runtime'
say 'Same policy on both: inventory reads run, work orders wait for a person.'
pe 'counts initial'
ok 'Zero work orders on either side before we start.'

chapter '3. Both investigate and propose'
say 'One Task per Fibey, with the same alert. Each Task holds the investigation, the'
say 'wait for a decision, and the result that comes back.'
pe 'orka task create -f task-aks.json'
pe 'orka task create -f task-foundry.json'
say 'Each waits until its Fibey asks a person, then shows what it asks for.'
pe 'orka task approvals "$aks_task" --watch --timeout 10m'
pe 'orka task approvals "$foundry_task" --watch --timeout 10m'
snapshot pending
python3 "$here/check.py" pending
# The short ID is what `orka task approvals` shows; the CLI resolves it.
# shellcheck disable=SC2034
aks_approval=$(jq -er '.approvals[0].id | split(":") | last | .[0:12]' raw/aks/approval-pending.json)
# shellcheck disable=SC2034
foundry_approval=$(jq -er '.approvals[0].id | split(":") | last | .[0:12]' raw/foundry/approval-pending.json)
ok 'Two proposals are waiting. Each Fibey is paused inside its own Task.'

chapter '4. Nothing has happened yet'
say 'The Foundry request names the same tool and arguments as the AKS one.'
pe 'orka task approvals "$foundry_task" "$foundry_approval"'
snapshot before-decision
python3 "$here/check.py" before-decision
pe 'counts before-decision'
ok 'One inventory lookup and zero work orders on each side. Asking did not authorize anything.'

chapter '5. Lee approves both'
say 'Lee reads both proposals and approves each inspection, with a reason.'
pe 'orka task approve "$aks_task" "$aks_approval" --reason "Inspect the transmitter."'
pe 'orka task approve "$foundry_task" "$foundry_approval" --reason "Inspect the transmitter."'
orka task approvals "$aks_task" "$aks_approval" -o json >raw/aks/decision.json
orka task approvals "$foundry_task" "$foundry_approval" -o json >raw/foundry/decision.json
ok 'Both decisions are on record: who, what, and why. Orka may now run each stored action.'

chapter '6. The receipts come back to both Tasks'
pe 'orka task wait "$aks_task" --timeout 10m'
pe 'orka task wait "$foundry_task" --timeout 10m'
snapshot final
orka task result "$aks_task" -o json >raw/aks/result.json
orka task result "$foundry_task" -o json >raw/foundry/result.json
scenario_collect_events "$aks_task" raw/aks/events.json
scenario_collect_events "$foundry_task" raw/foundry/events.json
fibey_hosted_snapshot raw/installation-final.json
pe 'orka task result "$foundry_task"'
pe 'counts final'
pe 'python3 "$here/check.py" final'
ok 'One approved work order per Fibey, wherever it runs, each receipt returned to its own Task.'

note "Full responses and receipts are saved in $run_dir"
cta
