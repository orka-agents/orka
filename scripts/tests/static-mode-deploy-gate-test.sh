#!/usr/bin/env bash
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d "${TMPDIR:-/tmp}/static-mode-deploy-gate-test.XXXXXX")"
cleanup() {
  rm -rf "${test_root}"
}
trap cleanup EXIT

fake_bin="${test_root}/bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/kubectl" <<'EOF_FAKE_KUBECTL'
#!/usr/bin/env bash
set -Eeuo pipefail

if [[ "$1" == "get" && "$2" == "crd" && $# -ge 3 ]]; then
  crd="$3"
  if [[ "${FAKE_MISSING_CRD:-}" == "${crd}" ]]; then
    exit 1
  fi
  case "${crd}" in
    agentexecutioncontrols.core.orka.ai|agentexecutionpolicies.core.orka.ai|agentexecutionadjudications.core.orka.ai)
      if [[ "${FAKE_OBSOLETE_CRD:-}" == "${crd}" ]]; then
        printf '%s\n' '{}'
        exit 0
      fi
      exit 1
      ;;
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
      task_schema='{"spec":{"versions":[{"name":"v1alpha1","served":true,"schema":{"openAPIV3Schema":{"properties":{"status":{"properties":{"agentExecutionBinding":{"type":"object","properties":{"contractVersion":{"enum":["orka.harness.v2","orka.harness.v1"]}}}},"x-kubernetes-validations":[{"message":"agentExecutionBinding is write-once and immutable"}]}},"x-kubernetes-validations":[{"message":"Task spec is immutable after execution authority is recorded"}]}}}]}}'
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
        legacy-disposition)
          jq '.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.agentExecutionQuarantine = {"type":"object"}' <<<"${task_schema}"
          ;;
        old-spec-immutability)
          jq '.spec.versions[0].schema.openAPIV3Schema["x-kubernetes-validations"][0].message = "Task spec is immutable after execution authority or a migration disposition is recorded"' <<<"${task_schema}"
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
  make --no-print-directory -s -C "${root}" verify-static-mode-crds KUBECTL="${fake_bin}/kubectl"
}

