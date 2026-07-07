#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
Usage: switch-backend.sh <http|agentkit|foundry> [task-name] [namespace]

Patches the Fibey demo Task to use the Agent bound to the selected namespace-local
AgentRuntime facade. The selected AgentRuntime/Agent manifests must already be
applied. This changes only the Orka Agent reference; workflow input and Tool
policy remain Orka-owned.
USAGE
}

backend="${1:-}"
task_name="${2:-fibey-quincy-north-alert}"
namespace="${3:-default}"

case "${backend}" in
  http)
    agent="fibey-remote-http"
    runtime="fibey-http-runtime"
    ;;
  agentkit)
    agent="fibey-remote-agentkit"
    runtime="fibey-agentkit-runtime"
    ;;
  foundry)
    agent="fibey-remote-foundry"
    runtime="fibey-foundry-runtime"
    ;;
  -h|--help|help|"")
    usage
    exit 0
    ;;
  *)
    echo "unknown backend: ${backend}" >&2
    usage
    exit 2
    ;;
esac

kubectl -n "${namespace}" get agentruntime "${runtime}" >/dev/null
kubectl -n "${namespace}" get agent "${agent}" >/dev/null
kubectl -n "${namespace}" patch task "${task_name}" --type merge \
  -p "{\"spec\":{\"agentRef\":{\"name\":\"${agent}\"}}}"

echo "${task_name} now targets ${agent} (${runtime}) in namespace ${namespace}"
