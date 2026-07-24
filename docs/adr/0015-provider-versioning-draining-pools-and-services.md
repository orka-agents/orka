# ADR 0015: Pin provider/class revisions and model draining, pools, and services generically

Date: 2026-07-16

## Status

Accepted.

## Context

Provider upgrades, warm-capacity pools, and MCP actor hosting currently expose provider-native names and version behavior. Active workspaces must remain bound to the adapter/backend contract that created them while new allocations can move to a newer installation.

## Decision

Provider identity is immutable. `ExecutionWorkspace.spec.providerBinding` pins provider name, UID, generation, selected generic contract, and the observed adapter/backend version in status. `classBinding` pins class name, UID, generation, and functional profile hash. Active workspace bindings are never migrated in place; a different provider or functional class requires a new workspace.

Provider lifecycle states have exact allocation semantics:

- **Active:** accepts new workspaces and reconciles existing lifecycle operations.
- **Draining:** rejects new workspace identities, but permits existing reattachment, detach, suspension, disposition, and deletion.
- **Disabled:** permits observation, credential revocation, quarantine, and cleanup/delete only. It does not attach or resume user work.

Deleting a provider with referenced workspaces or pools is blocked by a finalizer. An administrator may use an explicit force-orphan operation that records an audit condition and leaves affected workspaces quarantined; silent orphaning is not allowed.

A class is functionally immutable. Operators publish a new class name for profile or policy changes. A class has exactly one provisioning source:

- direct provider plus adapter-owned profile parameters, or
- a namespaced `ExecutionWorkspacePool`.

Pools expose only `minReady`, `maxSize`, and generic available/allocated/suspended/total status. The adapter owns prewarming, queues, provider-native pool objects, worker density, and resize behavior. Capacity exhaustion reports `Admitted=False` with reason `CapacityUnavailable`; it is not provider failure. Namespace-wide initial capacity is bounded with object-count `ResourceQuota` for `executionworkspaces.workspace.orka.ai`. Kueue integration is deferred.

Interactive and Service modes share provider/class/pool versioning but differ in access:

- Interactive workspaces are exclusively Task-attached and may use Session reuse.
- Service workspaces are Tool-owned, do not use Task attachment credentials, publish declared ports, and are health-checked through Orka's existing SSRF-safe Tool path.

Tool status contains class/workspace reference, generic state, and a sanitized endpoint. It contains no actor ID, provider credential, or provider-native pool/template name on the new path.

Side-by-side upgrades use distinct provider resources. Operators create the new provider and classes, direct new allocations to them, set the old provider to Draining, and delete it only after references are gone. Backend incompatibility marks a provider unusable before new allocation without invalidating already-pinned status history.

## Consequences

- Upgrades do not mutate active workspace assumptions.
- Capacity and service hosting work uniformly across Substrate, Agent Sandbox, the fake provider, and future adapters.
- Provider-specific scheduling remains adapter-owned.
- Cross-provider checkpoint restore and live workspace migration are not supported.
