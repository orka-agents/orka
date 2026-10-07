#!/usr/bin/env bash
# Orka — from equipment alert to approved work order
# An alert arrives through the gateway. Orka runs a data-analysis job on AKS and a specialist agent in Microsoft Foundry, and Mark approves the work order before it runs.
# pe expands the visibly typed commands when it executes them.
# shellcheck disable=SC2016
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
here=$demo_root/14-fibey-alert-flow
# shellcheck source=demo/14-fibey-alert-flow/lib.sh
source "$here/lib.sh"
# Mark presents and approves; the approval record names his ServiceAccount.
export ORKA_CLIENT_SA=${DEMO_FLOW_PRESENTER:-mark}
scenario_init 14-fibey-alert-flow
state=$demo_root/setup/state/14-fibey-alert-flow/installation
[[ -f $state/ready.json && -x $repo_root/bin/demo-fibey-flow/a2a-client ]] || {
  echo 'Run demo/setup/fibey-flow.sh images, install, and ready first.' >&2; exit 1;
}
jq -e --arg context "$(kubectl config current-context)" --arg namespace "$ORKA_NAMESPACE" \
  '.context == $context and .namespace == $namespace' "$state/ready.json" >/dev/null
cp "$state/ready.json" ready.json
fibey_flow_snapshot raw/installation.json
python3 "$here/check.py" installation
# A short ID keeps the receipt short; models copy it into their answers.
alert_id=qn-${run_id##*-}
conversation=alert-$run_id
printf '%s  Quincy North, pump-1\n%s\n%s\n' "$alert_id" \
  '02:15  PT-101 discharge pressure LOW: 0.2 bar (normal 3.1 bar)' \
  '       Maintenance ended at 02:00; the PT-101 transmitter was replaced.' >alert.txt
jq -n --arg namespace "$ORKA_NAMESPACE" --arg alert "$alert_id" --arg conversation "$conversation" \
  --arg reviewer "system:serviceaccount:$ORKA_NAMESPACE:$ORKA_CLIENT_SA" '
  {namespace:$namespace, alertID:$alert, conversation:$conversation, reviewerActor:$reviewer}' |
  jq --slurpfile ready ready.json '. + ($ready[0] | {coordinatorModel, specialistModel})' >run.json
scenario_connect

pids=()
cleanup() {
  local pid
  for pid in "${pids[@]}"; do
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  done
  stop_port_forward
}
trap cleanup EXIT
# forward NAME SERVICE LOCAL REMOTE — one port-forward per service, torn down at exit.
forward() {
  kubectl -n "$ORKA_NAMESPACE" port-forward --address 127.0.0.1 "svc/$2" "$3:$4" >"raw/$1-forward.log" 2>&1 &
  pids+=("$!")
}
forward simulator demo-fibey-tools '' 8099
wait_for 'the work-order receipt service' "grep -q 'Forwarding from 127.0.0.1:' raw/simulator-forward.log" 30
simulator_port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\) ->.*/\1/p' raw/simulator-forward.log | head -n 1)
a2a_port=${DEMO_FLOW_A2A_PORT:-8443}
forward adapter fibey-alerts-adapter "$a2a_port" 8443
a2a_url=https://127.0.0.1:$a2a_port
wait_for 'the verified HTTPS gateway adapter' "curl -fsS --max-time 2 --cacert '$state/ca.crt' '$a2a_url/readyz'" 60

a2a() {
  "$repo_root/bin/demo-fibey-flow/a2a-client" -url "$a2a_url" -token-file "$state/client.token" -ca-file "$state/ca.crt" "$@"
}
receipts() { curl -fsS --max-time 10 "http://127.0.0.1:$simulator_port/counts?runID=$alert_id"; }
# wait_event WANTED PREFIX — poll the gateway's own record of this alert, keeping every response.
wait_event() {
  local attempt file phase
  for ((attempt = 1; attempt <= 300; attempt++)); do
    printf -v file 'raw/event-%s-%03d.json' "$2" "$attempt"
    orka gateway events get "$event_id" -o json >"$file"
    phase=$(jq -er '.state' "$file")
    case $phase in
      Rejected|DeadLettered|Expired) echo "Orka ended this alert in $phase; see $file." >&2; return 1 ;;
    esac
    if { [[ $1 == dispatched ]] && jq -e '.taskName != null and .taskUid != ""' "$file" >/dev/null; } ||
       { [[ $1 == completed && $phase == Completed ]] && jq -e '.deliveryId != null and .deliveryId != ""' "$file" >/dev/null; }; then
      cp "$file" "raw/event-$2.json"
      return 0
    fi
    sleep 3
  done
  echo "Timed out waiting for the gateway to record $1." >&2
  return 1
}
# snapshot STAGE — the coordinator, its children, Fibey's approval, and the service's counts.
snapshot() {
  receipts >"raw/counts-$1.json"
  [[ $1 == initial ]] && return 0
  kubectl -n "$ORKA_NAMESPACE" get task "$coordinator" -o json >"raw/coordinator-$1.json"
  kubectl -n "$ORKA_NAMESPACE" get tasks -l "orka.ai/parent-task=$coordinator" -o json >"raw/children-$1.json"
  orka task approvals "$fibey_task" -o json >"raw/approval-$1.json"
}

