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
- It is **never extended**. A later pass by the same Task, or by a restarted controller, leaves
  it unchanged. A Task whose own start expired never renews it.
- It is recorded in a write the Task already makes, so it cannot lose a race to core admission:
  - **first turn:** the write that creates the workspace;
  - **resume:** the single resume-request write described below.

  Any other recording retries a conflicting write from a fresh read.
- It is written **before** any pool demand.
- Then the Task activates the RuntimePool. It creates the pool for a first turn, or scales the
  existing one to one replica for a resume. This uses exactly the pool-identity checks of the
  ordinary path: frozen class, provider, profile, required features, workspace link, UID pin,
  image, and profile digest. Only the post-attach handshake is skipped.
- The start is best-effort. Any failure leaves the ordinary path in force and never fails the
  Task.

#### The staged attachment

Round 5 measured why the first implementation saved nothing:

- **Publication waited on the flip.** It still trailed the start by about 3 s, because the
  RuntimePool could publish only after Core re-admitted the flip back to `Ready` and the provider
  observed it.
- **The marker lost races.** Its own write lost optimistic-lock races to core admission 31 times
  across 22 Tasks.
- **Spare runtimes were admitted twice.** A spare runtime that booted before the attach landed
  was admitted, then closed and re-admitted about 9 s later, because the attach bumped the
  workspace generation and the admission fence ("admit only after the provider observed the
  current generation") applied again.

So the flip, the publication, and the attachment now land in **one workspace write**:

- **The Task stages its attachment.** It runs the attachment manager's own steps: claim the
  Lease, create (or recover) the next epoch's bearer Secret, and fence the Lease to that epoch.
  It records only a digest-only intent, `acp.workspace.orka.ai/attachment-intent` (Task, epoch,
  Secret name, bearer digest, expiry), and never writes the attachment itself.
- **The resume request.** For a continuation of a settled suspension with verified retained data
  (the current request observed `Stopped` at the current generation, with retained data bound to
  that exact instance), one write records:
  - its demand;
  - its frozen detach action;
  - the resumed-lineage marker;
  - the speculative start;
  - the attachment intent.

  Desired state stays `Suspended`. A rival continuation waits for the flip instead of making its
  own.
- **The RuntimePool.** It holds that pending resume's pool and treats it as not stopping. It
  proves the previous instance terminated, rotates the consumed bootstrap material, and then, in
  one optimistic write:
  - flips desired state to `Ready`;
  - publishes the next request with the retained data;
  - installs the staged attachment as `spec.attachment` at its epoch.

  A first turn's publication installs its staged attachment in the same way.
- **The install rechecks every attach fence** against the exact object version the write is
  locked on:
  - the intent's Task owns the demand and the start;
  - no attachment exists, and the next epoch equals the intent's;
  - the intent has not expired;
  - no taint (workspace or linked Task), no quarantine, no pending settlement;
  - the Secret's owner, epoch, and bearer digest match;
  - the Lease is held by that Task, unexpired, and fenced to that epoch.

  A failed check publishes without attaching, and the Task attaches itself.
- **The provider observes one generation.** It acknowledges the epoch and starts the runtime in
  the same pass. The runtime is attested and admitted once, and no later attach invalidates it.
  The admission fence is unchanged.

#### Fallbacks

- **The request expires unclaimed.** The Task flips the workspace itself and attaches through the
  ordinary path. That attach recovers the staged Secret (`recoverOrphanedAttachmentSecret`) and
  renews the same Lease, at the same epoch.
- **The RuntimePool cannot be activated.** The Task flips at once.
- **No verified retained data.** The Task flips as before.
- **The pool publishes before a first turn's intent exists.** The first turn attaches through the
  ordinary path after the publication, and the rule in the next section keeps the runtime
  serving.

#### First turns: the publication carries the attachment

Round 6 showed first turns unchanged: bind → provider start of 2.3–4.9 s on private workers, and
speculative start → publish of 1.6–3.5 s. There were two causes:

- **The pool was activated a pass late.** The creation pass's speculative start read the workspace
  it had just created from the informer cache, missed it, and gave up. It now falls back to the
  uncached read.
- **The Task attached first.** The Task's own attach, allowed as soon as core admitted the fresh
  workspace and the provider observed it, landed before the publication. It bumped the
  generation, made the publication wait for a re-admission, and left the staged attachment
  unused.

  Now, while a first turn's attachment is staged, its speculative start is open, no request is
  published, and its RuntimePool exists, holds a replica, and is not deleting or degraded, the
  Task does not attach. The publication installs the attachment instead. The wait ends when the
  publication lands (with or without the install), the start expires, or the pool stops
  qualifying; then the Task attaches itself.

A `speculativePoolActivated` pre-dispatch milestone times the pool activation.

#### Serving across an attachment-only generation change

Round 6's main tail was readmission. A Task attaching a pool that already serves (a warm window,
a spare runtime that booted before its attach, or a shared worker), or a settlement revoking one,
bumps the workspace generation, and core's re-admission bumps it again. The serving path closed
admission until the provider observed the new generation, then re-attested, re-bootstrapped, and
re-probed the same runtime. That cost 7–38 s of probe → serving, and accounted for 73 of the 99
shared admission lines, with up to 9 in one sequence.

