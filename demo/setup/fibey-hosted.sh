#!/usr/bin/env bash
# Prepare demo 13: one Fibey hosted by Orka on AKS and one hosted in Microsoft
# Foundry, both governed by the same Orka approval policy. Run the steps in
# order; each is idempotent. Image builds run on a Docker host (FIBEY_HOSTED_BUILD_HOST).
#
#   fibey-hosted.sh vekil      # model route for the Orka-hosted Fibey
#   (build images with fibey-hosted/build.sh; see the demo README)
#   fibey-hosted.sh tools      # work-order service, Tools, Agents, presenter
#   fibey-hosted.sh runtimes   # runtime Deployments and AgentRuntime registrations
#   fibey-hosted.sh ready      # snapshot the installation for the walkthrough
set -Eeuo pipefail
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster
here=$demo_root/setup/fibey-hosted
state=$demo_root/setup/state/13-fibey-hosted-approval/platform
umask 077
mkdir -p "$state"

require_env() {
  local name
  for name in "$@"; do
    [[ -n ${!name:-} ]] || { printf 'Set %s in demo/setup/env.sh.\n' "$name" >&2; exit 1; }
  done
}

# render FILE — substitute ${NAME} references from the environment, failing on
# any unset name so a manifest never reaches the cluster half-filled.
render() {
  python3 - "$1" <<'PY'
import os, re, sys
text = open(sys.argv[1]).read()
missing = sorted({m for m in re.findall(r"\$\{([A-Z0-9_]+)\}", text) if not os.environ.get(m)})
if missing:
    sys.exit("unset: " + ", ".join(missing))
sys.stdout.write(re.sub(r"\$\{([A-Z0-9_]+)\}", lambda m: os.environ[m.group(1)], text))
PY
}

# vekil_routes — one model route per distinct deployment: Fibey's model, plus
# demo 14's coordinator model (FIBEY_FLOW_MODEL) when that is another one.
vekil_routes() {
  local model id target
  for model in $(printf '%s\n' "$FIBEY_HOSTED_MODEL" "${FIBEY_FLOW_MODEL:-}" | awk 'NF && !seen[$0]++'); do
    id=fibey-model-route target=foundry-model
    [[ $model == "$FIBEY_HOSTED_MODEL" ]] || id=$model-route target=foundry-$model
    printf '      - id: %s\n        public_id: %s\n        name: %s\n' "$id" "$model" "$model"
    printf '        endpoints:\n          - /chat/completions\n          - /responses\n'
    printf '        parallel_tool_calls: true\n        targets:\n'
    printf '          - id: %s\n            provider: foundry\n            upstream_model: %s\n' "$target" "$model"
  done
}

step_vekil() {
  require_env FIBEY_HOSTED_IDENTITY_CLIENT_ID FIBEY_HOSTED_PROJECT_ENDPOINT FIBEY_HOSTED_MODEL
  kubectl create namespace vekil-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  FIBEY_VEKIL_MODEL_ROUTES=$(vekil_routes)
  export FIBEY_VEKIL_MODEL_ROUTES
  render "$here/vekil.yaml" >"$state/vekil.yaml"
  kubectl apply -f "$state/vekil.yaml"
  kubectl -n vekil-system rollout restart deployment/vekil >/dev/null
  kubectl -n vekil-system rollout status deployment/vekil --timeout=180s
  # The Orka provider proxy already forwards to vekil.vekil-system:1337.
  kubectl -n "$ORKA_NAMESPACE" rollout status deployment/orka-provider-auth-proxy --timeout=60s
}

# fetch_build — copy the build outputs the cluster steps need. A build host of
# `local` means build.sh ran on this machine.
fetch_build() {
  require_env FIBEY_HOSTED_BUILD_HOST FIBEY_HOSTED_BUILD_DIR
  mkdir -p "$state/setup/context"
  local file
  for file in images.json agentkit-profile.json foundry-profile.json tools.yaml \
    context/foundry.json context/agent-direct.yaml context/agent-hosted.yaml; do
    if [[ $FIBEY_HOSTED_BUILD_HOST == local ]]; then
      cp "$FIBEY_HOSTED_BUILD_DIR/$file" "$state/setup/$file"
    else
      scp -q "$FIBEY_HOSTED_BUILD_HOST:$FIBEY_HOSTED_BUILD_DIR/$file" "$state/setup/$file"
    fi
  done
}

# ensure_auth NAME RUNTIME SOURCE — create the runtime's private credentials
# once and never print them. SOURCE `proxy` reuses Orka's provider-proxy
# bearer; `random` mints a broker bearer and adds the continuation proof.
ensure_auth() {
  local name=$1 runtime=$2 source=$3
  kubectl -n "$ORKA_NAMESPACE" get secret "$name-auth" -o name >/dev/null 2>&1 && return 0
  python3 "$here/runtimes.py" secret --name "$name" --runtime "$runtime" --source "$source" \
    --proof "$state/private/continuation-proof" | kubectl create -f - >/dev/null
}

