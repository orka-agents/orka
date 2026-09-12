# Agent Sandbox Deploy — Validation

Validation steps for `$agent-sandbox-deploy`. Read after the standard workflow completes.

> **Known gate:** Workspace-provider-backed RuntimeSession dispatch is flag-gated behind `--agent-sandbox-enabled` **and** `--acp-workspace-dispatch-enabled`. Manual skill deployments leave the dispatch flag off unless explicitly enabled, so a `Task.spec.execution.workspace` agent Task then fails closed with `WorkspaceValidationFailed`. The bundled E2E enables both gates and runs a real Codex prompt against a local Responses-compatible fixture. The Task must omit `templateRef`; ACP RuntimeSessions run only controller-rendered sandbox templates.

Validate the relevant surfaces separately: installation/configuration, direct
workspace-adapter lifecycle, fixture-backed workspace ACP execution, and
external model access when configured.

- **Model path through ACP** (requires the optional `AGENTIC=1` step and
  vekil ready): run a plain agent Task with no `execution.workspace` and wait
  for it to succeed.

```bash
"$kindctl" kubectl -n demo-magic apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: sandbox-codex-agent
  namespace: demo-magic
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: gpt-5.5
  secretRef:
    name: sandbox-model-key
---
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-model-smoke
  namespace: demo-magic
spec:
  type: agent
  agentRef:
    name: sandbox-codex-agent
  agentRuntime:
    maxTurns: 1
  timeout: 10m0s
  prompt: "Reply exactly: ORKA_LIVE_MODEL_OK"
YAML

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.phase}'=Succeeded task/orka-live-model-smoke --timeout=10m
```

- **No-external-model provider and workspace ACP paths**: run
  `scripts/live-agent-sandbox-e2e.sh` from the CI parity section. It verifies
  provider installation, direct claim → ready → router exec → delete/reuse
  cleanup, enables workspace dispatch, then executes a real Codex prompt through
  a workspace-backed RuntimePool using a local Responses-compatible fixture.

To demonstrate the API shape while the dispatch flag is off, run it as an
**expected-failure** check and wait for the gate instead of `Succeeded`:

```bash
"$kindctl" kubectl apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: sandbox-codex-agent
  namespace: demo-magic
spec:
  runtime:
    type: codex
    defaultMaxTurns: 1
    defaultAllowBash: true
  model:
    name: gpt-5.5
  secretRef:
    name: sandbox-model-key
---
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-sandbox-smoke
  namespace: demo-magic
spec:
  type: agent
  agentRef:
    name: sandbox-codex-agent
  agentRuntime:
    maxTurns: 1
  timeout: 10m0s
  execution:
    workspace:
      enabled: true
      reusePolicy: none
      cleanupPolicy: delete
  prompt: "Reply exactly: ORKA_LIVE_SANDBOX_OK"
YAML

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.executionWorkspace.reason}'=WorkspaceValidationFailed \
  task/orka-live-sandbox-smoke --timeout=2m
```

With `--acp-workspace-dispatch-enabled` set (plus a digest-pinned ACP runtime
image and either the local fixture or provider-proxy model access), the same Task binds a
dedicated `acp-ws-<runtime>-<hash>` RuntimePool whose SandboxClaim hosts the
RuntimeSession, and this becomes a live success smoke waiting for `Succeeded`.
Orka Task status stays provider-neutral (`status.executionWorkspace` carries
provider/phase/reason only, never claim or sandbox names) — read the
RuntimePool status and upstream agent-sandbox resources for lifecycle detail.

## No-external-model CI parity

`scripts/live-agent-sandbox-e2e.sh` (run by the `Live Agent Sandbox E2E`
workflow) uses a local Responses-compatible fixture and no external model
access. It creates the named kind cluster when absent or reuses it when present.
After validating installation, router health, and controller-flag rollout, it
runs the direct `AgentSandboxExecutor` lifecycle and a workspace-backed ACP Task
through the real Codex supervisor, waits for `Succeeded`, verifies provider-
neutral status, and cleans up. Set `ORKA_AGENT_SANDBOX_ACP_TASK_SMOKE=0` only
when intentionally skipping the ACP Task portion:

```bash
bash scripts/live-agent-sandbox-e2e.sh
```

If the script creates the cluster, it deletes it on exit. If the named cluster
already exists, the script reuses it and leaves its changes in place. Do not run
it against a cluster you need to keep untouched.
