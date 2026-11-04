---
description: "Run a coding agent inside a sandbox provider that can suspend, resume, and checkpoint its files."
---

# Execution workspaces

By default, a coding-agent Task runs as a private session inside a shared, controller-owned
RuntimePool Pod. An **execution workspace** runs it somewhere stronger instead: a sandbox from
an external provider, dedicated to one Session. That buys two things.

- **Isolation.** One workspace serves one Session, inside the provider's sandbox.
- **Continuity.** The workspace can be suspended when the agent goes idle and resumed for the
  next message with its files intact. With Agent Substrate, its files can also be
  checkpointed, restored into a new workspace, or used as the starting point of a fork.

Execution workspaces are off by default and flag-gated. Each provider is a separate
Deployment from the [orka-workspace](https://github.com/orka-agents/orka-workspace)
repository. Orka does not install or upgrade the providers or their backends; you run them.

:::note[Two different "workspaces"]
`Task.spec.workspace` is the **repository** an agent works on: what to clone and where to
publish. Coding agents need it to clone or publish; without it they get an empty workspace.
`Task.spec.execution.workspace` is **where the agent runs**. This page is about the second.
:::

## Choose a provider

| | [Agent Sandbox](agent-sandbox.md) | [Agent Substrate](substrate.md) |
| --- | --- | --- |
| Project | [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) | [Agent Substrate](https://github.com/agent-substrate/substrate) |
| Where the agent runs | A sandbox Pod the provider creates from Orka's frozen runtime request | A gVisor-isolated actor on a dedicated worker pool |
| Suspend and cold resume | Yes, keeping a durable volume | Yes, keeping a data-only snapshot |
| Checkpoint, restore, and fork from saved files | No | Yes |
| Also hosts | — | MCP servers for [Tools](../reference/resources.md#tool) |

Suspension keeps files, never process memory. A resumed workspace cold-boots with fresh
credentials, so nothing that should have expired comes back.

:::tip[Video demos]
Watch [Suspend an agent workspace and keep its files](https://www.youtube.com/watch?v=DyS9JioSRa0)
(Agent Sandbox) and [Checkpoint and restore an agent workspace](https://www.youtube.com/watch?v=jsdRB-0LLAc)
(Agent Substrate).
:::

## Request a workspace

An operator publishes `ExecutionWorkspaceClass` objects that bind a registered provider, its
profile, and lifecycle rules. Users pick one by name:

```yaml
spec:
  type: agent
  sessionRef: {name: work-session, create: true, append: true}
  execution:
    workspace:
      classRef: {name: sandbox-session}
      reusePolicy: session
```

Repository access still comes from top-level `spec.workspace`, and each workspace gets its
own single-session RuntimePool named `acp-ws-*`. Task status reports a provider-neutral phase
and reason; provider-native names never appear in it. Provider, template, and pool selectors
do not belong in a Task. Options the class does not permit are rejected before anything is
created.

## Turn it on

Core needs `--enable-workspace-provider-api`, `--acp-workspace-dispatch-enabled`, and the Task
provenance and workspace-class-use admission webhooks. Core has no Sandbox or Substrate
allocation flag: install the provider and admit its registration and classes as described in
[Agent Sandbox](agent-sandbox.md#install-and-select-a-class) and
[Agent Substrate](substrate.md#install-the-external-acp-provider).

Choosing a class is authorized: the caller needs Kubernetes `use` on that
`ExecutionWorkspaceClass`, and restoring needs `use` on the checkpoint.
[Workspace providers](../reference/configuration.md#workspace-providers) covers the flags,
admission webhooks, the class resources, detach behavior, and expiry.

## Read next

- [Agent Sandbox workspaces](agent-sandbox.md) — installation, startup ownership, suspension, and deletion.
- [Agent Substrate workspaces](substrate.md) — provider setup, cold suspension, checkpoints, forks, and recovery.
- [Architecture decision records](https://github.com/orka-agents/orka/tree/main/docs/adr) 0024–0031 —
  the full provider-neutral contract.
