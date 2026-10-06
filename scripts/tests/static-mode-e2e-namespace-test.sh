#!/usr/bin/env bash
set -Eeuo pipefail

# scripts/tests suites rely on 'set -e' stopping on failed (( )) arithmetic,
# which macOS's stock bash 3.2 does not honor; failures would be silently
# masked there. Require a modern bash (for example: brew install bash).
if [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "error: this test suite requires bash >= 4; found ${BASH_VERSION}" >&2
  exit 1
fi

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
security_script="${root}/scripts/security-scan-e2e.sh"
substrate_script="${root}/scripts/agent-substrate-e2e.sh"
substrate_helper="${root}/scripts/lib/substrate-orka-local.sh"
e2e_suite="${root}/test/e2e/e2e_suite_test.go"

grep -Fq 'test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-${orka_namespace}}"' "${security_script}"
grep -Fq '[[ "${test_namespace}" == "${orka_namespace}" ]]' "${security_script}"
grep -Fq 'ORKA_NAMESPACE=orka-system' "${substrate_script}"
grep -Fq 'source "${ROOT_DIR}/scripts/lib/substrate-orka-local.sh"' "${substrate_script}"
grep -Fq 'scripts", "lib", "ensure-static-mode-namespace.sh"' "${e2e_suite}"
if grep -Fq 'exec.Command("kubectl", "create", "ns", namespace)' "${e2e_suite}"; then
  echo 'Go E2E must not pre-create an unlabeled controller namespace' >&2
  exit 1
fi

live_main="$(awk '/^main\(\) {/,/^}/' "${root}/scripts/live-agent-sandbox-e2e.sh")"
live_runtime_line="$(grep -nF 'run make deploy' <<<"${live_main}" | cut -d: -f1 || true)"
live_controller_patch_line="$(grep -nF 'patch_controller_for_agent_sandbox' <<<"${live_main}" | cut -d: -f1 || true)"
if [[ ! "${live_runtime_line}" =~ ^[0-9]+$ || ! "${live_controller_patch_line}" =~ ^[0-9]+$ ]] ||
  ((live_runtime_line >= live_controller_patch_line)); then
  echo 'live agent-sandbox E2E must run the production deployment, which installs controller-served admission, before enabling protected workspace settlement' >&2
  exit 1
fi

grep -Fq 'agent_sandbox_version="${AGENT_SANDBOX_VERSION:-v1.0.3}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'e2e_kubeconfig="${work_dir}/kubeconfig"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'export KUBECONFIG="${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'run kind export kubeconfig --name "${kind_cluster}" --kubeconfig "${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'run kind create cluster --name "${kind_cluster}" --config "${kind_config}" --kubeconfig "${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
if grep -Fq 'kubectl config use-context' "${root}/scripts/live-agent-sandbox-e2e.sh"; then
  echo 'live agent-sandbox E2E must not mutate the user kubeconfig context' >&2
  exit 1
fi
grep -Fq "jsonpath='{.spec.sandboxTemplateRef.name}'" "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_volume_directory="/durable/orka-workspace"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_session_relative_path="ws-${durable_session_uid}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_marker_relative_path="${durable_session_relative_path}/e2e-durability-marker-${durable_session_uid}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_marker_path="${durable_volume_directory}/${durable_marker_relative_path}"' "${root}/scripts/live-agent-sandbox-e2e.sh"

substrate_deploy="$(awk '/^deploy_orka\(\) {/,/^}/' "${substrate_helper}")"
substrate_component_line="$(grep -nF -- '- ../controller-webhook' <<<"${substrate_deploy}" | cut -d: -f1 || true)"
substrate_flags_line="$(grep -nF -- '--workspace-class-use-admission-enabled' <<<"${substrate_deploy}" | cut -d: -f1 || true)"
if [[ ! "${substrate_component_line}" =~ ^[0-9]+$ || ! "${substrate_flags_line}" =~ ^[0-9]+$ ]] ||
  ((substrate_component_line >= substrate_flags_line)); then
  echo 'agent-substrate E2E must install controller-served admission before enabling protected workspace settlement' >&2
  exit 1
fi
grep -Fq -- '--webhook-cert-rotation-webhook=orka-admission' <<<"${substrate_deploy}"

substrate_resource_setup="$(awk '/^create_native_resources\(\) {/,/^}/' "${substrate_script}")"
grep -Fq 'scripts/lib/ensure-static-mode-namespace.sh" kubectl "${ORKA_NAMESPACE}" harness-v2' <<<"${substrate_resource_setup}"
substrate_main="$(awk '/^main\(\) {/,/^}/' "${substrate_script}")"
namespace_create_line="$(grep -nF 'create_native_resources ' <<<"${substrate_main}" | cut -d: -f1 || true)"
secret_line="$(grep -nF 'kubectl -n orka-system create secret generic orka-substrate-bootstrap' <<<"${substrate_main}" | cut -d: -f1 || true)"
if [[ ! "${namespace_create_line}" =~ ^[0-9]+$ || ! "${secret_line}" =~ ^[0-9]+$ ]] ||
  ((namespace_create_line >= secret_line)); then
  echo 'agent-substrate E2E must establish the fail-closed Orka namespace identity before writing bootstrap Secrets' >&2
  exit 1
fi

for script in "${security_script}" "${substrate_script}" "${substrate_helper}"; do
  if grep -Eq '(^|[[:space:]])-n[[:space:]]+default([[:space:]]|$)|^[[:space:]]*namespace:[[:space:]]+default([[:space:]]|$)|^[[:space:]]*value:[[:space:]]+default([[:space:]]|$)' "${script}"; then
    echo "${script#"${root}/"} still places controller-owned resources in the pre-isolation default namespace" >&2
    exit 1
  fi
done

# Native templates belong to an Atespace, not a Kubernetes namespace. Exercise
# the same manifest emitter used by the installer for every template role.
(
  source "${substrate_script}"
  for template in orka-direct orka-mcp orka-acp-infra; do
    native_template_manifest "${template}" example.invalid/runtime:test public-key |
      jq -e --arg namespace "${ORKA_NAMESPACE}" '.metadata.atespace == $namespace' >/dev/null
  done
)
workspace_class="$(awk '/^create_workspace_class\(\) {/,/^}/' "${substrate_script}")"
grep -Fq 'templateRef: {namespace: orka-system, name: orka-acp-infra}' <<<"${workspace_class}"
mcp_resources="$(awk '/^exercise_mcp\(\) {/,/^}/' "${substrate_script}")"
[[ "$(grep -Fc 'templateRef: {name: orka-mcp, namespace: orka-system}' <<<"${mcp_resources}")" -eq 2 ]]
if grep -Eq 'get actortemplate|grant_substrate_provider_template_access' "${substrate_script}"; then
  echo 'native Substrate E2E must not use Kubernetes ActorTemplate resources or permissions' >&2
  exit 1
fi

printf '%s\n' 'ok - static-mode E2E controller-owned resources stay in the isolated installation namespace'
