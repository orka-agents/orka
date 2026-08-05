# Agent Sandbox Deploy — Validation

Validation steps for `$agent-sandbox-deploy`. Read after the standard workflow completes.

> **Known gate:** Orka ACP RuntimeSessions do not yet map to agent-sandbox claims. The direct adapter lifecycle is supported for local validation, but a `Task.spec.execution.workspace` agent Task must still fail closed with `WorkspaceValidationFailed`. Plain Codex/Claude Tasks run through controller-owned ACP RuntimePools and validate the model path separately.

Do **not** use an execution-workspace agent Task as the success criterion yet.
Validate the three current surfaces separately: installation/configuration,
direct workspace-adapter lifecycle, and the model path through a plain agent
Task.

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

- **Model-free installation/configuration and direct workspace-adapter
  paths**: run `scripts/live-agent-sandbox-e2e.sh` from the `Model-free CI
  parity` section for a self-contained cluster bring-up with a model-free/
  no-network runtime. It verifies provider installation, applies the controller
  flags and confirms rollout, then exercises claim → ready → router exec →
  delete and retained release/reuse → claim cleanup through
  `AgentSandboxExecutor`. It skips only the full Orka agent Task
  workspace path while the ACP workspace-dispatch gate is present.

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

Once ACP RuntimeSessions map agent Tasks to execution workspaces, the expected-failure
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
result wiring, ACP Task execution, or model access:

```bash
bash scripts/live-agent-sandbox-e2e.sh
```

If the script creates the cluster, it deletes it on exit. If the named cluster
already exists, the script reuses it and leaves its changes in place. Do not run
it against a cluster you need to keep untouched.
