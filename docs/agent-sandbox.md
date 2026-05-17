# Agent Sandbox Workspaces

Agent sandbox workspace support is **experimental** and disabled by default. When enabled, a `type: agent` Task can request an upstream `agent-sandbox` workspace with `Task.spec.execution.workspace`. Orka still creates the normal Kubernetes worker Job, but the agent worker wrapper uses the resolved workspace settings to claim and wait for an `agent-sandbox` workspace, then runs the configured agent runtime inside that sandbox.

Use this feature for durable, warm, or reusable coding environments backed by an existing upstream `agent-sandbox` installation. For one-shot pod isolation, continue to use Kubernetes `RuntimeClass` through `spec.execution.runtimeClassName`.

See the [agent sandbox integration plan](../agent-sandbox-integration.md) for the broader design, rollout phases, and upstream references.

## When to Use RuntimeClass vs Agent Sandbox

| Need | Use |
|------|-----|
| One task runs once, produces a result, and cleans up | `spec.execution.runtimeClassName` on the Orka worker Job, if extra pod isolation is needed |
| Stronger pod isolation with gVisor, Kata, or another runtime handler for the worker wrapper | `spec.execution.runtimeClassName` |
| A checked-out repository, dependency cache, dev server, browser session, or notebook-like state should survive across turns | `Task.spec.execution.workspace` agent sandbox request |
| Session-scoped warm or reusable coding environment | `Task.spec.execution.workspace` with `reusePolicy: session` |

RuntimeClass and agent sandbox are complementary. `runtimeClassName` applies to the outer Kubernetes worker Job that starts the Orka worker wrapper. The upstream sandbox template and runtime determine the inner execution environment where the agent CLI runs after the wrapper claims a sandbox workspace.

## Current Status and Limitations

- The feature is disabled by default.
- When enabled, the Task controller validates `Task.spec.execution.workspace` requests for `type: agent` Tasks, resolves/defaults the effective `SandboxTemplate` and workspace settings, and passes those settings to the agent worker Job.
- The agent worker wrapper claims an upstream `agent-sandbox` workspace, waits for it to become ready, and re-executes the same worker binary inside the sandbox with recursive sandboxing disabled.
- Orka uses the upstream `sigs.k8s.io/agent-sandbox` Go SDK. With the currently pinned SDK, the worker creates claims with `CreateSandbox(templateName, taskNamespace)`. The SDK does not expose a separate template namespace argument, so `templateRef.namespace` is propagated in Orka metadata/env but the template must be usable from the claim namespace in the upstream installation.
- The current worker-as-client path always creates or reattaches claims in the Task namespace. `namespaceStrategy` is validated and propagated for compatibility with future policies, but the alpha worker path does not yet create claims in the controller namespace.
- `cleanupPolicy: delete` deletes the workspace after execution. `cleanupPolicy: retain` disconnects from the sandbox and leaves the upstream claim/resource for operator inspection or external reattach.
- `reusePolicy: session` derives and passes a session reuse key, and the adapter supports local in-process reuse plus explicit reattach by claim name. Orka does not yet persist sandbox claim identity on Task or Session status, so automatic reuse across separate worker Jobs is not first-class in this alpha.
- The SDK command API accepts one shell command string. Orka safely renders argv/env/workdir into that string; stdin is not supported. Upstream command responses are capped by the SDK, and Orka applies its own configured output truncation where requested.
- The SDK file write API accepts plain filenames only. Orka's adapter rejects nested upload paths instead of flattening them; recursive download/list is supported for files visible through the SDK.
- Orka does not install or manage upstream `agent-sandbox` CRDs, router services, templates, or warm pools. Install and operate those components separately before enabling workspace-backed Tasks.
- Task status does not report sandbox claim, reuse, command, or cleanup state. Inspect worker logs and upstream `agent-sandbox` resources for sandbox lifecycle details.
- `Agent.spec.execution.workspace` is not an effective defaulting mechanism today; use `Task.spec.execution.workspace` for workspace-backed execution.

## Execution Flow

1. The Task controller validates the workspace request and resolves/defaults the effective `SandboxTemplate` and workspace settings, including template name/namespace, reuse policy, cleanup policy, claim timeout, and command timeout.
2. The controller still creates an ordinary Orka worker Job. Any `runtimeClassName`, node selector, tolerations, and affinity settings apply to this outer worker pod.
3. For `type: agent` Tasks, the Job includes environment variables that describe the resolved sandbox workspace request.
4. The worker wrapper sees those variables, claims an upstream `agent-sandbox` workspace, waits for it to become ready, and executes the same worker command inside that workspace.
5. The inner worker run disables sandbox recursion and performs the normal agent lifecycle: load config, clone or prepare the repository workspace, run the configured agent runtime, submit results, and upload artifacts.
6. After the inner command exits, the wrapper applies `cleanupPolicy`: delete the workspace, or release/retain it.

