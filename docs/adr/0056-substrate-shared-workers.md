# 56. Let opted-in Substrate profiles share workers, with interim termination evidence

Date: 2026-11-18

## Status

Accepted as an interim decision, approved by the project owner. Only for
`SubstrateWorkspaceProfile`s that set `spec.placement.sharedWorkers`, it supersedes:

- the **Single-Actor workers** bullet of ADR 0049;
- the clause "termination is proven by worker Pod UID absence, Actor assignments, and
  the worker epoch" in ADR 0049's **Least privilege** bullet;
- the matching ADR 0031 sentences "Drain the supervisor and exact single-Actor worker
  before snapshot capture" (only its single-Actor requirement) and "Delete and observe
  the exact worker Pod before deleting the source Actor and reporting suspension".

Every other profile keeps ADRs 0031 and 0049 unchanged. ADR 0030's full-memory gate and
ADR 0031's data-only cold path stay in force. The provider side is `orka-workspace`
ADR 0008.

## Context

ADR 0049 runs each Substrate Actor on a private worker sized for exactly that Actor.
The provider proves an Actor stopped by deleting its worker Pod and observing the exact
Pod UID absent. Every cold start and resume therefore waits for a new worker Pod.
kagent and Google AX resume onto workers that are already running.

Agent Substrate v1.0.0 (`b34fbee742906494b5eb4f1f36987176bf8770a4`) is built for shared
workers, but gives no per-Actor proof of sandbox exit:

- Each Actor has its own `runsc` sandbox, cgroup leaf and network namespace. A `VOLUMES`
  checkpoint runs only `runsc fscheckpoint` over durable directories, with no memory
  image, and deactivates the Actor's tunnel first.
- Teardown after a checkpoint is best effort (`_ = terminate()` in
  `cmd/ateworker-gvisor/ckptcgroup.go`). The control plane still marks the Actor
  `SUSPENDED` and releases its assignment.
- Deleting a `SUSPENDED` Actor tears nothing down. A leaked sandbox keeps its memory,
  without a tunnel, until the worker Pod dies.
- A worker restart is strong evidence: a new process registers a higher epoch and the
  control plane crashes the earlier Actors.
- Co-hosted Actors share one privileged worker process, the node's `/var/lib/ate`, and
  their egress mTLS keys. Upstream's threat model states that workers are not a
  boundary between Actors.

## Decision

### Opt-in and scope

- **A frozen profile field.** Sharing is opt-in per profile through
  `spec.placement.sharedWorkers: {maxActorsPerWorker: 2..16, maxWorkerLifetime: ≤24h,
  default 24h}`. The field is part of the immutable, hashed profile spec, so opting in
  requires a new profile and changes the frozen profile hash. An absent field keeps
  today's behavior exactly.
- **One pool, one scope.** A shared pool serves exactly one workspace namespace and
  Atespace, profile UID and hash, Actor size, and operator WorkerPool. It is never
  shared across namespaces or profiles.
- **Pool identity and ledger.** Pool labels carry a random value that is journaled. The
  derived template selects it. A resourceVersion-protected ledger on the pool lists
  every hosted allocation and Actor UID.
- **A foreign Actor fails closed.** Any assigned Actor that is not in the ledger fails
  observation, and the pool stops admitting and is retired through the strong path.

### Never shared

These keep the private single-Actor path:

- operator-tainted, quarantined, or `Failed` workspaces;
- restores and recoveries from a public checkpoint (`restoreFrom`);
- workspaces without a `DataOnly` (`VOLUMES`) profile;
- every profile without the field.

The provider also requires gateway injection of the provider-proxy credential, so the
long-lived provider token never enters a shared sandbox. An Actor on a shared worker
whose workspace is tainted, quarantined, or fails is retired through the strong path.

### Interim termination evidence

The interim evidence replaces worker-Pod absence on the normal path. All four must
hold:

