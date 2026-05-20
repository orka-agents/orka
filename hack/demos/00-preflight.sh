#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/../.." && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"

require_demo_base
require_pr_demo_env
require_chat_client
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

cd "${repo_root}"

clear

p "# Demo 00: Preflight"
p "Brief: verify cluster, Orka API, model surface, and demo credentials before the live workflows start."

p "Show: the demo namespace and Orka controller are present."
pe "kubectl get namespace ${DEMO_NAMESPACE}"
pe "kubectl -n ${ORKA_NAMESPACE} get svc,pods"

p "Show: the Provider and demo Secrets exist (only resource names are printed)."
pe "verify_provider ${DEMO_PROVIDER_REF} ${DEMO_NAMESPACE}"
pe "verify_secret ${DEMO_RUNTIME_SECRET_REF} 'runtime credential' ${DEMO_NAMESPACE}"
pe "verify_secret ${DEMO_GIT_SECRET_REF} 'git credential' ${DEMO_NAMESPACE}"

p "Show: the local API tunnel is reachable before chat clients connect."
pe "require_orka_api_reachable"
pe "curl -fsS \"\$ORKA_SERVER/healthz\" | jq ."
pe "curl -fsS \"\$ORKA_SERVER/readyz\" | jq ."

p "Show: Orka exposes the Anthropic-compatible surface Claude Code will call."
pe "orka_api GET /anthropic/v1/models | jq '{models: [.data[]?.id]}'"
pe "${DEMO_CLAUDE_BIN} --version"
pe "print_anthropic_target"
pe "printf 'demo workdir: %s\\n' ${DEMO_WORKDIR}"

p "Run: clean only the named demo resources so the rest of the flow starts from a known state."
pe "hack/demos/reset.sh"

p "Summary."
pe "summarize_preflight"
pe "printf '\\nPreflight looks good. Start with 10-chat-pr.sh when you are ready.\\n'"
