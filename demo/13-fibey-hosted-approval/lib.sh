# shellcheck shell=bash
# These are named demo resources, never a namespace-wide snapshot.
fibey_hosted_snapshot() {
  kubectl -n "$ORKA_NAMESPACE" get \
    "namespace/$ORKA_NAMESPACE" \
    agentruntime/fibey-on-aks-runtime agentruntime/fibey-on-foundry-runtime \
    agent/demo-fibey-aks agent/demo-fibey-foundry \
    tool/read-inventory tool/create-work-order configmap/demo-fibey-tools \
    deployment/demo-fibey-tools service/demo-fibey-tools pvc/demo-fibey-tools -o json >"$1"
}
