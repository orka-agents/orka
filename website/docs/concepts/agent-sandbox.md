---
slug: /agent-sandbox
---

# Agent Sandbox Workspaces

Upstream `agent-sandbox` integration is a deferred execution-workspace provider for the ACP core runtime. It is not part of the current built-in ACP v2 Task path.

A `type: agent` Task that sets `Task.spec.execution.workspace` is currently rejected with a fail-closed validation error. Use top-level `Task.spec.workspace` for the source repository, workspace intent, and clean-room publication policy:

```yaml
spec:
  type: agent
  workspace:
    intent: read
    gitRepo: https://github.com/example/project.git
    readCredentialRef:
      name: project-read
```

`spec.workspace` does not request a durable sandbox. It describes the verified source and publication boundary for the ephemeral RuntimeSession workspace.

## Current ACP v2 execution model

Built-in agent Tasks run in controller-owned RuntimePools:

```text
Task
  -> RuntimePool
  -> ephemeral RuntimeSession inside the exact runtime Pod
  -> workspace validation
  -> optional clean-room Workspace/Publisher transaction
```

The runtime Pod has no Git credential and no direct SCM publication egress. Source materialization and publication are separate Orka operations.

## Deferred provider seam

A future `agent-sandbox` integration must preserve the same `orka.harness.v2` lifecycle and governance boundaries. In particular, it must:

- keep the Task attempt, RuntimeSession, prompt lease, fences, and publication records authoritative in Orka;
- expose one RuntimeSession through the v2 session operations rather than creating an agent Job fallback;
- materialize only verified workspace artifacts;
- keep source-read, target-read, target-write, and forge credentials outside the ACP child process tree;
- produce an Orka-owned workspace delta after prompt settlement;
- block eviction while validation, publication, finalization, or Session lease work is active;
- fail closed on ambiguous runtime loss instead of replaying an accepted prompt;
- preserve exact process cleanup and credential-revocation guarantees across suspension/resume.

Durable workspace resume, provider-session restore, prompt replay, stream reconnect, and transparent migration are not implied by ACP v2 and require separate reviewed designs.

## RuntimeClass

`Task.spec.execution.runtimeClassName`, per-Task placement, and custom Task resource requests are also not supported by the current built-in ACP path. Runtime isolation and resources are selected through reviewed RuntimePool profiles. Container and native `ai` Tasks keep their existing `spec.execution` behavior.

## Local evaluation material

The repository still contains local/kind evaluation scripts for the older execution-workspace prototype. They are not the supported ACP v2 deployment path and should not be used as release evidence. Live ACP validation should instead verify RuntimePool scale-up, exact-instance fencing, Session continuation, cancellation, workspace validation, clean-room publication, controller restart behavior, pool replacement, and cleanup.
