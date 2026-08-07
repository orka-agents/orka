#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/coexistence-deploy-gate-test.XXXXXX")"
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

fake_bin="${test_root}/bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/kubectl" <<'EOF_FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$1" == "get" && "$2" == "deployments.apps,services,secrets" && "$3" == "-A" && "$4" == "-o" && "$5" == "json" ]]; then
  if [[ -n "${FAKE_INVENTORY_FILE:-}" ]]; then
    cat "${FAKE_INVENTORY_FILE}"
  else
    printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}'
  fi
  exit 0
fi

if [[ "$1" == "get" && "$2" == "crd" && $# -ge 3 ]]; then
  crd="$3"
  if [[ "${FAKE_MISSING_CRD:-}" == "${crd}" ]]; then
    exit 1
  fi
  case "${crd}" in
    agentruntimes.core.orka.ai)
      case "${FAKE_SCHEMA_MODE:-dual}" in
        dual)
          printf '%s\n' '{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"contractVersion":{"enum":["orka.harness.v2","orka.harness.v1"]}}}}}}}]}}'
          ;;
        v2-only)
          printf '%s\n' '{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"contractVersion":{"enum":["orka.harness.v2"]}}}}}}}]}}'
          ;;
        no-served-version)
          printf '%s\n' '{"spec":{"versions":[{"name":"v1alpha1","served":false,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"contractVersion":{"enum":["orka.harness.v1","orka.harness.v2"]}}}}}}}]}}'
          ;;
        missing-enum)
          printf '%s\n' '{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{}}}}}}]}}'
          ;;
        *)
          echo "unknown FAKE_SCHEMA_MODE: ${FAKE_SCHEMA_MODE}" >&2
          exit 2
          ;;
      esac
      ;;
    agents.core.orka.ai)
      agent_schema='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"runtime":{"properties":{"contractVersion":{"enum":["orka.harness.v2","orka.harness.v1"]}},"x-kubernetes-validations":[{"message":"runtime.contractVersion is immutable once set"}]}}}}}}}]}}'
      case "${FAKE_AGENT_SCHEMA_MODE:-dual}" in
        dual)
          printf '%s\n' "${agent_schema}"
          ;;
        v2-only)
          jq '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.runtime.properties.contractVersion.enum = ["orka.harness.v2"]' <<<"${agent_schema}"
          ;;
        missing-immutability)
          jq '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.runtime["x-kubernetes-validations"] = []' <<<"${agent_schema}"
          ;;
        *)
          echo "unknown FAKE_AGENT_SCHEMA_MODE: ${FAKE_AGENT_SCHEMA_MODE}" >&2
          exit 2
          ;;
      esac
      ;;
    tasks.core.orka.ai)
      task_schema='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"status":{"properties":{"agentExecutionBinding":{"type":"object","properties":{"contractVersion":{"enum":["orka.harness.v2","orka.harness.v1"]}}},"agentExecutionNoExecution":{"type":"object"},"agentExecutionQuarantine":{"type":"object"},"agentExecutionResolutionRef":{"type":"object"}},"x-kubernetes-validations":[{"message":"agentExecutionBinding is write-once and immutable"}]}},"x-kubernetes-validations":[{"message":"Task spec is immutable after execution authority or a migration disposition is recorded"}]}}}]}}'
      case "${FAKE_TASK_SCHEMA_MODE:-dual}" in
        dual)
          printf '%s\n' "${task_schema}"
          ;;
        missing-authority)
          jq 'del(.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.agentExecutionBinding)' <<<"${task_schema}"
          ;;
        missing-immutability)
          jq '.spec.versions[0].schema.openAPIV3Schema.properties.status["x-kubernetes-validations"] = []' <<<"${task_schema}"
          ;;
        *)
          echo "unknown FAKE_TASK_SCHEMA_MODE: ${FAKE_TASK_SCHEMA_MODE}" >&2
          exit 2
          ;;
      esac
      ;;
    *)
      exit 0
      ;;
  esac
  exit 0
fi

