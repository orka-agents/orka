#!/usr/bin/env bash
# Fibey investigates; a person approves the work
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
  '{namespace:$namespace,runtimeName:$runtime,task:$task}' >run.json
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
       "or its denial or error. Explain the next investigation briefly. Do not claim the equipment was repaired.")}}
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

banner 'Fibey investigates; a person approves the work' \
  'An equipment alert, an AI assistant, and a work order waiting for a decision.'

chapter '1. An alert after maintenance'
say 'One pressure reading has fallen. The other readings look normal.'
say 'Fibey is an AI assistant that helps the team investigate equipment alerts.'
pe 'cat incident.txt'
say 'We use a fictional incident and a service that creates test work orders.'

chapter '2. Give Fibey a clear boundary'
say 'Orka runs the request and keeps its work records.'
say 'A Tool is an action Fibey can ask Orka to carry out. The saved policy'
say 'lets it check inventory, but requires a person to approve a work order.'
pe 'jq ".items[] | select(.kind == \"AgentRuntime\") | .spec.capabilities.mcpPolicy | {allowedTools, approvalRequiredTools}" raw/installation.json'
pe 'jq "{workOrders: .workOrderExecutions}" raw/counts-initial.json'

chapter '3. Ask Fibey to investigate'
say 'A Task is the record of this request. We will keep the same Task while'
say 'Fibey checks inventory, waits for review, and receives the result.'
pe 'orka task create -f task.json | tee raw/create.txt'
pe 'wait_for_review'

chapter '4. Inspect the proposed action'
pe 'jq ".approvals[] | {targetTool, targetArgsPreview, status, executionOutcome}" raw/approval-pending.json'
say 'This is the exact action waiting for review. It has not run yet.'
say 'The inspection request identifies the pump and the work to carry out.'

chapter '5. Check that work is still waiting'
orka task get "$task" -o json >raw/task-before-decision.json
orka task approvals "$task" -o json >raw/approval-before-decision.json
receipts >raw/counts-before-decision.json
python3 "$here/check.py" before-decision
pe 'jq "{inventoryReads, workOrders: .workOrderExecutions}" raw/counts-before-decision.json'
say 'The service received one inventory lookup and zero work orders.'
say 'Fibey asking for an action did not authorize it.'

chapter '6. The shift lead approves'
# Used by the command evaluated in pe below.
# shellcheck disable=SC2034
approval=$(jq -er '.approvals[0].id' raw/approval-before-decision.json)
say 'The presenter acts as the shift lead and approves this inspection request.'
pe 'orka task approve "$task" "$approval" --reason "Inspect the transmitter; no equipment changes." -o json > raw/decision.json'
pe 'jq "{status, decisionActor, decisionReason}" raw/decision.json'
say 'Orka can now run the stored action and return its result to Fibey.'

chapter "7. Read the receipt and Fibey's answer"
pe 'wait_task "$task" 600'
orka task get "$task" -o json >raw/task-final.json
orka task approvals "$task" -o json >raw/approval-final.json
orka task result "$task" -o json >raw/result.json
receipts >raw/counts-final.json
scenario_collect_events "$task" raw/events.json
fibey_snapshot raw/installation-final.json
pe 'python3 "$here/check.py" final'
pe 'jq -r .result raw/result.json'
say 'Fibey checked inventory and prepared the inspection request.'
say 'A person approved it. One work order was created, and its receipt'
say 'came back to the same waiting Task. No equipment was changed.'
note "Full responses and receipts are saved in $run_dir"
