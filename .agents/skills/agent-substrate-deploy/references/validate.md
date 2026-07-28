# Agent Substrate Deploy — Validation

Validation steps for `$agent-substrate-deploy`. Read after the standard workflow completes.

> **Known gate (verified live 2026-06): agent Tasks with an execution workspace
> are rejected by the current service-backed harness runtime.** A
> `provider: substrate` (or `agent-sandbox`) agent Task fails immediately with
> `status.executionWorkspace.reason=WorkspaceValidationFailed` and message
> `execution workspace is not supported by harness runtime yet`. This is an
> unconditional gate in `internal/controller/agent_execution_plan.go`
> (`planAgentExecution`), not a misconfiguration — the agent CLI runtimes now
> run through the long-lived `agent-harness-wrapper` service, and the
> Task→workspace path for agents is not wired through it yet. The bundled e2e
> reflects this: it prints `Skipping agent Task execution-workspace checks:
> harness-wrapper runtime is service-backed`. What IS validated end-to-end today
> is the **direct** Substrate path (actor create/resume/router/daemon exec/
> suspend/delete) plus Substrate-backed MCP tool create/reconcile/cleanup,
> exercised during standup. After clearing the e2e's fake `CODEX_CLI_PATH`
> override in standard workflow step 4 (`Add the model proxy (vekil) — pause
> for the human`) of `../SKILL.md`, a **plain** agent Task (no
> `execution.workspace`)
> runs through the harness + model proxy. Treat the
> Task YAML below as the intended API once the harness wires workspaces; until
> then, validate via the e2e's direct-actor exercises.

The installer leaves a fully wired cluster. During standup it smoke-tests direct
actor create/resume/exec/suspend/delete and Substrate-backed MCP tool lifecycle.
It does **not** currently smoke-test retained workspace reuse for Orka agent
Tasks because those execution-workspace checks are skipped by the harness gate.

If you skipped standard workflow step 3 (`Export kubeconfig for follow-up
kubectl commands`) in `../SKILL.md`, do it before any
manual `kubectl` commands — the e2e standup uses an isolated kubeconfig and does
**not** leave `kind-<KIND_CLUSTER>` in your default one. Keep using the scoped
`KUBECONFIG` in that shell:

```bash
cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ctx="kind-${cluster}"
export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
```

To drive an Orka Task yourself (intended shape; currently gated as noted above):

```bash
cluster="${KIND_CLUSTER:-orka-agent-substrate-e2e}"
ctx="kind-${cluster}"
export KUBECONFIG="$(mktemp -t orka-substrate-kubeconfig.XXXXXX)"
kind export kubeconfig --name "${cluster}" --kubeconfig "${KUBECONFIG}"
kubectl --context "$ctx" -n default apply -f - <<'YAML'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: substrate-smoke
  namespace: default
spec:
  type: agent
  agentRef:
    name: codex-substrate-ci
  prompt: "Run make test and summarize the result."
  sessionRef:
    name: substrate-demo
    create: true
  execution:
    workspace:
      enabled: true
      provider: substrate
      templateRef:
        name: orka-codex-ci
        namespace: ate-demo
      reusePolicy: session
      cleanupPolicy: retain
YAML

kubectl --context "$ctx" -n default get task substrate-smoke -o yaml
```

Check the provider-neutral workspace lifecycle in
`status.executionWorkspace` (`phase`, `placement`, `density`, `resumeLatency`).
Status is intentionally sanitized — it must not expose actor IDs, snapshot URIs,
worker pod IPs, daemon URLs, or tokens.

## CI parity

`scripts/agent-substrate-e2e.sh` (the `Agent Substrate E2E` workflow) runs the
same path end-to-end and is secret-free. Run it directly when you want a clean,
self-contained validation with its own cluster lifecycle:

```bash
PATH="$(go env GOPATH)/bin:$PATH" SUBSTRATE_E2E_EXTENDED=1 bash scripts/agent-substrate-e2e.sh
```

Set `KEEP_CLUSTER=1` to inspect the cluster after a failure.
