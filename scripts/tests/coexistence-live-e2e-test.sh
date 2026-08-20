#!/usr/bin/env bash
# Static invariants for the live harness v1/v2 coexistence E2E.
set -Eeuo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
e2e_script="${root}/scripts/coexistence-live-e2e.sh"
fake_agent="${root}/scripts/fixtures/coexistence-fake-agent.sh"
workflow="${root}/.github/workflows/coexistence-live-e2e.yml"

fail() {
  echo "$1" >&2
  exit 1
}

[[ -x "${e2e_script}" ]] || fail "coexistence live E2E script must be executable"
[[ -f "${workflow}" ]] || fail "coexistence live E2E workflow is missing"

bash -n "${e2e_script}"
sh -n "${fake_agent}"

# The script must reuse the shared E2E libraries instead of reimplementing
# them. Anchor on the executed source lines, not header comments.
grep -Fq '. "${script_dir}/lib/e2e-common.sh"' "${e2e_script}" || fail 'E2E must source scripts/lib/e2e-common.sh'
grep -Fq '. "${script_dir}/lib/redact.sh"' "${e2e_script}" || fail 'E2E must source scripts/lib/redact.sh'
grep -Fq '. "${script_dir}/lib/kind-local-registry.sh"' "${e2e_script}" || fail 'E2E must source scripts/lib/kind-local-registry.sh'
grep -Fq '| redact >&2' "${e2e_script}" || fail 'E2E diagnostics must be piped through redact'

# Cluster, registry, and kubeconfig lifecycle safety: only clusters created by
# this invocation may be deleted, their owned registry is removed with them,
# and retained clusters keep their registry. The user's global kubeconfig stays
# untouched.
grep -Fq 'cluster_created_by_run=1' "${e2e_script}" || fail 'E2E must track whether it created the kind cluster'
cleanup_guard_line="$(grep -nF 'if [[ "${cluster_created_by_run}" == "1" && "${keep_cluster}" != "1" ]]; then' "${e2e_script}" | head -n1 | cut -d: -f1)"
registry_stop_line="$(grep -nF 'orka_kind_registry_stop "${kind_cluster}" "${registry_owner}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
cluster_delete_line="$(grep -nF 'kind delete cluster --name "${kind_cluster}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
for line in "${cleanup_guard_line}" "${registry_stop_line}" "${cluster_delete_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing ownership-scoped cluster and registry cleanup'
done
((cleanup_guard_line < registry_stop_line && registry_stop_line < cluster_delete_line)) ||
  fail 'E2E must remove the owned registry only when it also removes the owned cluster'
grep -Fq 'Retaining the owned kind registry because cluster ${kind_cluster} is retained' "${e2e_script}" ||
  fail 'E2E must retain its registry when the cluster is retained or reused'
grep -Fq 'export KUBECONFIG="${work_dir}/kubeconfig"' "${e2e_script}" ||
  fail 'E2E must use an isolated kubeconfig instead of mutating global state'
grep -Fq 'orka_kind_registry_start "${kind_cluster}" "${registry_owner}"' "${e2e_script}" ||
  fail 'E2E must start the kind registry with a per-run ownership label'

# Fail-closed ordering: shared CRD wave, then mode-labeled namespace identity,
# then secrets, then the two Helm releases.
crd_line="$(grep -nF 'run bash "${script_dir}/apply-helm-crds.sh"' "${e2e_script}" | head -n1 | cut -d: -f1)"
v1_ns_line="$(grep -nF 'run bash "${script_dir}/lib/ensure-static-mode-namespace.sh" kubectl "${v1_namespace}" harness-v1' "${e2e_script}" | head -n1 | cut -d: -f1)"
v2_ns_line="$(grep -nF 'run bash "${script_dir}/lib/ensure-static-mode-namespace.sh" kubectl "${v2_namespace}" harness-v2' "${e2e_script}" | head -n1 | cut -d: -f1)"
secret_line="$(grep -nF 'create_namespace_secrets "${v1_namespace}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
v1_install_line="$(grep -nF 'helm install "${v1_release}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
v2_install_line="$(grep -nF 'helm install "${v2_release}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
for line in "${crd_line}" "${v1_ns_line}" "${v2_ns_line}" "${secret_line}" "${v1_install_line}" "${v2_install_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing the CRD wave, namespace identity, secret, or Helm install steps'
done
((crd_line < v1_ns_line && v1_ns_line < v2_ns_line && v2_ns_line < secret_line && secret_line < v1_install_line && v1_install_line < v2_install_line)) ||
  fail 'E2E must apply CRDs, claim mode-labeled namespaces, and write secrets before installing either release'

