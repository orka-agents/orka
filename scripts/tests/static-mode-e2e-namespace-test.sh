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
label_script="${root}/scripts/live-github-label-trigger-e2e.sh"
e2e_suite="${root}/test/e2e/e2e_suite_test.go"
tls_helper="${root}/scripts/lib/e2e-admission-tls.sh"

# shellcheck source=scripts/lib/e2e-admission-tls.sh
. "${tls_helper}"

grep -Fq 'test_namespace="${ORKA_SECURITY_SCAN_E2E_NAMESPACE:-${orka_namespace}}"' "${security_script}"
grep -Fq '[[ "${test_namespace}" == "${orka_namespace}" ]]' "${security_script}"
grep -Fq 'ORKA_NAMESPACE=orka-system' "${substrate_script}"
grep -Fq 'source "${ROOT_DIR}/scripts/lib/e2e-admission-tls.sh"' "${substrate_script}"
grep -Fq 'source "${ROOT_DIR}/scripts/lib/substrate-orka-local.sh"' "${substrate_script}"
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

grep -Fq 'config/orka-admission"' "${tls_helper}"
grep -Fq 'config/orka-admission-webhooks"' "${tls_helper}"
grep -Fq 'rollout status deployment/orka-admission' "${tls_helper}"
grep -Fq '.clientConfig.caBundle == $ca' "${tls_helper}"
grep -Fq 'service_proxy="http://127.0.0.1:${admission_proxy_port}/api/v1/namespaces/${namespace}/services/https:orka-admission:443/proxy"' "${tls_helper}"
grep -Fq -- '--arg namespace "${namespace}"' "${tls_helper}"

admission_deploy="$(awk '/^orka_e2e_deploy_admission\(\) \(/,/^\)/' "${tls_helper}")"
admission_endpoints_line="$(grep -nF 'if ((endpoint_count < 2))' <<<"${admission_deploy}" | cut -d: -f1 || true)"
admission_smoke_line="$(grep -nF '_orka_e2e_smoke_admission_handlers' <<<"${admission_deploy}" | tail -1 | cut -d: -f1 || true)"
admission_webhooks_line="$(grep -nF 'apply -f "${render_dir}/webhooks.yaml"' <<<"${admission_deploy}" | cut -d: -f1 || true)"
if [[ ! "${admission_endpoints_line}" =~ ^[0-9]+$ || ! "${admission_smoke_line}" =~ ^[0-9]+$ || ! "${admission_webhooks_line}" =~ ^[0-9]+$ ]] ||
  ((admission_endpoints_line >= admission_smoke_line || admission_smoke_line >= admission_webhooks_line)); then
  echo 'E2E admission must expose two endpoints and smoke every handler before publishing fail-closed webhooks' >&2
  exit 1
fi

if [[ "$(grep -c '^/validate-' <<<"${admission_deploy}")" -ne 10 ]]; then
  echo 'E2E admission smoke must cover all ten checked-in handlers' >&2
  exit 1
fi

admission_test_log="$(mktemp "${TMPDIR:-/tmp}/orka-e2e-admission-test.XXXXXX")"
trap 'rm -f -- "${admission_test_log}"' EXIT

fake_kubectl() {
  local last_arg="${!#}"

  if [[ "$1" == "proxy" ]]; then
    echo 'Starting to serve on 127.0.0.1:43210'
    exec sleep 300
  fi
  if [[ "$1" == "kustomize" ]]; then
    if [[ "$2" == */config/orka-admission-webhooks ]]; then
      cat <<'YAML'
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: orka-admission
webhooks:
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      name: orka-admission
      namespace: orka-system
      path: /validate-v1-namespace-execution-mode
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-v1-secret-workspace-attachment
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-coordination-k8s-io-v1-acp-suspend-quota-lease
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-task-provenance
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-task-workspace-class-use
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-tool-workspace-class-use
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-workspace-orka-ai-v1alpha1-checkpoint-source-use
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-agent-contract
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-agentruntime-contract
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      path: /validate-core-orka-ai-v1alpha1-task-execution-authority
YAML
    else
      cat <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: orka-admission
  namespace: orka-system
spec:
  template:
    spec:
      containers:
      - image: controller:latest
YAML
    fi
    return 0
  fi
  if [[ " $* " == *' get secret orka-admission-tls '* ]]; then
    printf 'dGVzdA=='
    return 0
  fi
  if [[ " $* " == *' rollout status deployment/orka-admission '* ]]; then
    return 0
  fi
  if [[ " $* " == *' get endpoints orka-admission '* ]]; then
    printf '%s\n' '{"subsets":[{"addresses":[{"ip":"10.0.0.1"},{"ip":"10.0.0.2"}]}]}'
    return 0
  fi
  if [[ " $* " == *' apply -f '* ]]; then
    if [[ "${last_arg}" == */webhooks.yaml ]]; then
      printf 'webhooks-applied\n' >>"${admission_test_log}"
    fi
    return 0
  fi
  if [[ " $* " == *' get validatingwebhookconfiguration orka-admission '* ]]; then
    printf '%s\n' '{"webhooks":[{"clientConfig":{"caBundle":"dGVzdA=="}}]}'
    return 0
  fi

  echo "unexpected fake kubectl invocation: $*" >&2
  return 1
}