step_runtimes() {
  require_env FIBEY_HOSTED_IDENTITY_CLIENT_ID
  fetch_build
  local epoch name provider port forward
  epoch=$(kubectl -n "$ORKA_NAMESPACE" get controllerepochs -o json |
    jq -er '[.items[].status.epoch // empty] | if length == 1 then .[0] else error("expected one controller epoch") end')
  ensure_auth fibey-on-aks fibey-on-aks-runtime proxy
  ensure_auth fibey-on-foundry fibey-on-foundry-runtime random
  python3 "$here/runtimes.py" render --setup "$state/setup" --epoch "$epoch" \
    --identity-client-id "$FIBEY_HOSTED_IDENTITY_CLIENT_ID" >"$state/runtimes.json"
  # Registrations pin the profile digest. Delete a stale one while the runtime
  # it was conformance-checked against still serves: its cleanup proof is bound
  # to that exact runtime instance and profile, so rolling the Pod first wedges it.
  for provider in agentkit foundry; do
    name=fibey-on-aks
    [[ $provider == foundry ]] && name=fibey-on-foundry
    if kubectl -n "$ORKA_NAMESPACE" get agentruntime "$name-runtime" -o json 2>/dev/null |
      jq -e --slurpfile profile "$state/setup/$provider-profile.json" \
        '.spec.capabilities.profile.digest != $profile[0].profile.digest' >/dev/null; then
      kubectl -n "$ORKA_NAMESPACE" delete agentruntime "$name-runtime" --timeout=300s >/dev/null
    fi
  done
  kubectl apply -f "$state/runtimes.json"
  for name in fibey-on-aks fibey-on-foundry; do
    kubectl -n "$ORKA_NAMESPACE" rollout status "deployment/$name" --timeout=300s
  done
  for provider in agentkit foundry; do
    name=fibey-on-aks
    [[ $provider == foundry ]] && name=fibey-on-foundry
    kubectl -n "$ORKA_NAMESPACE" get agentruntime "$name-runtime" -o name >/dev/null 2>&1 && continue
    port=$((18000 + RANDOM % 1000))
    kubectl -n "$ORKA_NAMESPACE" port-forward "svc/$name" "$port:8080" >"$state/$name-forward.log" 2>&1 &
    forward=$!
    wait_for "$name capabilities" "curl -fsS http://127.0.0.1:$port/v2/capabilities -o '$state/$name-capabilities.json'" 30
    kill "$forward" 2>/dev/null || true
    python3 "$here/runtimes.py" register --setup "$state/setup" --provider "$provider" \
      --capabilities "$state/$name-capabilities.json" >"$state/$name-runtime.json"
    kubectl create -f "$state/$name-runtime.json"
  done
  for name in fibey-on-aks fibey-on-foundry; do
    wait_for "$name runtime conformance" \
      "kubectl -n '$ORKA_NAMESPACE' get agentruntime '$name-runtime' -o json | jq -e '.status.ready == true and .status.observedGeneration == .metadata.generation' >/dev/null" 300
  done
}