# --- on-camera helpers ---------------------------------------------------
# alert — the plant's monitoring system sends alert.txt through the gateway (A2A).
alert() {
  a2a -message-id "$alert_id" -context-id "$conversation" -return-immediately -text "$(cat alert.txt)" >raw/admission.json
  jq -r '"reference: " + (.id | .[:28]) + "…", "state:     " + .status.state' raw/admission.json
}
# counts STAGE — the work-order service's own counters for this alert.
counts() {
  jq -r '"inventory lookups  \(.inventoryReads // 0)", "work orders        \(.workOrderExecutions)"' "raw/counts-$1.json"
}
# answer — what the monitoring system receives back through the gateway.
answer() {
  a2a -task-id "$task_ref" >raw/answer.json
  jq -r '.artifacts[].parts[].text' raw/answer.json
}
# record — the closing table, checked against every saved record.
record() {
  python3 "$here/check.py" final
}

snapshot initial
python3 "$here/check.py" initial

banner 'Orka — from equipment alert to approved work order' \
  'An alert arrives through the gateway. Orka runs a data-analysis job on AKS and a specialist agent in Microsoft Foundry. Mark approves the work order before it runs.'
helpers_note alert, counts, answer, record

chapter '1. An alert comes in through the gateway'
pe 'cat alert.txt'
say 'The plant'"'"'s monitoring system sends it to Orka'"'"'s gateway over A2A.'
pe 'alert'
task_ref=$(jq -er '.id' raw/admission.json)
event_id=$(python3 "$here/check.py" event-id raw/admission.json)
wait_event dispatched dispatched
coordinator=$(jq -er '.taskName' raw/event-dispatched.json)
ok 'Accepted. The gateway turned the alert into an Orka Task.'

chapter '2. Orka coordinates the work'
say 'Orka'"'"'s coordinator starts a data-analysis job on AKS, then hands its findings'
say 'to Fibey, a specialist agent hosted in Microsoft Foundry.'
# Wait for the hand-off to Fibey, or stop early if the analysis job failed.
wait_for 'the Foundry specialist Task' \
  "kubectl -n '$ORKA_NAMESPACE' get tasks -l 'orka.ai/parent-task=$coordinator' -o json |
    jq -e '[.items[] | select(.spec.type == \"agent\" or .status.phase == \"Failed\")] | length > 0'" 600
kubectl -n "$ORKA_NAMESPACE" get tasks -l "orka.ai/parent-task=$coordinator" -o json >raw/children-dispatched.json
analysis_task=$(jq -er '[.items[] | select(.spec.type == "container")] | if length == 1 then .[0].metadata.name else error("expected one analysis job") end' raw/children-dispatched.json)
# Fail before narrating if the job could not run (for example, no free CPU).
analysis_phase=$(jq -r --arg name "$analysis_task" '.items[] | select(.metadata.name == $name) | .status.phase' raw/children-dispatched.json)
[[ $analysis_phase == Succeeded ]] || {
  bad "The analysis job is $analysis_phase: $(kubectl -n "$ORKA_NAMESPACE" get task "$analysis_task" -o jsonpath='{.status.message}')"
  exit 1
}
fibey_task=$(jq -er '[.items[] | select(.spec.type == "agent")] | if length == 1 then .[0].metadata.name else error("expected one Fibey Task") end' raw/children-dispatched.json)
pe 'orka task children "$coordinator"'
ok 'Two child Tasks under one coordinator: the job on AKS and Fibey in Foundry.'

chapter '3. The job analyzes the sensor history'
pe 'orka task result "$analysis_task"'
orka task result "$analysis_task" -o json >raw/analysis-result.json
ok 'Only the replaced transmitter changed. Flow, current, and vibration are normal.'

chapter '4. Fibey proposes a work order'
pe 'orka task approvals "$fibey_task" --watch --timeout 10m'
snapshot pending
python3 "$here/check.py" pending
# The short ID is what `orka task approvals` shows; the CLI resolves it.
approval=$(jq -er '.approvals[0].id | split(":") | last | .[0:12]' raw/approval-pending.json)
pe 'counts pending'
ok 'Fibey looked up inventory and asked for an inspection. No work order exists yet.'

chapter '5. Mark approves the exact work order'
pe 'orka task approvals "$fibey_task" "$approval"'
snapshot before-decision
python3 "$here/check.py" before-decision
pe 'orka task approve "$fibey_task" "$approval" --reason "Inspect the transmitter."'
orka task approvals "$fibey_task" "$approval" -o json >raw/decision.json
ok 'Mark'"'"'s decision is on record: who approved what, when, and why.'

chapter '6. The work order runs, and the result returns through the gateway'
pe 'orka task wait "$coordinator" --timeout 15m'
wait_event completed completed
snapshot final
orka task result "$coordinator" -o json >raw/coordinator-result.json
orka task result "$fibey_task" -o json >raw/fibey-result.json
scenario_collect_events "$coordinator" raw/coordinator-events.json
scenario_collect_events "$fibey_task" raw/fibey-events.json
fibey_flow_snapshot raw/installation-final.json
pe 'counts final'
pe 'answer'
ok 'One work order, created after approval. The answer went back through the gateway.'

chapter '7. On the record'
# Azure publishes per-minute token counts a few minutes late; wait for them off camera.
python3 "$here/check.py" meter
pe 'record'
ok 'Tasks, the approval, elapsed time, tokens, and estimated cost for this one alert.'

note "Full responses and receipts are saved in $run_dir"
cta
