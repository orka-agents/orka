#!/usr/bin/env bash
# Prepare demo 14 on top of demo 13's platform: the plant's alert gateway (the
# A2A adapter from demo 08, with its own names), the coordinator Agent, the
# data-analysis image, and Mark's identity. Run demo 13's setup first; demo 14
# reuses its Foundry Fibey, Tools, and work-order service unchanged.
#
#   fibey-flow.sh images    # build and push the analysis job and adapter images
#   fibey-flow.sh install   # gateway, coordinator, Mark; patches the controller CA
#   fibey-flow.sh ready     # snapshot the installation for the walkthrough
set -Eeuo pipefail
# shellcheck source=demo/lib/scenario.sh
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster
scenario_require_commands go openssl curl docker
here=$demo_root/setup/fibey-flow
walkthrough=$demo_root/14-fibey-alert-flow
state=$demo_root/setup/state/14-fibey-alert-flow/installation
platform=$demo_root/setup/state/13-fibey-hosted-approval/platform
a2a_repo=${DEMO_A2A_REPO:-$repo_root/../orka-gateway-a2a}
controller=${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}
container=${ORKA_CONTROLLER_CONTAINER:-manager}
umask 077
mkdir -p "$state"
git -C "$repo_root" check-ignore --quiet "$state/credential-check" || {
  printf 'The demo setup state directory must be gitignored before preparing credentials.\n' >&2; exit 1;
}
: "${FIBEY_HOSTED_IMAGE_REPOSITORY:?Set FIBEY_HOSTED_IMAGE_REPOSITORY in demo/setup/env.sh}"
: "${FIBEY_HOSTED_MODEL:?Set FIBEY_HOSTED_MODEL in demo/setup/env.sh}"
# The coordinator gets its own deployment so each Azure token meter belongs to one agent.
: "${FIBEY_FLOW_MODEL:?Set FIBEY_FLOW_MODEL in demo/setup/env.sh}"

# push_amd64 CONTEXT TARGET [ARGS...] — build for the cluster, push, print TARGET@digest.
push_amd64() {
  local context=$1 target=$2
  shift 2
  docker buildx build --platform linux/amd64 --provenance=false --push \
    --metadata-file "$state/build-metadata.json" -t "$target" "$@" "$context" >&2
  printf '%s@%s\n' "${target%:*}" "$(jq -er '."containerimage.digest"' "$state/build-metadata.json")"
}

step_images() {
  [[ -z $(git -C "$a2a_repo" status --porcelain) ]] || {
    printf 'Use a clean A2A checkout so the recorded revision identifies the build.\n' >&2; exit 1;
  }
  local revision analysis adapter
  revision=$(git -C "$a2a_repo" rev-parse HEAD)
  # Tag the analysis image by its own content; the digest is what the coordinator runs.
  analysis=$(push_amd64 "$walkthrough/analysis" "$FIBEY_HOSTED_IMAGE_REPOSITORY/orka-demo-pump-analysis:$(
    cat "$walkthrough"/analysis/{Dockerfile,analyze.py,sensor-history.csv} | shasum -a 256 | cut -c1-12)")
  adapter=$(push_amd64 "$a2a_repo" "$FIBEY_HOSTED_IMAGE_REPOSITORY/orka-gateway-a2a:${revision:0:12}")
  mkdir -p "$repo_root/bin/demo-fibey-flow"
  go -C "$a2a_repo" build -trimpath -o "$repo_root/bin/demo-fibey-flow/a2a-client" ./cmd/client
  jq -n --arg analysis "$analysis" --arg adapter "$adapter" --arg revision "$revision" \
    '{analysis:$analysis, adapter:$adapter, adapterRevision:$revision}' >"$state/images.json"
  jq . "$state/images.json"
}

