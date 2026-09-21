#!/usr/bin/env bash
# Prepare the supplier and its gateway for demo 09, outside the walkthrough.
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster
scenario_require_commands openssl

here=$demo_root/09-governed-tools
state=$demo_root/setup/state/09-governed-tools/platform
umask 077
mkdir -p "$state"

# Installing the shared gateway controller is a separate, explicit setup step.
# Reuse the integration's pinned installer instead of copying its Helm workflow.
if [[ ${INSTALL_AGENTGATEWAY:-0} == 1 ]]; then
  integration=${AGENTGATEWAY_SOURCE:-$(dirname "$repo_root")/orka-integration-agentgateway}
  [[ -f $integration/scripts/install-agentgateway.sh ]] || {
    printf 'Set AGENTGATEWAY_SOURCE to an orka-integration-agentgateway checkout.\n' >&2
    exit 1
  }
  bash "$integration/scripts/install-agentgateway.sh" "$state"
fi
for crd in agentgatewaypolicies.agentgateway.dev agentgatewayparameters.agentgateway.dev \
           httproutes.gateway.networking.k8s.io outboundaccesspolicies.core.orka.ai; do
  kubectl get crd "$crd" -o name >/dev/null || {
    printf 'Install Orka and agentgateway first. See demo/09-governed-tools/README.md.\n' >&2
    exit 1
  }
done

[[ $ORKA_NAMESPACE =~ ^[a-z0-9][a-z0-9-]*$ ]] || { echo 'Invalid demo namespace' >&2; exit 1; }
namespace_uid=$(kubectl get namespace "$ORKA_NAMESPACE" -o jsonpath='{.metadata.uid}')
if [[ -f $state/namespace-uid && $(cat "$state/namespace-uid") != "$namespace_uid" ]]; then
  echo 'This setup directory belongs to another namespace. Use a fresh demo checkout or state directory.' >&2
  exit 1
fi

owned_or_absent() {
  local type=$1 name=$2 identity
  identity=$(kubectl -n "$ORKA_NAMESPACE" get "$type" "$name" --ignore-not-found \
    -o 'jsonpath={.metadata.uid}/{.metadata.labels.demo\.orka\.ai/name}')
  if [[ -n $identity && $identity != */09-governed-tools ]]; then
    printf 'Refusing to change an existing resource not owned by demo 09: %s/%s\n' "$type" "$name" >&2
    return 1
  fi
}

for resource in \
  gatewayclasses.gateway.networking.k8s.io/demo-supplier-agentgateway \
  agentgatewayparameters.agentgateway.dev/demo-supplier-gateway \
  gateways.gateway.networking.k8s.io/demo-supplier-gateway \
  httproutes.gateway.networking.k8s.io/demo-supplier-stock \
  agentgatewaypolicies.agentgateway.dev/demo-supplier-credential \
  agentgatewaypolicies.agentgateway.dev/demo-supplier-receipts \
  outboundaccesspolicies.core.orka.ai/demo-supplier-gateway \
  deployment/demo-supplier service/demo-supplier configmap/demo-supplier-code secret/demo-supplier-credential; do
  owned_or_absent "${resource%/*}" "${resource##*/}"
done
printf '%s\n' "$namespace_uid" >"$state/namespace-uid"

if [[ -z $(kubectl -n "$ORKA_NAMESPACE" get secret demo-supplier-credential --ignore-not-found -o name) ]]; then
  credential_file=$(mktemp "$state/credential.XXXXXX")
  trap 'rm -f "$credential_file"; stop_port_forward' EXIT
  openssl rand -hex 32 >"$credential_file"
  kubectl -n "$ORKA_NAMESPACE" create secret generic demo-supplier-credential \
    --from-file="Authorization=$credential_file" --dry-run=client -o json |
    jq '.metadata.labels = {"demo.orka.ai/name":"09-governed-tools"}' |
    kubectl create -f - >/dev/null
  rm -f "$credential_file"
fi
kubectl -n "$ORKA_NAMESPACE" create configmap demo-supplier-code \
  --from-file="supplier.py=$here/supplier.py" --dry-run=client -o json |
  jq '.metadata.labels = {"demo.orka.ai/name":"09-governed-tools"}' |
  kubectl apply -f - >/dev/null
supplier_code_sha=$(python3 - "$here/supplier.py" <<'PY'
import hashlib, pathlib, sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)
sed -e "s/DEMO_NAMESPACE/$ORKA_NAMESPACE/g" -e "s/SUPPLIER_CODE_SHA/$supplier_code_sha/g" \
  "$here/manifests/platform.yaml" >"$state/platform.yaml"
kubectl apply -f "$state/platform.yaml"
kubectl -n "$ORKA_NAMESPACE" rollout status deployment/demo-supplier --timeout=180s
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Programmed \
  gateway.gateway.networking.k8s.io/demo-supplier-gateway --timeout=180s
# The listener can be ready before its route and credential/logging policies.
gateway_config_ready() {
  kubectl -n "$ORKA_NAMESPACE" get \
    httproute.gateway.networking.k8s.io/demo-supplier-stock \
    agentgatewaypolicy/demo-supplier-credential agentgatewaypolicy/demo-supplier-receipts -o json |
    jq -e 'all(.items[];
      .metadata.generation as $generation |
      (if .kind == "HTTPRoute" then .status.parents[0].conditions
       else .status.ancestors[0].conditions end) as $conditions |
      ["Accepted", (if .kind == "HTTPRoute" then "ResolvedRefs" else "Attached" end)] as $needed |
      all($needed[]; . as $type | any($conditions[]?;
        .type == $type and .status == "True" and .observedGeneration == $generation))
    )' >/dev/null
}
wait_for 'the supplier route, credential, and request logging policies' gateway_config_ready 120
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Accepted \
  outboundaccesspolicy/demo-supplier-gateway --timeout=120s
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=ResolvedRefs \
  outboundaccesspolicy/demo-supplier-gateway --timeout=120s
printf 'Supplier setup ready in %s. Run demo/09-governed-tools/demo.sh to rehearse without recording.\n' "$ORKA_NAMESPACE"
