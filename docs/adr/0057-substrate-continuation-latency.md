# 57. Start a workspace runtime before its attachment settles, and keep new Substrate workspaces warm by default

Date: 2026-11-19

## Status

Accepted, approved by the project owner. It amends:

- ADR 0026: RuntimePool demand may now come before the attachment, but prompt demand still may not.
- ADR 0027: on cold resume, the pool intent lifts when the continuation flips the workspace back to Ready, not after the attachment handshake.
- ADR 0029: through ADR 0055, retention now also ends default windows.
- ADR 0055: its **No default** rule changes. New eligible Substrate workspaces get a controller default window.

ADR 0030's full-memory gate, ADR 0031's data-only cold path, ADR 0049's credential model, and
ADR 0056's shared-worker evidence are unchanged.

## Context

kagent v1.0.1 reaches the agent prompt about 4.2 s after a Session follow-up Task is created.
Orka takes about 6.1 s on the warm path (ADR 0055). On the resume path it takes about 9.8 s
with spare workers and 11.6 s with shared workers.

Live medians for a follow-up that resumes from a Tag, cumulative from Task creation:

| Step | Owner | Time |
| --- | --- | --- |
| Observed, status, execution bound | Core | 0.8–1.2 s |
| Bound → attach requested (workspace flipped back to Ready) | Core | +1.05–1.36 s |
| Attach → provider acknowledges the epoch | Provider | +0.93–1.12 s |
| Workspace ready → RuntimePool ready → workload published | Core | +0.82 s |
| **Subtotal before the provider can start** | | **3.6–4.5 s** |
| Allocation, template and Actor work, resume → challenge → Ready | Provider, Substrate | ~2.1–2.5 s |
| Sealed bootstrap and probe | Core | 0.7–0.9 s |
| Reservation | Core | 0.8–1.2 s |
| Session plan | Core | 0.7–1.1 s |
| RuntimeSession create | Core | ~1 s |

Substrate's own resume → challenge step takes about 1.1 s. The rest of the gap is Orka's control
plane.

The serial chain has two causes:

- **The resume waits on the attachment.** The workload is published only after the attachment
  epoch is acknowledged and the RuntimePool handshake holds. `WorkspaceRuntimePoolReconciler`
  scales every unattached pool to zero. The `orka-workspace` provider does not need this
  ordering: it runs `EnsureAllocation` with or without an attachment, and it acknowledges an
  epoch whatever state the allocation is in.
- **Most follow-ups take the resume path.** ADR 0055's warm window is opt-in with no default.
  So a Session follow-up resumes cold unless an operator created a new profile.

The runtime does not depend on the attachment. Its credentials are scoped to the pool, not the
attachment (ADR 0055). The attachment Secret never reaches it (ADR 0026). Every prompt-path
fence is independent of when the runtime booted, for example controller epoch, pool UID and
generation, runtime instance and boot, RuntimeSession generation, and the per-prompt digest.

## Decision

### 1. Speculative start

#### When a Task may start a runtime speculatively

A Task whose execution binding is frozen may record a speculative start for its class-backed
workspace before it holds the attachment. It may do so only when all of these hold:

- the workspace is desired `Ready`, unattached, and not deleting;
- it is not `Failed`, quarantined, or operator-tainted;
- operator taint counts on the workspace and on every Task linked to this incarnation, and an
  unreadable Task refuses the start;
- no revocation stamp is present, so no settlement is pending;
- the outstanding demand stamp (`resume-requested-at`) names this Task's UID;
- the Task is linked to this exact workspace UID;
- the workspace's frozen lifecycle allows `Suspend` with a `DataOnly` profile. Only such a
  workspace retires an unclaimed runtime with a data-preserving `Suspend`. A `Stop` of a
  never-attached request would leave a stopped request without retained data, which the
  attachment rule cannot accept, so every other class keeps the attach-first path;
- the workspace is one of:
  - **a resumed lineage** that this Task just flipped back from a settled `Suspended`;
  - **a fresh workspace** that has never published a workload or held an attachment (a first
    turn).

#### What a speculative start records

The start is the protected workspace annotation `acp.workspace.orka.ai/speculative-start`. Its
value is `<RFC3339Nano deadline> <Task UID>`.

- The deadline is two minutes after the start, so an unclaimed runtime cannot idle on pool
  credentials. A normal attach takes seconds; the margin covers a controller restart.
- It is written once per Task with an optimistic lock and **never extended**. A later pass by
  the same Task, or by a restarted controller, leaves it unchanged. Once it has expired, that
  Task does not record it again.
- It is written **before** any pool demand.
- Then the Task activates the RuntimePool. It creates the pool for a first turn, or scales the
  existing one to one replica for a resume. This uses exactly the pool-identity checks of the
  ordinary path: frozen class, provider, profile, required features, workspace link, UID pin,
  image, and profile digest. Only the post-attach handshake is skipped.
