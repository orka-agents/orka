# 61. Re-admit a resumed ACP workspace in its publication write, queue on Core's fence, and reserve its slot while the runtime starts

Date: 2026-11-20

## Status

Proposed. This is a design only; nothing here is implemented. Everything ships behind
`--acp-resume-admission`, default `off`.

**Relationship to ADR 0060** (`acp-attach-readmission`, Proposed, parked). This record stands
beside ADR 0060. It neither supersedes it nor depends on it being accepted:

- It **extends ADR 0060 §1 (inline re-admission)** from an attachment-only write to the resume
  publication write, which also changes the runtime digest. Both would use the same shared
  validation helper. If ADR 0060 never lands, this ADR carries that helper itself.
- It **does not adopt ADR 0060 §2**, which replaces the provider's acknowledgement. On resume, the
  acknowledgement always arrives before the RuntimePool can reach `Serving` (Context, "What the
  acknowledgement proves on resume"). So this ADR **moves** the acknowledgement from the queue
  gate to the contact gate, as ADR 0059 did for reuse, and keeps ADR 0023 step 7 literal. It
  needs no `AttachmentAdvisory` provider feature.
- It adopts **ADR 0060 §3 (revocation by generation)** for the Tasks it queues early.

It would amend:

- **ADR 0024 and AGENTS.md: "only `Serving` + `Accepting` admits new RuntimeSessions".** A
  workspace pool that is still `Starting` its current published request may hold one
  *instance-unbound* reservation. Only `Serving` + `Accepting` may bind that reservation to a
  runtime instance. Without a bound reservation, no RuntimeSession is created and no prompt is
  sent. The new rule's text is in section 6.
- **ADR 0021/0022 core admission**, on the resume publication only. The RuntimePool's
  publication write carries `coreAdmission` for the new generation.
- **ADR 0057's queue handshake** (`verifyACPWorkspaceReadyForPool`), on the resume path only.
  The Task queues on Core's resume fence. The unchanged handshake runs immediately before first
  runtime contact.

It changes none of these:

- ADR 0023's Lease, epoch, bearer Secret, and revocation ordering. Step 7 holds before first
  runtime contact;
- ADR 0026: the bearer never reaches the runtime;
- ADR 0027/0031/0049 data-only resume, credential rotation before cold boot, and proof that the
  retired Pod is absent before republish;
- ADR 0030's closed full-memory gate;
- ADR 0055/0057 warm-window and serving rules;
- ADR 0056's termination evidence and strong fallback;
- ADR 0058's interlock;
- the exact-instance and boot fences on `CreateRuntimeSession` and every prompt (ADR 0024).

**Headline.**

- **(A) and (B) are worth doing only together.**
  - In round 11, the Task's queue (`runtimePoolReady`) lands within 30–260 ms of pool
    admission. So queueing on Core's fence saves almost nothing alone (≤0.26 s).
  - Its value is that it lets the reservation run while the runtime is still starting. That
    reservation is (B), and it is what removes the 0.7–1.0 s `admitted → reserved` hop.
  - Inline re-admission (A1) helps on every path, independently of the rest.
- **Estimated saving.**
  - With A1, A2 and B1: private resume 7.8 s → **~6.5–6.8 s**; shared 7.7 s → **~6.6–6.9 s**.
  - The optional B2 stage takes off another ~0.3–0.45 s, for **~6.0–6.5 s** on both.
- **Nothing here is unsafe.** B2 is marginal for its state-machine cost. The recommendation is
  to defer it (open question 5).

## Context

### Round 11 resume, medians from Task creation (ms)

Columns, as labelled by the harness:

- **private**: `warmdefault` resume after the 5 m window (a spare was claimed);
- **shared**: `shareddefault` resume from Tag;
- **spare**: `spare` resume from Tag (detach path).

Medians come from different runs, so a column need not be monotone.

| Milestone | private | shared | spare |
| --- | --- | --- | --- |
| `published` (RuntimePool publication write) | 873 | 1133 | 1018 |
| `workspaceReady` (Task sees `attachedEpoch`) | 2277 | 2352 | 2016 |
| `dataPlaneReady` (provider `DataPlaneReady`, whole-second truncated) | 3468 | 3985 | 3956 |
| `runtimePoolReady` (Task sees `observedGeneration`) | 4560 | 5029 | 4766 |
| `admitted` (pool `Serving`) | 4796 | 5350 | 4991 |
| `promptAttemptPersisted` | 4911 | 5327 | 4972 |
| `dispatchQueued` | 5052 | 5381 | 5030 |
| `dispatchScanned` | 5096 | 5623 | 5011 |
| `reserved` | 5787 | 6079 | 6000 |
| `runtimeClient` | 6062 | 6286 | 6204 |
| `sessionPlan` | 6983 | 7003 | 6813 |
| `sessionCreate` | 7649 | 7443 | 8215 |
| **agent receipt** | **7764** | **7713** | **8563** |
| `derived.publishToProviderStartMs` (biased ~0.5 s low by truncation) | 372 | 665 | 481 |

Per-window medians, in ms (`windows-r9c-vs-r11.txt`):

| Window | private | shared |
| --- | --- | --- |
| Core before the provider (create → published) | 873 | 1133 |
| Provider before restore (published → `runsc create`) | 1443 | 1808 |
| Restore | 838 | 756 |
| Credential bootstrap | 985 | 1132 |
| Serving → session created | 2554 | 2297 |
| Created → receipt | 825 | 575 |

kagent resumes in 2.8 s, and Substrate's native restore matches it (~1 s). The rest of the
gap is Orka's own ordering. This ADR targets two parts of it:

- the Core re-admission round trip between the publication and the provider's start;
- the queue, reserve and plan chain, which today runs only after `Serving`.

### Hop map: publication → first prompt

Code references are against `origin/substrate-parity-integration` at `5d3f89b5f`, under
`internal/controller/`. `P:` paths are orka-workspace `providers/substrate/` (provider
`068ec6a`, as cited in `resume-attribution.md`).

Kinds:

- **Adm**: Core admission;
- **Ack**: provider acknowledgement or observation;
- **Prov**: provider work;
- **Res**: reservation;
- **Sess**: session plan or create.

| # | Hop | Code | Kind | Serial after |
| --- | --- | --- | --- | --- |
| 0 | Task resume request: demand, detach action, lineage marker, speculative start, and the staged attachment intent. Annotation-only, so the generation stays at *G* | `acp_workspace_speculative_start.go:393` (`requestACPWorkspaceSpeculativeResume`), `:566` (stage) | – | binding |
| 1 | RuntimePool: proves the stop (`externalObservedInstanceTerminated`), rotates the consumed auth Secret (one pass per rotation step) | `runtime_pool_external_workspace.go:195-216`, `runtime_pool_controller.go:2069` | Adm | 0 |
| 2 | **Publication write**: `spec.workload` = sequence+1 with retained data, desired `Ready`, the staged attachment installed. Generation *G*→*G*+1 | `runtime_pool_external_workspace.go:481-490`, `:507-520`; install fences `acp_workspace_speculative_start.go:594` | Adm | 1 |
| 3 | **Core re-admission, write 1**: `ExecutionWorkspaceReconciler` wakes on *G*+1, which is not admitted, validates the bindings, and writes the `Admitted=True` condition at the predicted *G*+2 | `execution_workspace_controller.go:121-168` | **Adm** | 2 |
| 4 | **Core re-admission, write 2**: wakes on its own status event, validates again, and patches `spec.coreAdmission.admittedGeneration` = *G*+2 | `execution_workspace_controller.go:170-190`, `:549-593` | **Adm** | 3 |
| 5 | Provider: wakes on *G*+2, now currently admitted (it is `pending` until then, `P:reconcile.go:55-64`). Pass 1 echoes `attachedEpoch`, `state=Attached`, `Attached=True@G+2` | `P:reconcile.go:53-84` | **Ack** | 4 |
| 6 | Provider resume: allocation, spare claim or shared ledger, template and Actor, worker wait, `ResumeActor`, challenge, `observeReady`; then a status write with `observedGeneration` = *G*+2, a `Ready` allocation and startup evidence | `P:lifecycle.go:69-405`, `P:native.go:385-518`, `P:reconcile.go:218-282` | Prov + **Ack** | 5 |
| 7 | Task: wakes on `attachedEpoch`, marks `workspaceReady`. Its pool handshake is recovery-pending while `observedGeneration` ≠ generation (`acp_task_queue.go:1910-1915`), so it requeues | `acp_task_queue.go:127-149`, `:1630`, `:1770-1823` | **Ack** wait | 5 |
| 8 | Pool: `startupObserved`, attest, bind evidence, bind the bootstrap instance, the served record, credential bootstrap, probe, post-probe fence, `Serving` + `Accepting`. Requires `observedGeneration` = generation and current core admission | `runtime_pool_external_workspace.go:231-303` | Adm | 6 |
| 9 | Task: wakes on `observedGeneration`, reruns the queue prelude; `verifyACPWorkspaceReadyForPool` passes → `runtimePoolReady` | `acp_task_queue.go:150-169`, `:1859-1941` | **Ack** wait | 6 |
| 10 | Credentials, fenced `CreatePromptAttempt` (ADR 0058 interlock), Task metadata patch, `Queued` status patch | `acp_task_queue.go:190-280` | – | 9 |
| 11 | Dispatcher wake on `Serving` (`runtimePoolServingForDispatch`), 200 ms minimum scan gap, full scan | `acp_dispatch_wake.go:27`, `:138-170`; `acp_dispatcher.go:256-314` | Res | 8 **and** 10 |
| 12 | `reserveTask`: uncached Task read, `CurrentFence`, settle, **pool reservation CAS** (`runtimePoolReservationAdmission` requires `Serving`+`Accepting` and records `RuntimeInstanceID`), credential freeze, refresh, fenced Queued→Reserved, Reserved Task patch | `acp_dispatcher.go:4144-4323`, `:3572-3685` | **Res** | 11 |
| 13 | `executeReservedTask`: preflight, `runtimePoolClient`, `Capabilities` | `acp_dispatcher.go:1067-1160` | Sess | 12 |
| 14 | Session plan: lineage, MCP digest, namespace read, ADR 0059 claim (always a miss on resume), session read, fenced Reserved→SessionStarting **with `RuntimeInstanceID`**, Session lease, `OpenTurn`, `setSlots`, SessionStarting→Planned, Task patches | `acp_dispatcher.go:1211-1290`; `acp_dispatcher_session.go:631-691` | Sess | 13 |
| 15 | `CreateRuntimeSession` (exact instance, boot fences), then prompt submit | `acp_dispatcher.go:1637`, `:1824` | Sess | 14 |

**What is serial.**

- **The admission side (hops 1–4) runs before the provider can start.** Hops 3 and 4 exist
  only because the publication changes the generation, and core admission follows it in two
  writes on two wakes. Nothing in them depends on the provider. They are Core re-checking
  bindings it checked one pass earlier.
- **The queue side waits on the provider's last acknowledgement write (hops 7 and 9).** The
  Task cannot queue until `observedGeneration` catches up. On resume, that write is the same
  one that reports the `Ready` allocation (hop 6), so the Task queues only once the data plane
  is ready.
- **The pool's admission (hop 8) gates the same write.** Hops 8 and 9–10 therefore race. In
  round 11 they finish together (`runtimePoolReady` 4560 vs `admitted` 4796, private).
- **Reservation (hop 12) is gated on `Serving`.** So hops 11–15 (≈2.3–2.9 s) are all serial
  after `Serving`, even though only hops 13 (in part), 14 (in part) and 15 need a verified
  instance.

**Which waits are acknowledgement.** The only provider-acknowledgement waits are on the Task
path (hops 7 and 9). The pool's hop 8 waits on provider *evidence*: the `Ready` allocation,
startup and attestation. That is not an acknowledgement of the attachment.

### What the acknowledgement proves on resume

On resume, the attachment is installed in the publication write (ADR 0057), so the provider
observes the attachment and the new request in one generation.

- **The pool cannot reach `Serving` without the observation.** Hop 8's gate requires
  `status.observedGeneration == generation` and current core admission
  (`runtime_pool_external_workspace.go:231-232`).
- **The acknowledgement is in the same write or an earlier one.** The provider writes
  `attachedEpoch` in pass 1 (hop 5), before or together with the status write that carries
  `observedGeneration`.

So whenever a resumed pool is `Serving` its current request, ADR 0023 step 7's condition
(`attachedEpoch` = epoch, `Attached` current) has already held. The only exception is a later
attachment-only change, such as a rotation at expiry; the contact gate below then waits for it.

Queueing on Core's fence and checking the unchanged handshake at first contact therefore
**costs nothing on resume and needs no new provider contract**. It is ADR 0059's relocation,
applied to a resume publication.

## Decision

### Scope

A **resume publication** is a RuntimePool publication (`publishExternalWorkspaceWorkload` with
`pendingResume`) for which all of these hold, on the exact object version the write is locked
on:

- `--acp-resume-admission` is at least `readmit` (Rollout);
- the pool is an `orka.harness.v2` `acp-ws-*` workspace pool. That makes it single-session, with
  capacity pinned to 1 resident and 1 running prompt (ADR 0024);
- the workspace is class-backed, with `reusePolicy: session`, Substrate `DataOnly`, and a
  resumed lineage (`acpWorkspaceResumedLineageAnnotation`);
- the workspace already carries `coreAdmission` (a re-admission, never a first one);
- the staged attachment is installed by this write (`installableACPWorkspaceAttachmentIntent`
  returned non-nil). A resume whose install fails keeps today's path end to end.

First turns (a fresh workspace whose publication carries the attachment) have the same shape.
They are left out of scope (open question 6).

### 1. (A1) Inline re-admission in the resume publication

`publishExternalWorkspaceWorkload` admits the generation it creates. This is ADR 0060 §1
applied to the publication write, inside the existing optimistic retry.

1. **Build the post-write object.** Apply `applyExternalWorkspacePublication` to a copy. That
   sets the request, desired `Ready`, and the attachment.
2. **Validate exactly as core does.**
   - Call `ExecutionWorkspaceReconciler.validateWorkspaceBindings` on that copy, through the
     uncached reader, with `previouslyAdmitted = true`. The function moves to a shared helper
     (`workspaceadmission.ValidateBindings`) that all callers use; it is not copied.
   - Re-admission does not touch pool capacity: `acquireWorkspacePoolAdmission` runs only when
     `coreAdmission` is nil (`execution_workspace_controller.go:563-573`), and that case is out
     of scope.
   - The copy must also carry no maintenance intent (`workspaceHasMaintenanceIntent`) and need no
     attachment revocation.
   - Any result other than `Ready` drops the inline step, and the publication proceeds exactly
     as today.
3. **Write the condition first.** Write status `Admitted=True, Reason=Ready` with
   `observedGeneration` = *G*+1, the generation the publication will produce. This is the same
   predicted-generation step as `reconcileWorkspaceAdmission` (`:153-163`), one generation
   sooner.
4. **Publish and admit together.** The publication's single optimistic spec patch also sets
   `spec.coreAdmission.admittedGeneration` = *G*+1.

**Who writes.** `RuntimePoolReconciler` becomes a third writer of `coreAdmission`, beside
`ExecutionWorkspaceReconciler` and ADR 0060's attach path. It runs under the same manager
identity and leader election, with the shared helper (open question 2).

**Safety.** The rules are the reconciler's own:

- **A lost spec write admits nothing.** The condition then names a generation that no
  `coreAdmission` matches, so `workspaceCurrentlyAdmittedByCore` stays false, and the reconciler
  re-admits as today.
- **The reconciler stands down.** It sees *G*+1 already admitted and takes its condition-only
  branch (`:127-142`).
- **The provider starts one round trip sooner.** It observes *G*+1 already admitted. Nothing
  about what the provider checks changes.

This removes hops 3 and 4. It changes no provider and no contract.

### 2. (A2) Queue on Core's resume fence; the acknowledgement moves to first contact

With `fence` or above, `queueACPRuntimeTask` does not wait for `attachedEpoch`,
`observedGeneration` or `Attached` for a Task whose attachment was installed by a resume
publication. Every input to the **resume fence** is read uncached, in the pass the publication
wakes. The Task wake watch gains one trigger: a workspace generation change where
`spec.attachment.taskRef.uid` is this Task.

**Attachment (Core-owned).** These are ADR 0060 §2's checks:

- `spec.attachment.taskRef.uid` is this Task, and `spec.attachment.epoch` =
  `spec.attachmentEpoch` = the Task's staged epoch;
- the attachment has not expired;
- the attachment Lease is held by this Task, unexpired, and fenced to that epoch;
- the bearer Secret's owner, epoch and digest match;
- no revocation stamp stands;
- the workspace is not tainted (on the workspace or any linked Task), quarantined, or `Failed`;
- `maxLifetime` has time remaining.

**Admission and request (Core-owned):**

- the workspace is currently admitted by core at the current generation (section 1);
- desired state is `Ready`, `spec.retirement` is nil, the workspace is not deleting, and it
  carries no `warm-window-drained-at` and no `suspend-pending`;
- `spec.workload` passes `ValidateWorkspaceWorkload`, and its `PoolBinding` is the frozen pool:
  name, UID, generation and profile hash;
- the frozen controller epoch in its runtime template equals the current fence
  (`runtimePoolPodTemplateValidationTarget`);
- its sequence and revision are recorded as the fence's **target request**.

**Provider evidence: absence of refusal only.** No `Ready` allocation exists yet. The fence
requires:

- no `Admitted=False` condition;
- no `Failed` allocation for the target request;
- `status.attachedEpoch` ≤ this epoch;
- a provider object with the bound UID, not `Disabled`, that serves the binding
  (`externalProviderServesBinding`).

**Pool:**

- the frozen name and UID, the workspace link, not deleting, `desiredReplicas` ≥ 1;
- lifecycle `Starting` or `Serving`;
- no `external-runtime-retirement-requested` and no `external-runtime-eviction` annotation.

If the fence holds, the Task runs pool demand without the handshake, `CreatePromptAttempt`, and
the `Queued` patch. It marks `workspaceResumeFenced` in place of `workspaceReady` and
`runtimePoolReady`; those two are still marked, off the critical path, when they are observed.

**When the fence fails:**

- an item that may still change (a provider item, or a pool still `Starting`) leaves the
  ordinary wait in force;
- any other item fails exactly as today's handshake does. That includes
  `deleteExactACPRuntimePool` for a workspace that is not ready after materialization
  (`acp_task_queue.go:1812-1822`).

**The contact gate.** The unchanged `acpWorkspacePoolReadinessFailure` (the full ADR 0023 step 7
handshake) runs again immediately before first runtime contact. It runs inside the bind step of
section 3 (or, with `fence` only, before `CreateRuntimeSession`), together with the pool items
of section 3. Its outcomes:

- if it fails as recovery-pending, the Task waits;
- if it fails in any other way, the Task requeues before submission with no runtime mutation
  (`workspace_binding_changed`). The TaskReconciler's next pass then runs today's handshake and
  its abort path.

### 3. (B1) A pending reservation while the pool is Starting

#### Types

In `api/v1alpha1/runtime_pool_types.go`:

```go
type RuntimePoolCapacityReservationStatus struct {
    // ... existing fields ...

    // RuntimeInstanceID binds admission to the exact selected Pod/boot pair.
    // Empty only while Pending is set.
    // +optional
    RuntimeInstanceID string `json:"runtimeInstanceID,omitempty"`

    // Pending holds the slot for one published request on a Starting
    // workspace pool. It authorizes no runtime contact.
    // +optional
    Pending *RuntimePoolPendingReservation `json:"pending,omitempty"`
}

type RuntimePoolPendingReservation struct {
    WorkspaceUID    string      `json:"workspaceUID"`
    RequestSequence int64       `json:"requestSequence"`
    RequestRevision string      `json:"requestRevision"`
    AttachmentEpoch int64       `json:"attachmentEpoch"`
    // PendingUntil is fixed at claim and never extended.
    PendingUntil    metav1.Time `json:"pendingUntil"`
}

type RuntimePoolStatus struct {
    // ... existing fields ...

    // StartingRequest names the published workspace request this pool is
    // starting. The RuntimePool reconciler writes it with every Starting
    // progress status of an external workspace pool and clears it otherwise.
    // +optional
    StartingRequest *RuntimePoolStartingRequest `json:"startingRequest,omitempty"`
}

type RuntimePoolStartingRequest struct {
    WorkspaceUID    string `json:"workspaceUID"`
    Sequence        int64  `json:"sequence"`
    Revision        string `json:"revision"`
    ControllerEpoch int64  `json:"controllerEpoch"`
}
```

A CEL rule enforces that exactly one of `runtimeInstanceID` and `pending` is set. Both are
`make manifests generate` outputs; no generated file is hand-edited.

#### What a pending reservation holds

It holds:

- the pool's **one resident slot and one prompt slot**, for one Task attempt and one controller
  epoch (as every reservation does);
- one published request (`WorkspaceUID`, `RequestSequence`, `RequestRevision`);
- one attachment epoch.

It holds no runtime instance, no Session lease, no SessionTurn, and no credential. It is the
right to be first when that exact request serves, and nothing else.

#### Who may claim one

`reserveTask` claims a pending reservation instead of failing with
`errACPRuntimePoolNotAdmitting` only when all of these hold:

- the flag is at least `prereserve`;
- the Task was queued through the resume fence (section 2);
- `reserveTask` re-ran the resume fence, uncached, in this pass. This makes the Task the
  workspace's exclusive attachment holder at the recorded epoch;
- the pool is an `acp-ws-*` workspace pool in lifecycle `Starting`;
- `status.startingRequest` equals the fence's target request;
- `status.startingRequest.controllerEpoch` equals the current fence;
- no retirement or eviction annotation is set, and the pool is not deleting;
- no other reservation of any kind exists on the pool. Its single slot is free.

The claim is the same resourceVersion-preconditioned pool status `Update` as today
(`claimRuntimePoolReservation`). Every other step of `reserveTask` then runs unchanged before
the PromptAttempt moves Queued→Reserved:

- settle-before-admission;
- the credential freeze (`freezeWorkspaceCredentialVersions`);
- the target refresh;
- the fenced Queued→Reserved transition;
- the Reserved Task patch.

The PromptAttempt's `Reserved` state already carries no instance binding (`acp_dispatcher.go`,
`preparePromptAttemptReservation`, a nil binding). The control store therefore needs no change
for B1.

