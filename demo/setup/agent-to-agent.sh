#!/usr/bin/env bash
# Prepare demo 08 on the explicitly selected local kind cluster. Never records.
# Shared helpers define demo_root and repo_root after loading the selected env file.
# shellcheck disable=SC2154
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../lib/scenario.sh"
scenario_require_cluster
scenario_require_commands go docker kind openssl curl
umask 077

here=$demo_root/08-agent-to-agent
state=$demo_root/setup/state/08-agent-to-agent/installation
source_repo=${DEMO_A2A_REPO:-$repo_root/../orka-gateway-a2a}
controller=${ORKA_CONTROLLER_DEPLOYMENT:-orka-controller-manager}
container=${ORKA_CONTROLLER_CONTAINER:-manager}
api_service=${ORKA_API_SERVICE:-orka-api}
model=${DEMO_A2A_MODEL:-gpt-5.5}
runtime_secret=${DEMO_A2A_RUNTIME_SECRET:-copilot-runtime-key}
port=${DEMO_A2A_PORT:-8443}
[[ $port =~ ^[0-9]+$ ]] && ((port >= 1024 && port <= 65535)) || {
  printf 'DEMO_A2A_PORT must be a port between 1024 and 65535.\n' >&2; exit 1;
}
public_url=https://127.0.0.1:$port
context=$(kubectl config current-context)
cluster=${context#kind-}
git -C "$repo_root" check-ignore --quiet "$state/credential-check" || {
  printf 'The demo setup state directory must be gitignored before preparing credentials.\n' >&2; exit 1;
}
[[ -f $source_repo/go.mod && -f $source_repo/cmd/client/main.go ]] || {
  printf 'Set DEMO_A2A_REPO to a checkout of orka-gateway-a2a.\n' >&2; exit 1;
}
[[ $(git -C "$source_repo" status --porcelain) == "" ]] || {
  printf 'Use a clean A2A checkout so the recorded source revision identifies the build.\n' >&2; exit 1;
}
revision=$(git -C "$source_repo" rev-parse HEAD)
image=orka-demo-a2a:${revision:0:12}
local_nodes=$(kind get nodes --name "$cluster" | sort)
cluster_nodes=$(kubectl get nodes -o json | jq -r '.items[].metadata.name' | sort)
[[ -n $local_nodes && $local_nodes == "$cluster_nodes" ]] || {
  printf 'The kubeconfig and local kind cluster do not identify the same nodes.\n' >&2; exit 1;
}
kubectl -n "$ORKA_NAMESPACE" get secret "$runtime_secret" -o name >/dev/null
kubectl -n "$ORKA_NAMESPACE" get serviceaccount orka-client -o name >/dev/null
for crd in gatewayclasses.gateway.orka.ai gateways.gateway.orka.ai gatewaybindings.gateway.orka.ai; do
  kubectl wait --for=condition=Established "crd/$crd" --timeout=10s >/dev/null
done
mkdir -p "$state"
namespace_uid=$(kubectl get namespace "$ORKA_NAMESPACE" -o jsonpath='{.metadata.uid}')
cluster_uid=$(kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')
target=$(jq -n --arg context "$context" --arg ns "$ORKA_NAMESPACE" --arg uid "$namespace_uid" \
  --arg cluster "$cluster_uid" '{context:$context, namespace:$ns, namespaceUID:$uid, clusterUID:$cluster}')
if [[ -f $state/target.json ]]; then
  jq -e --argjson target "$target" '. == $target' "$state/target.json" >/dev/null || {
    printf 'Saved A2A installation belongs to a different cluster or namespace incarnation.\n' >&2; exit 1;
  }
else
  printf '%s\n' "$target" >"$state/target.json"
fi
if [[ ! -f $state/installation-id ]]; then
  python3 -c 'import secrets; print(secrets.token_hex(12))' >"$state/installation-id"
fi
installation=$(cat "$state/installation-id")
[[ $installation =~ ^[a-f0-9]{24}$ ]] || { printf 'Invalid saved installation identity.\n' >&2; exit 1; }

# Check ownership before updating any existing object, including the cluster-scoped class.
assert_owned_or_absent() {
  local kind=$1 name=$2 actual
  actual=$(kubectl -n "$ORKA_NAMESPACE" get "$kind" "$name" --ignore-not-found \
    -o jsonpath='{.metadata.uid}{" "}{.metadata.labels.demo\.orka\.ai/installation}')
  if [[ -n ${actual// /} && ${actual#* } != "$installation" ]]; then
    printf 'Refusing to replace existing %s/%s, which is not owned by this installation.\n' "$kind" "$name" >&2
    return 1
  fi
}
for name in client inbound outbound tls; do
  assert_owned_or_absent secret "demo-a2a-$name"
done
kubectl -n "$ORKA_NAMESPACE" get service "$api_service" -o json >"$state/api-service.json"
kubectl -n "$ORKA_NAMESPACE" get deployment "$controller" -o json |
  python3 "$here/prepare.py" controller-patch --namespace "$ORKA_NAMESPACE" --container "$container" \
    --service "$state/api-service.json" --info "$state/controller-info.json" >"$state/controller-ca-patch.json"
store_claim=$(jq -er '.storeClaim' "$state/controller-info.json")
kubectl -n "$ORKA_NAMESPACE" get pvc "$store_claim" -o json |
  jq -e '.status.phase == "Bound"' >/dev/null

# Private keys and token values stay in mode-600 files and Kubernetes Secrets.
if [[ ! -f $state/ca.crt ]]; then
  [[ ! -e $state/ca.key ]] || { printf 'Incomplete CA files; inspect the installation state.\n' >&2; exit 1; }
  cat >"$state/ca.conf" <<'EOF'
[req]
distinguished_name = dn
x509_extensions = ca
prompt = no
[dn]
CN = Orka local A2A demo CA
[ca]
basicConstraints = critical, CA:true
keyUsage = critical, keyCertSign, cRLSign
EOF
  openssl req -x509 -newkey rsa:3072 -nodes -days 30 -config "$state/ca.conf" \
    -keyout "$state/ca.key" -out "$state/ca.crt" 2>"$state/tls-build.log"
fi
if [[ ! -f $state/tls.crt ]]; then
  cat >"$state/server.conf" <<EOF
[req]
distinguished_name = dn
prompt = no
[dn]
CN = demo-a2a-adapter.$ORKA_NAMESPACE.svc
[server]
basicConstraints = critical, CA:false
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:demo-a2a-adapter.$ORKA_NAMESPACE.svc,DNS:localhost,IP:127.0.0.1
EOF
  openssl req -new -newkey rsa:3072 -nodes -config "$state/server.conf" \
    -keyout "$state/tls.key" -out "$state/tls.csr" 2>>"$state/tls-build.log"
  openssl x509 -req -days 30 -in "$state/tls.csr" -CA "$state/ca.crt" -CAkey "$state/ca.key" \
    -CAcreateserial -extfile "$state/server.conf" -extensions server -out "$state/tls.crt" 2>>"$state/tls-build.log"
fi
openssl x509 -checkend 86400 -noout -in "$state/tls.crt" >/dev/null || {
  printf 'The demo certificate expires within a day; renew it before running this setup.\n' >&2; exit 1;
}
openssl verify -CAfile "$state/ca.crt" "$state/tls.crt" >/dev/null
python3 "$here/prepare.py" resources --namespace "$ORKA_NAMESPACE" --installation "$installation" \
  --image "$image" --public-url "$public_url" --api-service "$api_service" --model "$model" \
  --secret "$runtime_secret" --ca "$state/ca.crt" --controller "$state/controller-info.json" >"$state/resources.json"
while IFS=$'\t' read -r kind name; do
  assert_owned_or_absent "$kind" "$name"
done < <(jq -r '.items[] | [(.kind + (if (.apiVersion | contains("/")) then "." + (.apiVersion | split("/")[0]) else "" end)), .metadata.name] | @tsv' "$state/resources.json")

printf 'Preparing A2A on %s, namespace %s.\n' "$context" "$ORKA_NAMESPACE"
printf 'Setup will add its CA to deployment/%s container %s and wait for that rollout.\n' "$controller" "$container"
printf 'Building adapter and client from A2A revision %s.\n' "${revision:0:12}"
mkdir -p "$repo_root/bin/demo-a2a"
go -C "$source_repo" build -trimpath -o "$repo_root/bin/demo-a2a/a2a-client" ./cmd/client
docker build -t "$image" "$source_repo"
kind load docker-image "$image" --name "$cluster"

verify_secret() {
  local name=$1
  shift
  kubectl -n "$ORKA_NAMESPACE" get secret "$name" -o json |
    python3 -c '
import base64, json, pathlib, sys
actual = json.load(sys.stdin).get("data", {})
for item in sys.argv[1:]:
    key, path = item.split("=", 1)
    if base64.b64decode(actual.get(key, "")) != pathlib.Path(path).read_bytes():
        sys.exit("Saved credential files do not match the existing Secret; refusing rotation.")
' "$@"
}
for role in client inbound outbound; do
  name=demo-a2a-$role
  if [[ ! -f $state/$role.token ]]; then
    if [[ -n $(kubectl -n "$ORKA_NAMESPACE" get secret "$name" --ignore-not-found -o name) ]]; then
      printf 'Local %s credential is missing; refusing to replace the existing Secret.\n' "$role" >&2; exit 1
    fi
    openssl rand -hex 32 >"$state/$role.token"
  fi
  if [[ -n $(kubectl -n "$ORKA_NAMESPACE" get secret "$name" --ignore-not-found -o name) ]]; then
    verify_secret "$name" "token=$state/$role.token"
  else
    kubectl -n "$ORKA_NAMESPACE" create secret generic "$name" --from-file="token=$state/$role.token" \
      --dry-run=client -o json |
      jq --arg id "$installation" --arg role "$role" --arg endpoint "https://demo-a2a-adapter.$ORKA_NAMESPACE.svc:8443" '
        .metadata.labels = {"demo.orka.ai/name":"08-agent-to-agent", "demo.orka.ai/installation":$id} |
        if $role == "client" then . else
          .metadata.labels["gateway.orka.ai/" + $role + "-auth"] = "true" |
          .metadata.annotations = {"gateway.orka.ai/gateway-name":"demo-a2a"} |
          if $role == "outbound" then .metadata.annotations["gateway.orka.ai/adapter-endpoint"] = $endpoint else . end
        end' | kubectl create -f - >/dev/null
  fi
done
if [[ -n $(kubectl -n "$ORKA_NAMESPACE" get secret demo-a2a-tls --ignore-not-found -o name) ]]; then
  verify_secret demo-a2a-tls "tls.crt=$state/tls.crt" "tls.key=$state/tls.key"
else
  kubectl -n "$ORKA_NAMESPACE" create secret tls demo-a2a-tls --cert="$state/tls.crt" --key="$state/tls.key" \
    --dry-run=client -o json |
    jq --arg id "$installation" '.metadata.labels = {"demo.orka.ai/name":"08-agent-to-agent", "demo.orka.ai/installation":$id}' |
    kubectl create -f - >/dev/null
fi
kubectl apply -f "$state/resources.json" >/dev/null
kubectl -n "$ORKA_NAMESPACE" patch deployment "$controller" --type=strategic \
  --patch-file "$state/controller-ca-patch.json" >/dev/null
kubectl -n "$ORKA_NAMESPACE" rollout status "deployment/$controller" --timeout=600s
kubectl -n "$ORKA_NAMESPACE" rollout status deployment/demo-a2a-adapter --timeout=180s
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready gateway.gateway.orka.ai/demo-a2a --timeout=180s
kubectl -n "$ORKA_NAMESPACE" wait --for=condition=Ready gatewaybinding.gateway.orka.ai/demo-a2a-inventory --timeout=180s

# Bind the walkthrough to the exact installed objects, not only their names.
kubectl -n "$ORKA_NAMESPACE" get deployment/demo-a2a-adapter gateway.gateway.orka.ai/demo-a2a \
  gatewaybinding.gateway.orka.ai/demo-a2a-inventory agent/demo-a2a-inventory -o json |
  jq --slurpfile target "$state/target.json" --arg installation "$installation" --arg url "$public_url" \
    --arg revision "$revision" --arg image "$image" \
    '{target:$target[0], installation:$installation, url:$url, revision:$revision, image:$image,
      identities:[.items[] | {kind, name:.metadata.name, uid:.metadata.uid}]}' >"$state/ready.json"
printf 'Prepared. Run bash demo/08-agent-to-agent/demo.sh for an unrecorded rehearsal.\n'
