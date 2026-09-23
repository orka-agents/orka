#!/usr/bin/env bash
# Orka — check the supplier, keep purchasing closed
# Jordan's assistant may look up stock but not buy. The gateway and the supplier's own receipts prove which request got through.
# pe expands the visibly typed commands when it executes them.
# shellcheck disable=SC2016
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
# Used by the commands evaluated in pe below.
# shellcheck disable=SC2034
stock_tool=$(jq -r '.stockTool' run.json)
# shellcheck disable=SC2034
order_tool=$(jq -r '.orderTool' run.json)
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
# --- on-camera helpers ---------------------------------------------------
# orders_created — the supplier's own count of orders for this run.
orders_created() {
  jq -r '"orders created: \(.totalOrdersCreated)"' "$1"
}
# route — the one request the gateway is configured to forward.
route() {
  jq -r '"host:   " + .spec.hostnames[0], (.spec.rules[0].matches[] | "allows: " + .method + " " + .path.value)' raw/stock-route.json
}
# supplier_saw FILE — the supplier's own record of what reached it.
supplier_saw() {
  jq -r '.receipts[] | "\(.method) \(.path)  credential accepted: \(.credentialAccepted)  available: \(.available)"' "$1"
}
# summary — both sides of both requests, from gateway logs and supplier receipts.
summary() {
  python3 evidence.py summary
}

supplier_receipts >raw/supplier-before.json
python3 evidence.py initial

banner 'Orka — check the supplier, keep purchasing closed' \
  "Jordan's assistant may look up stock but not buy. The gateway and the supplier's own receipts prove which request got through."
say "Jordan, on the inventory team, needs 20 more filters. An assistant can ask"
say "the supplier. Whether it may also buy is the platform team's decision, not the model's."
helpers_note orders_created, route, supplier_saw, summary

chapter '1. Jordan needs 20 filters'
pe 'cat request.txt'
say 'The supplier here is a small service with made-up stock. Its receipts are the proof.'
pe 'orders_created raw/supplier-before.json'
ok 'Zero orders before we start.'

chapter '2. The assistant can request two actions'
say 'A Tool is an action the model can ask Orka to carry out. This assistant has'
say 'two: check stock and place an order. Both go through a gateway.'
pe 'orka tool get "$stock_tool"'
pe 'orka tool get "$order_tool"'
ok 'Neither Tool contains the supplier credential. A Secret holds it; the gateway supplies it.'

chapter '3. The gateway allows one of them'
say 'The gateway forwards only what its routes allow. There is one route.'
pe 'route'
ok 'Stock lookups pass. Nothing is configured for orders.'

chapter '4. Ask for the stock check'
say "A Task is Orka's record of one piece of work."
pe 'orka task create -f lookup.json'
capture_task "$lookup_task" lookup
supplier_receipts >raw/supplier-lookup.json
gateway_logs raw/gateway-lookup.jsonl
python3 evidence.py lookup >lookup-evidence.json
say 'The Task keeps every event. These are the tool calls: what was asked, and what came back.'
pe 'orka task events "$lookup_task" --type ToolCallStarted --type ToolCallCompleted --type ToolCallFailed'
pe 'orka task result "$lookup_task"'
say "And the supplier's own record of what arrived:"
pe 'supplier_saw raw/supplier-lookup.json'
ok 'The lookup reached the supplier with the credential the model never saw.'

chapter '5. Ask it to buy'
say 'Now we explicitly ask the assistant to place the order.'
say 'Asking for an action does not grant permission to perform it.'
pe 'orka task create -f order.json'
capture_task "$order_task" order
supplier_receipts >raw/supplier-after.json
gateway_logs raw/gateway-order.jsonl
# Prove the exact refusal first. pex alone would accept any command failure.
python3 evidence.py order >order-evidence.json
pe 'orka task events "$order_task" --type ToolCallStarted --type ToolCallCompleted --type ToolCallFailed'
say 'The gateway had no route for the order, so it refused with HTTP 404.'
pex 'python3 evidence.py order-result'
pe 'orka task result "$order_task"'
pe 'orders_created raw/supplier-after.json'
ok 'The assistant asked. The platform said no. The supplier never heard about it.'

pe 'summary'
note "Full responses and receipts are saved in $run_dir"
cta