if [[ "$1" == "wait" && "$2" == "--for=condition=Established" && "$3" == "--timeout=60s" && "$4" == crd/* ]]; then
  crd="${4#crd/}"
  [[ "${FAKE_UNESTABLISHED_CRD:-}" != "${crd}" ]]
  exit
fi

echo "unexpected fake kubectl invocation: $*" >&2
exit 2
EOF_FAKE_KUBECTL
chmod +x "${fake_bin}/kubectl"

run_gate() {
  make --no-print-directory -s -C "${root}" verify-coexistence-crds KUBECTL="${fake_bin}/kubectl"
}

expect_gate_failure() {
  local expected="$1"
  shift
  local output
  if output="$(env "$@" make --no-print-directory -s -C "${root}" verify-coexistence-crds KUBECTL="${fake_bin}/kubectl" 2>&1)"; then
    echo "coexistence gate unexpectedly passed: ${expected}" >&2
    exit 1
  fi
  grep -F "${expected}" <<<"${output}" >/dev/null || {
    echo "expected gate failure not found: ${expected}" >&2
    printf '%s\n' "${output}" >&2
    exit 1
  }
}

run_gate >/dev/null
for crd in \
  runtimepools.core.orka.ai \
  agentexecutioncontrols.core.orka.ai \
  agentexecutionpolicies.core.orka.ai \
  agentexecutionadjudications.core.orka.ai \
  agents.core.orka.ai \
  tasks.core.orka.ai; do
  expect_gate_failure "missing coexistence CRD: ${crd}" FAKE_MISSING_CRD="${crd}"
done
for schema_mode in v2-only no-served-version missing-enum; do
  expect_gate_failure 'AgentRuntime CRD is not the dual orka.harness.v1/orka.harness.v2 bridge schema' FAKE_SCHEMA_MODE="${schema_mode}"
done
for schema_mode in v2-only missing-immutability; do
  expect_gate_failure 'Agent CRD is missing the immutable dual-contract coexistence schema' \
    FAKE_AGENT_SCHEMA_MODE="${schema_mode}"
done
for schema_mode in missing-authority missing-immutability; do
  expect_gate_failure 'Task CRD is missing the immutable coexistence execution-authority schema' \
    FAKE_TASK_SCHEMA_MODE="${schema_mode}"
done
expect_gate_failure 'coexistence CRD is not Established: agentexecutioncontrols.core.orka.ai' \
  FAKE_UNESTABLISHED_CRD=agentexecutioncontrols.core.orka.ai

valid_inventory="${test_root}/valid-wrapper.json"
cat >"${valid_inventory}" <<'EOF_VALID_INVENTORY'
{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"orka-agent-harness-wrapper","namespace":"orka-system","labels":{"app.kubernetes.io/component":"agent-harness-wrapper"}},"spec":{"strategy":{"type":"Recreate"},"template":{"spec":{"volumes":[{"name":"ledger","persistentVolumeClaim":{"claimName":"orka-agent-harness-wrapper-ledger"}}]}}}}]}
EOF_VALID_INVENTORY
FAKE_INVENTORY_FILE="${valid_inventory}" run_gate >/dev/null

rolling_inventory="${test_root}/rolling-wrapper.json"
jq '.items[0].spec.strategy.type = "RollingUpdate"' "${valid_inventory}" >"${rolling_inventory}"
expect_gate_failure 'strategy is not Recreate' FAKE_INVENTORY_FILE="${rolling_inventory}"

ephemeral_inventory="${test_root}/ephemeral-wrapper.json"
jq '.items[0].spec.template.spec.volumes[0] = {"name":"ledger","emptyDir":{}}' "${valid_inventory}" >"${ephemeral_inventory}"
expect_gate_failure 'missing PVC-backed ledger volume' FAKE_INVENTORY_FILE="${ephemeral_inventory}"

if default_output="$(FAKE_INVENTORY_FILE="${valid_inventory}" KUBECTL="${fake_bin}/kubectl" "${root}/scripts/check-legacy-wrapper-resources.sh" 2>&1)"; then
  echo 'v2-only wrapper gate unexpectedly allowed a legacy wrapper' >&2
  exit 1
fi
grep -F 'legacy harness-wrapper resources remain' <<<"${default_output}" >/dev/null

grep -Eq '^deploy: .*verify-coexistence-crds' "${root}/Makefile"
if grep -Eq '^deploy: .*verify-acp-crd-cutover' "${root}/Makefile"; then
  echo 'deploy still depends on the v2-only hard-cutover gate' >&2
  exit 1
fi

printf '%s\n' 'ok - coexistence deployment requires Established dual CRDs, the AgentRuntime/Agent/Task bridge schemas, and only durable Recreate-mode v1 wrappers'