# step_tools — the counted work-order service, both Tools, both Fibey Agents,
# and the presenter's orka-client identity. Mirrors demo 11's setup.
step_tools() {
  : "${DEMO_APPROVAL_SOURCE:=$HOME/projects/orka}"
  local source_dir=$state/source revision label='"demo.orka.ai/name":"13-fibey-hosted-approval"'
  revision=$(git -C "$DEMO_APPROVAL_SOURCE" rev-parse HEAD)
  mkdir -p "$source_dir"
  for file in simulated_tools.py simulator.yaml.example; do
    git -C "$DEMO_APPROVAL_SOURCE" show "$revision:examples/human-approval-v2/$file" >"$source_dir/$file"
  done
  local resource existing
  for resource in tool/read-inventory tool/create-work-order configmap/demo-fibey-tools \
    deployment/demo-fibey-tools service/demo-fibey-tools pvc/demo-fibey-tools; do
    existing=$(kubectl -n "$ORKA_NAMESPACE" get "$resource" --ignore-not-found \
      -o 'jsonpath={.metadata.uid}/{.metadata.labels.demo\.orka\.ai/name}')
    if [[ -n $existing && $existing != */13-fibey-hosted-approval ]]; then
      printf 'Refusing to change %s, which is not owned by demo 13.\n' "$resource" >&2
      exit 1
    fi
  done
  kubectl -n "$ORKA_NAMESPACE" create configmap demo-fibey-tools \
    --from-file="simulated_tools.py=$source_dir/simulated_tools.py" --dry-run=client -o json |
    jq ".metadata.labels = {$label}" >"$state/code.json"
  local code_sha
  code_sha=$(shasum -a 256 "$source_dir/simulated_tools.py" | cut -d' ' -f1)
  kubectl -n "$ORKA_NAMESPACE" create --dry-run=client --validate=false \
    -f "$source_dir/simulator.yaml.example" -o json |
    jq -s --arg namespace "$ORKA_NAMESPACE" --arg sha "$code_sha" "
      {apiVersion:\"v1\",kind:\"List\",items:[.[] | (if .kind == \"List\" then .items[] else . end) |
        walk(if type == \"string\" and . == \"human-approval-tools\" then \"demo-fibey-tools\" else . end) |
        .metadata.namespace = \$namespace | .metadata.labels = {$label} |
        if .kind == \"Deployment\" then
          .spec.template.metadata.annotations[\"demo.orka.ai/source-sha\"] = \$sha |
          .spec.template.spec.automountServiceAccountToken = false |
          .spec.template.spec.containers[0].image = \"docker.io/library/python:3.14.7-alpine3.24@sha256:c6ead215bfd31f1e433d968853b7a769989117115b728874824e6c0a27cb96fc\"
        else . end]}" >"$state/simulator.json"
  kubectl -n "$ORKA_NAMESPACE" create --dry-run=client --validate=false -f "$state/setup/tools.yaml" -o json |
    jq -s --arg namespace "$ORKA_NAMESPACE" "
      {apiVersion:\"v1\",kind:\"List\",items:[.[] | (if .kind == \"List\" then .items[] else . end) |
        .metadata.namespace = \$namespace | .metadata.labels = {$label}]}" >"$state/tools.json"
  jq -n --arg namespace "$ORKA_NAMESPACE" --arg sa "$ORKA_CLIENT_SA" "
    def metadata(\$name): {name:\$name,namespace:\$namespace,labels:{$label}};
    {apiVersion:\"v1\",kind:\"List\",items:[
      {apiVersion:\"core.orka.ai/v1alpha1\",kind:\"Agent\",metadata:metadata(\"demo-fibey-aks\"),
       spec:{runtime:{runtimeRef:{name:\"fibey-on-aks-runtime\"}}}},
      {apiVersion:\"core.orka.ai/v1alpha1\",kind:\"Agent\",metadata:metadata(\"demo-fibey-foundry\"),
       spec:{runtime:{runtimeRef:{name:\"fibey-on-foundry-runtime\"}}}},
      {apiVersion:\"v1\",kind:\"ServiceAccount\",metadata:metadata(\$sa),automountServiceAccountToken:false},
      {apiVersion:\"rbac.authorization.k8s.io/v1\",kind:\"Role\",metadata:metadata(\"demo-fibey-presenter\"),
       rules:[{apiGroups:[\"core.orka.ai\"],resources:[\"tasks\",\"agents\",\"agentruntimes\",\"tools\",\"sessions\"],
               verbs:[\"get\",\"list\",\"watch\"]},
              {apiGroups:[\"core.orka.ai\"],resources:[\"tasks\"],verbs:[\"create\",\"delete\",\"patch\"]},
              {apiGroups:[\"core.orka.ai\"],resources:[\"tasks/approvals\"],verbs:[\"update\"]},
              {apiGroups:[\"core.orka.ai\"],resources:[\"agents\",\"agentruntimes\",\"tools\"],verbs:[\"use\"]}]},
      {apiVersion:\"rbac.authorization.k8s.io/v1\",kind:\"RoleBinding\",metadata:metadata(\"demo-fibey-presenter\"),
       subjects:[{kind:\"ServiceAccount\",name:\$sa,namespace:\$namespace}],
       roleRef:{apiGroup:\"rbac.authorization.k8s.io\",kind:\"Role\",name:\"demo-fibey-presenter\"}}
    ]}" >"$state/agents-and-presenter.json"
  kubectl apply -f "$state/code.json" -f "$state/simulator.json" -f "$state/tools.json" \
    -f "$state/agents-and-presenter.json"
  kubectl -n "$ORKA_NAMESPACE" rollout status deployment/demo-fibey-tools --timeout=180s
  printf '%s\n' "$revision" >"$state/approval-source-revision"
}

# step_ready — snapshot the prepared installation for the walkthrough's checks.
step_ready() {
  source "$demo_root/13-fibey-hosted-approval/lib.sh"
  local context
  context=$(kubectl config current-context)
  fibey_hosted_snapshot "$state/installed.json"
  jq -n --arg context "$context" --arg namespace "$ORKA_NAMESPACE" '
    {context:$context,namespace:$namespace,lanes:{
      aks:{agent:"demo-fibey-aks",runtimeName:"fibey-on-aks-runtime"},
      foundry:{agent:"demo-fibey-foundry",runtimeName:"fibey-on-foundry-runtime"}}}' >"$state/setup.json"
  (cd "$state" && python3 "$demo_root/13-fibey-hosted-approval/check.py" installation)
  printf 'Prepared both Fibeys. Rehearse without recording with demo/13-fibey-hosted-approval/demo.sh.\n'
}

case "${1:-}" in
  vekil) step_vekil ;;
  runtimes) step_runtimes ;;
  tools) step_tools ;;
  ready) step_ready ;;
  *) printf 'Usage: %s vekil|runtimes|tools|ready\n' "$0" >&2; exit 2 ;;
esac