#### Capacity and max-resident-sessions accounting

- **It counts like any reservation.** A pending reservation counts in
  `runtimePoolReservedSlots`, `ReservedSessions` and `ReservedPrompts` exactly as a bound one
  does. A Starting pool reports no probed pressure (`ResidentSessions` = `RunningPrompts` = 0)
  and no probed limits, so `effectiveRuntimePoolCapacityLimits` falls back to the spec, pinned at
  1/1.
- **One per pool.** The "no other reservation" rule makes that explicit. A second Task cannot
  claim against a pool that has not been probed.
- **It cannot overcommit.** At bind (below), `runtimePoolReservationAdmission` runs again with
  the probed limits (`status.capacity.max*`, the minimum of config and the supervisor's
  `Capabilities.Limits`) and the probed pressure, counting every reservation except this one. If
  the probe reports less room, the bind fails and the reservation is released. Capacity
  accounting never trusts a pre-probe number past the bind.
- **ADR 0059.** A resumed pool runs a fresh process, so no Resident record can still be claimed
  (section 8). Any pending reservation therefore holds `ResidentSlots` = 1.

#### Validity, renewal and release

`activeRuntimePoolReservations` keeps a pending reservation only while all of these hold. The
check is pure, on the pool object:

- the pool UID, the controller epoch, and `ExpiresAt` in the future, as today;
- `PendingUntil` in the future;
- **either** the pool is `Starting` and `status.startingRequest` equals the pending target,
  with the same controller epoch,
