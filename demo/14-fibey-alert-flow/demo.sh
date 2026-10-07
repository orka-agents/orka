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
simulator_port=${DEMO_FLOW_SIMULATOR_PORT:-8099}
forward simulator demo-fibey-tools "$simulator_port" 8099
wait_for 'the work-order receipt service' "grep -q 'Forwarding from 127.0.0.1:' raw/simulator-forward.log" 30
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
    if { [[ $1 == dispatched ]] && jq -e '(.taskName // "") != "" and (.taskUid // "") != ""' "$file" >/dev/null; } ||
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

snapshot initial
python3 "$here/check.py" initial

# Mark narrates this video live after the slides introduce Orka and the flow,
# so each step gets a clean screen with only its evidence. Step numbers match
# the flow slide.
step() { printf '\033[H\033[2J'; chapter "$1"; }

step '1 · Alert in, through the gateway'
aside 'Recorded live on AKS and Microsoft Foundry, with sample plant data and a test maintenance system.'
# The A2A client plays the plant's monitoring system; its reply is kept for the checks.
a2a -message-id "$alert_id" -context-id "$conversation" -return-immediately -text "$(cat alert.txt)" >raw/admission.json
task_ref=$(jq -er '.id' raw/admission.json)
event_id=$(python3 "$here/check.py" event-id raw/admission.json)
wait_event dispatched dispatched
coordinator=$(jq -er '.taskName' raw/event-dispatched.json)
pe 'orka gateway events get "$event_id"'
ok 'Orka opened one Task for the alert.'
nap 1.5

step '2 · Orka runs a data-analysis job on AKS'
# Wait for the hand-off to Fibey, or stop early if the analysis job failed.
wait_for 'the Foundry specialist Task' \
  "kubectl -n '$ORKA_NAMESPACE' get tasks -l 'orka.ai/parent-task=$coordinator' -o json |
    jq -e '[.items[] | select(.spec.type == \"agent\" or .status.phase == \"Failed\")] | length > 0'" 600
kubectl -n "$ORKA_NAMESPACE" get tasks -l "orka.ai/parent-task=$coordinator" -o json >raw/children-dispatched.json
analysis_task=$(jq -er '[.items[] | select(.spec.type == "container")] | if length == 1 then .[0].metadata.name else error("expected one analysis job") end' raw/children-dispatched.json)
# Fail before showing anything if the job could not run (for example, no free CPU).
analysis_phase=$(jq -r --arg name "$analysis_task" '.items[] | select(.metadata.name == $name) | .status.phase' raw/children-dispatched.json)
[[ $analysis_phase == Succeeded ]] || {
  bad "The analysis job is $analysis_phase: $(kubectl -n "$ORKA_NAMESPACE" get task "$analysis_task" -o jsonpath='{.status.message}')"
  exit 1
}
fibey_task=$(jq -er '[.items[] | select(.spec.type == "agent")] | if length == 1 then .[0].metadata.name else error("expected one Fibey Task") end' raw/children-dispatched.json)
# The time window must hold exactly this alert's three Tasks.
orka task list --since 3m -o json >raw/task-window.json
jq -e --arg c "$coordinator" --arg a "$analysis_task" --arg f "$fibey_task" \
  '[.[].name] | sort == ([$c, $a, $f] | sort)' raw/task-window.json >/dev/null || {
  bad 'Other Tasks started in the last 3 minutes; wait and record again.'
  exit 1
}
pe 'orka task list --since 3m'
nap 1.5
pe 'orka task result "$analysis_task"'
orka task result "$analysis_task" -o json >raw/analysis-result.json
ok 'Only the replaced transmitter changed. The pump runs normally.'
nap 1.5

step '3 · A specialist agent in Microsoft Foundry proposes a work order'
orka task approvals "$fibey_task" --watch --timeout 10m >raw/approval-watch.txt
snapshot pending
python3 "$here/check.py" pending
# The short ID is what `orka task approvals` shows; the CLI resolves it.
approval=$(jq -er '.approvals[0].id | split(":") | last | .[0:12]' raw/approval-pending.json)
pe 'orka task approvals "$fibey_task" "$approval"'
snapshot before-decision
python3 "$here/check.py" before-decision
ok 'Orka holds this exact action. No work order exists yet.'
nap 3

step '4 · Mark approves the exact work order'
# The decision prints the same block again; the record below says it in one line.
decide='orka task approve "$fibey_task" "$approval" --reason "Inspect the transmitter."'
p "$decide"
eval "$decide" >raw/approve.txt
orka task approvals "$fibey_task" "$approval" -o json >raw/decision.json
ok "$(jq -er '"Approved by \(.decisionActor | split(":") | last): \"\(.decisionReason)\""' raw/decision.json)"
nap 2

step '5 · The work order runs   6 · The result goes out through the gateway'
orka task wait "$coordinator" --timeout 15m >raw/coordinator-wait.txt
wait_event completed completed
delivery_id=$(jq -er '.deliveryId' raw/event-completed.json)
wait_for 'the gateway delivery' \
  "orka gateway deliveries get '$delivery_id' -o json | tee raw/delivery.json | jq -e '.state == \"Delivered\"'" 60
snapshot final
a2a -task-id "$task_ref" >raw/answer.json
orka task result "$coordinator" -o json >raw/coordinator-result.json
orka task result "$fibey_task" -o json >raw/fibey-result.json
scenario_collect_events "$coordinator" raw/coordinator-events.json
scenario_collect_events "$fibey_task" raw/fibey-events.json
fibey_flow_snapshot raw/installation-final.json
# The service's own count (one work order) is checked off camera from the final snapshot.
pe 'orka task result "$coordinator"'
ok 'The gateway delivered this answer back to the monitoring system.'
nap 2

step 'On the record'
# Azure publishes per-minute token counts a few minutes late; wait for them off camera.
python3 "$here/check.py" meter
python3 "$here/check.py" final
nap 2
