#!/usr/bin/env bash
# Prepare demo 10 on the existing local demo cluster. Does not run or record it.
set -euo pipefail
# shellcheck source-path=SCRIPTDIR
# shellcheck source=../lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster

: "${DEMO_PROVIDER_REF:=copilot}"
: "${DEMO_AI_MODEL:=claude-opus-4.7}"
here=$demo_root/10-reviewed-memory
setup_dir=$demo_root/setup/state/10-reviewed-memory/setup
umask 077
mkdir -p "$setup_dir"

kubectl -n "$ORKA_NAMESPACE" get provider "$DEMO_PROVIDER_REF" -o name >/dev/null
kubectl -n "$ORKA_NAMESPACE" get serviceaccount orka-client -o name >/dev/null

# Check ownership before applying the fixed demo names. Preserve existing work.
for resource in agent/demo-memory-author agent/demo-memory-reader role/demo-reviewed-memory rolebinding/demo-reviewed-memory; do
  kubectl -n "$ORKA_NAMESPACE" get "$resource" --ignore-not-found -o json >"$setup_dir/existing.json"
  if [[ -s $setup_dir/existing.json ]] && ! jq -e '.metadata.labels["demo.orka.ai/name"] == "10-reviewed-memory"' "$setup_dir/existing.json" >/dev/null; then
    printf 'Refusing to replace %s: it is not owned by this demo.\n' "$resource" >&2
    exit 1
  fi
done

for role in author reader; do
  kubectl -n "$ORKA_NAMESPACE" create --dry-run=client -f "$here/manifests/$role-agent.yaml" -o json |
    jq --arg namespace "$ORKA_NAMESPACE" --arg provider "$DEMO_PROVIDER_REF" --arg model "$DEMO_AI_MODEL" '
      .metadata.namespace = $namespace |
      .spec.providerRef.name = $provider | .spec.model.name = $model
    ' >"$setup_dir/$role-agent.json"
  kubectl -n "$ORKA_NAMESPACE" apply -f "$setup_dir/$role-agent.json"
done

# The namespace is also part of the RoleBinding subject. No credentials enter
# the manifests; the existing demo connection mints its client token at runtime.
kubectl -n "$ORKA_NAMESPACE" create --dry-run=client -f "$here/manifests/client-rbac.yaml" -o json |
  jq -s --arg namespace "$ORKA_NAMESPACE" '
    {apiVersion:"v1",kind:"List",items:[.[] |
      (if .kind == "List" then .items[] else . end) |
      .metadata.namespace = $namespace |
      if .kind == "RoleBinding" then .subjects[].namespace = $namespace else . end]}
  ' >"$setup_dir/client-rbac.json"
kubectl -n "$ORKA_NAMESPACE" apply -f "$setup_dir/client-rbac.json"

printf 'Prepared demo-memory-author and demo-memory-reader in %s.\n' "$ORKA_NAMESPACE"
printf 'Run demo/10-reviewed-memory/demo.sh to rehearse without recording.\n'