- The start is best-effort. Any failure leaves the ordinary attach-then-demand path in force and
  never fails the Task.

#### Pool demand

`WorkspaceRuntimePoolReconciler` keeps an unattached pool's replicas while the start is open:

- the deadline has not passed;
- the workspace is still in the allowed state above, including outstanding demand.

It wakes at the deadline. Before it scales any unattached pool to zero, it re-reads the
workspace from the API server. A cached copy older than the pool event that woke it must not
undo the start or a fresh attachment.

#### The provider starts at once

The RuntimePool controller is unchanged. Once the workspace is admitted at its current generation
and the provider has observed it:

- it proves the previous instance terminated;
- it rotates the consumed bootstrap material;
- it publishes the next workload request.

The provider then boots the runtime while Core finishes the attachment and its handshakes.

#### Attachment during the boot

The class attachment rule also accepts a recorded speculative start whose exact published request
is still booting:

- the request is unretired;
- the observation is `Pending` for the same key and sequence, at the same revision if it reports
  one;
- for a first turn, the provider may not yet report any observation at all.

Every other guard still applies: current core admission, the provider observed the current
generation, `Pending` state, no quarantine, no revocation stamp, no taint. A `Ready` allocation
already passes the generic rule, exactly as a warm-window attach does. The attach patch removes
the start along with the demand.

### 2. Default warm window for new Substrate workspaces

- **Where the default comes from.** The controller flag
  `--acp-workspace-default-suspend-after-detach` sets it, with a default of `5m`.
  - `0` disables only the controller default; a class annotation still applies.
  - The value must be at most 1 h; otherwise the controller refuses to start.
- **When it applies.** Only when Core materializes a new class-backed workspace whose binding is:
  - session-reused;
  - an external `substrate.workspace.orka.ai` `SubstrateWorkspaceProfile` with a `DataOnly`
    suspend mode, in a class that allows `Suspend`;
  - from a profile that sets no `spec.retention.suspendAfterDetach`.
- **Overrides.**
  - An explicit profile window always wins.
  - The annotation `acp.workspace.orka.ai/default-suspend-after-detach` on the exact frozen
    `ExecutionWorkspaceClass` can set another duration for new workspaces of that class, whatever
    the flag is, including `0`.
  - The value `disabled` opts the class out. An invalid value also disables the default.
- **ADR 0055's validation.** The window must be at most 1 h and strictly shorter than
  `idleTimeout` and `maxLifetime`. Suspend, `DataOnly`, and Session reuse are required. A
  default that does not fit is not applied, and the workspace freezes no window rather than
  failing the Task.
- **How it is frozen.**
  - **Only into the new workspace**, as `acp.workspace.orka.ai/suspend-after-detach` plus
    `acp.workspace.orka.ai/suspend-after-detach-source: controller-default`.
  - It never enters a profile, the class profile hash, the Task's class binding, the execution
    snapshot, or the binding digest, which also names the Session pool. Those stay
    byte-identical with the default on or off.
  - The ADR 0055 backfill copies only a profile window. An existing workspace without a window
    keeps none.
  - Verification accepts a default-sourced window only for a binding whose profile sets none,
    whatever the current flag is. It rejects a source marker without a window, an unknown
    source, and a default-sourced window under a profile window.
  - Changing the flag or the class annotation affects new workspaces only.
- Every ADR 0055 rule then applies unchanged to the frozen value: settlement eligibility, pool
  hold, retention expiry, the cap, taint, and races.

### Fencing argument

1. **No prompt before the attachment.** The queue path still requires the exclusive attachment,
   current re-admission, and the provider's epoch acknowledgement. It also runs the full
   post-attach handshake (`verifyACPWorkspaceReadyForPool`) before it persists a PromptAttempt.
   A speculative runtime with no attachment fails that handshake, so it is never reserved or
   dispatched to.
2. **One writer.** The attachment is the same optimistic, epoch-fenced Lease and Secret handoff.
   Two Tasks never hold it, because the attach validation reruns on every optimistic-lock
   attempt. A concurrent continuation of the same Session may claim a speculative runtime its
   rival started. That is safe for two reasons:
   - the runtime is scoped to the Session's pool, cold booted, and holds no Task state;
   - the claimant passes every check the requester would.

   The rival then queues behind it as today.
3. **Every startup proof still runs.** A speculative runtime cold-boots under rotated bootstrap
   material and still goes through all of these before the pool can serve:
   - exact-instance startup attestation;
   - router or ingress verification;
   - the instance-evidence binding;
   - the sealed signed credential bootstrap;
   - the authenticated probe with its post-probe fence.

   ADR 0027's `expectDurableResume` assertion and checkpoint checks run at RuntimeSession
   creation, after the attachment.
4. **No new credential exposure.** Only the pool credentials and the sealed bootstrap reach the
   runtime. Task workspace read credentials are brokered at RuntimeSession creation. The
   attachment bearer never reaches it. An unclaimed speculative runtime is the ADR 0055 warm
   state, a supervisor idling on pool credentials with no attachment bearer and no ACP child,
   bounded by two minutes instead of the warm window.
