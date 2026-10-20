#!/usr/bin/env bash
# Real AgentKit/Foundry adapters; deterministic model and Azure transport fixtures.
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "$root"
fixture="$root/scripts/fixtures/human-approval-v2"
kindctl="$root/.agents/skills/kindctl/bin/kindctl"
run_id="approval-$(date +%s)-$$"
work="$root/bin/human-approval-v2-e2e/$run_id"
mkdir -p "$work/evidence" "$work/private"
chmod 700 "$work/private"
cluster=""
cluster_created=0
registry_created=0
builder_created=0
# shellcheck source=lib/kind-local-registry.sh
source "$root/scripts/lib/kind-local-registry.sh"

cleanup() {
  local status=$?
  trap - EXIT
  set +e
  if [[ "$builder_created" == 1 ]]; then
    docker buildx rm --timeout 120s "$run_id" >/dev/null || status=1
  fi
  if [[ "$cluster_created" == 1 ]]; then
    if [[ -n "$cluster" ]]; then
      kubectl --context "kind-$cluster" -n orka-system get pods -o wide >"$work/evidence/pods.txt" 2>/dev/null
    fi
    "$kindctl" delete --tag "$run_id" || status=1
  fi
  if [[ "$registry_created" == 1 ]]; then
    orka_kind_registry_stop "$cluster" "$run_id" || status=1
  fi
  rm -rf -- "$work/private"
  printf 'Human approval E2E evidence: %s\n' "$work/evidence"
  exit "$status"
}
trap cleanup EXIT

# These commits contain the matching brokered-approval contracts. Fetch only
# these public sources; no sibling worktrees or provider credentials are used.
agentkit_revision=490bb6d6c968d0240f3034318e05f9561baf4334
foundry_revision=9f994c305989ffa63835b9f97dccd6c45426bdcc
checkout_source() {
  local name="$1" url="$2" revision="$3"
  git init -q "$work/$name"
  git -C "$work/$name" fetch --quiet --depth=1 "$url" "$revision"
  git -C "$work/$name" checkout --quiet --detach FETCH_HEAD
  [[ "$(git -C "$work/$name" rev-parse HEAD)" == "$revision" ]]
}
checkout_source agentkit https://github.com/sozercan/agentkit.git "$agentkit_revision"
checkout_source foundry https://github.com/orka-agents/agent-runtime-foundry.git "$foundry_revision"
python3 -m venv "$work/venv"
python="$work/venv/bin/python"
"$python" -m pip --quiet install "$work/agentkit/runtimes/common"
"$python" -B -m unittest discover -s examples/human-approval-v2 -p 'test_*.py' -v
"$python" -B -m unittest discover -s "$fixture" -p 'test_*.py' -v
"$python" "$fixture/prepare.py" --agentkit "$work/agentkit" --output "$work/context"

architecture="$(docker info --format '{{.Architecture}}')"
case "$architecture" in
  x86_64|amd64) architecture=amd64 ;;
  aarch64|arm64) architecture=arm64 ;;
  *) echo "unsupported Docker architecture: $architecture" >&2; exit 1 ;;
esac
(
  cd "$work/foundry"
  CGO_ENABLED=0 GOOS=linux GOARCH="$architecture" go build -trimpath \
    -o "$work/context/agent-runtime-foundry" ./cmd/agent-runtime-foundry
)

cluster_created=1
"$kindctl" create --tag "$run_id"
export KUBECONFIG
KUBECONFIG="$("$kindctl" path --tag "$run_id")"
context="$(kubectl --kubeconfig "$KUBECONFIG" config current-context)"
cluster="${context#kind-}"
registry_created=1
orka_kind_registry_start "$cluster" "$run_id"
# BuildKit runs in the kind network so digest-pinned source images resolve on
# both Linux runners and Docker Desktop, where host loopback is outside its VM.
cat >"$work/buildkitd.toml" <<EOF
[registry."$ORKA_KIND_REGISTRY_ADDR"]
  mirrors = ["$ORKA_KIND_REGISTRY_NAME:5000"]
  http = true
[registry."$ORKA_KIND_REGISTRY_NAME:5000"]
  http = true
EOF
builder_created=1
docker buildx create --name "$run_id" --driver docker-container \
  --driver-opt network=kind --driver-opt default-load=true \
  --buildkitd-config "$work/buildkitd.toml" >/dev/null
