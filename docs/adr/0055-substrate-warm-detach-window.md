# 55. Keep a detached Substrate workspace warm before suspending it

Date: 2026-11-17

## Status

Accepted. Amends the detach step of ADR 0027, the idle Ready rule of ADR 0029, and the
Detach and finalization section of ADR 0026 for Substrate classes that opt in. ADR
0031's data-only cold path and ADR 0030's full-memory gate are unchanged.

## Context

A session-reused Substrate workspace whose frozen detach action is `Suspend` is torn
down the moment its Task detaches. `WorkspaceRuntimePoolReconciler` sets the pool's
desired replicas to zero for any workspace without an attachment, so the supervisor
drains and the provider takes a `VOLUMES` Tag, deletes the private worker Pod and
WorkerPool, and deletes the Actor. The next turn of the same Session then pays the full
cold resume:

- a new private WorkerPool and worker Pod (scheduling, worker image pull, registration);
- an Actor restored from the Tag;
- rotated pool credentials and a signed bootstrap;
- a cold supervisor boot;
- a new RuntimeSession.

Follow-up turns often arrive within minutes, so most of that work buys nothing.

kagent suspends after every turn, but it resumes Actors onto shared workers that are
already running, so its resume is cheap. Orka's private single-Actor workers (ADR 0049)
make each resume expensive.

A running supervisor needs nothing re-seeded when it is attached again. Its pool
credentials (controller token, capability secret, provider token) are scoped to the
pool, not the attachment. Each attachment brings a new epoch, bearer Secret, and Lease,
which the mid-Task bearer rotation already applies to a running Actor.

## Decision

### Operator-owned policy

- `SubstrateWorkspaceProfile.spec.retention.suspendAfterDetach` is an optional
  duration. When it is set, a detached workspace stays Ready, with its Actor and
  supervisor running, for that long before the frozen `Suspend` applies.
- **No default.** An unset field keeps today's behavior, and existing profile and
  class hashes are unchanged.
- **Validation when the class is resolved.** The value must be positive and at most one
  hour. The class must allow `Suspend` with a `DataOnly` profile and Session reuse, as
  suspension already requires. When `idleTimeout` or `maxLifetime` is set, the window
  must be shorter than it.
- **Frozen like the retention cap.** The window freezes through the class binding, the
  execution snapshot (omitted when unset), the binding digest (a field only when set),
  and the immutable workspace annotation `acp.workspace.orka.ai/suspend-after-detach`.
  Changing the profile affects only new workspaces.
- **Substrate only in practice.** Core reads the field from any external profile,
  but only the Substrate profile CRD defines it. Agent Sandbox
  resume reattaches a PVC and stays immediate (ADR 0028).

### Settlement

- **When the window applies.** It applies when the settling Task executed a turn
  that did not park (ADR 0041: parking promises to release runtime capacity), its
  effective detach action is `Suspend`, and the session-reused workspace:
  - is Ready, and not Failed, quarantined, or operator-tainted;
  - carries the durable-session commit stamp.
- **The deadline is stamped when revocation begins.** Revocation clears the
  attachment before the adapter releases the epoch, and an unattached workspace
  without a window would scale its pool to zero at once, starting a drain that
  cannot be undone. So the same patch that records the revocation stamp and
  `last-detached-at` also writes the absolute `acp.workspace.orka.ai/suspend-at`
  deadline, measured from the start of revocation. Every revocation path stamps
  it, including Task finalization.
  - Each detach gets its own window: a deadline left by an earlier epoch is
    replaced, never carried forward.
  - An ineligible detach removes any deadline in the same patch.
- **What settlement does instead of suspending.** Once the adapter releases the
  epoch, and if the deadline has not passed, one optimistic patch records the
  Task's settlement receipt and `last-detached-at`. It clears the revocation stamp
  and any resume demand, so a continuation can attach the Ready workspace
  directly. The Task is then marked settled.
  - The deadline is never rewritten or extended.
  - A settlement that finds the deadline already passed takes today's cold path.
  - Paths that do not keep the workspace warm remove the deadline: a Failed
    workspace kept for recovery, and a cold Suspend blocked by the cap.

### Pool demand

- **What keeps the pool running.** `WorkspaceRuntimePoolReconciler` keeps the pool's
  replicas for an unattached workspace when all of these hold:
  - it is Ready, with a `suspend-at` deadline;
  - its recorded detach action is `Suspend`;
  - it is not Failed, quarantined, or tainted.
- **Only retention ends the window,** by changing desired state. The pool reconciler
  never times the window out itself, because a supervisor drain cannot be undone.
- **Losing the warm runtime is safe, just cold.** A controller epoch change, upgrade
  drain, eviction, or provider failure retires it exactly as today, and the next turn
  cold-resumes from the last Tag.

### Expiry

- **When the deadline passes,** retention applies the same guards as idle suspension:
  - tainted, uncommitted, or Failed workspaces go to Delete;
  - otherwise `suspendACPWorkspaceWithinQuota` runs with the frozen `Suspend`, removing
    `suspend-at` in the same write.

  This is evaluated before, and independently of, `idleTimeout`. Suspension restamps
  `last-detached-at`, so a suspended workspace keeps its full `idleTimeout` afterward.
- **If the suspended-workspace cap is exhausted,** retention replaces `suspend-at`
  with `acp.workspace.orka.ai/suspend-pending` and leaves the workspace Ready. The
  pool then scales to zero with data-preserving retirement, so Actors never keep
  running until `maxLifetime`. Because no settlement is still pending to retry the
  frozen `Suspend`, retention retries it on each pass and otherwise applies ordinary
  idle retention. Attaching, suspending, or a new detach clears the marker.
- **Warm Ready workspaces do not count against `maxSuspendedWorkspaces`.**
- **Operator access cuts the window short.** A workspace tainted during the window
  expires immediately instead of waiting for its deadline.

### Continuation during the window

- **A continuation attaches the Ready workspace through the existing path.** Recording
  its detach action removes `suspend-at`. No credential rotation, bootstrap, or
  supervisor boot happens. A new RuntimeSession is still created from the canonical
  transcript, because write-intent RuntimeSessions are still retired after every turn.
- **Races.**
  - Attach and the delayed suspension both write the workspace under its
    resourceVersion, so exactly one wins. A direct attach is refused once desired
    state is no longer Ready, so a retried attach can never undo a suspension that
    landed after its first read.
  - If the suspension wins, the continuation takes today's cold-resume path.
  - A continuation still planning when the deadline passes resumes cold.

## Consequences

- **Faster follow-ups.** A turn that arrives within the window skips the WorkerPool,
  worker Pod, Actor restore, credential rotation, bootstrap, and supervisor boot. Only
  the RuntimeSession start remains.
- **Credentials stay live, for a bounded time.** During the window, a supervisor
  holding pool credentials idles with no attachment bearer and no ACP child. It is
  bounded by the window, cut short by taint or quarantine, and capped by `maxLifetime`.
  This is the same exposure as Deployment pools keeping supervisors resident between
  turns.
- **Compute cost.** Each warm workspace holds one private worker Pod for up to the
  window. Operators size the window against worker capacity.
- **Releases.** Core reads the profile field as unstructured data, so it needs no new
  provider module version. The provider must ship the CRD field before operators can
  set it.
