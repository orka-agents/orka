# 46. Survive Agent Substrate 1.x upgrades and worker eviction

Date: 2026-11-06

## Status

Accepted. Refines the cold-resume infrastructure rule of ADR 0031 and the
data-only suspension of ADR 0039: a data-only resume may now follow the operator
WorkerPool's current, digest-pinned worker image, and a worker termination
triggers the existing drain and data-only suspension instead of losing the
instance. Full-memory restore stays prohibited (ADR 0030). Task-level eviction
status and proactive rolling of running workspaces are left open.

## Context

Agent Substrate 1.x upgrades in place. Operators roll every WorkerPool to the
new worker image before the API server, because an API server must never be
newer than a worker. On worker `SIGTERM` each hosted Actor receives `SIGTERM`
and has 30 minutes to be suspended, synchronously, before it is killed;
Substrate does not suspend it. Node drains and Spot reclaims (15 seconds to 2
minutes) arrive the same way. A gVisor restore without memory runs under the
worker's default `runsc` without checking that it can read another version's
filesystem checkpoint; a failed restore crashes the Actor.

Orka had three gaps. The provider froze the worker image in the private pool's
restore digest, so after an upgrade a suspended workspace either resumed on the
old image or was refused. The ACP supervisor exited on `SIGTERM`, so any worker
termination crashed the Actor and lost work since its last checkpoint. And a
cross-runtime restore crash looked like any other lost instance.

## Decision

- **Image follows the operator pool, nothing else.** A data-only resume or
  `RestoreFrom` import compares the retained private pool specification with
  the one compiled from the operator pool now. Only `workerImage` may differ,
  and the new image must be pinned by digest. Replicas, sandbox class, pinned
  SandboxConfig, scheduling, size, template labels, and the operator pool's own
  labels must be identical. Each transition is appended to the journal (from,
  to, previous pool digest, time). Anything else fails closed. Operators patch
  operator pools only, never the provider's private pools.
- **Pin the SandboxConfig.** The private pool names the operator pool's
  resolved `configRef` explicitly. An unresolved, switching, or contradictory
  config fails closed.
- **Hold on eviction.** A supervisor that is a Substrate Actor (its SystemInfo
  identity projection is present) and hosts a durable workspace does not exit
  on `SIGTERM`. It closes admission through the same drain the controller uses,
  with reason `worker_eviction`, and stays alive for at most 30 minutes. The
  hold is skipped only if the controller already requested a Stop/Delete
  retirement drain (`workspace_retirement`). Core now sends `workspace_suspend`
  when the planned action is Suspend. No template variable selects the hold;
  the native backend admits at most 32 environment entries.
- **Core owns the trigger.** Core observes either the exact attested worker Pod
  terminating (`deletionTimestamp` or `DisruptionTarget`) or the supervisor's
  reported worker eviction time in an authenticated probe. The supervisor
  reports that time separately from the drain reason, so a `SIGTERM` during a
  suspension or capacity drain is still seen. It records an eviction
  bound to workspace UID, sequence, and instance identity, with the earliest
  signal and the shortest deadline, then requests exact retirement. Retirement
  reuses the authenticated drain, quiescence, and epoch fences. Suspend is
  authorized only for a quiescent runtime and only before the deadline. After
  the deadline it is never authorized: the instance fails through the provider,
  its last checkpoint stays retained, and accepted prompts are never replayed.
  A class that cannot suspend keeps the existing lost-instance semantics.
- **Admission never reaches a terminating worker.** The admission attestation
  rejects it. Drain, settlement of accepted prompts, and exact retirement may
  still reach the same fenced worker. The provider keeps reporting the fenced
  running instance while its worker terminates, never adopts an unfenced
  terminating worker, and its suspend path tolerates a Pod already deleting.
- **Surface it.** RuntimePools carry a `WorkerEviction` condition with
  `EvictionDraining`, `EvictionSuspended`, `EvictionDeadlineExceeded`, or
  `EvictionNotSuspendable`.
- **Classify runtime mismatches.** Each data checkpoint records the snapshot's
  `sandbox_runtime`. A restore that crashes before its supervisor boots under a
  different runtime fails with `SandboxRuntimeRestoreFailed`, not
  `InstanceFailed`.

## Consequences

A minor Substrate upgrade no longer strands suspended workspaces, and a running
workspace survives a worker roll, node drain, or Spot reclaim when it drains in
time. An active prompt that outlives the window is lost, as before, and is
never replayed. Short Spot grace is best effort. A Stop or Delete that starts
after an unrelated suspend drain can wait out the hold. Lineages that predate
the pinned SandboxConfig fail closed on resume. Running workspaces keep their
old image until they are evicted or suspended, so the upgrade runbook must
evict them before the API server moves. Live conformance of eviction under
gVisor is still required.