- **or** the pool is `Serving`, and its `orka.ai/external-runtime-served` record names the
  pending target's sequence and revision. That is the window between `Serving` and the bind.

Anything else voids it. That covers every way the pool fails to start its request:

- it never reaches `Serving` and `PendingUntil` passes;
- it goes `Degraded`, `Ambiguous`, `Draining`, `Stopping` or `Stopped`;
- it is replaced (a new UID), or its request is retired or republished (a new sequence or
  revision);
- the epoch changes.

A void reservation is dropped by the next pool status writer. Two rules make sure a writer drops
it:

- **The pool reconciler prunes it.** It drops void pending reservations in any status write
  that leaves `Starting` for anything other than `Serving` on the same request.
- **Quiescence ignores it.** `runtimePoolRolloutControllerWorkIsQuiescent` and
  `runtimePoolControllerWorkIsQuiescent` (`runtime_pool_controller.go:1586-1589`) count only
  reservations that are valid and bound. A pending reservation authorizes no contact, so it can
  never hold up drain, scale-down, or finalization. "Drain and finalization complete before
  replacement" is unchanged, because there is nothing to drain.

**Renewal.** The dispatcher's reservation lease renews `ExpiresAt` with the usual TTL, but never
past `PendingUntil`.

- `PendingUntil` = claim time + `--acp-resume-prereserve-timeout`, default 2 m. That is ADR
  0057's speculative-start bound, and the fixed `PendingUntil` cap is never extended.
