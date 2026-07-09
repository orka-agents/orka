#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: examples/harness/foundry-responses/validate.sh [--agentkit PATH] [--full]

Runs deterministic local validation for the Orka Foundry hosted Responses adapter.
No live Foundry endpoint, token, API key, or Kubernetes cluster is required.

Options:
  --agentkit PATH  Also run AgentKit's deterministic Foundry brokered protocol
                   tests from PATH/runtimes/common. If omitted, the script uses
                   a sibling ../agentkit checkout when present.
  --full           Also run `make test` for the full non-e2e Orka suite.
  -h, --help       Show this help.
USAGE
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
agentkit_root=""
run_full=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --agentkit)
      [[ $# -ge 2 ]] || { echo "--agentkit requires a path" >&2; exit 2; }
      agentkit_root="$2"
      shift 2
      ;;
    --full)
      run_full=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage
      exit 2
      ;;
  esac
done

run() {
  printf '\n==> %s\n' "$*" >&2
  "$@"
}

cd "$repo_root"

run go test ./examples/harness/foundry-responses -count=1
run go test \
  ./examples/harness/foundry \
  ./examples/harness/foundry-responses \
  ./examples/harness/echo \
  ./internal/harness \
  ./internal/harness/conformance
run go test ./internal/controller -run 'Test.*(AgentRuntime|Harness|Brokered|Runtime)'
while IFS= read -r -d '' script; do
  run bash -n "$script"
done < <(find examples -type f -name '*.sh' -print0 | sort -z)

if [[ "$run_full" == "1" ]]; then
  run make test
fi

if [[ -z "$agentkit_root" ]]; then
  sibling_agentkit="$(cd "$repo_root/.." && pwd)/agentkit"
  if [[ -d "$sibling_agentkit/runtimes/common" ]]; then
    agentkit_root="$sibling_agentkit"
  fi
fi

if [[ -n "$agentkit_root" ]]; then
  common_dir="$agentkit_root/runtimes/common"
  if [[ ! -d "$common_dir" ]]; then
    echo "AgentKit common runtime directory not found: $common_dir" >&2
    exit 2
  fi
  if ! command -v uv >/dev/null 2>&1; then
    echo "uv is required for AgentKit validation but was not found in PATH" >&2
    exit 2
  fi
  (
    cd "$common_dir"
    run uv run --extra dev pytest -q \
      tests/test_foundry_brokered_protocol.py \
      tests/test_brokered_schema.py \
      tests/test_foundry_protocol.py
  )
fi

printf '\nFoundry hosted Responses local validation passed.\n' >&2