export BUILDX_BUILDER="$run_id"

printf '{}\n' >"$work/images.json"
for image in agentkit-base agentkit-hosted foundry-base fixture; do
  docker build --platform "linux/$architecture" -f "$work/context/Dockerfile.$image" \
    -t "orka-approval-$image:$run_id" "$work/context"
  ref="$(orka_kind_registry_push "orka-approval-$image:$run_id" "orka/approval-$image")"
  jq --arg name "$image" --arg ref "$ref" '. + {($name): {image: $ref}}' \
    "$work/images.json" >"$work/images.next.json"
  mv "$work/images.next.json" "$work/images.json"
done
printf '{}\n' >"$work/composed-images.json"
for provider in agentkit foundry; do
  source_image="$(jq -r --arg name "$provider-base" '.[$name].image' "$work/images.json")"
  upper="$(printf '%s' "$provider" | tr '[:lower:]' '[:upper:]')"
  docker build --platform "linux/$architecture" \
    --build-arg "${upper}_RUNTIME_IMAGE=$source_image" \
    --build-arg "${upper}_ADAPTER_DIGEST=${source_image##*@}" \
    -f "workers/acp/images/$provider/Dockerfile" -t "orka-approval-$provider:$run_id" .
  ref="$(orka_kind_registry_push "orka-approval-$provider:$run_id" "orka/approval-$provider")"
  jq --arg name "$provider" --arg ref "$ref" '. + {($name): $ref}' \
    "$work/composed-images.json" >"$work/composed-images.next.json"
  mv "$work/composed-images.next.json" "$work/composed-images.json"
  config="$work/context/foundry.json"
  [[ "$provider" != agentkit ]] || config="$work/context/agent-direct.yaml"
  go run ./examples/human-approval-v2/profile --provider "$provider" \
    --adapter-digest "${source_image##*@}" --config "$config" >"$work/$provider-profile.json"
done

make docker-build IMG="orka-approval-controller:$run_id"
make docker-build-workspace-publisher WORKSPACE_PUBLISHER_IMG="orka-approval-publisher:$run_id"
controller="$(orka_kind_registry_push "orka-approval-controller:$run_id" orka/approval-controller)"
publisher="$(orka_kind_registry_push "orka-approval-publisher:$run_id" orka/approval-publisher)"
make install
"$python" "$fixture/e2e.py" secrets --work "$work" --context "$context" --cluster "$cluster"
kubectl --context "$context" create namespace vekil-system
# shellcheck source=lib/e2e-admission-tls.sh
source "$root/scripts/lib/e2e-admission-tls.sh"
orka_e2e_bootstrap_admission_tls
unused="example.invalid/unused@sha256:$(printf '0%.0s' {1..64})"
make deploy IMG="$controller" WORKSPACE_PUBLISHER_IMG="$publisher" \
  ACP_CODEX_RUNTIME_IMG="$unused" ACP_CLAUDE_RUNTIME_IMG="$unused" \
  ACP_COPILOT_RUNTIME_IMG="$unused" ACP_OPENCODE_RUNTIME_IMG="$unused"
kubectl --context "$context" -n orka-system rollout status deployment/orka-controller-manager --timeout=300s
jq -n --arg head "$(git rev-parse HEAD)" --arg agentkit "$agentkit_revision" \
  --arg foundry "$foundry_revision" --arg controller "$controller" --arg publisher "$publisher" \
  --slurpfile images "$work/images.json" --slurpfile composed "$work/composed-images.json" \
  '{head:$head,agentkit:$agentkit,foundry:$foundry,controller:$controller,publisher:$publisher,
    images:$images[0],supervisors:$composed[0],transport:"local-fixture"}' >"$work/evidence/build.json"
"$python" "$fixture/e2e.py" run --work "$work" --context "$context" --cluster "$cluster"
# The EXIT trap always disposes of the cluster. Success additionally requires
# receipts, normal Task deletion, and retired runtime authority beforehand.
jq -e '.passed == true and .taskCount == .normallyDeletedTasks and
  (.agentRuntimesDeleted | length) == 2 and .retainedBootAuthorityCount == 0' \
  "$work/evidence/cleanup.json" >/dev/null