- When a reservation is voided or expires, the PromptAttempt stays `Reserved`.
- Today's dispatcher already treats a `Reserved` Task whose pool reservation is gone as one to
  reclaim (`claimRuntimePoolReservation` matches by Task, attempt and epoch). The Task waits with
  `WaitingForCapacity` and a bounded message, consumes no retry, and makes no runtime mutation.
- A Task cancelled while pending settles through `settleTaskBeforeRuntimeAdmission`, as any
  `Reserved` Task does, and releases its reservation.

#### The bind: where Serving + Accepting is enforced

A pool that moves to `Serving` wakes the dispatcher. The wake is **targeted**: it reads the
pool's own pending reservation and handles that Task directly, with no 200 ms gap and no full
scan.

The new step `bindPendingReservation` runs before `executeReservedTask`. Every check reads
uncached, and all must hold:

1. **Gate and fence.** The admission gate is open; `CurrentFence` equals the reservation's
   epoch.
2. **Pool.**
   - same UID; `Serving` + `Accepting`;
   - `activeInstance` present, with `activeInstance.controllerEpoch` = the fence;
   - the served record names the pending target's sequence and revision, and the workspace UID;
   - no retirement or eviction annotation; not deleting.
3. **Workspace.**
   - the contact gate of section 2: the full `acpWorkspacePoolReadinessFailure` handshake;
   - `spec.workload` sequence and revision still equal the pending target;
   - the attachment epoch still equals `Pending.AttachmentEpoch`;
   - no taint, `warm-window-drained-at`, `suspend-pending`, or retirement.
