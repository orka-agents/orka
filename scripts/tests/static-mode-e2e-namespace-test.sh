#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
security_script="${root}/scripts/security-scan-e2e.sh"
substrate_script="${root}/scripts/agent-substrate-e2e.sh"
label_script="${root}/scripts/live-github-label-trigger-e2e.sh"

grep -Fq 'test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-${orka_namespace}}"' "${security_script}"
grep -Fq '[[ "${test_namespace}" == "${orka_namespace}" ]]' "${security_script}"
grep -Fq 'ORKA_NAMESPACE="${ORKA_NAMESPACE:-orka-system}"' "${substrate_script}"
grep -Fq 'kubectl -n "${ORKA_NAMESPACE}" apply -f -' "${substrate_script}"
grep -Fq 'for ns in ate-demo "${ORKA_NAMESPACE}"; do' "${substrate_script}"
grep -Fq 'ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE="${orka_namespace}"' "${label_script}"
grep -Fq 'namespace: ${orka_namespace}' "${label_script}"

substrate_resource_setup="$(awk '/^create_substrate_resources\(\) {/,/^}/' "${substrate_script}")"
namespace_create_line="$(grep -nF 'kubectl create namespace "${ORKA_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -' <<<"${substrate_resource_setup}" | cut -d: -f1 || true)"
secret_loop_line="$(grep -nF 'for ns in ate-demo "${ORKA_NAMESPACE}"; do' <<<"${substrate_resource_setup}" | cut -d: -f1 || true)"
if [[ ! "${namespace_create_line}" =~ ^[0-9]+$ || ! "${secret_loop_line}" =~ ^[0-9]+$ ]] ||
  ((namespace_create_line >= secret_loop_line)); then
  echo 'agent-substrate E2E must create the isolated Orka namespace before writing bootstrap Secrets' >&2
  exit 1
fi

for script in "${security_script}" "${substrate_script}" "${label_script}"; do
  if grep -Eq '(^|[[:space:]])-n[[:space:]]+default([[:space:]]|$)|^[[:space:]]*namespace:[[:space:]]+default([[:space:]]|$)|^[[:space:]]*value:[[:space:]]+default([[:space:]]|$)' "${script}"; then
    echo "${script#"${root}/"} still places controller-owned resources in the pre-isolation default namespace" >&2
    exit 1
  fi
done

grep -Fq 'namespace: ate-demo' "${substrate_script}"

printf '%s\n' 'ok - static-mode E2E controller-owned resources stay in the isolated installation namespace'
