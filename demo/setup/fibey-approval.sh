#!/usr/bin/env bash
# Prepare the counted tools and presenter access for a qualified Fibey runtime.
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster
scenario_require_commands git
here=$demo_root/11-fibey-approval
# shellcheck source=demo/11-fibey-approval/lib.sh
source "$here/lib.sh"
: "${DEMO_FIBEY_RUNTIME:=fibey-approval-agentkit-runtime}"
: "${DEMO_APPROVAL_SOURCE:=$repo_root}"
if [[ ! -f $DEMO_APPROVAL_SOURCE/examples/human-approval-v2/simulated_tools.py ]]; then
  printf 'Set DEMO_APPROVAL_SOURCE to the Orka checkout containing PR #589, such as ~/projects/orka.human-approval-v2.\n' >&2
  exit 1
fi
state=$demo_root/setup/state/11-fibey-approval/platform
umask 077
mkdir -p "$state"
revision=$(git -C "$DEMO_APPROVAL_SOURCE" rev-parse HEAD)
# Read one committed revision even if other work continues in that checkout.
source_dir=$state/source
mkdir -p "$source_dir"
for file in simulated_tools.py simulator.yaml.example tools.yaml; do
  git -C "$DEMO_APPROVAL_SOURCE" show "$revision:examples/human-approval-v2/$file" >"$source_dir/$file"
done
context=$(kubectl config current-context)
namespace_uid=$(kubectl get namespace "$ORKA_NAMESPACE" -o jsonpath='{.metadata.uid}')
if [[ -f $state/target.json ]]; then
  jq -e --arg context "$context" --arg namespace "$ORKA_NAMESPACE" --arg uid "$namespace_uid" \
    '. == {context:$context,namespace:$namespace,uid:$uid}' "$state/target.json" >/dev/null || {
    echo 'This setup directory belongs to another cluster or namespace.' >&2; exit 1;
  }
fi
kubectl -n "$ORKA_NAMESPACE" get serviceaccount orka-client -o name >/dev/null
kubectl -n "$ORKA_NAMESPACE" get agentruntime "$DEMO_FIBEY_RUNTIME" -o json >"$state/runtime.json"
jq -e '.spec.contractVersion == "orka.harness.v2" and
  .spec.capabilities.mcpPolicy == {allowedTools:["create-work-order","read-inventory"],
  disallowedTools:[],allowBash:false,approvalRequiredTools:["create-work-order"]}' \
  "$state/runtime.json" >/dev/null || {
  echo 'Prepare Fibey with the approval policy from PR #589. The earlier read-only Fibey runtime is insufficient.' >&2
  exit 1
}
for resource in agent/demo-fibey tool/read-inventory tool/create-work-order \
  configmap/demo-fibey-tools deployment/demo-fibey-tools service/demo-fibey-tools pvc/demo-fibey-tools \
  role/demo-fibey-reviewer rolebinding/demo-fibey-reviewer; do
  existing=$(kubectl -n "$ORKA_NAMESPACE" get "$resource" --ignore-not-found \
    -o 'jsonpath={.metadata.uid}/{.metadata.labels.demo\.orka\.ai/name}')
  if [[ -n $existing && $existing != */11-fibey-approval ]]; then
    printf 'Refusing to change %s, which is not owned by demo 11. Use a dedicated demo namespace.\n' "$resource" >&2
    exit 1
  fi
done
jq -n --arg context "$context" --arg namespace "$ORKA_NAMESPACE" --arg uid "$namespace_uid" \
  '{context:$context,namespace:$namespace,uid:$uid}' >"$state/target.json"

# Reuse #589's persistent counted HTTP service. Never reset its receipt database.
kubectl -n "$ORKA_NAMESPACE" create configmap demo-fibey-tools \
  --from-file="simulated_tools.py=$source_dir/simulated_tools.py" --dry-run=client -o json |
  jq '.metadata.labels = {"demo.orka.ai/name":"11-fibey-approval"}' >"$state/code.json"
