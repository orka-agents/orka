#!/usr/bin/env bash
# Validate shared compatibility routing with two real namespace-scoped
# controllers and container workers. All credentials stay in Go process memory.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
kindctl="$repo_root/.agents/skills/kindctl/bin/kindctl"

for required_command in docker kind kubectl go python3; do
  command -v "$required_command" >/dev/null || { echo "missing $required_command" >&2; exit 1; }
done
python3 - <<'PY'
import subprocess
import sys
try:
    result = subprocess.run(["docker", "info"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=10)
except subprocess.TimeoutExpired:
    sys.exit("Docker daemon did not respond within 10 seconds")
if result.returncode:
    sys.exit("Docker daemon is unavailable")
PY
if [[ "${1:-}" == --preflight-only ]]; then
  echo "Compatibility router E2E prerequisites are available."
  exit 0
fi
if [[ $# -ne 0 ]]; then
  echo "usage: $0 [--preflight-only]" >&2
  exit 2
fi

export ORKA_COMPAT_ROUTER_E2E_IMAGE=orka-compat-controller:e2e
export ORKA_COMPAT_ROUTER_E2E_WORKER_IMAGE=orka-compat-worker:e2e
export ORKA_COMPAT_ROUTER_E2E_MODEL_IMAGE=orka-compat-model:e2e

make manifests ensure-ui-embed
make docker-build IMG="$ORKA_COMPAT_ROUTER_E2E_IMAGE"
docker build -f workers/general/Dockerfile -t "$ORKA_COMPAT_ROUTER_E2E_WORKER_IMAGE" .
docker build -f test/fixtures/compatmodel/Dockerfile -t "$ORKA_COMPAT_ROUTER_E2E_MODEL_IMAGE" .

"$kindctl" create --tag compat-router
cleanup() {
  result=$?
  trap - EXIT
  if [[ $result -eq 0 ]]; then
    "$kindctl" delete --tag compat-router
  else
    echo "E2E failed; inspect the scoped cluster with $kindctl kubectl --tag compat-router" >&2
  fi
  exit "$result"
}
trap cleanup EXIT
for test_image in "$ORKA_COMPAT_ROUTER_E2E_IMAGE" "$ORKA_COMPAT_ROUTER_E2E_WORKER_IMAGE" "$ORKA_COMPAT_ROUTER_E2E_MODEL_IMAGE"; do
  "$kindctl" load --tag compat-router "$test_image"
done
"$kindctl" kubectl --tag compat-router apply --server-side -f config/crd/bases
"$kindctl" kubectl --tag compat-router wait --for=condition=Established --timeout=90s crd --all
export ORKA_COMPAT_ROUTER_E2E_KUBECONFIG
ORKA_COMPAT_ROUTER_E2E_KUBECONFIG="$("$kindctl" path --tag compat-router)"
"$kindctl" exec --tag compat-router -- go test ./test/compatrouter -run TestDeployedCompatibilityRouting -count=1 -timeout=15m -v
