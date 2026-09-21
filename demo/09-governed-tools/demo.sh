#!/usr/bin/env bash
# Orka: check the supplier, keep purchasing closed
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_init 09-governed-tools
scenario_connect
here=$demo_root/09-governed-tools
: "${DEMO_PROVIDER_REF:=copilot}"
: "${DEMO_AI_MODEL:=claude-opus-4.7}"

# Each run gets its own Tools, Agent, Tasks, and evidence directory. Infrastructure
# and credentials are prepared separately by demo/setup/governed-tools.sh.
python3 "$here/prepare.py" "$run_dir" "$ORKA_NAMESPACE" "$run_id" "$DEMO_PROVIDER_REF" "$DEMO_AI_MODEL"
cp "$here/evidence.py" evidence.py
lookup_task=$(jq -r '.lookupTask' run.json)
order_task=$(jq -r '.orderTask' run.json)
kubectl -n "$ORKA_NAMESPACE" get provider "$DEMO_PROVIDER_REF" -o name >/dev/null
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Programmed \
  gateway.gateway.networking.k8s.io/demo-supplier-gateway --timeout=120s >/dev/null
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Accepted \
  outboundaccesspolicy/demo-supplier-gateway --timeout=120s >/dev/null
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=ResolvedRefs \
  outboundaccesspolicy/demo-supplier-gateway --timeout=120s >/dev/null
kubectl create -f tools-and-agent.json >raw/resources-created.txt
kubectl -n "$ORKA_NAMESPACE" get outboundaccesspolicy demo-supplier-gateway -o json >raw/outbound-policy.json
kubectl -n "$ORKA_NAMESPACE" get httproute.gateway.networking.k8s.io demo-supplier-stock -o json >raw/stock-route.json
kubectl -n "$ORKA_NAMESPACE" get agentgatewaypolicy demo-supplier-credential -o json >raw/credential-policy.json

# Pick a free local port; kubectl prints the chosen number into a private log.
supplier_pf=
cleanup() {
  if [[ -n $supplier_pf ]]; then
    kill "$supplier_pf" 2>/dev/null || true
    wait "$supplier_pf" 2>/dev/null || true
  fi
  stop_port_forward
}
trap cleanup EXIT
kubectl -n "$ORKA_NAMESPACE" port-forward svc/demo-supplier :8080 >raw/supplier-forward.log 2>&1 &
supplier_pf=$!
wait_for 'supplier connection' "grep -q 'Forwarding from 127.0.0.1:' raw/supplier-forward.log" 30
supplier_port=$(sed -n 's/.*127\.0\.0\.1:\([0-9]*\) ->.*/\1/p' raw/supplier-forward.log | head -n 1)
supplier_receipts() {
  curl -fsS --max-time 10 "http://127.0.0.1:$supplier_port/receipts?run=$run_id"
}
gateway_logs() {
  kubectl -n "$ORKA_NAMESPACE" logs deployment/demo-supplier-gateway -c agentgateway >"$1"
}
capture_task() {
  local name=$1 prefix=$2
  wait_task "$name" 600
  kubectl -n "$ORKA_NAMESPACE" get task "$name" -o json >"raw/$prefix-task.json"
  orka task result "$name" -o json >"raw/$prefix-result.json"
  scenario_collect_events "$name" "raw/$prefix-events.json"
}
show_calls() {
  jq -r '.events[] | select(.type | startswith("ToolCall")) | [.type, .summary] | @tsv' "$1"
}

supplier_receipts >raw/supplier-before.json
python3 evidence.py initial

banner 'Check the supplier, keep purchasing closed' \
  'An assistant can look up stock. The platform controls whether it can buy.'

chapter '1. The shortage'
say 'Orka runs AI work and keeps a record of what happened.'
say 'Our inventory team needs 20 more filters. The supplier can check stock'
say 'and take orders. Purchasing has not been enabled for this assistant.'
pe 'cat request.txt'
pe "jq '{ordersCreated: .totalOrdersCreated}' raw/supplier-before.json"
say 'The supplier here is a small HTTP service with made-up stock.'
say 'Its requests and receipts will come from this run.'

chapter '2. How the assistant reaches the supplier'
say 'A Tool is a named action the model can ask Orka to carry out.'
say 'These are selected fields from the stock Tool and its outbound policy.'
pe "jq '{name: .metadata.name, operation: .spec.http}' stock-tool.json"
pe "jq '{name: .metadata.name, gateway: .spec.gateway}' raw/outbound-policy.json"
say 'The policy tells Orka to send this request through agentgateway.'
say 'The model can request both a stock lookup and an order in this demo.'

chapter '3. What the gateway permits'
say 'A gateway sits between Orka and the supplier. A route says which'
say 'requests it can forward. This route permits only the stock lookup.'
pe "jq '{host: .spec.hostnames, match: .spec.rules[0].matches}' raw/stock-route.json"
pe "jq '{credentialSecret: .spec.backend.auth.secretRef.name}' raw/credential-policy.json"
say 'A Secret holds the supplier credential. The gateway supplies it.'
say 'The Tool definition contains the operation, with no supplier credential.'

chapter '4. Run the lookup'
say "A Task is Orka's record of one piece of work. Let's create one."
pe 'orka task create -f lookup.json'
capture_task "$lookup_task" lookup
supplier_receipts >raw/supplier-lookup.json
gateway_logs raw/gateway-lookup.jsonl
python3 evidence.py lookup >lookup-evidence.json
pe 'show_calls raw/lookup-events.json'
pe "jq -r '.result' raw/lookup-result.json"

chapter "5. Check the supplier's receipt"
say "This is the supplier's own record. It shows the stock request arrived"
say 'and the credential was accepted.'
pe "jq '.receipts[] | {method, path, credentialAccepted, item, available}' raw/supplier-lookup.json"

chapter '6. Ask it to place an order'
say 'Now we explicitly ask the assistant to use its order Tool.'
say 'Requesting an action does not give the model permission to perform it.'
pe 'orka task create -f order.json'
capture_task "$order_task" order
supplier_receipts >raw/supplier-after.json
gateway_logs raw/gateway-order.jsonl
# Prove the exact refusal first. pex alone would accept any command failure.
python3 evidence.py order >order-evidence.json
pe 'show_calls raw/order-events.json'
say 'The recorded tool result is a failure. HTTP 404 means this gateway'
say 'had no matching route for the order request.'
pex 'python3 evidence.py order-result'
pe "jq -r '.result' raw/order-result.json"

chapter '7. Check what happened'
say 'The gateway saw the order attempt, but the supplier did not receive it.'
pe 'python3 evidence.py summary'
say 'The assistant checked availability through an authenticated API.'
say 'Purchasing stayed closed when it requested an order. The platform'
say 'controlled the route and the supplier credential for these two Tools.'
note "Full responses and receipts are saved in $run_dir"