## Task Example

Enable a durable workspace request on an agent Task:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: coding-agent-task
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Continue implementing the feature in this session."
  sessionRef:
    name: feature-123
    create: true
  execution:
    runtimeClassName: gvisor
    workspace:
      enabled: true
      templateRef:
        name: coding-agent
      reusePolicy: session
      cleanupPolicy: retain
```

Notes:

- `runtimeClassName` applies to the outer Orka worker Job; the sandbox template/runtime controls the inner sandbox environment.
- `workspace.enabled: true` requests workspace-backed execution for this agent Task.
- `reusePolicy: session` requires `spec.sessionRef.name` because the session name is the reuse key.
- `cleanupPolicy: retain` asks the worker wrapper to retain/release the claimed workspace after the Task instead of deleting it.

## `spec.execution.workspace` Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | boolean | `false` | Enables workspace-backed execution for this agent Task. When omitted or false, the normal worker path is used without sandbox env propagation. |
| `templateRef.name` | string | Controller default template, if configured | Workspace template name to instantiate or reuse. Required when `enabled: true` unless the controller has `--agent-sandbox-default-template`. |
| `templateRef.namespace` | string | Task namespace | Namespace containing the workspace template in Orka metadata. Current SDK calls create claims with the Task namespace and template name, so the upstream template must be usable from that claim namespace. |
| `reusePolicy` | string | `none` | Workspace reuse intent. Supported values: `none`, `session`; current automatic reuse is limited because claim identity is not persisted across worker Jobs. |
| `cleanupPolicy` | string | Controller default cleanup policy, defaulting to `delete` | Cleanup behavior after use. Supported values: `delete`, `retain`. |

## Validation Rules

The Task controller validates workspace requests before creating worker Jobs:

- `--agent-sandbox-enabled=true` or `ORKA_AGENT_SANDBOX_ENABLED=true` is required.
- Workspace requests are only supported for `type: agent` Tasks.
- `templateRef.name` is required unless `--agent-sandbox-default-template` or `ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE` is configured.
- `reusePolicy` must be one of `none` or `session` when set.
- `cleanupPolicy` must be one of `delete` or `retain` when set.
- `reusePolicy: session` requires `spec.sessionRef.name`.
- Controller sandbox config must be valid: supported warm pool policy, supported namespace strategy, positive timeouts, and supported cleanup policy.

## Controller Configuration

Controller flags can be set directly, through environment variables, or through Helm values under `controller.agentSandbox`. The Helm chart emits the sandbox flags only when `controller.agentSandbox.enabled` is true.

| Flag | Environment variable | Helm value | Default | Description |
|------|----------------------|------------|---------|-------------|
| `--agent-sandbox-enabled` | `ORKA_AGENT_SANDBOX_ENABLED` | `controller.agentSandbox.enabled` | `false` | Enable experimental agent sandbox workspace support for agent Tasks. |
| `--agent-sandbox-router-url` | `ORKA_AGENT_SANDBOX_ROUTER_URL` | `controller.agentSandbox.routerUrl` | empty | Optional upstream agent-sandbox router base URL. |
| `--agent-sandbox-default-template` | `ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE` | `controller.agentSandbox.defaultTemplate` | empty | Default workspace template name used when a Task omits `templateRef.name`. |
| `--agent-sandbox-warm-pool-policy` | `ORKA_AGENT_SANDBOX_WARM_POOL_POLICY` | `controller.agentSandbox.warmPoolPolicy` | `disabled` | Warm pool policy for workspace claims. Supported values: `disabled`, `template`. |
| `--agent-sandbox-namespace-strategy` | `ORKA_AGENT_SANDBOX_NAMESPACE_STRATEGY` | `controller.agentSandbox.namespaceStrategy` | `task` | Namespace strategy for sandbox resources. Supported values: `task`, `controller`. |
| `--agent-sandbox-claim-timeout` | `ORKA_AGENT_SANDBOX_CLAIM_TIMEOUT` | `controller.agentSandbox.claimTimeout` | `2m` | Timeout for workspace claim and readiness operations. |
| `--agent-sandbox-command-timeout` | `ORKA_AGENT_SANDBOX_COMMAND_TIMEOUT` | `controller.agentSandbox.commandTimeout` | `30m` | Timeout for agent runtime execution inside the sandbox. |
| `--agent-sandbox-cleanup-policy` | `ORKA_AGENT_SANDBOX_CLEANUP_POLICY` | `controller.agentSandbox.cleanupPolicy` | `delete` | Default cleanup policy when a Task omits `workspace.cleanupPolicy`. Supported values: `delete`, `retain`. |

Example Helm configuration:

```yaml
controller:
  agentSandbox:
    enabled: true
    routerUrl: "http://agent-sandbox-router.agent-sandbox-system.svc:8080"
    defaultTemplate: coding-agent
    warmPoolPolicy: template
    namespaceStrategy: task
    claimTimeout: 2m
    commandTimeout: 30m
    cleanupPolicy: retain
