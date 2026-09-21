# shellcheck shell=bash
# These are named demo resources, never a namespace-wide snapshot.
fibey_snapshot() {
  kubectl -n "$ORKA_NAMESPACE" get \
    "namespace/$ORKA_NAMESPACE" "agentruntime/$DEMO_FIBEY_RUNTIME" agent/demo-fibey \
    tool/read-inventory tool/create-work-order configmap/demo-fibey-tools \
    deployment/demo-fibey-tools service/demo-fibey-tools pvc/demo-fibey-tools -o json >"$1"
}
