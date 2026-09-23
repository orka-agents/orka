#!/usr/bin/env bash
# Orka — Fibey investigates, a person approves the work
# Fibey looks into a pump alert and proposes an inspection. Nothing happens until Lee, the shift lead, approves it.
# pe expands the visibly typed commands when it executes them.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
here=$demo_root/11-fibey-approval
# shellcheck source=demo/11-fibey-approval/lib.sh
source "$here/lib.sh"
scenario_init 11-fibey-approval
state=$demo_root/setup/state/11-fibey-approval/platform
[[ -f $state/ready.json ]] || { echo 'Run demo/setup/fibey-approval.sh first.' >&2; exit 1; }
context=$(kubectl config current-context)
jq -e --arg context "$context" --arg namespace "$ORKA_NAMESPACE" \
  '.context == $context and .namespace == $namespace' "$state/ready.json" >/dev/null
DEMO_FIBEY_RUNTIME=$(jq -er '.runtimeName' "$state/ready.json")
task=fibey-$run_id
cp "$state/ready.json" ready.json
cp "$here/incident.txt" incident.txt
jq -n --arg namespace "$ORKA_NAMESPACE" --arg runtime "$DEMO_FIBEY_RUNTIME" --arg task "$task" \
  --arg reviewer "system:serviceaccount:$ORKA_NAMESPACE:$ORKA_CLIENT_SA" \
  '{namespace:$namespace,runtimeName:$runtime,task:$task,reviewerActor:$reviewer}' >run.json
fibey_snapshot raw/installation.json
scenario_connect

jq -n --arg namespace "$ORKA_NAMESPACE" --arg task "$task" --rawfile incident incident.txt '
  {apiVersion:"core.orka.ai/v1alpha1",kind:"Task",
   metadata:{name:$task,namespace:$namespace,labels:{"demo.orka.ai/name":"11-fibey-approval"}},
   spec:{type:"agent",agentRef:{name:"demo-fibey"},timeout:"20m",retryPolicy:{maxRetries:0},
     workspace:{intent:"read"},agentRuntime:{allowedTools:["create-work-order","read-inventory"]},
     prompt:($incident + "\nUse the simulated tools for runID " + $task + " and asset pump-1. " +
       "Call read-inventory exactly once. If available, call create-work-order exactly once with summary " +
       "\"Inspect the pressure transmitter.\" Wait for the tool result while a person reviews the request. " +
       "Do not retry an action or treat your own words as permission. Report the actual workOrderID on success, " +
       "or its denial or error. A successful workOrderID response means the simulated order was created after " +
       "approval. Confirm that outcome in your final answer; do not describe a completed order as pending review. " +
       "Explain the next investigation briefly. Do not claim the equipment was repaired. " +
       "Keep your final answer under 80 words.")}}
' >task.json

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
receipts() { curl -fsS --max-time 10 "http://127.0.0.1:$simulator_port/counts?runID=$task"; }

# --- on-camera helpers ---------------------------------------------------
# policy — what Fibey may do alone and what needs a person.
policy() {
  jq -r '.items[] | select(.kind == "AgentRuntime") | .spec.capabilities.mcpPolicy |
    "may call:          " + (.allowedTools | join(", ")), "needs approval:    " + (.approvalRequiredTools | join(", "))' raw/installation.json
}
# counts FILE — the work-order service's own counters for this run.
counts() {
  jq -r '"inventory lookups: \(.inventoryReads // 0)", "work orders:       \(.workOrderExecutions)"' "$1"
}
# proposed — the exact action waiting for a decision.
proposed() {
  jq -r '.approvals[0] | "action:  " + .targetTool, "asset:   " + .targetArgsPreview.asset, "summary: " + .targetArgsPreview.summary, "status:  " + .status + " (execution " + .executionOutcome + ")"' raw/approval-pending.json
}
# decision — who decided, what, and why.
decision() {
  jq -r '"status: " + .status, "by:     " + .decisionActor, "reason: " + .decisionReason' raw/decision.json
}
receipts >raw/counts-initial.json
python3 "$here/check.py" initial