curl() {
  local data_file="" url="" uid

  while (($# > 0)); do
    case "$1" in
    --data-binary)
      data_file="${2#@}"
      shift 2
      ;;
    http://*)
      url="$1"
      shift
      ;;
    *)
      shift
      ;;
    esac
  done
  [[ -n "${data_file}" && -n "${url}" ]]
  uid="$(jq -r '.request.uid' "${data_file}")"
  printf 'smoke:%s\n' "${url##*/}" >>"${admission_test_log}"
  jq -n --arg uid "${uid}" \
    '{apiVersion:"admission.k8s.io/v1",kind:"AdmissionReview",response:{uid:$uid,allowed:false}}'
}

orka_e2e_deploy_admission example.invalid/controller:test fake_kubectl custom-system

while IFS= read -r handler; do
  grep -Fxq "smoke:${handler}" "${admission_test_log}"
done <<'EOF_EXPECTED_HANDLERS'
validate-v1-namespace-execution-mode
validate-v1-secret-workspace-attachment
validate-coordination-k8s-io-v1-acp-suspend-quota-lease
validate-core-orka-ai-v1alpha1-task-provenance
validate-core-orka-ai-v1alpha1-task-workspace-class-use
validate-core-orka-ai-v1alpha1-tool-workspace-class-use
validate-workspace-orka-ai-v1alpha1-checkpoint-source-use
validate-core-orka-ai-v1alpha1-agent-contract
validate-core-orka-ai-v1alpha1-agentruntime-contract
validate-core-orka-ai-v1alpha1-task-execution-authority
EOF_EXPECTED_HANDLERS

last_smoke_line="$(grep -n '^smoke:' "${admission_test_log}" | tail -1 | cut -d: -f1)"
webhooks_applied_line="$(grep -n '^webhooks-applied$' "${admission_test_log}" | cut -d: -f1)"
if [[ "$(grep -c '^smoke:' "${admission_test_log}")" -ne 10 ]] ||
  ((last_smoke_line >= webhooks_applied_line)); then
  echo 'E2E admission deployment did not smoke all handlers before applying webhooks' >&2
  exit 1
fi

runtime_rendered="$(
  _orka_e2e_render_admission_runtime custom-system example.invalid/controller:test <<'YAML'
metadata:
  namespace: orka-system
spec:
  containers:
  - args:
    - --webhook-service-dns-name=orka-admission.orka-system.svc
    - --controller-usernames=system:serviceaccount:orka-system:orka-controller-manager
    image: controller:latest
YAML
)"
grep -Fq 'namespace: custom-system' <<<"${runtime_rendered}"
grep -Fq 'image: example.invalid/controller:test' <<<"${runtime_rendered}"
grep -Fq 'system:serviceaccount:custom-system:orka-controller-manager' <<<"${runtime_rendered}"
if grep -Fq 'orka-system' <<<"${runtime_rendered}"; then
  echo 'E2E admission runtime rendering retained the default namespace' >&2
  exit 1
fi

webhooks_rendered="$(
  _orka_e2e_render_admission_webhooks custom-system dGVzdA== <<'YAML'
webhooks:
- admissionReviewVersions:
  - v1
  clientConfig:
    service:
      namespace: orka-system
YAML
)"
grep -Fq '    caBundle: dGVzdA==' <<<"${webhooks_rendered}"
grep -Fq '      namespace: custom-system' <<<"${webhooks_rendered}"
if grep -Fq 'orka-system' <<<"${webhooks_rendered}"; then
  echo 'E2E admission webhook rendering retained the default namespace' >&2
  exit 1
