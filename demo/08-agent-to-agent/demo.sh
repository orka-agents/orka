#!/usr/bin/env bash
# Orka — the order desk asks for help
# Sam's order-desk app asks the inventory team's agent for advice, retries safely, and keeps the conversation going.
# Shared helpers define the run variables; pe evaluates the quoted commands below.
# shellcheck disable=SC2034,SC2154
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_init 08-agent-to-agent
here=$demo_root/08-agent-to-agent
evidence=$here/evidence.py
state=$demo_root/setup/state/08-agent-to-agent/installation
[[ -f $state/ready.json && -x $repo_root/bin/demo-a2a/a2a-client ]] || {
  printf 'Run bash demo/setup/agent-to-agent.sh first.\n' >&2; exit 1;
}
context=$(kubectl config current-context)
namespace_uid=$(kubectl get namespace "$ORKA_NAMESPACE" -o jsonpath='{.metadata.uid}')
cluster_uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
jq -e --arg context "$context" --arg ns "$ORKA_NAMESPACE" --arg uid "$namespace_uid" --arg cluster "$cluster_uid" '
  .target == {context:$context, namespace:$ns, namespaceUID:$uid, clusterUID:$cluster}
' "$state/ready.json" >/dev/null || { printf 'The prepared adapter belongs to another cluster or namespace.\n' >&2; exit 1; }
installation=$(jq -er '.installation' "$state/ready.json")
a2a_url=$(jq -er '.url' "$state/ready.json")
port=${a2a_url##*:}
[[ $a2a_url == https://127.0.0.1:* && $port =~ ^[0-9]+$ ]] || exit 1
kubectl -n "$ORKA_NAMESPACE" get deployment/demo-a2a-adapter gateway.gateway.orka.ai/demo-a2a \
  gatewaybinding.gateway.orka.ai/demo-a2a-inventory agent/demo-a2a-inventory -o json >raw/installed.json
jq -e --slurpfile ready "$state/ready.json" --arg installation "$installation" '
  ([.items[] | {kind, name:.metadata.name, uid:.metadata.uid}] | sort_by(.kind)) ==
    ($ready[0].identities | sort_by(.kind)) and
  all(.items[]; .metadata.labels["demo.orka.ai/installation"] == $installation)
' raw/installed.json >/dev/null || { printf 'Installed A2A object identities changed; run setup again.\n' >&2; exit 1; }
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready gateway.gateway.orka.ai/demo-a2a --timeout=60s >/dev/null
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready gatewaybinding.gateway.orka.ai/demo-a2a-inventory --timeout=60s >/dev/null
scenario_connect

# These connection options are prepared once, as they would be in an application.
# Only the caller token file is given to the real SDK client.
a2a-client() {
  "$repo_root/bin/demo-a2a/a2a-client" -url "$a2a_url" -token-file "$state/client.token" -ca-file "$state/ca.crt" "$@"
}
_a2a_pf_pid=
stop_a2a_forward() {
  [[ -n $_a2a_pf_pid ]] || return 0
  kill "$_a2a_pf_pid" 2>/dev/null || true
  wait "$_a2a_pf_pid" 2>/dev/null || true
  _a2a_pf_pid=
}
trap 'stop_a2a_forward; stop_port_forward' EXIT
a2a_forward_ready() {
  kill -0 "$_a2a_pf_pid" 2>/dev/null &&
    grep -Fq "Forwarding from 127.0.0.1:$port" "$raw_dir/adapter-port-forward.log" &&
    curl --silent --fail --max-time 2 --cacert "$state/ca.crt" "$a2a_url/readyz" >/dev/null
}
start_a2a_forward() {
  python3 -c 'import socket,sys; s=socket.socket(); s.bind(("127.0.0.1", int(sys.argv[1]))); s.close()' "$port"
  kubectl -n "$ORKA_NAMESPACE" port-forward --address 127.0.0.1 svc/demo-a2a-adapter "$port:8443" >"$raw_dir/adapter-port-forward.log" 2>&1 &
  _a2a_pf_pid=$!
  wait_for "the verified HTTPS adapter connection" a2a_forward_ready 60
}
start_a2a_forward
cp "$here/order.txt" order.txt
request_id=order-$run_id
followup_id=reply-$run_id
conversation=customer-$run_id

# Preserve every polled event, including an unexpected denial. No invented progress.
wait_event() {
  local event=$1 prefix=$2 wanted=$3 attempt file phase
  for ((attempt = 1; attempt <= 200; attempt++)); do
    printf -v file '%s/%s-poll-%03d.json' "$raw_dir" "$prefix" "$attempt"
    orka gateway events get "$event" -o json >"$file"
    phase=$(jq -er '.state' "$file")
    case $phase in
      Rejected|DeadLettered|Expired) printf 'Orka ended this request in %s; inspect %s.\n' "$phase" "$file" >&2; return 1 ;;
    esac
    if { [[ $wanted == dispatched ]] && jq -e '.taskName != null and .taskUid != null and .taskUid != ""' "$file" >/dev/null; } ||
       { [[ $wanted == completed && $phase == Completed ]] && jq -e '.deliveryId != null and .deliveryId != ""' "$file" >/dev/null; }; then
      cp "$file" "$raw_dir/$prefix-event.json"
      return 0
    fi
    sleep 3
  done
  printf 'Timed out waiting for the saved %s record.\n' "$wanted" >&2
  return 1
}
snapshot_tasks() {
  kubectl -n "$ORKA_NAMESPACE" get tasks -l gateway.orka.ai/gateway=demo-a2a -o json >"$1"
}
snapshot_pods() {
  kubectl -n "$ORKA_NAMESPACE" get pods -l "demo.orka.ai/installation=$installation,app.kubernetes.io/name=demo-a2a-adapter" -o json >"$1"
}

# --- on-camera helpers ---------------------------------------------------
# card — what the inventory agent says it can do, from its Agent Card.
card() {
  curl --silent --show-error --fail --cacert "$state/ca.crt" "$a2a_url/.well-known/agent-card.json" >raw/card.json
  jq -r '"name:        " + .name, "description: " + .description, (.skills[] | "skill:       " + .name)' raw/card.json
}
# ask ID TEXT — send one message in this conversation and return at once.
ask() {
  a2a-client -message-id "$1" -context-id "$conversation" -return-immediately -text "$2"
}
# receipt FILE — the acknowledgement: a reference, the conversation, the state.
receipt() {
  jq -r '"reference:    " + (.id | .[:24]) + "…", "conversation: " + .contextId, "state:        " + .status.state' "$1"
}
# answer REF — the answer text for a reference, from the A2A side.
answer() {
  a2a-client -task-id "$1" | jq -r '.artifacts[].parts[].text'
}
# summary — the closing table, checked against every saved record.
summary() {
  python3 "$evidence" summary raw
}

banner "Orka — the order desk asks for help" \
  "Sam's order-desk app asks the inventory team's agent for advice, retries safely, and keeps the conversation going."
say "Sam runs the order desk. A customer wants 24 filters today and only 18 are"
say "in stock. Sam's app will ask the inventory team's agent, which runs in Orka."
helpers_note card, ask, receipt, answer, summary

chapter "Sam has a customer waiting"
pe 'cat order.txt'
ok "That is the whole request. No prompt engineering, no API key."

chapter "Find the inventory agent"
say "Sam's app and Orka speak A2A, a common format for asking another agent for"
say "help. The agent publishes a card saying what it does."
pe 'card'
ok "One skill: recommend a customer reply from the stock facts supplied."

chapter "Send the work and get a receipt"
say "The app gives the request a stable ID and asks for an immediate acknowledgement."
pe 'ask "$request_id" "$(cat order.txt)" > raw/first-admission.json'
task_ref=$(jq -er '.id' raw/first-admission.json)
event_id=$(python3 "$evidence" event-id raw/first-admission.json)
pe 'receipt raw/first-admission.json'
say "Behind that reference is a Task: Orka's record of one piece of work."
wait_event "$event_id" first dispatched
first_task=$(jq -er '.taskName' raw/first-event.json)
kubectl -n "$ORKA_NAMESPACE" get task "$first_task" -o json >raw/first-task.json
python3 "$evidence" correlate raw/first-admission.json raw/first-event.json raw/first-task.json
pe 'kubectl -n "$ORKA_NAMESPACE" get task "$first_task"'
ok "Accepted. Sam's app can check on it with the reference alone."

chapter "Read the recommendation"
pe 'wait_task "$first_task" 600'
wait_event "$event_id" first-completed completed
pe 'answer "$task_ref" | tee raw/first-answer.txt'
a2a-client -task-id "$task_ref" >raw/first-answer.json
pe 'result "$first_task"'
orka task result "$first_task" -o json >raw/first-result.json
python3 "$evidence" reply raw/first-answer.json raw/first-result.json
ok "Same answer through A2A and in Orka's record. It used the stock and delivery facts we gave it."
snapshot_tasks raw/tasks-after-first.json

chapter "Sam's app retries"
say "Apps retry when they are unsure a request arrived. This sends the same"
say "request ID, conversation, and text again."
pe 'ask "$request_id" "$(cat order.txt)" > raw/retry.json'
orka gateway events get "$event_id" -o json >raw/retry-event.json
kubectl -n "$ORKA_NAMESPACE" get task "$first_task" -o json >raw/retry-task.json
snapshot_tasks raw/tasks-after-retry.json
pe 'python3 "$evidence" retry raw'
ok "Same reference, same Task. A retry does not do the work twice."

chapter "Sam asks a follow-up"
say "A new request ID starts new work. Keeping the conversation ID connects it"
say "to the earlier exchange; Orka calls that shared conversation a Session."
pe 'ask "$followup_id" "Make that two sentences." > raw/followup-admission.json'
followup_ref=$(jq -er '.id' raw/followup-admission.json)
followup_event=$(python3 "$evidence" event-id raw/followup-admission.json)
wait_event "$followup_event" followup dispatched
followup_task=$(jq -er '.taskName' raw/followup-event.json)
pe 'wait_task "$followup_task" 600'
wait_event "$followup_event" followup-completed completed
kubectl -n "$ORKA_NAMESPACE" get task "$followup_task" -o json >raw/followup-task.json
python3 "$evidence" correlate raw/followup-admission.json raw/followup-completed-event.json raw/followup-task.json
a2a-client -task-id "$followup_ref" >raw/followup-answer.json
orka task result "$followup_task" -o json >raw/followup-result.json
python3 "$evidence" reply raw/followup-answer.json raw/followup-result.json
jq -e --slurpfile first raw/first-completed-event.json '.sessionName == $first[0].sessionName and .taskUid != $first[0].taskUid' raw/followup-completed-event.json >/dev/null
pe 'answer "$followup_ref"'
snapshot_tasks raw/tasks-after-followup.json
kubectl -n "$ORKA_NAMESPACE" get task "$first_task" "$followup_task" -o json >raw/conversation-tasks.json
pe 'python3 "$evidence" conversation raw/conversation-tasks.json'
ok "Two Tasks, one Session. The follow-up knew what the first request said."

pe 'summary'
cta
