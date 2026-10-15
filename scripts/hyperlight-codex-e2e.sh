#!/usr/bin/env bash
# Run after the Hyperlight kind suite builds the pinned runtime/bundle images.
set -Eeuo pipefail
[[ "$(uname -s)" == Linux && -c /dev/kvm ]] || { echo 'real KVM is required' >&2; exit 1; }
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
runtime="${E2E_ACP_CODEX_RUNTIME_IMAGE:-ghcr.io/orka-agents/orka/acp-codex-runtime:e2e}"
bundle="${E2E_HYPERLIGHT_BUNDLE_IMAGE:-ghcr.io/orka-agents/orka/hyperlight-bundle:e2e}"
# Resolve tags once; never pull or mutate an image during the conformance run.
runtime="$(docker image inspect "$runtime" --format '{{.Id}}')"
bundle="$(docker image inspect "$bundle" --format '{{.Id}}')"
volume="orka-hyperlight-codex-$$-${RANDOM}"
container="${volume}-runtime"
if docker volume inspect "$volume" >/dev/null 2>&1; then echo 'probe volume already exists' >&2; exit 1; fi
cleanup() {
  local status=$?
  trap - EXIT
  docker rm -f "$container" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null || status=90
  exit "$status"
}
docker volume create "$volume" >/dev/null
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker run --rm --pull=never --network none --user 0:0 \
  --mount "type=volume,src=$volume,dst=/bundle" --entrypoint /bin/cp \
  "$bundle" -a /opt/orka/hyperlight/. /bundle/
docker run -i --rm --name "$container" --pull=never --network none --read-only \
  --device /dev/kvm --user 0:0 --cap-drop ALL --security-opt no-new-privileges \
  --memory 4g --cpus 2 --pids-limit 256 --tmpfs /tmp:rw,nosuid,nodev,size=2g,mode=1777 \
  --mount "type=volume,src=$volume,dst=/opt/orka/hyperlight,readonly" \
  --env HOME=/tmp --entrypoint /usr/bin/node "$runtime" --input-type=module \
  < "$root/test/e2e/testdata/hyperlight-codex-probe.mjs"
