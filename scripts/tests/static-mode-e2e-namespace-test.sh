#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
security_script="${root}/scripts/security-scan-e2e.sh"
substrate_script="${root}/scripts/agent-substrate-e2e.sh"
label_script="${root}/scripts/live-github-label-trigger-e2e.sh"
e2e_suite="${root}/test/e2e/e2e_suite_test.go"
tls_helper="${root}/scripts/lib/e2e-admission-tls.sh"

grep -Fq 'test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-${orka_namespace}}"' "${security_script}"
grep -Fq '[[ "${test_namespace}" == "${orka_namespace}" ]]' "${security_script}"
grep -Fq 'ORKA_NAMESPACE="${ORKA_NAMESPACE:-orka-system}"' "${substrate_script}"
grep -Fq 'kubectl -n "${ORKA_NAMESPACE}" apply -f -' "${substrate_script}"
grep -Fq 'for ns in ate-demo "${ORKA_NAMESPACE}"; do' "${substrate_script}"
grep -Fq 'ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE="${orka_namespace}"' "${label_script}"
grep -Fq 'namespace: ${orka_namespace}' "${label_script}"
grep -Fq 'scripts", "lib", "ensure-static-mode-namespace.sh"' "${e2e_suite}"
if grep -Fq 'exec.Command("kubectl", "create", "ns", namespace)' "${e2e_suite}"; then
  echo 'Go E2E must not pre-create an unlabeled controller namespace' >&2
  exit 1
fi

tls_verify_line="$(grep -nF 'openssl verify -CAfile' "${tls_helper}" | cut -d: -f1 || true)"
tls_identity_line="$(grep -nF 'ensure-static-mode-namespace.sh' "${tls_helper}" | cut -d: -f1 || true)"
tls_secret_line="$(grep -nF 'create secret generic "${secret_name}"' "${tls_helper}" | cut -d: -f1 || true)"
if [[ ! "${tls_verify_line}" =~ ^[0-9]+$ || ! "${tls_identity_line}" =~ ^[0-9]+$ || ! "${tls_secret_line}" =~ ^[0-9]+$ ]] ||
  ((tls_verify_line >= tls_identity_line || tls_identity_line >= tls_secret_line)); then
  echo 'E2E TLS bootstrap must verify its certificate, establish namespace identity, then write the Secret' >&2
  exit 1
fi

substrate_resource_setup="$(awk '/^create_substrate_resources\(\) {/,/^}/' "${substrate_script}")"
namespace_create_line="$(grep -nF 'scripts/lib/ensure-static-mode-namespace.sh' <<<"${substrate_resource_setup}" | cut -d: -f1 || true)"
secret_loop_line="$(grep -nF 'for ns in ate-demo "${ORKA_NAMESPACE}"; do' <<<"${substrate_resource_setup}" | cut -d: -f1 || true)"
if [[ ! "${namespace_create_line}" =~ ^[0-9]+$ || ! "${secret_loop_line}" =~ ^[0-9]+$ ]] ||
  ((namespace_create_line >= secret_loop_line)); then
  echo 'agent-substrate E2E must establish the fail-closed Orka namespace identity before writing bootstrap Secrets' >&2
  exit 1
fi

for script in "${security_script}" "${substrate_script}" "${label_script}"; do
  if grep -Eq '(^|[[:space:]])-n[[:space:]]+default([[:space:]]|$)|^[[:space:]]*namespace:[[:space:]]+default([[:space:]]|$)|^[[:space:]]*value:[[:space:]]+default([[:space:]]|$)' "${script}"; then
    echo "${script#"${root}/"} still places controller-owned resources in the pre-isolation default namespace" >&2
    exit 1
  fi
done

grep -Fq 'namespace: ate-demo' "${substrate_script}"

mcp_template_namespace="$(
  awk '
    $0 == "  name: orka-mcp-ci" { found = 1; next }
    found && $1 == "namespace:" { print $2; exit }
  ' "${substrate_script}"
)"
if [[ "${mcp_template_namespace}" != '${ORKA_NAMESPACE}' ]]; then
  echo 'agent-substrate E2E must colocate the MCP ActorTemplate with its isolated Orka Tool' >&2
  exit 1
fi
grep -Fq 'kubectl -n ${ORKA_NAMESPACE} get actortemplate orka-mcp-ci' "${substrate_script}"

acp_template_namespace="$(
  awk '
    $0 == "  name: orka-acp-infra" { found = 1; next }
    found && $1 == "namespace:" { print $2; exit }
  ' "${substrate_script}"
)"
if [[ "${acp_template_namespace}" != '${ORKA_NAMESPACE}' ]]; then
  echo 'agent-substrate E2E must colocate the ACP infrastructure ActorTemplate with its isolated Task' >&2
  exit 1
fi

workspace_task_body="$(awk '/^exercise_workspace_backed_acp_task\(\) {/,/^}/' "${substrate_script}")"
workspace_template_namespace="$(
  awk '
    $1 == "templateRef:" { in_template_ref = 1; next }
    in_template_ref && $1 == "namespace:" { namespace = $2; next }
    in_template_ref && $1 == "name:" && $2 == "orka-acp-infra" { print namespace; exit }
  ' <<<"${workspace_task_body}"
)"
if [[ "${workspace_template_namespace}" != '${ORKA_NAMESPACE}' ]]; then
  echo 'agent-substrate E2E workspace Task must use its same-namespace ACP infrastructure ActorTemplate' >&2
  exit 1
fi

if grep -Fq 'kubectl -n ate-demo get "${derived_template}"' <<<"${workspace_task_body}"; then
  echo 'agent-substrate E2E must inspect derived ACP ActorTemplates in the infrastructure template namespace' >&2
  exit 1
fi

for function_name in create_substrate_actor_pools create_mcp_tool; do
  function_body="$(awk "/^${function_name}\\(\\) {/,/^}/" "${substrate_script}")"
  template_namespace="$(
    awk '
      $0 ~ /^[[:space:]]+name: orka-mcp-ci$/ { found = 1; next }
      found && $1 == "namespace:" { print $2; exit }
      found { exit }
    ' <<<"${function_body}"
  )"
  if [[ -n "${template_namespace}" ]]; then
    echo "agent-substrate E2E ${function_name} must default the MCP ActorTemplate reference to its Orka namespace" >&2
    exit 1
  fi
done

if grep -Fq 'grant_substrate_provider_template_access' "${substrate_script}"; then
  echo 'agent-substrate E2E must not grant cross-namespace ActorTemplate access' >&2
  exit 1
fi

printf '%s\n' 'ok - static-mode E2E controller-owned resources stay in the isolated installation namespace'
