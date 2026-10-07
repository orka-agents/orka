#!/usr/bin/env bash
# Build demo 13's adapter images on a Docker host. Pins the AgentKit and Foundry
# revisions qualified by Orka's human-approval-v2 E2E.
#
#   build.sh adapters WORK ORKA_DIR   # Orka-hosted Fibey + hosted Fibey image
#   build.sh foundry  WORK ORKA_DIR   # Foundry bridge for the chosen hosted version
#
# Inputs come from the environment: FIBEY_HOSTED_MODEL,
# FIBEY_HOSTED_PROJECT_ENDPOINT, FIBEY_HOSTED_AGENT, FIBEY_HOSTED_REGISTRY,
# FIBEY_HOSTED_IMAGE_REPOSITORY, FIBEY_HOSTED_TAG, and for `foundry`,
# FIBEY_HOSTED_VERSION. Results are written to WORK/images.json.
set -Eeuo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
agentkit_revision=490bb6d6c968d0240f3034318e05f9561baf4334
foundry_revision=9f994c305989ffa63835b9f97dccd6c45426bdcc
mode=${1:?mode}
work=${2:?work directory}
orka=${3:?Orka checkout}
: "${FIBEY_HOSTED_MODEL:?}" "${FIBEY_HOSTED_PROJECT_ENDPOINT:?}" "${FIBEY_HOSTED_AGENT:?}"
: "${FIBEY_HOSTED_IMAGE_REPOSITORY:?}" "${FIBEY_HOSTED_TAG:?}"
mkdir -p "$work"
[[ -f $work/images.json ]] || printf '{}\n' >"$work/images.json"

checkout_source() {
  local name=$1 url=$2 revision=$3
  if [[ ! -d $work/$name/.git ]]; then
    git init -q "$work/$name"
    git -C "$work/$name" fetch --quiet --depth=1 "$url" "$revision"
    git -C "$work/$name" checkout --quiet --detach FETCH_HEAD
  fi
  [[ "$(git -C "$work/$name" rev-parse HEAD)" == "$revision" ]]
}

# profile PROVIDER BASE_IMAGE CONFIG — the qualified RuntimeProfile for this
# exact adapter digest and baked configuration.
profile() {
  (cd "$orka" && go run ./examples/human-approval-v2/profile --provider "$1" \
    --adapter-digest "${2##*@}" --config "$3") >"$work/$1-profile.json"
}

record() {
  jq --arg name "$1" --arg ref "$2" '. + {($name): $ref}' "$work/images.json" >"$work/images.next.json"
  mv "$work/images.next.json" "$work/images.json"
}

# push_digest LOCAL TARGET — push and return TARGET@digest.
push_digest() {
  docker tag "$1" "$2" >/dev/null
  local digest
  digest=$(docker push "$2" | sed -n 's/.*digest: \(sha256:[0-9a-f]\{64\}\).*/\1/p' | tail -1)
  [[ $digest == sha256:* ]] || { echo "push did not report a digest for $2" >&2; return 1; }
  printf '%s@%s\n' "${2%:*}" "$digest"
}

compose_supervisor() {
  local provider=$1 source=$2 upper
  upper=$(printf '%s' "$provider" | tr '[:lower:]' '[:upper:]')
  docker build --platform linux/amd64 \
    --build-arg "${upper}_RUNTIME_IMAGE=$source" --build-arg "${upper}_ADAPTER_DIGEST=${source##*@}" \
    -f "$orka/workers/acp/images/$provider/Dockerfile" -t "demo13-$provider-supervisor:$FIBEY_HOSTED_TAG" "$orka" >&2
  push_digest "demo13-$provider-supervisor:$FIBEY_HOSTED_TAG" \
    "$FIBEY_HOSTED_IMAGE_REPOSITORY/orka-demo13-fibey-$provider:$FIBEY_HOSTED_TAG"
}

checkout_source agentkit https://github.com/sozercan/agentkit.git "$agentkit_revision"
checkout_source foundry https://github.com/orka-agents/agent-runtime-foundry.git "$foundry_revision"
if [[ ! -x $work/venv/bin/python ]]; then
  python3 -m venv "$work/venv"
  "$work/venv/bin/python" -m pip --quiet install "$work/agentkit/runtimes/common"
fi
prepare=("$work/venv/bin/python" "$here/prepare.py" --agentkit "$work/agentkit" --orka "$orka"
  --tools "$work/tools.yaml" --output "$work/context" --model "$FIBEY_HOSTED_MODEL"
  --direct-base-url http://orka-provider-auth-proxy.orka-system.svc:8080/v1
  --project-endpoint "$FIBEY_HOSTED_PROJECT_ENDPOINT" --hosted-agent "$FIBEY_HOSTED_AGENT")

case $mode in
  adapters)
    : "${FIBEY_HOSTED_REGISTRY:?}"
    # The demo applies these exact Tools; hosted Fibey bakes their safe schemas.
    sed 's|http://human-approval-tools:|http://demo-fibey-tools:|' \
      "$orka/examples/human-approval-v2/tools.yaml" >"$work/tools.yaml"
    "${prepare[@]}"
    docker build --platform linux/amd64 -f "$work/context/Dockerfile.agentkit-base" \
      -t "demo13-agentkit-base:$FIBEY_HOSTED_TAG" "$work/context" >&2
    base=$(push_digest "demo13-agentkit-base:$FIBEY_HOSTED_TAG" \
      "$FIBEY_HOSTED_IMAGE_REPOSITORY/orka-demo13-fibey-agentkit-base:$FIBEY_HOSTED_TAG")
    record agentkit-base "$base"
    record agentkit "$(compose_supervisor agentkit "$base")"
    profile agentkit "$base" "$work/context/agent-direct.yaml"
    docker build --platform linux/amd64 -f "$work/context/Dockerfile.agentkit-hosted" \
      -t "demo13-agentkit-hosted:$FIBEY_HOSTED_TAG" "$work/context" >&2
    record agentkit-hosted "$(push_digest "demo13-agentkit-hosted:$FIBEY_HOSTED_TAG" \
      "$FIBEY_HOSTED_REGISTRY.azurecr.io/fibey-hosted:$FIBEY_HOSTED_TAG")"
    ;;
  foundry)
    : "${FIBEY_HOSTED_VERSION:?}"
    "${prepare[@]}" --hosted-version "$FIBEY_HOSTED_VERSION"
    (cd "$work/foundry" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
      -o "$work/context/agent-runtime-foundry" ./cmd/agent-runtime-foundry)
    docker build --platform linux/amd64 -f "$work/context/Dockerfile.foundry-base" \
      -t "demo13-foundry-base:$FIBEY_HOSTED_TAG" "$work/context" >&2
    base=$(push_digest "demo13-foundry-base:$FIBEY_HOSTED_TAG" \
      "$FIBEY_HOSTED_IMAGE_REPOSITORY/orka-demo13-fibey-foundry-base:$FIBEY_HOSTED_TAG")
    record foundry-base "$base"
    record foundry "$(compose_supervisor foundry "$base")"
    profile foundry "$base" "$work/context/foundry.json"
    ;;
  *) echo "unknown mode: $mode" >&2; exit 2 ;;
esac
jq . "$work/images.json"
