#!/usr/bin/env bash

set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=hack/demos/lib/common.sh
. "${script_dir}/lib/common.sh"

require_demo_base
require_pr_demo_env
source_demo_magic "$@"
configure_demo_magic
ensure_demo_workdir
prepare_api_env

clear

p "# Demo readiness"
pe "kubectl get namespace ${DEMO_NAMESPACE}"
pe "kubectl get provider ${DEMO_PROVIDER_REF} -n ${DEMO_NAMESPACE}"
pe "kubectl get secret ${DEMO_RUNTIME_SECRET_REF} ${DEMO_GIT_SECRET_REF} -n ${DEMO_NAMESPACE}"
pe "orka_cli status"
pe "printf 'Rendered demo files will land in %s\\n' ${DEMO_WORKDIR}"

p "# Optional reset before you present"
pe "${script_dir}/reset.sh"
pe "printf '\\nPreflight looks good. Start with 10-chat-pr.sh when you are ready.\\n'"