4. **Lease.** The attachment Lease is still held by this Task and fenced to that epoch.
5. **CAS.** One pool status `Update`, with a resourceVersion precondition:
   - it runs `runtimePoolReservationAdmission` on the latest pool. That is the exact
     `Serving`+`Accepting`, active-instance, epoch and capacity check of today;
   - then it replaces `Pending` with `RuntimeInstanceID` = `activeInstance.RuntimeInstanceID`.

After the bind, the Task is an ordinary reserved Task on an exact instance:

- `executeReservedTask` runs unchanged, with every exact-instance, boot and RuntimeSession
  create fence;
- the Reserved→SessionStarting transition records the instance;
- `setSlots` runs as today.

**When a bind check fails:**

- a recovery-pending or `Starting`-shaped failure waits until `PendingUntil`;
- a terminal failure releases the reservation and requeues before submission
  (`workspace_binding_changed`).

### 4. (B2) The durable session prelude before Serving (optional stage)

With `prereserve-session`, the steps that depend only on the Session and the controller epoch
run after the pending claim, while the pool is still `Starting`:

- lineage and MCP digests;
- the namespace read;
- the ADR 0059 claim attempt;
- the session read;
- the Session mutation lease;
- `OpenTurn`.

This needs one state-machine change:

- **Reserved→SessionStarting without an instance.** It records `SessionUID` and the lease
  generation, with `RuntimeInstanceID` empty.
- **A new fenced same-state transition, `SessionStarting` + `bind-instance`.** It records
  `RuntimeInstanceID` exactly once, after section 3's bind and before `setSlots`.

The transition table in `internal/store/control_types.go`, the kube and sqlite stores, and every
recovery path that reads `attempt.RuntimeInstanceID` must treat an empty instance in
`SessionStarting` as "not yet bound". That means it is cleanup-trivial and a pre-submission
requeue. Those recovery paths include:

- `reconcileUnfinalizedTaskSession`;
- `quiesceInterruptedTaskSessionPreparation`;
- `settleFrozenWorkspaceCredentialBlocked`;
- `prepareTaskSession`'s instance check.

**What still needs the instance**, and stays after the bind:

- the SessionStarting instance bind;
- `setSlots`;
- runtime-session status reads;
- `Capabilities` and the profile-digest check;
- the Planned transition;
- `CreateRuntimeSession`.

**If the pool never serves after `OpenTurn`**, the turn is abandoned through today's
pre-submission path (`handleSessionTurnOpenFailure` / `requeuePreSubmissionTask`). The user
prompt appended by `OpenTurn` is not replayed twice, because the turn reopens under the same
PromptAttempt and request digest.

The saving is ~0.3–0.45 s. The cost is a store state-machine change that every recovery path
must honor. The recommendation is to land A1, A2 and B1, measure, and decide B2 separately (open
question 5).

### 5. Revocation by generation

A Task queued through the resume fence may settle, for example when cancelled while pending,
before the provider ever acknowledged its epoch. With `fence` or above, `FinalizeRevocation`
therefore also requires:

- `status.observedGeneration` ≥ the generation of the `BeginRevocation` write;
- the epoch not reported as attached.

This is ADR 0060 §3. ADR 0023's revocation step 2 stays literal: it means an observation *after*
the revocation.

### 6. The admission rule, amended

When B1 is implemented, the AGENTS.md sentence "Only `Serving` + `Accepting` admits new
RuntimeSessions" becomes:

> Only `Serving` + `Accepting` binds a RuntimePool capacity reservation to a runtime instance,
> and no RuntimeSession is created and no prompt is sent without a reservation bound to the
> exact active instance. Under `--acp-resume-admission=prereserve`, a single-session `acp-ws-*`
> pool that is `Starting` its current published request may hold one instance-unbound pending
> reservation for the Task that holds the workspace attachment. It authorizes no runtime
> contact, never blocks drain or finalization, and is void as soon as the pool leaves
> `Starting` for anything other than `Serving` on that same request.

ADR 0024's ownership state machine is unchanged. `Starting → Serving` still requires one Ready
instance and a passing authenticated fence probe.

### 7. Safety argument

Each row names a wait that this ADR removes or moves, and the fence that takes its place. Every
fence is read uncached and fails toward "no contact".

