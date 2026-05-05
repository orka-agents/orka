#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"

require_demo_base
require_pr_demo_env
require_chat_client
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

clear

p "# Demo 00: Preflight"

p "Brief: verify the cluster, Orka API, model surface, and demo credentials before the live workflows start."

p "Show: the demo namespace and Orka controller are present."
pe "kubectl get namespace ${DEMO_NAMESPACE}"
pe "kubectl -n ${ORKA_NAMESPACE} get svc,pods"

p "Show: the provider and Secret references exist. Only resource names are printed."
pe "require_demo_provider \"${DEMO_PROVIDER_REF}\" \"${DEMO_NAMESPACE}\""
pe "require_demo_secret \"${DEMO_RUNTIME_SECRET_REF}\" \"runtime credential\" \"${DEMO_NAMESPACE}\""
pe "require_demo_secret \"${DEMO_GIT_SECRET_REF}\" \"git credential\" \"${DEMO_NAMESPACE}\""

p "Tell: the local API tunnel must be reachable before chat clients can connect."
pe "require_orka_api_reachable"
pe "curl -fsS \"\$ORKA_SERVER/healthz\" | jq ."
pe "curl -fsS \"\$ORKA_SERVER/readyz\" | jq ."

p "Show: Orka exposes the Anthropic-compatible model surface that Claude Code will call."
pe "orka_api GET \"/anthropic/v1/models\" | jq '{models: [.data[]?.id]}'"
pe "${DEMO_CLAUDE_BIN} --version"
pe "printf 'Claude Code will call %s with model %s\\n' \"$(demo_anthropic_base_url)\" \"$(demo_anthropic_model)\""
pe "printf 'Rendered demo files will land in %s\\n' ${DEMO_WORKDIR}"

p "Run: clean only the named demo resources so the rest of the flow starts from a known state."
pe "${script_dir}/reset.sh"

p "Summary: preflight is complete."
pe "summarize_preflight"
pe "printf '\\nPreflight looks good. Start with 10-chat-pr.sh when you are ready.\\n'"