fi

live_main="$(awk '/^main\(\) {/,/^}/' "${root}/scripts/live-agent-sandbox-e2e.sh")"
live_tls_line="$(grep -nF 'orka_e2e_bootstrap_admission_tls' <<<"${live_main}" | cut -d: -f1 || true)"
live_runtime_line="$(grep -nF 'run make deploy' <<<"${live_main}" | cut -d: -f1 || true)"
live_admission_line="$(grep -nF 'orka_e2e_deploy_admission' <<<"${live_main}" | cut -d: -f1 || true)"
live_controller_patch_line="$(grep -nF 'patch_controller_for_agent_sandbox' <<<"${live_main}" | cut -d: -f1 || true)"
if [[ ! "${live_tls_line}" =~ ^[0-9]+$ || ! "${live_runtime_line}" =~ ^[0-9]+$ || ! "${live_admission_line}" =~ ^[0-9]+$ || ! "${live_controller_patch_line}" =~ ^[0-9]+$ ]] ||
  ((live_tls_line >= live_runtime_line || live_runtime_line >= live_admission_line || live_admission_line >= live_controller_patch_line)); then
  echo 'live agent-sandbox E2E must bootstrap TLS and run the production admission deployment before enabling protected workspace settlement' >&2
  exit 1
fi

grep -Fq 'agent_sandbox_version="${AGENT_SANDBOX_VERSION:-v1.0.0}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'e2e_kubeconfig="${work_dir}/kubeconfig"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'export KUBECONFIG="${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'run kind export kubeconfig --name "${kind_cluster}" --kubeconfig "${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'run kind create cluster --name "${kind_cluster}" --config "${kind_config}" --kubeconfig "${e2e_kubeconfig}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
if grep -Fq 'kubectl config use-context' "${root}/scripts/live-agent-sandbox-e2e.sh"; then
  echo 'live agent-sandbox E2E must not mutate the user kubeconfig context' >&2
  exit 1
fi
grep -Fq "jsonpath='{.spec.sandboxTemplateRef.name}'" "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'orka_e2e_bootstrap_admission_tls kubectl "${orka_namespace}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'orka_e2e_deploy_admission "${manager_ref}" kubectl "${orka_namespace}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_volume_directory="/durable/orka-workspace"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_session_relative_path="ws-${durable_session_uid}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_marker_relative_path="${durable_session_relative_path}/e2e-durability-marker-${durable_session_uid}"' "${root}/scripts/live-agent-sandbox-e2e.sh"
grep -Fq 'durable_marker_path="${durable_volume_directory}/${durable_marker_relative_path}"' "${root}/scripts/live-agent-sandbox-e2e.sh"

substrate_deploy="$(awk '/^deploy_orka\(\) {/,/^}/' "${substrate_helper}")"
substrate_tls_line="$(grep -nF 'orka_e2e_bootstrap_admission_tls' <<<"${substrate_deploy}" | cut -d: -f1 || true)"
substrate_runtime_line="$(grep -nF 'orka_e2e_deploy_admission' <<<"${substrate_deploy}" | cut -d: -f1 || true)"
substrate_flags_line="$(grep -nF -- '--workspace-class-use-admission-enabled' <<<"${substrate_deploy}" | cut -d: -f1 || true)"
if [[ ! "${substrate_tls_line}" =~ ^[0-9]+$ || ! "${substrate_runtime_line}" =~ ^[0-9]+$ || ! "${substrate_flags_line}" =~ ^[0-9]+$ ]] ||
  ((substrate_tls_line >= substrate_runtime_line || substrate_runtime_line >= substrate_flags_line)); then
  echo 'agent-substrate E2E must bootstrap TLS and install admission before enabling protected workspace settlement' >&2
  exit 1
fi
grep -Fq 'orka_e2e_bootstrap_admission_tls kubectl "${ORKA_NAMESPACE}"' <<<"${substrate_deploy}"
grep -Fq 'orka_e2e_deploy_admission "${controller_image}" kubectl "${ORKA_NAMESPACE}"' <<<"${substrate_deploy}"

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

for script in "${security_script}" "${substrate_script}" "${substrate_helper}" "${label_script}"; do
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
