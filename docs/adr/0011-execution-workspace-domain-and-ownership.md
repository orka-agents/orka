# ADR 0011: Model execution workspaces as provider-neutral owned resources

Date: 2026-07-16

## Status

Accepted.

## Context

Orka's original Execution Workspace path embeds Agent Sandbox and Substrate choices in `Task`, worker environment, and provider-specific controllers. That shape cannot safely support independent adapters, shared warm capacity, durable service workspaces, or cleanup that outlives Task completion. Runtime conversation continuity also needs terminology that distinguishes model/runtime state from filesystem and process state.

## Decision

Orka introduces `workspace.orka.ai/v1alpha1` with four resources:

- `ExecutionWorkspaceProvider` is cluster-scoped and represents one adapter installation. The adapter owns heartbeat, backend identity, supported features, and provider readiness status. Orka core owns policy evaluation and never writes provider-native status.
- `ExecutionWorkspaceClass` is namespaced, user-selectable, and functionally immutable. It selects either a provider plus provider-owned profile parameters or a namespaced pool. Orka core resolves and authorizes it.
- `ExecutionWorkspacePool` is namespaced and represents provider-managed capacity. The matching adapter owns capacity reconciliation and pool status; core only validates references and projects admission state.
- `ExecutionWorkspace` is namespaced and controller-created. Orka core owns identity, immutable bindings, desired state, attachment intent, owner references, and credential Secrets. Exactly one adapter selected by the immutable provider binding owns provider-observed status.

The persistence terms are distinct:

- **Conversation Session** is the user-visible continuity boundary referenced by Tasks.
- **RuntimeSession** is the harness/runtime continuation record for model or agent runtime state.
- **ExecutionWorkspace** is filesystem, process, service, and provider lifecycle state. It may be reused by a Conversation Session, but it is not a RuntimeSession.

Workspace identity is deterministic:

- Session reuse: namespace + Conversation Session UID + workspace slot + class UID.
- No session reuse: Task UID + class UID.
- Service mode: owning Tool UID + class UID.

Classes and workspaces are namespaced. Providers are cluster-scoped. Provider-specific parameter references never carry a namespace: namespaced owners resolve them in their own namespace; cluster-scoped providers may reference only cluster-scoped parameter kinds. Cross-namespace workspace references are not supported in v1.

A Task or Tool may select only a class. It never selects a provider name, provider version, pool implementation, endpoint, or provider-native template. Use of a class requires an explicit `SubjectAccessReview` for verb `use` on `executionworkspaceclasses.workspace.orka.ai` in the object's namespace.

Provider adapters never write `Task` or `Tool` status. Orka core projects generic workspace state and conditions into those resources. Provider-native credentials, endpoints containing credentials, attachment tokens, and raw external objects are forbidden from generic specs, statuses, events, logs, and metric labels.

## Mutability and field ownership

| Resource/field | Writer | Mutability |
| --- | --- | --- |
| Provider `controllerName`, `parametersRef`, `requiredContracts` | operator | immutable |
| Provider `lifecycleState`, `usagePolicy` | operator | mutable |
| Provider adapter/backend/features/heartbeat status | matching adapter | mutable observation |
| Provider generic usability conditions | Orka core | mutable observation |
| Pool provider and parameters references | operator | immutable |
| Pool capacity | operator | mutable |
| Pool counts and conditions | matching adapter | mutable observation |
| Class functional spec | operator | immutable; changes require a new class |
| Class resolved-provider/readiness status | Orka core | mutable observation |
| Workspace mode, class/provider binding, session, slot, lifecycle | Orka core | immutable after creation |
| Workspace desired state, attachment, service endpoints requested | Orka core | mutable intent |
| Workspace external ID, state, enforced epoch, connection Secret reference, endpoints, disposition | matching adapter | mutable observation |
| Task/Tool generic workspace projection | Orka core | mutable observation |

## Modes and pools

`Interactive` workspaces permit one exclusive Task attachment and do not publish Tool service ports. `Service` workspaces are owned by a Tool, publish declared ports, and never use Task attachment credentials. A class either provisions directly or through one pool. Pools are capacity abstractions, not user-visible provider names; Tasks and Tools cannot select provider-native warm-pool or worker identities.

## Consequences

- Provider adapters can be installed, upgraded, drained, or removed independently.
- One status writer exists for each field, avoiding cross-controller conflicts.
- RuntimeSession and ExecutionWorkspace lifecycles can evolve independently.
- OpenSandbox can implement the same contract later without adding fields to Task or Tool.
- Remote `AgentRuntime` direct workspace access remains unsupported; future remote access must use brokered workspace operations.