# Reuse safety: a pre-existing cluster must be proven free of every Orka CRD
# and mode-labeled namespace as well as the fixed coexistence namespaces and
# Helm releases before any registry, CRD, or secret write.
guard_call_line="$(grep -nE '^[[:space:]]+assert_reused_cluster_is_unclaimed$' "${e2e_script}" | head -n1 | cut -d: -f1)"
registry_line="$(grep -nF 'orka_kind_registry_start "${kind_cluster}" "${registry_owner}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
for line in "${guard_call_line}" "${registry_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing the cluster-reuse refusal guard'
done
((guard_call_line < registry_line && registry_line < crd_line && crd_line < secret_line)) ||
  fail 'E2E must refuse a claimed pre-existing cluster before any registry, CRD, or secret write'
grep -Fq 'kubectl get namespace "${ns}"' "${e2e_script}" ||
  fail 'reuse guard must check for the fixed coexistence namespaces'
grep -Fq 'kubectl get customresourcedefinitions.apiextensions.k8s.io -o name' "${e2e_script}" ||
  fail 'reuse guard must reject any existing Orka CRD before replacing the shared CRD wave'
grep -Fq "kubectl get namespaces -l 'orka.ai/controller-mode' -o name" "${e2e_script}" ||
  fail 'reuse guard must reject any existing mode-labeled Orka namespace'
grep -Fq '"${crd}" == *.orka.ai' "${e2e_script}" ||
  fail 'reuse guard must classify cluster-wide Orka CRDs rather than only fixed resource names'
grep -Fq 'helm status "${release}" --namespace "${release_ns}"' "${e2e_script}" ||
  fail 'reuse guard must check for existing coexistence Helm releases'
grep -Fq 'refusing to reuse pre-existing kind cluster' "${e2e_script}" ||
  fail 'reuse guard must die with an operator-actionable refusal message'

# Both releases install against the shared platform-owned CRD wave.
[[ "$(grep -c -- '--skip-crds' "${e2e_script}")" -ge 2 ]] ||
  fail 'both Helm releases must install with --skip-crds'
grep -Fq -- '--set controller.mode=harness-v1' "${e2e_script}" || fail 'v1 release must set controller.mode=harness-v1'
grep -Fq -- '--set controller.mode=harness-v2' "${e2e_script}" || fail 'v2 release must set controller.mode=harness-v2'

# Admission must be proven in both directions with the exact webhook denials.
grep -Fq 'AgentRuntime contractVersion must match namespace execution mode "harness-v1"' "${e2e_script}" ||
  fail 'E2E must assert v2-contract AgentRuntime denial in the v1 namespace'
grep -Fq 'AgentRuntime contractVersion must match namespace execution mode "harness-v2"' "${e2e_script}" ||
  fail 'E2E must assert v1-contract AgentRuntime denial in the v2 namespace'
grep -Fq 'Agent contractVersion must match namespace execution mode "harness-v1"' "${e2e_script}" ||
  fail 'E2E must assert v2-contract Agent denial in the v1 namespace'
grep -Fq 'Agent contractVersion must match namespace execution mode "harness-v2"' "${e2e_script}" ||
  fail 'E2E must assert v1-contract Agent denial in the v2 namespace'