expect_gate_failure() {
  local expected="$1"
  shift
  local output
  if output="$(env "$@" make --no-print-directory -s -C "${root}" verify-static-mode-crds KUBECTL="${fake_bin}/kubectl" 2>&1)"; then
    echo "static-mode gate unexpectedly passed: ${expected}" >&2
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
  agentruntimes.core.orka.ai \
  agents.core.orka.ai \
  branchclaims.core.orka.ai \
  controllerepochs.core.orka.ai \
  executionworkspaceclasses.workspace.orka.ai \
  executionworkspacepools.workspace.orka.ai \
  executionworkspaceproviders.workspace.orka.ai \
  executionworkspaces.workspace.orka.ai \
  externaleffects.core.orka.ai \
  fakepoolparameters.fake.workspace.orka.ai \
  fakeproviderconfigs.fake.workspace.orka.ai \
  gatewaybindings.gateway.orka.ai \
  gatewayclasses.gateway.orka.ai \
  gateways.gateway.orka.ai \
  outboundaccesspolicies.core.orka.ai \
  promptattempts.core.orka.ai \
  providers.core.orka.ai \
  publications.core.orka.ai \
  repositorymonitors.core.orka.ai \
  repositoryscans.core.orka.ai \
  runtimepools.core.orka.ai \
  runtimesessioncontrols.core.orka.ai \
  skills.core.orka.ai \
  substrateactorpools.core.orka.ai \
  tasks.core.orka.ai \
  tools.core.orka.ai; do
  expect_gate_failure "missing shared CRD: ${crd}" FAKE_MISSING_CRD="${crd}"
done
for schema_mode in v2-only no-served-version missing-enum; do
  expect_gate_failure 'AgentRuntime CRD is not the shared orka.harness.v1/orka.harness.v2 schema' FAKE_SCHEMA_MODE="${schema_mode}"
done
for schema_mode in v2-only missing-immutability; do
  expect_gate_failure 'Agent CRD is missing the immutable shared contract selector' \
    FAKE_AGENT_SCHEMA_MODE="${schema_mode}"
done
for schema_mode in missing-authority missing-immutability legacy-disposition old-spec-immutability; do
  expect_gate_failure 'Task CRD is missing the static-mode execution-authority schema' \
    FAKE_TASK_SCHEMA_MODE="${schema_mode}"
done
expect_gate_failure 'shared CRD is not Established: runtimepools.core.orka.ai' \
  FAKE_UNESTABLISHED_CRD=runtimepools.core.orka.ai
for obsolete_crd in \
  agentexecutioncontrols.core.orka.ai \
  agentexecutionpolicies.core.orka.ai \
  agentexecutionadjudications.core.orka.ai; do
  expect_gate_failure "unsupported superseded coexistence CRD remains installed: ${obsolete_crd}" \
    FAKE_OBSOLETE_CRD="${obsolete_crd}"
done

grep -Eq '^deploy: .*verify-static-mode-crds' "${root}/Makefile"
if grep -Eq '^deploy: .*verify-(coexistence-crds|acp-crd-cutover)' "${root}/Makefile"; then
  echo 'deploy still depends on a superseded coexistence or hard-cutover gate' >&2
  exit 1
fi
namespace_helper_line="$(grep -nF 'scripts/lib/ensure-static-mode-namespace.sh' "${root}/Makefile" | head -n1 | cut -d: -f1 || true)"
first_secret_line="$(grep -nF 'get secret acp-artifact-capability' "${root}/Makefile" | head -n1 | cut -d: -f1 || true)"
if [[ ! "${namespace_helper_line}" =~ ^[0-9]+$ || ! "${first_secret_line}" =~ ^[0-9]+$ ]] ||
  ((namespace_helper_line >= first_secret_line)); then
  echo 'deploy must establish the static namespace identity before writing installation Secrets' >&2
  exit 1
fi
if grep -Fq 'create namespace orka-system --dry-run=client' "${root}/Makefile"; then
  echo 'deploy must not create or adopt an unlabeled orka-system namespace' >&2
  exit 1
fi
grep -F 'RUN_CONTROLLER_MODE ?= harness-v2' "${root}/Makefile" >/dev/null
grep -F 'RUN_WATCH_NAMESPACE ?= orka-system' "${root}/Makefile" >/dev/null
grep -F -- '--controller-mode="$(RUN_CONTROLLER_MODE)"' "${root}/Makefile" >/dev/null
grep -F -- '--watch-namespace="$(RUN_WATCH_NAMESPACE)"' "${root}/Makefile" >/dev/null
if grep -Eq 'RUN_LEGACY_FENCE_NAMESPACE|agent-execution-(host-mode|legacy-fence-namespace)' "${root}/Makefile"; then
  echo 'run target still uses superseded execution-host flags' >&2
  exit 1
fi

kustomize="${KUSTOMIZE:-${root}/bin/kustomize}"
rendered_default="$("${kustomize}" build "${root}/config/default")"
controller_store_pvc_count="$(
  awk '
    function finish_document() {
      if (kind == "PersistentVolumeClaim" && name == "orka-controller-manager-store" && namespace == "orka-system") {
        count++
      }
    }
    /^---$/ {
      finish_document()
      kind = ""
      name = ""
      namespace = ""
      in_metadata = 0
      next
    }
    /^kind: / {
      kind = $2
      next
    }
    /^metadata:$/ {
      in_metadata = 1
      next
    }
    /^[^ ]/ {
      in_metadata = 0
    }
    in_metadata && /^  name: / {
      name = $2
      next
    }
    in_metadata && /^  namespace: / {
      namespace = $2
    }
    END {
      finish_document()
      print count + 0
    }
  ' <<<"${rendered_default}"
)"
[[ "${controller_store_pvc_count}" == "1" ]] || {
  echo 'default manifest must contain one orka-system/orka-controller-manager-store PVC' >&2
  exit 1
}
[[ "$(grep -Fc 'claimName: orka-controller-manager-store' <<<"${rendered_default}")" == "1" ]] || {
  echo 'default manifest must mount the transformed controller store PVC exactly once' >&2
  exit 1
}
if grep -Fq 'claimName: controller-manager-store' <<<"${rendered_default}"; then
  echo 'default manifest retains the untransformed controller store PVC reference' >&2
  exit 1
fi

printf '%s\n' 'ok - static-mode deployment requires the Established 26-CRD shared bundle, dual AgentRuntime/Agent/Task selectors, and no superseded coexistence CRDs'