```

## Operational Guidance

1. Install and operate the upstream `agent-sandbox` CRDs, router, templates, and any warm pools outside of Orka before enabling workspace-backed Tasks.
2. Keep existing RuntimeClass configuration for the outer Orka worker Job when you need worker pod isolation guarantees.
3. Use `Task.spec.execution.workspace` only on `type: agent` Tasks; non-agent Tasks are rejected.
4. Use worker logs and upstream `agent-sandbox` resources to debug claim, readiness, execution, and cleanup because Orka Task status does not expose sandbox lifecycle state.
5. Choose `cleanupPolicy: delete` for single-use workspaces and `cleanupPolicy: retain` when the upstream sandbox installation should keep/release the workspace for reuse.
6. For custom manifests, make sure the worker ServiceAccount can create/delete/patch `sandboxclaims`, read `sandboxtemplates`, `sandboxwarmpools`, and `sandboxes`, create `pods/portforward`, and read `endpointslices`. The Helm chart and generated worker RBAC include these rules.
7. The sandbox template image must contain the agent CLI runtime used by the worker being re-executed, plus a shell, writable workspace/home directories, and network access to Orka API and the configured model/provider endpoint. Provider credentials are forwarded as command environment variables; do not log them.
8. The outer worker stages its own binary and a ServiceAccount token file into the sandbox before execution. The inner worker runs with recursive sandboxing disabled and uses `ORKA_SA_TOKEN_PATH` to submit results and artifacts back to Orka.

## Live Smoke Test

After enabling the controller feature and installing a usable sandbox template, validate the path with an actual agent Task rather than only checking manifests. This example assumes a namespace `orka-system`, an agent named `claude-agent`, and a template named `orka-live-template`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: orka-live-sandbox-smoke
  namespace: orka-system
spec:
  type: agent
  agentRef:
    name: claude-agent
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
```

Run and verify:

```bash
kubectl apply -f smoke-task.yaml
kubectl -n orka-system wait --for=jsonpath='{.status.phase}'=Succeeded task/orka-live-sandbox-smoke --timeout=10m

JOB=$(kubectl -n orka-system get task orka-live-sandbox-smoke -o jsonpath='{.status.jobName}')
POD=$(kubectl -n orka-system get pods -l job-name="$JOB" -o jsonpath='{.items[0].metadata.name}')
kubectl -n orka-system logs "$POD"
```

A successful sandbox wrapper log includes the claimed workspace name, for example:

```text
Task orka-system/orka-live-sandbox-smoke completed in sandbox workspace sandbox-claim-abc12
```

Fetch the result through the API using an authenticated ServiceAccount token; do not print the token:

```bash
kubectl -n orka-system port-forward svc/orka-api 18080:8080 >/tmp/orka-api-pf.log 2>&1 &
PF_PID=$!
TOKEN=$(kubectl -n orka-system create token orka-worker --duration=10m)
curl -fsS -H "Authorization: Bearer ${TOKEN}" \
  'http://127.0.0.1:18080/api/v1/tasks/orka-live-sandbox-smoke/result?namespace=orka-system'
kill "$PF_PID"
```

Expected response:

```json
{"result":"ORKA_LIVE_SANDBOX_OK\n"}
```

For `cleanupPolicy: delete`, the claimed `SandboxClaim` and `Sandbox` should be gone after completion. Completed Orka worker Job/Pod resources may remain according to normal Job history limits.

## Troubleshooting

- **Task rejected before Job creation**: check `--agent-sandbox-enabled=true`, `templateRef.name` or controller default template, `reusePolicy`, `cleanupPolicy`, and `sessionRef.name` for `reusePolicy: session`.
- **Worker cannot claim or execute**: check the worker ServiceAccount RBAC for upstream sandbox resources and `pods/portforward`.
- **Inner agent CLI reports connection refused**: exec into a retained sandbox or run a retained smoke test and verify DNS/TCP reachability to the configured provider base URL or proxy from inside the sandbox pod.
- **Result submission fails from the inner worker**: verify that the outer worker can read its ServiceAccount token and that the staged `ORKA_SA_TOKEN_PATH` file is available inside the sandbox.
- **Controller rollout fails after manual image patching**: preserve the controller container's `/data` and `/tmp` volume mounts, probes, resources, and security context. With the default SQLite store path, losing the `/data` mount prevents the controller from opening `/data/orka.db`.