5. **A rejected or cancelled Task.** Its settlement runs the existing path for a requester that
   never attached:
   - Suspend on a committed resume re-suspends, and that write removes the start;
   - an empty first turn is deleted, and the pool is torn down with it;
   - a queued successor may take over through the existing deferral.

   Desired state leaves `Ready`, so the hold ends and the pool scales to zero. The RuntimePool
   then retires the exact instance:
   - with `Suspend`, data-only, as for any Suspend-capable scale-down;
   - with `Stop` when the workspace is tainted (ADR 0044).

   A cancelled Task can never attach afterwards: a direct attach requires desired `Ready`.
6. **Unclaimed but still alive.** If the requester outlives the deadline without attaching, the
   pool scales to zero and the instance retires the same way. The workspace stays `Ready` with
   the stopped allocation and its retained data. That is exactly today's post-flip state. The
   requester's later attach takes the stopped-predecessor path and cold-boots again. This also
   holds when the expiry races the requester's own attach:
   - if the attach lands first, the pool controller's uncached re-read sees it and keeps the
     replicas;
   - if the scale-down lands first, the attach waits for the exact retirement to finish and then
     attaches the stopped request.

   A runtime retired by `Stop` (taint, quarantine, or failure) never strands its requester. Taint,
   quarantine, and `Failed` already fail the waiting Task. Any other unattached Suspend-capable
   workspace whose current request stopped without retained data also fails its Task with a
   terminal error. No later request could carry that data. The error says to retry the Task for
   a never-committed first turn, whose settlement deletes the empty workspace, or to start a new
   Session.
7. **Controller restart.** Every decision reads durable state only: the start annotation, the
   demand stamp, desired state, and the attachment. A restarted controller rebuilds the hold. It
   neither extends nor re-records a start. A new controller epoch retires a request published
   under the old fence, as before.
8. **Idempotence.** Every write uses an optimistic lock and every retirement is exact-instance.
   The pre-flip resume demand and settlement receipts still order settlements. A repeated pass
   changes nothing.

### Provider requirements

Neither change needs an `orka-workspace` code change. The provider must keep three properties of
today's `ExecutionWorkspaceReconciler.reconcileLifecycle` and cover them in its conformance
suite:

1. `EnsureAllocation` for a published request runs without `spec.attachment`. Startup never
   waits for an attachment.
2. The attachment epoch is acknowledged while the allocation is `Pending`. An attachment never
   restarts, replaces, or blocks a booting allocation.
3. A `Suspend` (or `Stop`) retirement of a still-`Pending`, never-bootstrapped allocation does
   one of two things:
   - captures `VOLUMES` only and reports `Stopped` with `RetainedData` bound to that instance;
   - fails without replacing the last retained data.

   Its sandbox gets the same deny-all egress and termination evidence as any retirement (ADR
   0056 on shared workers).

Two provider changes would shorten the path further. Neither is required, and neither is made
here:

- accepting a workload published in the same write as the resume flip, which saves one provider
  observation;
- acknowledging an attachment in the same pass that first observes it.

## Consequences

- **Expected saving on resume: about 1.8–2.5 s.**
  - The workload is published about one provider observation after the flip, instead of after
    the attach acknowledgement and the pool chain (about 1.75–1.95 s earlier).
  - The attach acknowledgement, pool handshake, PromptAttempt, and queue then overlap the
    provider's 2.1–2.5 s boot.
  - The re-read before scale-down also removes a replica flap that a stale cache could cause
    right after an attach.
  - The owner's estimate of 2.5–3.5 s may hold when queueing fully hides behind the boot. Live
    timing (`speculativeStartMs` in the pre-dispatch log) must confirm it.
- **Expected saving on a first turn: about 1.0–1.5 s.** The pool is created at workspace
  creation and publishes at core admission, in parallel with the attach and its acknowledgement.
- **Default warm window.** Most follow-ups within five minutes take ADR 0055's warm path (about
  6.1 s today) instead of a cold resume. Each eligible Session holds one idle Actor slot for up
  to five minutes after each detach:
  - on shared workers (ADR 0056), that is one Actor's CPU and memory reservation inside an
    existing worker;
  - on private workers, it is one worker Pod.

  Operators who cannot afford that set the flag to `0` or opt classes out.
- **Wasted boots.** A cancelled or rejected follow-up may boot and retire a runtime it never
  uses. That costs one extra `VOLUMES` Tag and up to two minutes of an idle runtime. Session data
  is never lost: the same retained data is re-captured or kept.
- **Pre-dispatch timing.** It gains the `speculativeStart` milestone.
- **Out of scope.** Change 1 does not apply to the warm window, which already attaches a running
  runtime. Change 2 does not apply to Agent Sandbox or non-Substrate profiles.
