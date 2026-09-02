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
script="${root}/scripts/live-github-oidc-e2e.sh"

grep -Fq 'oidc_namespace="${ORKA_GITHUB_OIDC_NAMESPACE:-${namespace}}"' "${script}"
grep -Fq '[[ "${namespace}" == "orka-system" ]]' "${script}"
grep -Fq '[[ "${oidc_namespace}" == "${namespace}" ]]' "${script}"
grep -Fq -- '--arg namespace "${oidc_namespace}"' "${script}"
grep -Fq 'namespace:$namespace' "${script}"

if grep -Fq 'namespace:"default"' "${script}"; then
  echo 'live GitHub OIDC E2E still submits Tasks to the pre-isolation default namespace' >&2
  exit 1
fi

printf '%s\n' 'ok - live GitHub OIDC identity and Task namespaces match the canonical isolated controller'