# ensure_tls — a private CA and the adapter's serving certificate, made once.
ensure_tls() {
  local dns=fibey-alerts-adapter.$ORKA_NAMESPACE.svc
  if [[ ! -f $state/ca.crt ]]; then
    printf '[req]\ndistinguished_name = dn\nx509_extensions = ca\nprompt = no\n[dn]\nCN = Orka demo 14 alert gateway CA\n[ca]\nbasicConstraints = critical, CA:true\nkeyUsage = critical, keyCertSign, cRLSign\nsubjectKeyIdentifier = hash\n' >"$state/ca.conf"
    openssl req -x509 -newkey rsa:3072 -nodes -days 30 -config "$state/ca.conf" \
      -keyout "$state/ca.key" -out "$state/ca.crt" 2>"$state/tls-build.log"
  fi
  if [[ ! -f $state/tls.crt ]]; then
    printf '[req]\ndistinguished_name = dn\nprompt = no\n[dn]\nCN = %s\n[server]\nbasicConstraints = critical, CA:false\nkeyUsage = critical, digitalSignature, keyEncipherment\nextendedKeyUsage = serverAuth\nauthorityKeyIdentifier = keyid\nsubjectAltName = DNS:%s,DNS:localhost,IP:127.0.0.1\n' \
      "$dns" "$dns" >"$state/server.conf"
    openssl req -new -newkey rsa:3072 -nodes -config "$state/server.conf" \
      -keyout "$state/tls.key" -out "$state/tls.csr" 2>>"$state/tls-build.log"
    openssl x509 -req -days 30 -in "$state/tls.csr" -CA "$state/ca.crt" -CAkey "$state/ca.key" \
      -CAcreateserial -extfile "$state/server.conf" -extensions server -out "$state/tls.crt" 2>>"$state/tls-build.log"
  fi
  openssl x509 -checkend 86400 -noout -in "$state/tls.crt" >/dev/null || {
    printf 'The demo certificate expires within a day; delete %s/tls.* and rerun.\n' "$state" >&2; exit 1;
  }
  openssl verify -CAfile "$state/ca.crt" "$state/tls.crt" >/dev/null
}

# ensure_secret NAME ROLE — bearer tokens live in mode-600 files and Secrets only.
ensure_secret() {
  local name=fibey-alerts-$1 role=$1
  [[ -f $state/$role.token ]] || {
    [[ -z $(kubectl -n "$ORKA_NAMESPACE" get secret "$name" --ignore-not-found -o name) ]] || {
      printf 'Local %s credential is missing; refusing to replace Secret %s.\n' "$role" "$name" >&2; exit 1;
    }
    openssl rand -hex 32 >"$state/$role.token"
  }
  kubectl -n "$ORKA_NAMESPACE" create secret generic "$name" --from-file="token=$state/$role.token" \
    --dry-run=client -o json |
    jq --arg installation "$installation" --arg role "$role" \
      --arg endpoint "https://fibey-alerts-adapter.$ORKA_NAMESPACE.svc:8443" '
      .metadata.labels = {"demo.orka.ai/name":"14-fibey-alert-flow", "demo.orka.ai/installation":$installation} |
      if $role == "client" then . else
        .metadata.labels["gateway.orka.ai/" + $role + "-auth"] = "true" |
        .metadata.annotations = {"gateway.orka.ai/gateway-name":"fibey-alerts"} |
        if $role == "outbound" then .metadata.annotations["gateway.orka.ai/adapter-endpoint"] = $endpoint else . end
      end' | kubectl apply -f - >/dev/null
}

controller_epoch() {
  kubectl -n "$ORKA_NAMESPACE" get controllerepochs -o json | jq -r '[.items[].status.epoch // empty] | first // empty'
}

