# Agent Sandbox Workspaces

Agent sandbox workspace support is **experimental scaffolding**. In the current release, Orka can accept and validate durable workspace requests on `Task.spec.execution.workspace`, but the controller does **not** yet route task execution through upstream `agent-sandbox` workspaces. Worker Jobs still use Orka's existing Kubernetes Job execution path.

Use this feature to prepare Task manifests and controller configuration for future durable, warm, reusable agent workspaces. For current one-shot task isolation, continue to use Kubernetes `RuntimeClass` through `spec.execution.runtimeClassName`.

See the [agent sandbox integration plan](../agent-sandbox-integration.md) for the broader design, rollout phases, and upstream references.

## When to Use RuntimeClass vs Agent Sandbox

| Need | Use |
|------|-----|
| One task runs once, produces a result, and cleans up | `spec.execution.runtimeClassName` on normal Orka worker Jobs |
| Stronger pod isolation with gVisor, Kata, or another runtime handler | `spec.execution.runtimeClassName` |
| A checked-out repository, dependency cache, dev server, browser session, or notebook-like state should survive across turns | `Task.spec.execution.workspace` agent sandbox request |
| Session-scoped warm or reusable coding environment | `Task.spec.execution.workspace` with `reusePolicy: session` |

RuntimeClass and agent sandbox are complementary. RuntimeClass routes worker pods through an isolation runtime. Agent sandbox is intended to provide durable, claimable, policy-managed execution environments whose lifecycle can outlive a single Task.

## Current Status and Limitations

- The feature is disabled by default.
- When enabled, the Task controller validates `spec.execution.workspace` requests.
- The controller resolves defaults into an internal workspace request object for future lifecycle integration.
- Worker Job creation is unchanged; sandbox workspaces are not claimed, attached, reused, or cleaned up yet.
- Upstream `agent-sandbox` CRDs, routers, templates, warm pools, and command execution paths are not installed or managed by Orka yet.
- Task status does not yet report sandbox claim, reuse, command, or cleanup state.
- `Agent.spec.execution.workspace` is not an effective defaulting mechanism today; use `Task.spec.execution.workspace` for the experimental request.

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

- `runtimeClassName` continues to apply to the current worker Job path.
- `workspace.enabled: true` requests future durable workspace behavior.
- `reusePolicy: session` requires `spec.sessionRef.name` because the session name is the reuse key.
- `cleanupPolicy: retain` requests retaining the workspace after the Task for future use; current scaffolding only validates and resolves this value.

## `spec.execution.workspace` Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | boolean | `false` | Enables a durable workspace request for this Task. When omitted or false, the workspace request is ignored. |
| `templateRef.name` | string | Controller default template, if configured | Workspace template name to instantiate or reuse. Required when `enabled: true` unless the controller has `--agent-sandbox-default-template`. |
| `templateRef.namespace` | string | Task namespace | Namespace containing the workspace template. |
| `reusePolicy` | string | `none` | Workspace reuse behavior. Supported values: `none`, `session`. |
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
| `--agent-sandbox-enabled` | `ORKA_AGENT_SANDBOX_ENABLED` | `controller.agentSandbox.enabled` | `false` | Enable experimental validation of Task `execution.workspace` requests. |
| `--agent-sandbox-router-url` | `ORKA_AGENT_SANDBOX_ROUTER_URL` | `controller.agentSandbox.routerUrl` | empty | Optional router base URL for future workspace lifecycle integration. |
| `--agent-sandbox-default-template` | `ORKA_AGENT_SANDBOX_DEFAULT_TEMPLATE` | `controller.agentSandbox.defaultTemplate` | empty | Default workspace template name used when a Task omits `templateRef.name`. |
| `--agent-sandbox-warm-pool-policy` | `ORKA_AGENT_SANDBOX_WARM_POOL_POLICY` | `controller.agentSandbox.warmPoolPolicy` | `disabled` | Warm pool policy for future workspace claims. Supported values: `disabled`, `template`. |
| `--agent-sandbox-namespace-strategy` | `ORKA_AGENT_SANDBOX_NAMESPACE_STRATEGY` | `controller.agentSandbox.namespaceStrategy` | `task` | Namespace strategy for future sandbox resources. Supported values: `task`, `controller`. |
| `--agent-sandbox-claim-timeout` | `ORKA_AGENT_SANDBOX_CLAIM_TIMEOUT` | `controller.agentSandbox.claimTimeout` | `2m` | Timeout for future workspace claim operations. |
| `--agent-sandbox-command-timeout` | `ORKA_AGENT_SANDBOX_COMMAND_TIMEOUT` | `controller.agentSandbox.commandTimeout` | `30m` | Timeout for future sandbox command execution. |
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

Until lifecycle integration is implemented:

1. Keep existing RuntimeClass configuration for required isolation guarantees.
2. Treat workspace-enabled Tasks as a validation-only preview.
3. Do not depend on files, caches, processes, or environment state being retained across Tasks.
4. Track the integration plan before installing or exposing upstream `agent-sandbox` components in production.

Future integration work is expected to add workspace claims, attach/command execution, reuse-key derivation, cleanup/reconciliation, status conditions, metrics, and operator installation guidance.
