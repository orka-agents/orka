# shellcheck shell=bash
# These are named demo resources, never a namespace-wide snapshot. Demo 13's
# Foundry Fibey, Tools, and work-order service are part of this demo's path.
fibey_flow_snapshot() {
  kubectl -n "$ORKA_NAMESPACE" get \
    "namespace/$ORKA_NAMESPACE" \
    gateway.gateway.orka.ai/fibey-alerts gatewaybinding.gateway.orka.ai/demo-maintenance-coordinator \
    deployment/fibey-alerts-adapter provider/demo-foundry-models \
    agent/demo-maintenance-coordinator agent/demo-fibey-foundry agentruntime/fibey-on-foundry-runtime \
    tool/read-inventory tool/create-work-order deployment/demo-fibey-tools pvc/demo-fibey-tools -o json >"$1"
}