| Threat | Removed or moved wait | Replacing fence | Fails closed because |
| --- | --- | --- | --- |
| **Stale attachment** (an older epoch, another Task, expired) | The queue-time acknowledgement (hops 7 and 9) | Resume fence: the attachment is this Task at the staged epoch, the Lease is held and fenced, the Secret digest matches, no revocation stamp. Checked again at reserve, at bind, and (contact gate) with the full ADR 0023 step 7 handshake | Every field is Core-written under optimistic locks. A rival attach needs no current attachment, so it cannot land. A revocation changes the spec and the Lease, and the bind sees it. Supervisor prompt fences (Task, attempt, prompt, lease generation) reject a settled Task, as today. |
| **Replaced or unverified instance** | Reservation gated on `Serving` (hop 12) | The bind: pool `Serving`+`Accepting`, `activeInstance` at the current epoch, a served record that names the pending request, then `runtimePoolReservationAdmission` in the same CAS that writes `RuntimeInstanceID` | A pending reservation names no instance, so nothing can contact one. The instance is recorded only from the attested, probed `activeInstance`, under a resourceVersion precondition. A supervisor restart after the bind is caught by `CreateRuntimeSession`'s exact-instance and boot fences (ADR 0024). |
| **Revoked, retired or deleted workspace** | Queue-time handshake; re-admission hops 3 and 4 | Resume fence + bind: desired `Ready`, no `spec.retirement`, not deleting or `Failed`, workload sequence and revision equal the pending target, currently admitted by core. Inline admission runs core's own `validateWorkspaceBindings` | Retirement, suspension, deletion and republication all change the spec. They must win an optimistic write against the publication, so they land either before it (the inline step or the fence refuses) or after it (the bind sees a different request and voids the reservation). A lost inline spec write leaves an unmatched condition, which admits nothing. |
| **ADR 0056 shared-worker strong retirement and eviction** | none removed: the pool's evidence wait (hop 8) is unchanged | The bind requires `Serving`+`Accepting`, no `external-runtime-eviction`, and a served record. Pending validity requires `Starting` or `Serving` on the target | The strong path closes the provider pool and drains co-hosted Actors through Core's eviction path (ADR 0046). That sets the eviction annotation and moves the pool out of `Starting`/`Serving`, which voids the pending reservation. Interim termination evidence is untouched, because it governs *suspension*, which this ADR does not change. |
| **Draining pool** | – | Pending validity excludes `Draining`; the bind requires `Accepting` | A pending reservation never counts toward quiescence, so it cannot block a drain. A bound reservation is today's case. |
| **Superseded controller epoch (ADR 0058 takeover)** | – | The reservation's `ControllerEpoch` = `startingRequest.controllerEpoch` = the current fence. Control-store writes (`CreatePromptAttempt`, Queued→Reserved, B2's lease and `OpenTurn`) are epoch-fenced under the interlock | A successor's epoch voids every pending reservation of the old epoch (`activeRuntimePoolReservations` filters by epoch). ADR 0057 retires a request whose frozen template epoch differs (`runtime_pool_external_workspace.go:163-176`), so `startingRequest` changes. The old leader's bind fails at `AdmissionGate`/`CurrentFence`, and its control-store writes fail at the interlock. |
| **Controller restart** | – | Every input is durable: workspace spec and status, Lease, Secret, pool status (`startingRequest`, reservations), pool annotations | There is no in-memory authority. A crash between the inline condition and the spec write is today's lost write. A crash with a pending reservation leaves it valid, or void by rule. The restarted dispatcher reclaims or binds it from durable state. |
| **Provider disabled, drifted, or lacking a feature** | Hops 3 and 4 | Inline `validateWorkspaceBindings`; the resume fence's provider items; the publication's existing provider checks (`:358-370`) | These are the same checks as core admission. Drift after the publication closes the pool on its own pass (`Degraded`), which voids the reservation. |
| **Capacity overcommit** | – | One reservation per pool before the probe; probed limits re-checked at the bind | Covered in section 3, capacity accounting. |

No removed wait was the only thing standing between a prompt and an unsafe runtime:

- the acknowledgement still holds before contact (section 2);
- the instance is still bound only on `Serving`+`Accepting` (section 3);
- admission is still Core's own validation (section 1).

### 8. Interactions

**ADR 0059 persistent sessions.**

- **Claimed.** A record's `expiresAt` is `keptAt` + the frozen `suspend-after-detach`, never
  extended, and a resume follows a suspension that runs at or after the window. So on the
  resume path the claim always misses (`record-expired`, as seen in r9c). The plan creates a
  fresh session above *g*. Under B2, the early lease CAS moves any `Resident` record to
  `Retiring` exactly as an ordinary acquisition does.
- **Never on this path.** ADR 0059's `residentSessionGate` and its slot credit never run here.
  A Task whose record is still claimable is a warm-window attach, not a resume publication, and
  is out of scope.
- **Kept.** A resumed turn that later keeps its session writes its record from settlement as
  today. Its `runtimeInstanceID` is the instance bound in section 3, because the
  RuntimeSession was created under that bound reservation.

**Spare and shared workers.** The spare claim (`P:spare_workers.go`), the shared ledger
(`P:shared_workers.go`) and private worker birth all happen inside hop 6. Core sees the same
published request, the same `Ready` allocation and startup evidence, and the same attestation:

- **Spare.** No change. A spare resume that reopens from `Degraded` (r9c open question 1) voids
  its pending reservation and reclaims after the reopen. That costs a reclaim, not safety.
- **Shared.** ADR 0056's termination evidence and strong fallback govern suspension and
  retirement, and are unchanged. Strong retirement during `Starting` voids the reservation
  (section 7).

**Suspension, eviction and window expiry racing a resume.**

- **Suspension before the publication.** The publication requires `pendingResume`, and a direct
  attach requires desired `Ready`. A suspension that wins first makes the publication refuse,
  and nothing was queued.
- **After the publication.** The workspace is attached, and retention cannot suspend an attached
  workspace.
- **ADR 0055 window expiry.** This applies to the *previous* window only, and ADR 0057's
  live-demand grace already orders it against the resume request.
- **Eviction or upgrade drain while `Starting`** (ADR 0046). The pool leaves `Starting`, the
  pending reservation is void, and the Task requeues pre-submission. The eviction's
  suspend-on-drain runs as today, and the Task resumes again through the ordinary or the fenced
  path.
- **Operator taint or quarantine while `Starting`** (ADR 0044). The bind's taint check fails
  terminally; the reservation is released and the handshake's abort path runs.
- **The pool never reaches `Serving`** (provider down, restore failure). `PendingUntil` passes,
  the reservation is void, and the Task waits as `WaitingForCapacity`. The pool's own
  `Degraded` handling and the speculative-start deadline apply unchanged.

### 9. Observability

- **Pre-dispatch milestones:** `resumeInlineAdmitted`, `workspaceResumeFenced`, `preReserved`,
  `reservationBound`.
- **A log line, `ACP resume admission`,** with:
  - `stage=readmit|fence|reserve|bind|contact`;
  - `result=hit|miss|fallback|void`;
  - a bounded `reason`;
  - `pendingMs`, from claim to bind.
- **Metrics,** with no names or UIDs:
  - `orka_acp_resume_admission_total{stage,result,reason}`;
  - `orka_runtime_pool_pending_reservations` (a gauge).