# The v1 execution proof must run before the restart-recovery proof, and the
# restart must wait for both the controller-side in-flight projection and the
# wrapper-side durable-admission marker of the held turn.
task_line="$(grep -nF 'task_phase_is coexistence-v1-task Succeeded' "${e2e_script}" | head -n1 | cut -d: -f1)"
hold_line="$(grep -nF 'task_harness_state_in coexistence-v1-restart-task Submitting SubmittedUnknown Accepted Running' "${e2e_script}" | head -n1 | cut -d: -f1)"
marker_line="$(grep -nF 'test -f /tmp/coexistence-hold-turn-active' "${e2e_script}" | head -n1 | cut -d: -f1)"
restart_line="$(grep -nF 'rollout restart "deployment/${v1_controller_deployment}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
settle_line="$(grep -nF 'task_phase_is coexistence-v1-restart-task Succeeded' "${e2e_script}" | head -n1 | cut -d: -f1)"
for line in "${task_line}" "${hold_line}" "${marker_line}" "${restart_line}" "${settle_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing the v1 execution or restart-recovery steps'
done
((task_line < hold_line && hold_line < marker_line && marker_line < restart_line && restart_line < settle_line)) ||
  fail 'E2E must complete a v1 Task, prove the wrapper durably admitted a held turn, restart the controller, then assert settlement'
grep -Fq ': >/tmp/coexistence-hold-turn-active' "${fake_agent}" ||
  fail 'fake agent must write the durable-admission hold marker before sleeping'

# v2 readiness must be sampled continuously across the v1 restart: sampler
# started before the restart, stopped both on the main path after the
# replacement controller settles and on every exit path, and asserted to have
# recorded zero violations.
monitor_start_line="$(grep -nE '^[[:space:]]+start_v2_readiness_monitor$' "${e2e_script}" | head -n1 | cut -d: -f1)"
exit_stop_line="$(grep -nE '^[[:space:]]+stop_v2_readiness_monitor$' "${e2e_script}" | head -n1 | cut -d: -f1)"
main_stop_line="$(grep -nE '^[[:space:]]+stop_v2_readiness_monitor$' "${e2e_script}" | tail -n1 | cut -d: -f1)"
for line in "${monitor_start_line}" "${exit_stop_line}" "${main_stop_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing the continuous v2 readiness sampler'
done
((exit_stop_line < monitor_start_line && monitor_start_line < restart_line && restart_line < main_stop_line)) ||
  fail 'E2E must stop the v2 readiness sampler on exit paths and bracket the v1 restart with start/stop'
grep -Fq '>>"${v2_readiness_violations}"' "${e2e_script}" ||
  fail 'v2 readiness sampler must record violations into the violations file'
grep -Fq '[[ -s "${v2_readiness_violations}" ]]' "${e2e_script}" ||
  fail 'E2E must assert the v2 readiness violations file stayed empty'
grep -Fq 'v2 controller lost readiness during the v1 controller restart' "${e2e_script}" ||
  fail 'E2E must fail when the sampler observed a v2 readiness violation'

# Isolation matrix: RBAC denial both directions plus NetworkPolicy selection.
grep -Fq 'kubectl auth can-i' "${e2e_script}" || fail 'E2E must run kubectl auth can-i isolation checks'
grep -Fq -- '--as="${v1_controller_sa}"' "${e2e_script}" || fail 'E2E must issue a real impersonated cross-namespace request as the v1 controller'
grep -Fq -- '--as="${v2_controller_sa}"' "${e2e_script}" || fail 'E2E must issue a real impersonated cross-namespace request as the v2 controller'
grep -Fq 'get networkpolicy' "${e2e_script}" || fail 'E2E must assert NetworkPolicies in both namespaces'

# The audited gap: check-legacy-wrapper-resources.sh must be executed live in
# its coexistence branch, and its default branch must be exercised both while
# the wrapper exists and after v1 retirement.
grep -Fq 'COEXISTENCE=1 bash "${script_dir}/check-legacy-wrapper-resources.sh"' "${e2e_script}" ||
  fail 'E2E must run check-legacy-wrapper-resources.sh with COEXISTENCE=1'
grep -Fq 'coexistence mode: wrapper resources are allowed' "${e2e_script}" ||
  fail 'E2E must assert the coexistence verdict of check-legacy-wrapper-resources.sh'
grep -Fq 'legacy harness-wrapper resources remain' "${e2e_script}" ||
  fail 'E2E must assert the default-branch verdict while the wrapper exists'

