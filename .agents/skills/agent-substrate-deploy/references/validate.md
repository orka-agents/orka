# Agent Substrate Deploy — Validation

Validation steps for `$agent-substrate-deploy`. Read after the standard workflow completes.

> **Known gate:** Substrate-backed ACP RuntimeSession dispatch requires `--substrate-enabled` **and** `--acp-workspace-dispatch-enabled` plus an infrastructure `templateRef`. The bundled installer/E2E enables the dispatch flag by default and validates a real Codex prompt against a local Responses-compatible fixture; set `SUBSTRATE_E2E_ACP_TASK_SMOKE=0` only to skip that path. A manual deployment with the flag off must still fail closed.

The installer leaves a fully wired cluster. During standup it smoke-tests direct
actor create/resume/exec/suspend/delete, Substrate-backed MCP tool lifecycle,
and a workspace-backed ACP Task through the real Codex supervisor and local
fixture. Retained session reuse is not part of this initial happy-path smoke.

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

To drive an Orka Task yourself on a deployment with workspace dispatch enabled:

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
      cleanupPolicy: delete
YAML

kubectl --context "$ctx" -n default get task substrate-smoke -o yaml
```

With the dispatch flag off, check `status.executionWorkspace.phase=Failed` and
`reason=WorkspaceValidationFailed`; provider/template metadata remains sanitized.
With `--acp-workspace-dispatch-enabled`, an infrastructure `templateRef`,
`cleanupPolicy: delete` (the only cleanup policy currently supported for ACP
RuntimeSessions), a digest-pinned ACP runtime image, and either the local fixture
or provider-proxy model access, the same Task becomes a live success smoke waiting for `Succeeded`, backed by a dedicated
`acp-ws-*` RuntimePool whose Actor hosts the supervisor. Status must never
expose actor IDs, route hosts, snapshot URIs, worker pod IPs, daemon URLs, or
tokens.

## CI parity

`scripts/agent-substrate-e2e.sh` (the `Agent Substrate E2E` workflow) runs the
direct Substrate and MCP paths end-to-end without externally supplied model or
Git credentials, then runs the fixture-backed workspace ACP Task by default. It
still generates local bootstrap and harness-auth tokens and stores them in
cluster Secrets. Run it directly when you want a clean, self-contained
validation with its own cluster lifecycle:

```bash
PATH="$(go env GOPATH)/bin:$PATH" bash scripts/agent-substrate-e2e.sh
```

Set `KEEP_CLUSTER=1` to inspect the cluster after a failure.