## Consequences

### Estimated saving per hop

Saving per hop, in ms. Baselines are the round 11 medians above. Estimates are not measured.

| Hop | Today, private / shared | A1 | + A2 | + B1 | + B2 |
| --- | --- | --- | --- | --- | --- |
| 3–4 re-admission (publication → provider start) | ~400–1100 / ~600–1200 (`publishToProviderStart` + ~0.5 s truncation) | −300 to −600, net of the +~100 inline condition write | same | same | same |
| 7, 9–10 queue behind the acknowledgement | ends 256 / 31 after `admitted` | – | −30 to −260 (the queue now lands at ~published + 0.5 s, off the critical path) | (included) | – |
| 11–12 wake, scan, reserve | 991 / 729 (`admitted → reserved`) | – | – | → ~200–300 (targeted wake + bind); **−450 to −750** | – |
| 13 runtime client | 275 / 207 | – | – | – | – |
| 14 session plan | 921 / 717 | – | – | – | −300 to −450 (durable prelude off the critical path) |
| 15 create → receipt | unchanged | – | – | – | – |
| **Receipt, private (7764)** | | ~7.2–7.5 s | ~7.0–7.4 s | **~6.5–6.8 s** | ~6.0–6.5 s |
| **Receipt, shared (7713)** | | ~7.1–7.4 s | ~7.1–7.4 s | **~6.6–6.9 s** | ~6.2–6.6 s |

Notes on the estimates:

- **The early chain finishes with slack.** With B1, the Task's chain (fence, `CreatePromptAttempt`
  with the interlock, `Queued` patch, pending claim, credential freeze, Reserved) completes
  around 2.2–2.8 s. The pool serves around 4.3–4.9 s, so there is 1.5–2 s of slack. B2 adds about
  0.45 s to that chain and still finishes before `Serving`.
- **The rest is additive.** These savings stack with the no-ADR items of
  `resume-attribution.md` §5, which Core does not change here:
  - rotate the auth Secret at the proven stop (−0.3 to −0.5 s on private);
  - remove plan and create redundancy (−0.15 to −0.3 s);
  - provider watches instead of polls.

  Together they take private resume to about 5.5–6.0 s.
- **kagent's 2.8 s stays out of reach.** Core still requires credential bootstrap and probe
  (~0.4 s), and an instance-bound plan and create (~0.5–0.7 s).

### Other consequences

- **The API changes.** The reservation's `runtimeInstanceID` becomes optional, and `pending` and
  `status.startingRequest` are new. An older dispatcher's `activeRuntimePoolReservations` drops
  every reservation whose `runtimeInstanceID` differs from the active instance, so it prunes
  pending ones safely on downgrade. The CRD must stay at the new version on rollback so those
  objects still validate until pruned (Rollout).
- **Task `Reserved` no longer implies a serving instance.** It is client-visible, and the
  harness's `clientState.ReservedMs` moves earlier. The Task message says it is waiting for the
  runtime (open question 4).
- **Core admission gets a third writer**, through one shared helper.
- **More contract surface.** The resume fence, the pending-validity predicate and the bind must
  stay consistent with ADR 0057's serving rule and ADR 0059's gate. All of them use shared
  predicates (`acpWorkspacePoolReadinessFailure`, `externalRuntimeServedRecord`,
  `runtimePoolReservationAdmission`).
- **Work, not only waiting, is removed.** That is two workspace writes, three binding
  validations, a wasted Task pass, a full dispatcher scan, and the 200 ms gap per resume.
- **Settlement may wait slightly longer** for a Task cancelled while pending (section 5).

## Rejected alternatives

- **Replace the acknowledgement, as ADR 0060 §2 does.** On resume, the acknowledgement always
  precedes `Serving`. Moving it to the contact gate keeps ADR 0023 step 7 literal, at no latency
  cost, and needs no `AttachmentAdvisory` provider feature.
- **Open admission early** (treat `Starting` as `Accepting`), **or create the RuntimeSession
  speculatively before the probe.** Either would let a session or prompt target an instance that
  is unattested, unbootstrapped and unprobed. That breaks ADR 0024's exact-instance contract and
  ADR 0049's credential-bootstrap ordering.
- **Predict the `RuntimeInstanceID` at reservation.** It derives from the Pod UID and boot ID,
  which exist only after attestation and probe. A predicted ID would name an unverified instance.
- **Let the pool reconciler write the reservation in its `Serving` status write.** That would
  save one more write (~100 ms), but the pool controller would become a Task-reservation writer
  and Task state would couple into pool reconcile. It is possible later; the gain is too small
  for v1.
- **Make the provider ignore `admittedGeneration` lag on a resume publication.** That would turn
  provider code into an admission authority, and every provider would need an orka-workspace
  contract change. Inline admission keeps Core as the authority.
- **Only speed up `reserveTask`** (targeted wake, fewer reads). This is complementary and needs
  no ADR, for −0.2 to −0.3 s. But the reserve still runs serially after `Serving`, so it leaves
  ~0.5–0.7 s on the path that B1 removes.
- **Queue early without B** (A2 alone). Measured headroom is ≤0.26 s in round 11. It is kept
  only as B1's prerequisite.

## Rollout

- **The flag.** `--acp-resume-admission=off|readmit|fence|prereserve|prereserve-session`, default
  **`off`**. Each value includes the ones before it. It requires
  `--acp-workspace-dispatch-enabled` and `--enable-workspace-provider-api`.
  - `readmit`: section 1.
  - `fence`: sections 2 and 5.
  - `prereserve`: section 3, the targeted bind wake, and section 6.
  - `prereserve-session`: section 4.
- **Lowering the flag.**
  - It takes effect on the next resume publication.
  - Tasks already queued through the fence keep their contact gate.
  - Pending reservations either bind or go void by rule. Only `prereserve` and above *create*
    them; every value still *honours* them, so lowering the flag strands none.
- **Stages.**
  1. `readmit`. It has no contract change, so it is a candidate for default-on after the
     harness.
  2. `fence` + `prereserve` together. A2 alone has no measurable value.
  3. `prereserve-session`, only if open question 5 is accepted.
- **CRD rollout.** The CRD change ships first, in a release where the dispatcher already prunes
  `pending` reservations while the flag is still off. Rollback keeps the CRD in place.
- **Security review.** Stages 2 and 3 share ADR 0057's pending security review (open
  question 8).

## Test plan

