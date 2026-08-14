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

# Cluster and kubeconfig lifecycle safety: only clusters created by this
# invocation may be deleted, and the user's global kubeconfig stays untouched.
grep -Fq 'cluster_created_by_run=1' "${e2e_script}" || fail 'E2E must track whether it created the kind cluster'
grep -Fq '[[ "${cluster_created_by_run}" == "1" && "${keep_cluster}" != "1" ]]' "${e2e_script}" ||
  fail 'E2E cleanup must delete only clusters owned by this invocation'
grep -Fq 'export KUBECONFIG="${work_dir}/kubeconfig"' "${e2e_script}" ||
  fail 'E2E must use an isolated kubeconfig instead of mutating global state'
grep -Fq 'orka_kind_registry_start "${kind_cluster}" "${registry_owner}"' "${e2e_script}" ||
  fail 'E2E must start the kind registry with a per-run ownership label'
grep -Fq 'orka_kind_registry_stop "${kind_cluster}" "${registry_owner}"' "${e2e_script}" ||
  fail 'E2E must stop only the registry owned by this run'

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
