# Agent Substrate Deploy — Validation

Validation steps for `$agent-substrate-deploy`. Read after the standard workflow completes.

> **Known gate:** Substrate-backed ACP RuntimeSession dispatch requires `--substrate-enabled` **and** `--acp-workspace-dispatch-enabled` plus an infrastructure `templateRef`; this skill's deployments leave the dispatch flag off, so a provider-backed agent Task remains expected-failure evidence here. The bundled E2E validates direct Actor create/resume/exec/suspend/delete plus Substrate-backed MCP lifecycle. Validate plain Codex/Claude ACP Tasks separately with `scripts/live-acp-runtime-e2e.sh`.

The installer leaves a fully wired cluster. During standup it smoke-tests direct
actor create/resume/exec/suspend/delete and Substrate-backed MCP tool lifecycle.
It does **not** currently smoke-test retained workspace reuse for Orka agent
Tasks because those execution-workspace checks are skipped by the ACP workspace-dispatch gate.

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

With the dispatch flag off, check `status.executionWorkspace.phase=Failed` and
`reason=WorkspaceValidationFailed`; provider/template metadata remains sanitized.
With `--acp-workspace-dispatch-enabled`, an infrastructure `templateRef`, real
digest-pinned ACP runtime images, and provider-proxy model access, the same Task
becomes a live success smoke waiting for `Succeeded`, backed by a dedicated
`acp-ws-*` RuntimePool whose Actor hosts the supervisor. Status must never
expose actor IDs, route hosts, snapshot URIs, worker pod IPs, daemon URLs, or
tokens.

## CI parity

`scripts/agent-substrate-e2e.sh` (the `Agent Substrate E2E` workflow) runs the
direct Substrate and MCP paths end-to-end without externally supplied model or
Git credentials. It still generates local bootstrap and harness-auth tokens and
stores them in cluster Secrets. Run it directly when you want a clean,
self-contained validation with its own cluster lifecycle:

```bash
PATH="$(go env GOPATH)/bin:$PATH" bash scripts/agent-substrate-e2e.sh
```

Set `KEEP_CLUSTER=1` to inspect the cluster after a failure.