# v1 retirement must explicitly remove every test-created object: gracefully
# wait for Tasks and the Agent while the v1 controller and wrapper can still
# settle finalizers, then remove the custom-named Secrets, the fake-agent
# ConfigMap, and the helm-kept ledger PVC, with a leftover assertion before the
# final retirement check.
task_delete_line="$(grep -nF 'delete task coexistence-v1-task coexistence-v1-restart-task' "${e2e_script}" | head -n1 | cut -d: -f1)"
agent_delete_line="$(grep -nF 'delete agent coexistence-v1-agent' "${e2e_script}" | head -n1 | cut -d: -f1)"
uninstall_line="$(grep -nF 'helm uninstall "${v1_release}"' "${e2e_script}" | head -n1 | cut -d: -f1)"
secret_delete_line="$(grep -nF 'orka-agent-snapshot-key orka-webhook-tls orka-wrapper-auth orka-wrapper-tls \' "${e2e_script}" | head -n1 | cut -d: -f1)"
configmap_delete_line="$(grep -nF 'delete configmap coexistence-fake-agent --ignore-not-found' "${e2e_script}" | head -n1 | cut -d: -f1)"
pvc_delete_line="$(grep -nF 'delete pvc "${v1_release}-harness-v1-ledger"' "${e2e_script}" | head -n1 | cut -d: -f1)"
leftover_line="$(grep -nF 'test-created objects survived v1 retirement' "${e2e_script}" | head -n1 | cut -d: -f1)"
final_check_line="$(grep -nF 'bash "${script_dir}/check-legacy-wrapper-resources.sh"' "${e2e_script}" | tail -n1 | cut -d: -f1)"
for line in "${task_delete_line}" "${agent_delete_line}" "${uninstall_line}" "${secret_delete_line}" "${configmap_delete_line}" "${pvc_delete_line}" "${leftover_line}" "${final_check_line}"; do
  [[ "${line}" =~ ^[0-9]+$ ]] || fail 'E2E is missing the explicit test-object retirement cleanup'
done
((task_delete_line < agent_delete_line && agent_delete_line < uninstall_line &&
  uninstall_line < secret_delete_line && secret_delete_line < configmap_delete_line &&
  configmap_delete_line < pvc_delete_line && pvc_delete_line < leftover_line &&
  leftover_line < final_check_line)) ||
  fail 'E2E must delete Tasks/Agent before uninstall, then Secrets/ConfigMap/PVC, then assert no leftovers before the final retirement check'
grep -Fq -- '--ignore-not-found --wait=true --timeout=120s' "${e2e_script}" ||
  fail 'E2E must wait for graceful Task finalization before uninstalling the v1 controller and wrapper'
if grep -Fq '"finalizers":null' "${e2e_script}"; then
  fail 'E2E must not bypass the Task cleanup finalizer during v1 retirement'
fi

# No controller-owned resources in the pre-isolation default namespace.
if grep -Eq '(^|[[:space:]])-n[[:space:]]+default([[:space:]]|$)|^[[:space:]]*namespace:[[:space:]]+default([[:space:]]|$)' "${e2e_script}"; then
  fail 'E2E must not place resources in the default namespace'
fi

# The fake agent fixture must stay model-free and offline.
if grep -Eq 'curl|wget|http://|https://|nc[[:space:]]' "${fake_agent}"; then
  fail 'fake agent fixture must not perform network access'
fi
grep -Fq 'cat' "${fake_agent}" || fail 'fake agent must read the turn prompt from stdin'

# Workflow shape: secret-free triggers and the shared composite actions.
grep -Fq 'workflow_dispatch' "${workflow}" || fail 'workflow must support manual dispatch'
grep -Fq 'pull_request' "${workflow}" || fail 'workflow must run on pull requests'
grep -Fq './.github/actions/free-disk-space' "${workflow}" || fail 'workflow must free disk space before Docker-heavy builds'
grep -Fq './.github/actions/setup-kind' "${workflow}" || fail 'workflow must install kind via the pinned composite action'
grep -Fq 'go-version-file: go.mod' "${workflow}" || fail 'workflow must pin Go via go.mod'
grep -Fq 'bash scripts/coexistence-live-e2e.sh' "${workflow}" || fail 'workflow must run the coexistence live E2E script'
if grep -Fq 'secrets.' "${workflow}"; then
  fail 'coexistence live E2E workflow must remain secret-free'
fi

echo 'coexistence-live-e2e static checks passed'
