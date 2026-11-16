# Agent Sandbox Deploy — Validation

Validation steps for `$agent-sandbox-deploy`. Read after the standard workflow completes.

> **Gate:** Class-backed ACP workspace dispatch requires the controller's
> workspace provider API, `--acp-workspace-dispatch-enabled`, class-use admission,
> Task provenance admission, and an admitted external provider whose class
> requires `acp.runtime.v2`. There is no core Agent Sandbox flag. Without these,
> a `Task.spec.execution.workspace.classRef` Task fails closed.

Validate the relevant surfaces separately: installation/configuration, the
installed backend's persistence, the real Core RuntimeSession path, and external
model access when configured.

- **Installation:** the agent-sandbox controllers are available, the
  `orka-live-template` SandboxTemplate exists, the Sandbox provider Deployment is
  Ready, and its `ExecutionWorkspaceProvider` registration reports supported
  contracts and a fresh heartbeat. The `ExecutionWorkspaceClass` reports
  admitted.

- **Model path through ACP** (requires the optional `AGENTIC=1` step and vekil
  ready): run a plain agent Task with no `execution.workspace` and wait for it to
  succeed.

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

- **Class-backed workspace Task:** with the gates on and a class named
  `sandbox-coding`, the same Agent runs in a Sandbox-hosted RuntimeSession:

```bash
"$kindctl" kubectl -n demo-magic apply -f - <<'YAML'
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
      classRef:
        name: sandbox-coding
      reusePolicy: none
  prompt: "Reply exactly: ORKA_LIVE_SANDBOX_OK"
YAML

"$kindctl" kubectl -n demo-magic \
  wait --for=jsonpath='{.status.phase}'=Succeeded task/orka-live-sandbox-smoke --timeout=10m
```

  With a gate off, the Task instead fails closed with a workspace validation
  reason. Task status carries provider-neutral workspace phase and reason only,
  never claim or Sandbox names.

## No-external-model proofs

Both proofs live in the orka-workspace checkout at the pinned revision and use
its kindctl tags. They preserve their clusters and write artifacts outside the
repository.

- `bash scripts/external-sandbox-e2e.sh` installs upstream agent-sandbox through
  this repository's `hack/demos/cluster/install-agent-sandbox.sh` (set
  `ORKA_AGENT_SANDBOX_INSTALLER` when the Core checkout is not adjacent), deploys
  the external Sandbox provider, and proves data-only suspension, cold resume
  with unchanged PVC/PV identity and contents, and exact Pod/PVC/PV deletion. Its
  fixture does not run Orka credential bootstrap or a RuntimeSession.
- `ORKA_CORE_BUILD_RELEASED=1 ORKA_CORE_SOURCE=/path/to/orka-checkout scripts/external-workspace-e2e.sh core`
  runs a real authenticated RuntimeSession and Task through Core and an external
  provider with a deterministic model-free ACP agent.

Together they cover installed-backend persistence and the generic Core handoff.
Neither proves a model-backed Sandbox Task; use the class-backed smoke above for
that.