1. **The Actor is suspended.** It is `SUSPENDED` (and then deleted) at a version after
   the issued suspend, with the expected new `VOLUMES` snapshot generation. A Stop issues
   the same suspend for evidence only.
2. **The fenced worker no longer hosts it.** Its assignment is absent from the same
   Worker UID and Pod UID, with `registered_epoch` and
   `observed_epoch == registered_epoch` unchanged, and the worker version is re-read
   unchanged.
3. **Everything it could use is revoked.**
   - Its egress policy, including credential injection, is replaced by deny-all and
     verified before `SuspendActor` is issued, so before the suspension is reported. A
     leaked sandbox has neither a tunnel nor egress.
   - The instance's Orka credentials are unusable (see the next section).
4. **The residue is bounded in time.** A worker past its admission window takes no new
   Actors. At `maxWorkerLifetime` it is deleted, and every hosted instance proves
   termination with today's Pod-absence proof.

### Credentials Core owns

Core already makes these unusable:

- **The attachment bearer** authenticates requests into the runtime, which the router
  no longer delivers to the sandbox.
- **The pool's controller bearer and capability secret** are accepted by the MCP broker
  only while the RuntimePool's exact active instance is set. Core clears that instance
  when it observes `Stopped`. It rotates both, by unpublishing and replacing the bound
  auth Secret, before any successor boots.

### Strong fallback

- **When it applies.** On any doubt (the assignment is still present, the epoch or
  identity changes mid-proof, the worker is terminating, or any upstream error), the
  strong path replaces the interim evidence.
- **What it does.** The provider closes the pool and deletes it. Core observes the
  terminating worker and drains each co-hosted Actor for data-only suspension inside
  its eviction window (ADR 0046). Every hosted instance then proves termination by exact
  worker Pod UID absence.

### Core needs no code change

- **Core never decides termination for native processes.** It accepts the provider's
  exact, Core-authorized terminal claim, and it never observes Substrate worker Pods to
  decide termination (`externalObservedInstanceTerminated`).
- **The binding already names the Actor and runtime, not the worker Pod.**
  - The private bootstrap binding is to the opaque per-sequence runtime UID
    (`externalNativeRuntimeUID`).
  - The sealed bootstrap is bound to the exact Actor identity (Atespace, name, UID).
  - The worker Pod reference is only an attestation and eviction fence. It stays
    correct when several instances name the same worker, because a Pod replacement
    kills every hosted sandbox. No binding moves to the worker epoch, and nothing
    changes for non-shared profiles.
- **The opt-in needs no new type or version.** Core reads Substrate profiles as
  unstructured data and freezes them by hash. The new field changes that hash without
  any new Core type or `orka-workspace` module version.

## Consequences

- **Faster starts, fewer Pods.** A turn that lands on a shared worker with a free slot
  skips worker birth, and up to 16 Actors of one namespace share one worker Pod.
- **Weaker than ADRs 0031 and 0049.** A per-Actor stop is proven by control-plane
  bookkeeping plus revocation, not by observed sandbox exit. The owner accepted this
  because sharing is opt-in, namespace-confined, time-bounded and revoked.
- **Residual risk: a leaked sandbox.** If teardown failed, that sandbox's memory,
  including its already-revoked credentials and workspace data, may persist on the node
  until the worker is recycled.
- **Residual risk: worker compromise.** A compromised worker exposes every co-hosted
  Actor of the same namespace and profile.
- **Strong retirements disturb neighbours.** A strong retirement suspends a pool's
  neighbours through the eviction path. A neighbour that cannot drain in time fails
  without replay.
- **Exit criterion.** Replace the interim evidence with an upstream per-Actor
  sandbox-exit attestation when Agent Substrate provides one. Until then, do not widen
  this decision.
- **Live validation is still needed.** Capacity, label propagation, `observed_epoch`,
  egress revocation on a running Actor, and eviction of a strongly retired shared worker
  must be validated live before the opt-in is recommended.