step_install() {
  [[ -f $state/images.json && -x $repo_root/bin/demo-fibey-flow/a2a-client ]] || {
    printf 'Run fibey-flow.sh images first.\n' >&2; exit 1;
  }
  [[ -f $platform/ready.json ]] || { printf 'Prepare demo 13 first (fibey-hosted.sh ready).\n' >&2; exit 1; }
  kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready agent/demo-fibey-foundry --timeout=10s >/dev/null
  for crd in gatewayclasses.gateway.orka.ai gateways.gateway.orka.ai gatewaybindings.gateway.orka.ai; do
    kubectl wait --for=condition=Established "crd/$crd" --timeout=10s >/dev/null
  done
  [[ -f $state/installation-id ]] || python3 -c 'import secrets; print(secrets.token_hex(12))' >"$state/installation-id"
  installation=$(cat "$state/installation-id")
  ensure_tls
  for role in client inbound outbound; do
    ensure_secret "$role"
  done
  kubectl -n "$ORKA_NAMESPACE" create secret tls fibey-alerts-tls --cert="$state/tls.crt" --key="$state/tls.key" \
    --dry-run=client -o json |
    jq --arg installation "$installation" \
      '.metadata.labels = {"demo.orka.ai/name":"14-fibey-alert-flow", "demo.orka.ai/installation":$installation}' |
    kubectl apply -f - >/dev/null
  kubectl -n "$ORKA_NAMESPACE" get service orka-api -o json >"$state/api-service.json"
  kubectl -n "$ORKA_NAMESPACE" get deployment "$controller" -o json |
    python3 "$here/prepare.py" controller-patch --namespace "$ORKA_NAMESPACE" --container "$container" \
      --service "$state/api-service.json" --info "$state/controller-info.json" >"$state/controller-ca-patch.json"
  python3 "$here/prepare.py" resources --namespace "$ORKA_NAMESPACE" --installation "$installation" \
    --adapter-image "$(jq -er .adapter "$state/images.json")" \
    --analysis-image "$(jq -er .analysis "$state/images.json")" --model "$FIBEY_FLOW_MODEL" \
    --ca "$state/ca.crt" --controller "$state/controller-info.json" >"$state/resources.json"
  printf 'Setup adds its CA to deployment/%s and waits for that rollout.\n' "$controller"
  local epoch_before generation_before
  # shellcheck disable=SC2034  # read by the wait_for condition below
  epoch_before=$(controller_epoch)
  generation_before=$(kubectl -n "$ORKA_NAMESPACE" get deployment "$controller" -o jsonpath='{.metadata.generation}')
  kubectl apply -f "$state/resources.json" >/dev/null
  kubectl -n "$ORKA_NAMESPACE" patch deployment "$controller" --type=strategic \
    --patch-file "$state/controller-ca-patch.json" >/dev/null
  kubectl -n "$ORKA_NAMESPACE" rollout status "deployment/$controller" --timeout=600s
  # A controller restart advances its epoch, and demo 13's runtimes are rendered
  # with the epoch they serve. Re-render them so they pass conformance again.
  if [[ $(kubectl -n "$ORKA_NAMESPACE" get deployment "$controller" -o jsonpath='{.metadata.generation}') != "$generation_before" ]]; then
    # wait_for evals its condition later, in this function's scope.
    # shellcheck disable=SC2016
    wait_for 'the new controller epoch' '[[ -n $(controller_epoch) && $(controller_epoch) != "$epoch_before" ]]' 300
    bash "$demo_root/setup/fibey-hosted.sh" runtimes
  fi
  kubectl -n "$ORKA_NAMESPACE" rollout status deployment/fibey-alerts-adapter --timeout=180s
  kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready gateway.gateway.orka.ai/fibey-alerts --timeout=180s
  kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready \
    gatewaybinding.gateway.orka.ai/demo-maintenance-coordinator --timeout=180s
  kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready agent/demo-maintenance-coordinator --timeout=60s
}

# step_ready — bind the walkthrough to the exact installed objects.
step_ready() {
  local context
  context=$(kubectl config current-context)
  # shellcheck source=demo/14-fibey-alert-flow/lib.sh
  source "$walkthrough/lib.sh"
  fibey_flow_snapshot "$state/installed.json"
  jq --arg context "$context" --arg namespace "$ORKA_NAMESPACE" --slurpfile images "$state/images.json" \
    --arg installation "$(cat "$state/installation-id")" --arg coordinator "$FIBEY_FLOW_MODEL" \
    --arg specialist "$FIBEY_HOSTED_MODEL" '
    {context:$context, namespace:$namespace, installation:$installation,
     coordinatorModel:$coordinator, specialistModel:$specialist, images:$images[0],
     identities:[.items[] | {kind, name:.metadata.name, uid:.metadata.uid}]}' "$state/installed.json" >"$state/ready.json"
  printf 'Prepared. Rehearse without recording with demo/14-fibey-alert-flow/demo.sh.\n'
}

case "${1:-}" in
  images) step_images ;;
  install) step_install ;;
  ready) step_ready ;;
  *) printf 'Usage: %s images|install|ready\n' "$0" >&2; exit 2 ;;
esac