- **Unit**
  - **Inline admission equals the reconciler.** A table over workspace states compares the
    inline decision with `reconcileWorkspaceAdmission`:
    - a resume publication;
    - a first admission (refused inline);
    - maintenance intent;
    - `Admitted=False`, retryable and not retryable;
    - class or provider drift;
    - a lost optimistic lock after the condition write, which must leave the workspace not
      currently admitted.
  - **The resume-fence predicate.** One case per field, each flipping the result. That covers
    every attachment, Lease, Secret, revocation, desired-state, retirement, deletion, taint,
    quarantine and `maxLifetime` item; the workload sequence, revision, pool binding and frozen
    epoch; `Admitted=False`; a `Failed` allocation; `attachedEpoch` above this epoch; and the
    pool lifecycle and annotations.
  - **Pending validity** (`activeRuntimePoolReservations`). A table over lifecycle × whether
    `startingRequest` matches × whether the served record matches × epoch × `PendingUntil` ×
    pool UID. Includes: a pending reservation is kept on `Serving` only when the served record
    names its request.
  - **Claim and bind admission.**
    - A second claim on a pool that already holds any reservation is refused.
    - A bind with probed limits of 0 is refused.
    - A bind against an `activeInstance` of another epoch is refused.
    - The CEL rule requires exactly one of `runtimeInstanceID` and `pending`.
  - **Quiescence.** `runtimePool*ControllerWorkIsQuiescent` ignores pending reservations and
    still counts bound ones.
  - **Revocation by generation.** `FinalizeRevocation` refuses while `observedGeneration` is
    below the revocation's generation.
  - **B2.** The store transition table, and every recovery function, given `SessionStarting`
    with an empty instance.
- **Envtest** (orka-workspace fake provider, fake supervisor)
  - **Fast path.** `coreAdmission` lands in the publication write, and the reconciler makes no
    admission write for *G*+1. The PromptAttempt persists before the provider's first status
    write. A pending reservation exists while `Starting`. `RuntimeInstanceID` appears only in
    the bind CAS, after `Serving`. `CreateRuntimeSession` runs only after the contact gate.
  - **The pool never serves.**
    - The provider is paused, so `PendingUntil` passes: the reservation goes void, the Task is
      `WaitingForCapacity`, and nothing touches the runtime.
    - The provider goes `Degraded`, or the pool reports `Ambiguous`: void, and the pool
      reconciler prunes it.
    - The pool is scaled to zero or deleted: drain is not blocked.
  - **Races before the bind.** Each of these voids or refuses with no runtime mutation:
    - an eviction annotation;
    - an ADR 0056 strong retirement;
    - operator taint;
    - a republication with a new sequence;
    - a retirement request;
    - a revocation (Task cancelled while pending);
    - a supervisor restart between probe and bind, caught by the create fences.
  - **ADR 0058 takeover** between the publication and the queue, between the queue and the
    claim, between the claim and `Serving`, and between `Serving` and the bind:
    - the old epoch's pending reservation is void;
    - the old leader's bind fails;
    - the successor retires the old-epoch request and reclaims.
  - **Controller restart** at each of the same points, and between the inline condition write
    and the spec write.
  - **ADR 0059 on.** The resume claim misses, and the plan recreates. A later kept session
    records the bound instance.
  - **Flag `off`.** The same write sequence as today (golden).
- **Live kind latency harness** (orka-workspace `hack/external-substrate-e2e/core/latency.go`,
  Substrate on gVisor, Vekil, aimock)
  - **Configurations.** Round 12 interleaved with kagent:
    - `warmdefault` resume after expiry (spare and fresh worker);
    - `shareddefault` resume;
    - `spare` and detach resume;
    - each with the flag `off`, `readmit`, `prereserve` and `prereserve-session`, and with ADR
      0059 on.
  - **Measurements.** Median and p90 of `published → providerStart`,
    `published → workspaceResumeFenced`, `preReserved`, `admitted → reservationBound`,
    `sessionPlan`, and agent receipt, plus `pendingMs`.
  - **Checks.**
    - zero `readmission` lines from `ExecutionWorkspaceReconciler` on resume;
    - `preReserved` before `admitted` in every iteration;
    - `RuntimeInstanceID` recorded only after `Serving`;
    - no `orka_acp_resume_admission_total{result="void"}` in clean runs.
  - **Adversarial cases.**
    - the provider Deployment scaled to 0 during `Starting`;
    - an eviction drain during `Starting`;
    - a shared-worker strong retirement during `Starting`;
    - operator exec taint during `Starting`;
    - controller SIGSTOP and takeover during `Starting`, using ADR 0058's negative-control
      recipe;
    - pool replacement.

    Run the AGENTS.md ACP real-world validation list once under `prereserve`.

## Open questions for the maintainer

1. **The ADR 0023 step 7 relocation.** Do you accept moving the acknowledgement from the queue
   gate to the contact gate on resume, as ADR 0059 did for reuse, instead of ADR 0060 §2's
   replacement?
2. **The inline admission writer.** May `RuntimePoolReconciler` write `coreAdmission` in the
   publication, through the shared `validateWorkspaceBindings` helper? Or must admission stay
   inside `ExecutionWorkspaceReconciler`, for example through a synchronous call?
3. **API change.** Is it acceptable in `v1alpha1` to make the reservation's
   `runtimeInstanceID` optional and to add `pending` and `status.startingRequest`? Or should the
   pending reservation live in a separate status list, which avoids loosening an existing
   required field but duplicates the accounting?
4. **Task `Reserved` semantics.** Should the Task show `Reserved` before an instance exists
   (client-visible)? Or should it stay `Queued`, with a reason, until the bind? Staying `Queued`
   puts one Task patch (~100 ms) back on the path.
5. **B2.** Is ~0.3–0.45 s worth the `SessionStarting`-without-instance state-machine change and
   its recovery-path audit? The recommendation is to defer it until B1 is measured.
6. **First turns.** Should the same three steps apply to a first turn's publication (ADR 0057
   "the publication carries the attachment")? The shape is identical, but the first admission
   acquires pool capacity, so inline admission would need that critical section.
7. **Revocation by generation.** Apply section 5 always, as a tightening, rather than only under
   the flag? This is shared with ADR 0060 open question 5.
8. **Review.** Should stages 2 and 3 share ADR 0057's pending security review, or have their own?
9. **`--acp-resume-prereserve-timeout`.** Is a default of 2 m (ADR 0057's speculative-start
   bound) right, or should it follow the pool's startup timeout?