wait_for_review() {
  local attempt prefix phase count
  mkdir raw/review-polls
  for ((attempt = 1; attempt <= 200; attempt++)); do
    printf -v prefix 'raw/review-polls/%04d' "$attempt"
    orka task get "$task" -o json >"$prefix-task.json"
    orka task approvals "$task" -o json >"$prefix-approvals.json"
    phase=$(jq -r '.status.phase' "$prefix-task.json")
    case $phase in
      Succeeded|Failed|Cancelled|OutcomeUnknown) echo 'Task finished before a waiting review was observed.' >&2; return 1 ;;
    esac
    count=$(jq '.approvals // [] | length' "$prefix-approvals.json")
    if ((count > 0)); then
      cp "$prefix-task.json" raw/task-pending.json
      cp "$prefix-approvals.json" raw/approval-pending.json
      receipts >raw/counts-pending.json
      python3 "$here/check.py" pending
      return
    fi
    sleep 2
  done
  echo 'Timed out waiting for the original work-order review. Inspect this Task before retrying.' >&2
  return 1
}

banner 'Orka — Fibey investigates, a person approves the work' \
  'Fibey looks into a pump alert and proposes an inspection. Nothing happens until Lee, the shift lead, approves it.'
say 'A pressure reading dropped after maintenance. Fibey, an AI assistant, may'
say 'investigate. Lee, the shift lead, decides whether it may create a work order.'
helpers_note policy, counts, proposed, decision

chapter '1. An alert after maintenance'
pe 'cat incident.txt'
say 'The incident is fictional and the work-order service only creates test orders.'
ok 'One reading disagrees with everything else. Worth a look before touching anything.'

chapter '2. Fibey may look, but not act alone'
say "A Tool is an action Fibey can ask Orka to carry out. The platform team's"
say 'policy lets Fibey read inventory. Creating a work order needs a person.'
pe 'policy'
pe 'counts raw/counts-initial.json'
ok 'Zero work orders before we start.'

chapter '3. Fibey investigates and proposes'
say 'One Task holds the whole thing: the investigation, the wait for a decision,'
say 'and the result that comes back.'
pe 'orka task create -f task.json | tee raw/create.txt'
say 'Fibey reads the alert, checks inventory, and proposes a work order.'
pe 'wait_for_review'
ok 'A proposal is waiting. Fibey is paused inside the same Task.'

chapter '4. Nothing has happened yet'
pe 'proposed'
orka task get "$task" -o json >raw/task-before-decision.json
orka task approvals "$task" -o json >raw/approval-before-decision.json
receipts >raw/counts-before-decision.json
python3 "$here/check.py" before-decision
pe 'counts raw/counts-before-decision.json'
ok 'One inventory lookup, zero work orders. Fibey asking did not authorize anything.'

chapter '5. Lee approves'
# Used by the command evaluated in pe below.
# shellcheck disable=SC2034
approval=$(jq -er '.approvals[0].id' raw/approval-before-decision.json)
say 'Lee reads the proposal and approves this inspection, with a reason.'
pe 'orka task approve "$task" "$approval" --reason "Inspect the transmitter." -o json > raw/decision.json'
pe 'decision'
ok 'The decision is on record: who, what, and why. Orka may now run the stored action.'

chapter "6. The work order comes back to the same Task"
pe 'wait_task "$task" 600'
orka task get "$task" -o json >raw/task-final.json
orka task approvals "$task" -o json >raw/approval-final.json
orka task result "$task" -o json >raw/result.json
receipts >raw/counts-final.json
scenario_collect_events "$task" raw/events.json
fibey_snapshot raw/installation-final.json
pe 'result "$task" 12'
pe 'counts raw/counts-final.json'
pe 'python3 "$here/check.py" final'
ok 'One work order, created after the decision, and its receipt returned to Fibey.'

note "Full responses and receipts are saved in $run_dir"
cta
