# Agent Sandbox Deploy — Validation

Validation steps for `$agent-sandbox-deploy`. Read after the standard workflow completes.

> **Known gate (verified live 2026-06): agent Tasks with an execution workspace
> are rejected by the current service-backed harness runtime.** Any Task that
> sets `spec.execution.workspace` fails immediately with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet` — an
> unconditional gate in `internal/controller/harness_wrapper.go`
> (`runHarnessWrapperTask`), not a misconfiguration. The agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→sandbox-workspace path for agents is not wired through it yet. A **plain**
> agent Task (no `execution.workspace`) runs fine through the harness + model
> proxy, so use that to confirm the model path. The model-free e2e currently
> confirms installation/configuration only; it deliberately skips the
> execution-workspace Task smoke while the harness gate is present. Treat only
> the execution-workspace YAML in the optional expected-failure check as the
> intended future API once the harness wires workspaces.

Do **not** use an execution-workspace agent Task as the success criterion yet.
Validate the two currently wired paths separately:

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

- **Installation/configuration parity**: run `scripts/live-agent-sandbox-e2e.sh`
  from the `Model-free CI parity` section when you want a self-contained cluster
  bring-up with fake model
  credentials. It verifies the install/config path, but it does **not** exercise
  claim → ready → exec → cleanup while the harness gate is present.

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
workflow) stands up a clean kind cluster with fake model credentials and **no
model access**. Because the current harness-wrapper runtime is service-backed,
the script logs `Skipping agent-sandbox Task smoke...` and does not create a
SandboxClaim or exercise the router exec data path. Use it for installation and
controller-flag CI parity, not as proof of workspace execution:

```bash
bash scripts/live-agent-sandbox-e2e.sh
```

That script owns its own cluster lifecycle; do not run it against a kindctl
cluster you want to keep.
