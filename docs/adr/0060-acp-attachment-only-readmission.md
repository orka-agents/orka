# 60. Admit an attachment-only change to an ACP execution workspace in Core, without the re-admission round trips

Date: 2026-11-20

## Status

Proposed. This is a design only; nothing here is implemented. It would amend:

- **ADR 0023, step 7** ("starts the worker only after the workspace status reports the same
  enforced epoch"). For an attachment-only change to a class-backed ACP execution workspace,
  Core's own attachment fence would replace the provider's acknowledgement as the condition for
  queueing a prompt. The acknowledgement would still be required before revocation is finalized.
- **ADR 0021/0022's core admission**, on the attach path only. The write that attaches would also
  carry core admission for the new generation, instead of leaving it to two later
  `ExecutionWorkspaceReconciler` writes.
- **ADR 0057's queue handshake** (`verifyACPWorkspaceReadyForPool`), on the attachment-only path
  only.

It changes none of these:

- the Lease, epoch, and owner-referenced bearer Secret of ADR 0023 steps 1–5, and its revocation
  ordering;
- ADR 0026's rule that the attachment bearer never reaches the runtime;
- ADR 0030, 0031, 0041, 0049, and 0056's evidence rules;
- ADR 0057's serving rule, which this depends on;
- ADR 0058's interlock;
- RuntimePool admission. Only `Serving` and `Accepting` admits a new RuntimeSession.

The headline finding is in **Context**. Since ADR 0057, the RuntimePool no longer closes or
re-probes on an attachment-only change. The ~1.9 s between `workspaceAttachRequested` and
`runtimePoolReady` comes from the workspace's two Core re-admission writes and the provider's two
status writes. And when ADR 0059 reuses a session, that chain is already off the critical path.

## Context

### Round 8, warm follow-up, from Task creation

| Step | ms | Δ |
| --- | --- | --- |
| executionBound | 1369 | |
| workspaceAttachRequested | 2023 | 654 |
| workspaceReady | 3030 | 1007 |
| runtimePoolReady | 3960 | 930 |
| promptAttemptPersisted | 4335 | 375 |
| reserved | 4937 | 602 |

ADR 0059 estimates its reuse path at 5.3–5.9 s. kagent takes 4.5 s.

### Hop attribution, from `workspaceAttachRequested` to `reserved`

The workspace is at generation *G*. It is Ready, unattached, admitted at *G*, and the provider has
observed *G*. The pool is `Serving` and `Accepting` (ADR 0055 warm window).

| # | Hop | Code | Kind |
| --- | --- | --- | --- |
| 0 | The Task's attach write sets `spec.attachment` and `spec.attachmentEpoch` (*G*→*G*+1). Then `markACPTaskAttachmentEpoch` patches the Task. | `acp_workspace_lifecycle.go:416-445`, `execution_workspace_attachment.go:187` | attach (before the mark) |
| 1 | `ExecutionWorkspaceReconciler` wakes on *G*+1. *G*+1 is not admitted (`AdmittedGeneration` is still *G*). `validateWorkspaceBindings` makes uncached class and provider reads, then writes **status**: `Admitted=True` with `observedGeneration` *G*+2 (a predicted generation). It then reads the owner projection. | `execution_workspace_controller.go:113-171`, `:278-316` | **core re-admission, write 1** |
| 2 | The same reconciler wakes on its own status event. It validates twice more (`:173`, then inside `patchWorkspaceCoreAdmission` at `:574`) and writes **spec** `coreAdmission.admittedGeneration` = *G*+2. The API server bumps the generation to *G*+2. | `execution_workspace_controller.go:147-191`, `:549-597` | **core re-admission, write 2** |
| 3 | The provider wakes on *G*+2 and sees it currently admitted. Its first status write echoes `spec.attachment.epoch` into `status.attachedEpoch`, with `state=Attached` and `Attached=True`@*G*+2. `status.observedGeneration` stays *G*. | orka-workspace `providers/substrate/reconcile.go:53-70`, `sdk/lifecycle.go:71` | **attach acknowledgement, write 1** |
| 4 | The Task wakes on the `attachedEpoch` change (`task_acp_wake_watches.go:239-250`). `ensureACPClassWorkspace` requires current core admission and `attachedEpoch` = epoch, and **marks `workspaceReady`**. | `acp_workspace_lifecycle.go:337-376`, `:643`, `acp_task_queue.go:149` | Task pass |
| 5 | In the same pass, `ensureACPRuntimePoolDemand` calls `verifyACPWorkspaceReadyForPool`, which reads the workspace uncached. `status.observedGeneration` ≠ generation, so it returns `errACPWorkspaceRecoveryPending` and the reconcile errors. | `acp_task_queue.go:1630`, `:1770-1823`, `:1912-1915` | wasted Task pass |
| 6 | The provider continues the same pass: `Observe`, `failure`, `EnsureAllocation` on the Ready Actor, and `revokeLiveEgressCredentials`. Then it makes a **second status write** with `observedGeneration` *G*+2. | orka-workspace `providers/substrate/reconcile.go:71-246` | **attach acknowledgement, write 2** (provider re-observation) |
| 7 | The Task wakes on `observedGeneration`. It reruns the whole queue prelude (`loadVerifiedBoundExecution`, `queuedACPPromptAttempt`, delivery plan, `ensureACPClassWorkspace`). `verifyACPWorkspaceReadyForPool` passes, `recordACPRuntimePoolImageProvenance` runs, and the Task **marks `runtimePoolReady`**. | `acp_task_queue.go:61-169`, `:1726` | Task pass |
| 8 | `resolvePromptCredentialBindings`, then the fenced `CreatePromptAttempt`. | `acp_task_queue.go:203-220` | neither |
| 9 | Task metadata patch, `Queued` status patch, dispatcher queue, scan, and `reserveTask`. | `acp_task_queue.go:222-262`, `acp_dispatcher.go:292`, `:4319` | neither |

- **Hops 1–4 (1007 ms)**: Core re-admission is two writes and three binding validations (hops 1–2,
  an estimated 0.5–0.7 s at round 8's ~30 ms per GET and ~100–130 ms per write). The provider's
  echo and the Task's pass make up the rest. No milestone today separates hop 2 from hop 3.
- **Hops 5–7 (930 ms)** are "the second hop before the dispatcher sees `runtimePoolReady`". The
  provider acknowledges in two writes. Core marks `workspaceReady` on the first and can queue only
  after the second, which follows a Substrate journal read, an `EnsureAllocation`, and an egress
  verification.
- **Hops 8–9 (977 ms)** are Core's queue and reservation writes. They involve neither re-admission
  nor acknowledgement.

### What the RuntimePool does: nothing on the critical path

ADR 0057's serving rule (commit 864764bf4) holds across hops 0–6:

- `externalAttachmentOnlyLag` returns true (`runtime_pool_external_attach_serving.go:88-111`);
- the pool requeues without changing status (`runtime_pool_external_workspace.go:129`, `:232-240`);
- once the provider catches up, the ordinary serving pass re-attests, re-seeds, and re-probes
  (`:242-301`), but it never leaves `Serving` or `Accepting` while the served record matches;
- `runtimePoolReservationAdmission` (`acp_dispatcher.go:3664`) reads only the pool's lifecycle,
  admission, active instance, controller epoch, and capacity. It never reads the workspace
  generation.

So the pool contributes nothing between `workspaceAttachRequested` and `reserved`. The round 6
close → re-probe → reopen tail (7–38 s) is gone, and the remaining cost is workspace re-admission
plus the provider's acknowledgement, which gate the **Task's** queue path. The live harness must
confirm this with zero `readmission=true` lines in `ACP workspace runtime admission timings` on
warm turns (test plan).

### What the acknowledgement proves on this path

For the Substrate provider, the acknowledgement is a pure echo:

```go
current.Status.AttachedEpoch = workspaceprovider.AttachmentEpochAcknowledgement(current)
```

It returns `spec.attachment.epoch` when the epoch fields agree. Nothing is activated in a data
plane, and no workspace-agent enforces the epoch. ADR 0026 keeps the bearer out of the runtime, and
ADR 0056 notes the router no longer delivers it to the sandbox. ADR 0057's provider requirements 1
and 2 already require that an attachment never starts, restarts, replaces, or blocks an allocation.

So on this path the acknowledgement proves only that the provider observed generation *G*+2 and
still considers it admitted. Everything that keeps a prompt off a stale or foreign runtime is
Core's:

- the exclusive Lease and epoch;
- the Task, attempt, prompt, and lease fences;
- the pool's exact-instance fences;
- the RuntimeSession create fences.

ADR 0057 left the two hops in place ("Removing them would change ADR 0023's admission contract").
This ADR proposes that change.

### The ADR 0059 interaction decides the saving

ADR 0059's reuse path persists the PromptAttempt once the attachment is held. It moves the
handshake to just before first runtime contact, where attempt → plan (~2.4 s) already hides the
~1.9 s chain. **On an ADR 0059 hit, removing the hops saves about 0 at the median.** ADR 0059's
claim that the remaining ~1 s gap is "mostly the two re-admission hops" does not hold for its own
hit path. That gap is hops 8–9 plus `runtimeClient` and `sessionPlan` (about 2.4 s of serial Core
writes).

The hops are on the critical path only for **fresh-session** turns:

- ADR 0059 off, a miss, or an ineligible scope (Codex native, gateway, Agent identity, transaction
  context);
- any future version of ADR 0059 whose attempt → plan chain becomes shorter than the ack chain.

## Decision

### Scope: an attachment-only change

An **attachment-only change** is an attach, or a rotation at expiry (`ensureACPWorkspaceAttachmentFresh`), of a
class-backed ACP execution workspace for which all of these hold on the exact object version the
write is locked on:

- `--acp-workspace-attach-fast-path` allows it (rollout section);
- the linked RuntimePool is an `orka.harness.v2` workspace pool with the `ACPRuntime` feature, and
  the workspace is session-reused (`reusePolicy: session`);
- the provider declares the new `AttachmentAdvisory` feature in
  `ExecutionWorkspaceProvider.status.supportedFeatures`. This means it meets ADR 0057 provider
  requirements 1 and 2, runs no data plane that enforces the attachment epoch, and covers this in
  its conformance suite. Substrate would declare it first (open question 2);
- the workspace is currently admitted by core at *G* (`workspaceCurrentlyAdmittedByCore`), with
  `coreAdmission` non-nil. A first admission always stays with `ExecutionWorkspaceReconciler`;
- the provider has observed *G* (`status.observedGeneration == G`). The attach rule already
  requires this;
- `externalWorkspaceRuntimeSpecDigest` is equal before and after the write. The write changes only
  `attachment`, `attachmentEpoch`, and `coreAdmission`, and the function asserts that.

Revocation (`BeginRevocation`) keeps its current flow, with the generation rule in section 3.

### 1. Inline re-admission: Core admits in the attach write

`WorkspaceAttachmentManager.attach`, and the rotation that calls it, admit the new generation
themselves instead of waking `ExecutionWorkspaceReconciler`. The steps run inside the existing
optimistic retry:

1. **Validate exactly as core does.** Call the reconciler's own `validateWorkspaceBindings` through
   the uncached reader, with `previouslyAdmitted = true`, on the version about to be patched. It is
   the same code, not a copy: the function moves to a shared helper both callers use. Any reason
   other than `Ready` abandons the inline path, and the ordinary attach proceeds unchanged.
2. **Write the condition first.** Write status `Admitted=True, Reason=Ready` with
   `observedGeneration` *G*+1. This is the same predicted-generation step as
   `reconcileWorkspaceAdmission` (`execution_workspace_controller.go:149-169`).
3. **Write the attachment and admission together.** One optimistic spec write sets
   `spec.attachment`, `spec.attachmentEpoch`, and `spec.coreAdmission.admittedGeneration` = *G*+1,
   plus the existing annotations: the detach action, and removal of `suspend-at`.

**Safety.** This is the same rule the reconciler applies:

- **A lost write admits nothing.** If step 3 loses the optimistic lock, the condition names a
  generation that no `coreAdmission` record matches, so `workspaceCurrentlyAdmittedByCore` stays
  false. The reconciler then re-admits as today.
- **One writer.** The Task reconciler and the workspace reconciler run under the same manager
  identity and leader election, so no new identity writes `coreAdmission`.
- **The reconciler stands down.** It sees *G*+1 already admitted and falls through to its
  condition-only branch.
- **The provider sees one generation.** It observes *G*+1 already admitted, so it acknowledges one
  generation sooner.

This removes hops 1 and 2. It needs no provider change, and it leaves ADR 0023 untouched.

### 2. The Core attachment fence replaces the acknowledgement as the queue condition

For an attachment-only change, `queueACPRuntimeTask` does not wait for `attachedEpoch`,
`observedGeneration`, or `Attached`. In the pass that made the attach write, it evaluates the
**attachment-only fence** and, if the fence holds, goes straight to pool demand and the
PromptAttempt. Everything in the fence is read uncached:

**Attachment (Core-owned):**

- `spec.attachment.taskRef.uid` is this Task, and `spec.attachment.epoch` = `spec.attachmentEpoch`
  = the Task's `acp.workspace.orka.ai/attachment-epoch`. The attachment has not expired.
- The attachment Lease is held by this Task, unexpired, and fenced to that epoch. The bearer
  Secret's owner, epoch, and digest match.
- The workspace is currently admitted by core at the current generation (section 1).
- No revocation stamp stands, desired state is `Ready`, and `spec.retirement` is nil.
- The workspace is not deleting, not `Failed`, not quarantined, and not operator-tainted, whether
  through the workspace or any linked Task.
- `maxLifetime` has time remaining.

**Provider evidence (the last observation, of the same runtime spec):**

- `status.observedGeneration` is the pre-attach generation *G* or later.
- The runtime spec digest is unchanged. The fence keeps the *G* digest from the attach write
  itself, so it cannot change unnoticed.
- `status.allocation` is `Ready`, with startup evidence, for the current request (key, sequence,
  revision).
- No `Admitted=False` condition exists.
- `status.attachedEpoch` is 0 or below this epoch, never above it.

**Pool and runtime (ADR 0057's lag predicate, now used for admission):**

- `externalAttachmentOnlyLag` holds, or the provider has already caught up. In either case the
  pool's `orka.ai/external-runtime-served` record equals `externalRuntimeServedRecord(workspace,
  pool.status.activeInstance)`. The record covers the digest, allocation sequence and ID, instance
  ID, request revision, Pod UID, and boot ID.
- The pool is `Serving` and `Accepting`, and its `activeInstance.controllerEpoch` is the current
  fence.
- No `orka.ai/external-runtime-retirement-requested` and no `external-runtime-eviction` is set.
  The workspace carries no `warm-window-drained-at` and no `suspend-pending`.
- The provider object has the bound UID, is not `Disabled`, and serves the binding
  (`externalProviderServesBinding`).

**It runs twice, and both checks fail closed:**

1. at queue, in place of `verifyACPWorkspaceReadyForPool`;
2. again immediately before first runtime contact, which is `CreateRuntimeSession`, or ADR 0059's
   `residentSessionGate` on a reuse.

If the queue check fails on a provider-evidence item, the Task falls back to the ordinary
acknowledgement wait. If it fails on any other item, it fails exactly as today's handshake does. If
the pre-contact check fails, the turn requeues pre-submission with no runtime mutation, which is
today's `workspace_binding_changed` behavior. A new milestone, `workspaceAttachFenced`, replaces
`workspaceReady` and `runtimePoolReady` on this path. Those two are still marked when they are
observed, off the critical path.

### 3. Revocation still waits for the provider, by generation

The provider's acknowledgement is now asynchronous, so a Task can settle before the provider ever
acknowledged its epoch. Under the flag, `FinalizeRevocation` additionally requires
`status.observedGeneration` ≥ the generation of the `BeginRevocation` write, and the epoch must not
be reported as attached.

ADR 0023's revocation step 2 ("observe attachment revocation in workspace status") therefore stays
literal. It means a provider observation *after* revocation, not merely one that does not name the
epoch. ADR 0055's warm settlement, the next Task's attach (which requires no revocation stamp), and
detach-timeout quarantine are otherwise unchanged.

### 4. Safety argument: no RuntimeSession on a stale attachment, a replaced instance, or a revoked or retired workspace

| Threat | Fence that replaces the acknowledgement | Fails closed because |
| --- | --- | --- |
| **Stale attachment** (an older epoch, another Task, an expired attachment) | Spec attachment = this Task at its recorded epoch, the Lease held and fenced to that epoch, unexpired, no revocation stamp | Every field is Core-written under optimistic locks. A rival attach or a revocation changes the spec or the Lease, and the pre-contact recheck sees it. The supervisor's prompt fences (Task UID, attempt, prompt ID, lease generation) reject a settled Task's prompt, as today. |
| **Replaced or lost instance** | Served record = current allocation identity + pool `activeInstance` Pod UID and boot ID; allocation `Ready` | The provider writes status only with the generation it observed. A provider observation of *G*+1 that reports another instance, or a non-`Ready` allocation, breaks equality and fails the pre-contact check. A supervisor restart between probes is caught by the exact-instance and boot fences on `CreateRuntimeSession` (ADR 0024), exactly as between any two serving passes. ADR 0057's fence-equivalence point 2 already accepts this for serving. |
| **Revoked, retired, or deleted workspace** | Desired `Ready`, no `spec.retirement`, unchanged runtime digest, not deleting or `Failed`, retirement and eviction annotations absent | Any retirement, suspension, deletion, or lifecycle write changes the digest or a checked field. It must also win an optimistic write against the attach, so it lands either before the attach (which refuses) or after it (which the pre-contact check sees). |
| **Taint and quarantine** (ADR 0044) | Workspace and linked-Task taint, quarantine label and condition | Unchanged from the attach rule, and read again before contact. |
| **ADR 0056 strong retirement and eviction on shared workers** | Pool `Serving`+`Accepting`, no `external-runtime-eviction`, served record unchanged, provider `Ready` allocation | The strong path closes the provider's pool and drains co-hosted Actors through Core's eviction path (ADR 0046), which sets the eviction annotation and closes admission. A worker termination the provider has not yet reported is the same window today's acknowledgement leaves open: the acknowledgement's `Observe` is incidental, not a liveness proof. Termination before contact fails `CreateRuntimeSession`'s exact-instance fence. After contact, it follows today's mid-turn drain. |
| **ADR 0058 takeover** | `activeInstance.controllerEpoch` = the current fence, at queue and at reservation; `CreatePromptAttempt` is epoch-fenced | A successor's epoch makes the reservation refuse (`runtimePoolReservationAdmission`). ADR 0057 §7 retires a request published under the old fence, which changes the digest and the served record. Inline admission writes no control-store state, so the interlock is unaffected. |
| **Controller restart** | Every fence input is durable: spec, status, Lease, Secret, pool annotations, and pool status | There is no in-memory authority. A crash between the inline condition write and the spec write leaves an unmatched condition, and the reconciler re-admits as today. A crash after the attach write is followed by a pass that evaluates the fence again from durable state. |
| **Provider disabled or drifted** | Provider UID, not `Disabled`, `externalProviderServesBinding`; inline admission's `validateWorkspaceBindings` | The same checks core admission applies. A drift after admission is caught by the pool's own pass, which closes, and by the pre-contact recheck. |
| **A provider that does enforce the epoch** | The `AttachmentAdvisory` feature is required | Without it, the ordinary ADR 0023 step 7 ordering applies. |

The prompt never depended on the acknowledgement for anything the runtime enforces. The runtime
never sees the bearer. What the acknowledgement added was a fresher provider observation, and the
second pre-contact check plus the exact-instance create fences give the same fail-closed outcome.

### 5. ADR 0059 interaction

- **Persistent session (an ADR 0059 hit).** ADR 0059 already queues once the attachment is held.
  Its `residentSessionGate` waits for `verifyACPWorkspaceReadyForPool`. Under this ADR:
  - the gate's handshake item is the attachment-only fence. Its other items (served digest,
    `attached-from-warm`, session status GET) are a superset of the fence's pool items, and both
    use one shared predicate;
  - `handshake-timeout` cannot occur for an eligible workspace;
  - inline re-admission makes the acknowledgement arrive one hop sooner on paths that still wait
    for it.

  Median saving ≈ 0, because the chain was already overlapped. It removes the gate's wait on a slow
  provider (p90), and it keeps the saving if attempt → plan gets shorter.
- **Fresh session** (ADR 0059 off, a miss, or ineligible). The Task goes from the attach write to
  the fence, pool demand, and PromptAttempt in **one pass**. The pre-contact recheck runs before
  `CreateRuntimeSession`. On an ADR 0059 miss that recreates, it runs before the fenced
  `DeleteRuntimeSession` of the resident session as well. This is where the saving below applies.

### 6. Observability

- **`ACP attachment fast path`.** Logged once per attach with `namespace`, `task`,
  `inlineAdmission=hit|miss`, `fence=hit|miss|fallback`, `stage=queue|contact`, a bounded
  `reason`, and `ackLagMs`. `ackLagMs` is measured from the attach write to the provider's
  `observedGeneration`, and is logged when it arrives.
- **Pre-dispatch timings** gain `workspaceAttachFenced`.
- **Metric.** `orka_acp_attach_fast_path_total{stage,result,reason}`, with no names or UIDs.

## Consequences

### Expected saving per hop (round 8 medians, warm follow-up)

| Hop | Today (ms) | Inline re-admission only | Plus the Core fence |
| --- | --- | --- | --- |
| 0 attach pass | (before the mark) | +~120–150: one status write and one validation | same |
| 1–2 core re-admission | ~500–700 of 1007 | 0 | 0 |
| 3–4 provider echo, Task pass | ~300–500 of 1007 | unchanged, one hop earlier | off the path |
| 5–7 provider re-observation, wasted pass, Task pass | 930 | unchanged | off the path; the fence costs ~60–120 ms (pool, provider, Lease GETs) |
| 8–9 queue, reserve | 977 | unchanged | unchanged |
| **attach → runtimePoolReady (or fenced)** | **1937** | **~1.35–1.55 s** | **~0.2–0.3 s** |
| **Saving, fresh session** | | **~0.4–0.6 s** | **~1.6–1.7 s** |
| **Saving, ADR 0059 hit** | | **~0 median** | **~0 median; p90 tail only** |

Plainly:

- **There are no RuntimePool hops left to remove on this path.** ADR 0057 already removed them.
- **The workspace hops can be removed safely**, given the scoped fence above.
- **The saving is about 1.6 s for fresh-session turns, and about 0 at the median for ADR 0059
  hits.** On an ADR 0059 hit, this ADR does not close the ~1 s gap to kagent. That gap is hops 8–9
  plus `runtimeClient` and `sessionPlan`, which are Core's serial per-write costs (ADR 0058's
  measurement).

### Other consequences

- **Removes work, not just waiting.** With the flag, three validations, two writes, and two
  wasted Task passes leave the attach path. The provider still does its pass, asynchronously.
- **New contract surface.**
  - The `AttachmentAdvisory` provider feature, with conformance: the attachment never gates
    allocation or runtime, and no epoch-enforcing data plane exists.
  - A shared predicate that must stay equivalent between the pool's serving rule, the queue
    fence, and ADR 0059's gate.
  - A shared `validateWorkspaceBindings` with two callers.
- **ADR 0057's pending security review now covers more.** The serving lag predicate becomes an
  admission input. The review must cover both rules together.
- **Settlement may wait longer.** A very short turn can finish before the provider observes its
  attachment. Its revocation then waits for the provider's observation of the revocation (section
  3), on the turn's tail rather than the next turn's head.

## Rejected alternatives

- **Keep the RuntimePool `Serving` across the change, or probe concurrently with reservation.**
  This already holds since ADR 0057. The pool is not on the critical path, so there is nothing to
  gain here.
- **Generalize ADR 0059's overlap to fresh sessions** (queue on a held attachment, and handshake
  before first contact) **without removing the hops.** It saves about the same 1.6 s today with no
  ADR 0023 change, because attempt → plan (~2.4 s) hides the 1.9 s chain. It is not chosen as the
  primary design because it keeps the work and the provider tail, and the saving disappears as soon
  as Core's chain gets shorter. It is the recommended fallback if change 2 is refused (open
  question 1).
- **Skip core re-admission for attachment fields entirely** (the provider ignores
  `admittedGeneration` lag when only attachment fields changed). That needs an orka-workspace
  contract change on every provider, and it turns provider code into an admission authority.
  Inline admission keeps Core as the authority.
- **Make the provider acknowledge in one write.** Moving `observedGeneration` into the first status
  write would remove hops 5–7 from today's path (~0.9 s) without a Core contract change. But it
  would report an observation before `Observe` and `EnsureAllocation` ran, and that would weaken
  ADR 0057's fence-equivalence point 2, which relies on provider status naming the generation it
  observed. A provider-side variant that is acceptable is to mark the Task-relevant fields (the
  acknowledged epoch with `observedAttachmentGeneration`) separately from the full observation.
  That needs a new status field. It is listed as open question 6.
- **Drop the bearer Secret and Lease for ACP workspaces.** The Lease is the exclusivity authority,
  and the epoch is what Task, settlement, and ADR 0059 records key on. This would remove nothing on
  the critical path.
- **Trust the cached workspace for the fence.** A stale cache could miss a revocation or
  retirement. The fence reads uncached, as `verifyACPWorkspaceReadyForPool` does.

## Rollout

- **The flag.** `--acp-workspace-attach-fast-path=off|readmit|fence`, default **`off`**. It
  requires `--acp-workspace-dispatch-enabled` and `--enable-workspace-provider-api`.
  - `readmit` enables section 1 only.
  - `fence` enables sections 1–3, and only for providers declaring `AttachmentAdvisory`.

  Turning the flag down takes effect on the next attach. A Task already queued through the fence
  keeps its pre-contact recheck. A Task not yet queued waits for the acknowledgement as today.
- **Stages.**
  1. `readmit`. No contract change, so it is a candidate for default-on after the harness.
  2. `fence` on Substrate, after the provider ships `AttachmentAdvisory` and its conformance tests.
  3. Agent Sandbox, if open question 2 allows it.

## Test plan

- **Unit**
  - **Inline admission is equivalent to the reconciler.** A table over workspace states compares
    the inline decision with `reconcileWorkspaceAdmission` in each state:
    - first admission;
    - maintenance intent;
    - `Admitted=False`, retryable and not retryable;
    - provider lag;
    - class or provider drift;
    - a digest change.

    It also covers a lost optimistic lock after the condition write, which must leave the
    workspace not currently admitted.
  - **The fence predicate.** One case per field, each flipping the result:
    - attachment Task, epoch, expiry, Lease holder, fence, and expiry, and the Secret digest;
    - the revocation stamp, desired state, retirement, deletion, `Failed`, quarantine, workspace
      and linked-Task taint, and `maxLifetime`;
    - each component of the served record (digest, sequence, allocation ID, instance, revision, Pod
      UID, boot ID);
    - pool lifecycle, admission, and controller epoch, and the retirement, eviction,
      `warm-window-drained-at`, and `suspend-pending` markers;
    - provider UID, `Disabled`, and feature absence;
    - `attachedEpoch` above this epoch.
  - **The digest.** Extend the ADR 0057 property test: every spec field except `attachment`,
    `attachmentEpoch`, and `coreAdmission` changes the digest.
  - **`FinalizeRevocation` generation rule.** It refuses while `observedGeneration` is below the
    revocation generation, even when `attachedEpoch` is 0.
- **Envtest** (orka-workspace fake provider, fake supervisor)
  - **Fast path.** The PromptAttempt is persisted in the attach pass, before the provider's
    acknowledgement. `CreateRuntimeSession` runs only after the pre-contact recheck.
    `ExecutionWorkspaceReconciler` makes no admission write for *G*+1.
  - **Provider paused** (reconciler stopped). The turn runs under `fence` and waits under `readmit`
    and `off`. Settlement finalizes revocation only after the provider resumes and observes the
    revocation.
  - **Changes landing before contact.** Each of these makes the pre-contact recheck requeue
    pre-submission with no runtime mutation:
    - the provider reports a new allocation or `Failed` at *G*+1;
    - an eviction annotation or retirement request;
    - an ADR 0056 strong retirement (pool `Draining` or `Degraded`);
    - taint;
    - a warm-window expiry that wins against the attach.
  - **ADR 0058 takeover** between the attach and the queue, and between the queue and reservation.
    Reservation refuses, and no RuntimeSession is created on the old instance.
  - **Controller restart** between the inline condition write and the spec write, and between the
    attach and the queue.
  - **Concurrency.** Concurrent continuations of one Session, and a concurrent
    `ExecutionWorkspaceReconciler` pass.
  - **Flag `off`.** The same write sequence as today (golden).
  - **ADR 0059 hit and miss**, once it lands.
- **Live kind latency harness** (orka-workspace `hack/external-substrate-e2e/core/latency.go`,
  Substrate on gVisor, Vekil, aimock)
  - **Configurations.** Warm scenario t2, private and shared workers, with the flag `off`,
    `readmit`, and `fence`, each with ADR 0059 off and on.
  - **Measurements.** Report `workspaceAttachRequested` → `workspaceAttachFenced` or
    `runtimePoolReady` → `reserved` → agent receipt, as median and p90, plus `ackLagMs`.
  - **Checks.**
    - Assert zero `readmission=true` lines in `ACP workspace runtime admission timings` on warm
      turns.
    - Keep the existing `warmAttach` checks (same instance, Actor, and worker; new epoch; no new
      Tag).
    - Add one check: revocation finalized only after the provider observed it.
  - **Adversarial cases.**
    - the provider Deployment scaled to 0 during t2;
    - an eviction drain during the window;
    - a shared-worker strong retirement;
    - operator exec taint during the window;
    - a controller restart between turns.

    Run the AGENTS.md ACP real-world validation list once under `fence`.

  Promote `readmit` to default after the harness shows no regression. Promote `fence` only after
  the security review and the conformance suite.

## Open questions for the maintainer

1. **ADR 0023 step 7.** Do you accept replacing the provider's acknowledgement with the Core fence
   for advisory providers? If not, the recommendation is `readmit` (saving ~0.5 s) plus
   generalizing ADR 0059's overlap to fresh sessions (~1.6 s today, no contract change).
2. **Scope signal.** Should this need a declared `AttachmentAdvisory` provider feature (an
   orka-workspace API change and a release), or is ADR 0057's provider conformance enough? Should
   Agent Sandbox be included?
3. **Priority.** On ADR 0059 hits this saves ~0 at the median. Should the parity effort target
   hops 8–9, `runtimeClient`, and `sessionPlan` (~2.4 s of serial Core writes) before this?
4. **Inline admission writer.** Is it acceptable for the attach path to write `coreAdmission`
   (same identity, shared validation code)? Or must admission stay inside
   `ExecutionWorkspaceReconciler`, for example through a synchronous call into it?
5. **Revocation by generation.** Apply section 3's `observedGeneration` rule always, as a
   tightening, rather than only under the flag?
6. **Provider-side alternative.** Would you prefer an orka-workspace status field
   (`observedAttachmentGeneration`) so the provider can acknowledge in its first write? That keeps
   ADR 0023's ordering and saves the ~0.9 s of hops 5–7.
7. **Review.** Should this share ADR 0057's pending security review, or have its own?
