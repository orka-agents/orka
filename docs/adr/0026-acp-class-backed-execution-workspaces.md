# 26. Class-backed ACP execution workspaces

Date: 2026-08-22

## Status

Accepted. Implements the admission, attachment, detach, and finalization
binding of issue #421 on the workspace-provider-backed RuntimePool seam from
ADRs 0024 and 0025 and the generic workspace domain from ADR 0021. Data-only
suspension and cold resume remain follow-up work (issues #409 and #422) behind
the fail-closed gates this ADR defines.

## Context

`Task.spec.execution.workspace` previously exposed only the legacy
provider-shaped request: an author-selected `provider`, a Substrate
`templateRef`, and a `cleanupPolicy`. The controller-first
`workspace.orka.ai/v1alpha1` API already models everything issue #420 needs —
operator-owned classes with `use` authorization, immutable
`ExecutionWorkspace` bindings, detach policy, lifetime policy, and per-category
deletion dispositions — but nothing connected it to ACP RuntimeSessions:
`classRef` was rejected at admission, `WorkspaceAttachmentManager` had no
production caller, and no adapter served the generic contract.

## Decision

ACP RuntimeSessions bind to the controller-first workspace lifecycle through an
in-tree adapter, without introducing a second persistence API.

### Reserved adapter identity and parameter kinds

The Orka controller is the workspace adapter for ACP RuntimePool execution.
`ExecutionWorkspaceProvider` objects that back ACP classes must carry the
reserved `controllerName: acp.workspace.orka.ai/runtime-pool`. The adapter owns
two parameter kinds in the new `acp.workspace.orka.ai/v1alpha1` group:

- `RuntimeProviderConfig` (cluster-scoped, referenced by the provider's
  `parametersRef`) selects the backend: `agent-sandbox` or `substrate`.
- `RuntimeWorkspaceProfile` (namespaced, referenced by the class's
  `parametersRef`) carries operator-owned backend inputs. A substrate profile
  must name the operator-owned infrastructure ActorTemplate; an agent-sandbox
  profile must stay empty because agent-sandbox RuntimeSessions run only
  controller-rendered sandbox templates (ADR 0024).

Provider advertisement is fail-closed: the heartbeat publishes contracts and
features only while `--acp-workspace-dispatch-enabled` and the matching backend
flag are on and the referenced config exists. The advertised `exec`, `files`,
and `reset` features name the ACP runtime data plane (prompt execution and the
brokered repository workspace), not the generic workspace-agent protocol. The
`suspend` feature is deliberately absent, so a class whose `allowedOnDetach`
includes `Suspend` never becomes Ready against this adapter until data-only
cold resume ships.

### Admission and freezing

A class-shaped request (`classRef` plus the still-valid `reusePolicy`,
`workspaceSlot`, and `onDetach` fields) is resolved at Task admission:

- The `use` SubjectAccessReview stays at the admission webhook and policy
  layer; the controller re-verifies object identity and policy, not caller
  authority.
- The class must be Interactive, directly provisioned, Ready at its current
  generation, and carry a pinned profile hash. The controller recomputes the
  profile hash from the live provider and profile objects and rejects drift
  behind an unchanged generation.
- The effective detach action is the Task's `onDetach` when the class allows
  it, otherwise `defaultOnDetach`. Only `Delete` is executable today; an
  effective `Suspend` fails closed.
- Deletion policies that retain provider resources, volumes, or checkpoints
  are rejected until bounded retention exists (issue #424).
- Class name, UID, generation, profile hash, provider identity, the complete
  lifecycle, and the effective detach action are frozen into the execution
  snapshot's workspace binding and folded into the binding digest. Legacy
  binding digests stay byte-identical. Snapshot verification recomputes and
  re-validates the frozen class binding offline.

### Materialization, attachment, and the RuntimePool link

Before any RuntimePool demand, the controller materializes one
`ExecutionWorkspace` per Task (reuse scope `None`) or per immutable Session UID
and slot (reuse scope `Session`), using the deterministic identity from ADR
0021 with the `acp-ws` prefix. Adoption is fail-closed: an existing workspace
must match the frozen class, provider, session, and slot bindings exactly.
Per-Task workspaces carry a non-controller Task owner reference — Task
workspace status stays owned by the ACP execution projection.

Prompt admission then waits for core admission of the workspace and takes the
exclusive attachment through `WorkspaceAttachmentManager`, giving
workspace-backed RuntimeSessions the generic epoch-fenced one-writer rule. The
attachment Secret is not the RuntimePool data-plane credential; ACP keeps its
own bootstrap and fence machinery (ADR 0024).

The RuntimePool and workspace cross-link by Orka-owned names only: the pool
carries the `acp.workspace.orka.ai/execution-workspace` label and the workspace
records the pool name in an annotation. Provider-native identifiers never
appear on either object or in Task status.

### Detach and finalization

When the Task settles terminally (or is deleted), the controller revokes the
attachment, waits for the adapter to release the enforced epoch, and applies
the effective detach action by deleting the `ExecutionWorkspace`. The in-tree
workspace adapter executes deletion by tearing down the linked RuntimePool
through its existing authenticated drain and finalizer, refuses to delete a
same-name pool that is not linked to the workspace, and reports the terminal
per-category disposition that core finalization validates against the frozen
class deletion policy. The idle-pool reaper retires class-backed pools through
their workspace so the disposition record is never skipped.

## Consequences

- Operators can now govern ACP execution workspaces with the same class
  vocabulary as every other workspace consumer: `use` authorization, reuse
  scopes, detach policy, lifetime bounds, and deletion dispositions.
- Session continuity across detach requires `Suspend`, which stays fail-closed
  until #409 (Substrate) and #422 (Agent Sandbox) implement data-only cold
  resume; with `Delete`, a continuation Task gets a fresh physical workspace
  while logical continuation still comes from Orka's canonical state.
- The legacy provider-shaped request keeps its exact behavior and digests
  during migration; classRef and legacy fields remain mutually exclusive at
  the CRD layer.
- Suspension, cold resume, retention, quotas, and garbage collection extend
  this seam (issues #409, #421 follow-ups, #422, #423, #424) instead of adding
  another lifecycle API.