- **What the pool records.** Once the pool serves an attested instance at the current
  generation, it records on itself (`orka.ai/external-runtime-served`):
  - the runtime-relevant spec digest, which is the workspace spec without `attachment`,
    `attachmentEpoch`, and `coreAdmission`;
  - the provider's allocation sequence, allocation ID, instance ID, and request revision;
  - the active Pod UID and boot ID.
- **When the pool keeps serving.** While the generation lags (the provider has not observed it,
  or core has not re-admitted it), the pool keeps serving only if all of these hold:
  - the pool is `Serving` and `Accepting` with that recorded active instance;
  - the digest is unchanged;
  - the provider's last observation is the recorded `Ready` allocation with startup evidence.

  Then the admission and observation gates, and the post-probe check, keep serving instead of
  closing. Nothing is re-attested, re-bootstrapped, or re-probed until the provider catches up,
  after which the ordinary serving pass runs as on any other pass.

**Fence-equivalence.** Closing and re-probing after a generation change guards against four
things. Each is covered as follows.

1. **The runtime changed because the spec changed:** a new request, a retirement, a desired-state
   change, a lifecycle change, or deletion. Any of these changes the digest or fails the explicit
   deletion, desired-state, and retirement checks, so the pool closes as before.
2. **The provider replaced or lost the instance.** The provider writes status only with the
   generation it observed. While the generation lags, the observation is the one the pool
   already attested. When the provider observes the new generation and reports another instance,
   that is no longer a lag, and the ordinary path re-attests. Even during the lag, a reported
   allocation that differs from the record closes the pool. A supervisor restart is caught by the
   next ordinary pass's probe, exactly as between any two serving passes.
3. **Core admission was withdrawn.** A lag needs the immutable class and provider bindings to
   still match core's last admission and no `Admitted=False` condition. A withdrawn admission
   (class or provider deleting, profile drift) closes as before. Taint, quarantine, `Failed`,
   retirement requests, and evictions close as before. Every fence that does not depend on the new
   observation still runs before the gate: provider usability, runtime configuration, the frozen
   controller-epoch and provider-token fence, and pool binding.
4. **A prompt races the attachment.** The pool's admission is not what authorizes a Task's
   prompts on a workspace pool. A Task is queued only after the workspace handshake
   (`verifyACPWorkspaceReadyForPool`) requires all of:
   - current core admission;
   - the provider's observation of the current generation;
   - its acknowledgement of exactly this attachment epoch;
   - a current `Attached` condition.

   The pool is single-session and the attachment exclusive, so no other Task can be queued on it.
   Keeping an admitted runtime open therefore lets no prompt reach it before the provider has
   accepted the new attachment. It only removes the re-admission that followed.

   The dispatcher reserves only Tasks that passed that handshake, and does not re-check the
   workspace. One holder can therefore keep dispatching across an attachment-only change: a Task
   whose attachment is rotated at expiry (ADR 0023) holds the next epoch without ever detaching.
   Today's pass would have paused it until the provider acknowledged. Letting it continue is
   equivalent, because the runtime never receives the attachment bearer (ADR 0026) and the
   rotation opens no ownership gap. A different Task attaches only after the holder's settlement
   revoked it, and a settled Task is never dispatched.

A revocation is the same attachment-only change. The runtime keeps serving no one until a new
attachment completes the handshake, which is ADR 0055's warm state.

The security review of this rule is requested before it is merged into integration.


#### Pool demand

`WorkspaceRuntimePoolReconciler` keeps an unattached pool's replicas in two cases:

- **while a start is open:** the deadline has not passed, and the workspace is still in the
  allowed state above, including outstanding demand;
- **while a speculative resume is pending.**

It wakes at the deadline. Before it scales any unattached pool to zero, it re-reads the
workspace from the API server. A cached copy older than the pool event that woke it must not
undo the start or a fresh attachment.

#### Attachment during the boot

When the Task attaches itself, the class attachment rule also accepts a recorded speculative start
whose exact published request is still booting:

- the request is unretired;
- the observation is `Pending` for the same key and sequence, at the same revision if it reports
  one;
- for a first turn, the provider may not yet report any observation at all.

