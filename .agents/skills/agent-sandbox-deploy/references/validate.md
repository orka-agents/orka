# Agent Sandbox Deploy — Validation

Validation steps for `$agent-sandbox-deploy`. Read after the standard workflow completes.

> **Known gate (verified live 2026-06): valid enabled provider-based agent
> workspace requests are rejected during execution planning by the current
> service-backed harness runtime.** After workspace validation/resolution, the
> provider-based request documented below fails with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet`. The gate is in
> `internal/controller/agent_execution_plan.go` (`planAgentExecution`), not a
> misconfiguration. The agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→sandbox-workspace path for agents is not wired through it yet. A **plain**
> agent Task (no `execution.workspace`) runs fine through the harness + model
> proxy, so use that to confirm the model path. The model-free e2e confirms
> installation/configuration and exercises the direct workspace adapter through
> SandboxClaim readiness, router exec, delete, retained release/reuse, and final
> claim cleanup. It skips only the full execution-workspace Task smoke while the
> harness gate is present. Treat the execution-workspace YAML in the optional
> expected-failure check as the intended future Task API once the harness wires
> workspaces.

Do **not** use an execution-workspace agent Task as the success criterion yet.
Validate the three current surfaces separately: installation/configuration,
direct workspace-adapter lifecycle, and the model path through a plain agent
Task.

- **Model path through the harness** (requires the optional `AGENTIC=1` step and
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

- **Model-free installation/configuration and direct workspace-adapter
  paths**: run `scripts/live-agent-sandbox-e2e.sh` from the `Model-free CI
  parity` section for a self-contained cluster bring-up with a model-free/
  no-network runtime. It verifies provider installation, applies the controller
  flags and confirms rollout, then exercises claim → ready → router exec →
  delete and retained release/reuse → claim cleanup through
  `AgentSandboxExecutor`. It skips only the full Orka agent Task
  workspace path while the harness gate is present.

If you need to demonstrate the intended API shape before harness workspace
support lands, run it only as an **expected-failure** check and wait for the gate
instead of `Succeeded`:

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
      templateRef:
        name: orka-live-template
      reusePolicy: none
      cleanupPolicy: delete
  prompt: "Reply exactly: ORKA_LIVE_SANDBOX_OK"
YAML

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.executionWorkspace.reason}'=WorkspaceValidationFailed \
  task/orka-live-sandbox-smoke --timeout=2m
```

Once the harness wires agent Tasks to execution workspaces, the expected-failure
check can become the live success smoke. At that point, a successful sandbox
wrapper log should include the claimed workspace name, e.g. `completed in
sandbox workspace sandbox-claim-...`. Orka Task status does **not** expose
sandbox claim/exec/cleanup state — read worker logs and upstream agent-sandbox
resources for lifecycle detail.

## Model-free CI parity

`scripts/live-agent-sandbox-e2e.sh` (run by the `Live Agent Sandbox E2E`
workflow) uses a model-free/no-network runtime. It creates the named kind cluster
when absent or reuses it when present. After validating installation, router
health, and controller-flag rollout, it runs a direct `AgentSandboxExecutor`
smoke that creates SandboxClaims, waits for readiness, executes through the
router, deletes one claim, retains and reuses another, and performs final claim
cleanup. It skips only the full Orka agent Task workspace smoke, so it proves the
provider-adapter path but not Task-to-workspace controller routing, Task status/
result wiring, harness execution, or model access:

```bash
bash scripts/live-agent-sandbox-e2e.sh
```

If the script creates the cluster, it deletes it on exit. If the named cluster
already exists, the script reuses it and leaves its changes in place. Do not run
it against a cluster you need to keep untouched.