code_sha=$(python3 - "$source_dir/simulated_tools.py" <<'PY'
import hashlib, pathlib, sys
print(hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest())
PY
)
kubectl -n "$ORKA_NAMESPACE" create --dry-run=client --validate=false \
  -f "$source_dir/simulator.yaml.example" -o json |
  jq -s --arg namespace "$ORKA_NAMESPACE" --arg sha "$code_sha" '
    {apiVersion:"v1",kind:"List",items:[.[] | (if .kind == "List" then .items[] else . end) |
      walk(if type == "string" and . == "human-approval-tools" then "demo-fibey-tools" else . end) |
      .metadata.namespace = $namespace | .metadata.labels = {"demo.orka.ai/name":"11-fibey-approval"} |
      if .kind == "Deployment" then
        .spec.template.metadata.annotations["demo.orka.ai/source-sha"] = $sha |
        .spec.template.spec.automountServiceAccountToken = false |
        .spec.template.spec.containers[0].image = "docker.io/library/python:3.14.7-alpine3.24@sha256:c6ead215bfd31f1e433d968853b7a769989117115b728874824e6c0a27cb96fc"
      else . end]}
  ' >"$state/simulator.json"
kubectl -n "$ORKA_NAMESPACE" create --dry-run=client --validate=false -f "$source_dir/tools.yaml" -o json |
  jq -s --arg namespace "$ORKA_NAMESPACE" '
    {apiVersion:"v1",kind:"List",items:[.[] | (if .kind == "List" then .items[] else . end) |
      .metadata.namespace = $namespace | .metadata.labels = {"demo.orka.ai/name":"11-fibey-approval"} |
      .spec.http.url |= sub("http://human-approval-tools:"; "http://demo-fibey-tools:")]}
  ' >"$state/tools.json"
jq -n --arg namespace "$ORKA_NAMESPACE" --arg runtime "$DEMO_FIBEY_RUNTIME" '
  def metadata($name): {name:$name,namespace:$namespace,labels:{"demo.orka.ai/name":"11-fibey-approval"}};
  {apiVersion:"v1",kind:"List",items:[
    {apiVersion:"core.orka.ai/v1alpha1",kind:"Agent",metadata:metadata("demo-fibey"),
     spec:{runtime:{runtimeRef:{name:$runtime}}}},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"Role",metadata:metadata("demo-fibey-reviewer"),
     rules:[{apiGroups:["core.orka.ai"],resources:["tasks/approvals"],verbs:["update"]},
            {apiGroups:["core.orka.ai"],resources:["tasks"],verbs:["patch"]}]},
    {apiVersion:"rbac.authorization.k8s.io/v1",kind:"RoleBinding",metadata:metadata("demo-fibey-reviewer"),
     subjects:[{kind:"ServiceAccount",name:"orka-client",namespace:$namespace}],
     roleRef:{apiGroup:"rbac.authorization.k8s.io",kind:"Role",name:"demo-fibey-reviewer"}}
  ]}
' >"$state/agent-and-reviewer.json"
kubectl apply -f "$state/code.json" -f "$state/simulator.json" -f "$state/tools.json" -f "$state/agent-and-reviewer.json"
kubectl -n "$ORKA_NAMESPACE" rollout status deployment/demo-fibey-tools --timeout=180s
runtime_ready() {
  kubectl -n "$ORKA_NAMESPACE" get agentruntime "$DEMO_FIBEY_RUNTIME" -o json |
    jq -e '.status.ready == true and .status.observedGeneration == .metadata.generation' >/dev/null
}
wait_for 'Fibey approval runtime conformance' runtime_ready 180
fibey_snapshot "$state/installed.json"
jq -n --arg context "$context" --arg namespace "$ORKA_NAMESPACE" --arg runtime "$DEMO_FIBEY_RUNTIME" \
  --arg revision "$revision" '{context:$context,namespace:$namespace,runtimeName:$runtime,sourceRevision:$revision}' >"$state/setup.json"
(cd "$state" && python3 "$here/check.py" installation)
printf 'Prepared Fibey. Rehearse without recording with demo/11-fibey-approval/demo.sh.\n'