Every other guard still applies: current core admission, the provider observed the current
generation, `Pending` state, no quarantine, no revocation stamp, no taint. A `Ready` allocation
already passes the generic rule, exactly as a warm-window attach does. The attach patch removes
the start, the demand, and any intent.

#### Warm-window expiry and live demand

This amends ADR 0055's expiry. At its deadline, a warm window defers for at most two minutes while
any of these exists:

- a live recorded requester;
- an open speculative start;
- any live continuation Task of the Session, bound or not yet linked.

A continuation that reaches a window within two minutes of its deadline records its demand before
it attaches. Retention's delayed Suspend is an optimistic write on the same object, so from then
on it either sees the demand or loses the write, exactly as against an attachment.

Round 5 iteration 3 showed why. A continuation bound four seconds before expiry was suspended
under it and waited 60 s for the suspension. A window that an eviction or upgrade drain ended
(ADR 0055) gets no grace.

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
   attempt.
   - **Staging claims the same Lease.** A rival therefore waits while a requester's staged
     attachment is pending, and the install puts the requester's exact staged attachment in
     place under the same epoch arithmetic as an attach.
   - **An unstaged start can go to a rival.** A concurrent continuation of the same Session may
     claim a speculative runtime its rival started without staging. That is safe for two reasons:
     the runtime is scoped to the Session's pool, cold booted, and holds no Task state; and the
     claimant passes every check the requester would.

   The other Task then queues behind it as today.
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
5. **A rejected or cancelled Task.** Its settlement first releases what it staged and never had
   installed. It removes the intent and, on a still-suspended workspace, its resume request,
   demand, and start, in one optimistic write. That write fences the RuntimePool's install and
   flip. Only then does it delete the bearer Secret and the Lease it holds. Revocation of an
   older epoch leaves an uninstalled newer staged Lease alone. Settlement then runs the existing
   path for a requester that never attached:
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

Neither change needs an `orka-workspace` code change. The provider must keep four properties of
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
4. **One write from `Suspended`.** A single generation that moves a settled `Suspended` workspace
   to desired `Ready`, sets `spec.workload` to the next sequence (`previousInstance` and
   `retainedData` from the stopped observation), clears `spec.retirement`, and sets
   `spec.attachment` and `spec.attachmentEpoch` must be handled in one pass once Core re-admits
   it:
   - the epoch is acknowledged;
   - `EnsureAllocation` is called for that request, resuming from the retained data;
   - no intermediate `Ready` generation with the stale request is required.

   Today's code already does this: the `DesiredSuspended` case no longer matches, and the
   `EnsureAllocation` case does. The conformance suite should assert it.

Further latency now depends on the two re-admission hops that every spec write costs: core
admission, then provider observation. Removing them would change ADR 0023's admission contract
and is not done here.

## Consequences

- **Expected saving on resume.**
  - The first implementation measured no saving in round 5: bind → publish went from 3.0 s to
    3.3–4.6 s.
  - With one write, publication follows the resume request after only the RuntimePool's own
    passes (rotation and publication), not after a core re-admission and provider observation of
    the flip. Bind → provider allocation start is then one re-admission hop plus one provider
    observation, an expected 1–1.5 s.
  - The 0.5 s target needs the re-admission hops above removed.
  - The pre-dispatch `speculativeStartMs` and the provider's allocation timing must confirm this
    live.
- **Spare and shared paths admit once.** Round 5's spare probe → serving of 5.4–6.6 s came from
  the second admission. That admission no longer happens when the intent is installed in the
  publication write.
- **Expected saving on a first turn: about 1.0–1.5 s.** The pool is created at workspace
  creation and publishes at core admission, carrying the staged attachment.
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
- **Round 6 measured the resume write.** Bind → provider start dropped to 1.1–1.7 s (from
  3.5–5.3 s), speculative start → publish to 0.18–0.63 s, and a spare's first-admission
  probe → serving to 0.72 s (from 5–6 s). Marker conflicts and missed starts were zero.
- **Readmission removed.** An attachment-only generation change no longer closes a serving
  runtime, which removes round 6's 7–38 s readmission tail on warm, spare, and shared paths.
- **First turns.** The pool is activated on the creation pass, and the publication carries the
  attachment. Bind → provider start should drop by the readmission hop the separate attach
  forced; live timing must confirm this.
- **Pre-dispatch timing.** It gains the `speculativeStart` and `speculativePoolActivated`
  milestones, and retention reports each deferred warm-window expiry with a log line and a
  `WarmWindowExpiryDeferred` Event.
- **Out of scope.** Change 1 does not apply to the warm window, which already attaches a running
  runtime. Change 2 does not apply to Agent Sandbox or non-Substrate profiles.
