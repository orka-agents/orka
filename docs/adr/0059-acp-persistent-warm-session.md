# 59. Keep a Session's ACP RuntimeSession live across warm-window turns

Date: 2026-11-20

## Status

Proposed. This is a design only; nothing here is implemented. It would amend:

- ADR 0024 and ADR 0055: a write-intent Session on a warm class-backed workspace would no
  longer retire its RuntimeSession after every turn. ADR 0055's "a new RuntimeSession is still
  created" would then stop applying to those Sessions.
- ADR 0057's queue ordering, on the reuse path only. A Task that already *holds* its attachment
  may persist its PromptAttempt before the provider acknowledges the epoch. The full post-attach
  handshake (`verifyACPWorkspaceReadyForPool`) moves to just before the first runtime contact. It
  is not removed.

It changes none of these:

- ADR 0023's attachment contract (exclusive, epoch-fenced, acknowledged before use);
- ADR 0030's closed full-memory gate;
- ADR 0031's data-only cold path;
- ADR 0041's parked-turn rules;
- ADR 0049's credential model;
- ADR 0056's shared-worker evidence;
- ADR 0058's interlock.

## Context

A follow-up turn inside a warm window (ADR 0055/0057) still takes about 9.0 s from Task creation
to agent receipt. kagent takes 4.5 s in the same kind window. Round 8 median waterfall, in ms from
Task creation:

| Step | Offset | Δ | What runs |
| --- | --- | --- | --- |
| informerAdded | 877 | 877 | Task Add event reaches the informer |
| executionBound | 1369 | 492 | write-once execution binding and snapshot |
| workspaceAttachRequested | 2023 | 654 | `ensureACPClassWorkspace` → `WorkspaceAttachmentManager.attach` |
| workspaceReady | 3030 | 1007 | core re-admission, then provider acknowledges the epoch (2 s requeue poll) |
| runtimePoolReady | 3960 | 930 | `ensureACPRuntimePoolDemand(handshake=true)` → `verifyACPWorkspaceReadyForPool` |
| promptAttemptPersisted | 4335 | 375 | `CreatePromptAttempt` |
| reserved | 4937 | 602 | queue, scan, `reserveTask` |
| runtimeClient | 5360 | 423 | `runtimePoolClient`, `Capabilities` |
| sessionPlan | 6329 | 969 | Session lease, SessionTurn open, `Reserved→SessionStarting→Planned` |
| sessionCreate | 7518 | 1189 | `CreateRuntimeSession`: materialize, baseline, provider home, ACP child, bootstrap |
| agentReceipt | 8991 | 1473 | prompt to a fresh child: ACP init, transcript bootstrap or native restore, model call |

`runtimeClient`, `sessionPlan` and `sessionCreate` are `ACP turn timings` steps, not pre-dispatch
milestones. `agentReceipt` is measured by the harness.

What exists today:

- **Read-only Sessions already keep their RuntimeSession.** `executeReservedTask` retires a
  session only when
  `runtimeSessionRetirementRequired := sessionExecution == nil || workspace.Intent == WorkspaceIntentWrite || nativeSessionsSupported`.
  A read-intent `sessionRef` turn leaves its session resident. The next turn's
  `PlanACPRuntimeSession` returns `reuse-live-runtime-session` when the profile, MCP digest,
  runtime instance and boot ID are unchanged. The authority for that is the in-memory
  `ACPDispatcher.runtimeSessions` map, plus Task status (`runtimeSessionBindingFromTaskStatus`).
  It is lost on restart, which is safe.
- **The round 8 turn creates a session every time.** `sessionCreate` is marked only after an
  actual `CreateRuntimeSession`. The measured Session is therefore write-intent or Codex-native.
- **A write session can carry one delta.** After `FinalizeRuntimeSessionPublication`, the
  supervisor keeps it in `finalizing` with a publication receipt. It is then deletable but takes
  no further prompts.
- **Codex native continuity stops the process.** `captureNativeSession` stops the child
  (`runtime.Delete`) to capture a consistent native state, and the next turn restores it via
  `NativeRestore`.
- **The warm window keeps the runtime, not the attachment.** `settleACPClassWorkspace` revokes the
  attachment at every Task settlement (`BeginRevocation`, `FinalizeRevocation`) and opens the
  window (`settleACPWorkspaceWarm`). The next Task attaches again, waits for core re-admission and
  provider acknowledgement, then runs the pool handshake.
- **Attachment is Task-bound in the provider API.** `ExecutionWorkspaceAttachment.TaskRef` is
  defined in `orka-workspace` `v0.1.0-alpha.6`. AGENTS.md requires Task attachment to be
  exclusive and epoch-fenced.

## Decision

### 1. Scope

A **persisted session** is the one ACP RuntimeSession of a Session that survives a turn boundary.
Its identity is `SessionControl.SessionUID` at generation *g*. It lives on the Session's dedicated
single-session `acp-ws-*` RuntimePool, on the active instance that served turn N.

Persistence is attempted only when all of these hold:

- `--acp-workspace-persistent-session` is on (section 9);
- the Task is a built-in runtime on an `orka.harness.v2` workspace pool. External `runtimeRef`
  dispatch is excluded (section 7);
- the Task has a `sessionRef` with `append: true`;
- the execution workspace is class-backed with `reusePolicy: session`. It is warm-window eligible
  (`acpWorkspaceWarmDetachWindow`), so in practice Substrate `DataOnly` with Suspend allowed. Agent
  Sandbox has no warm window and is excluded by construction;
- for write intent, the supervisor advertises the new `rebase` capability (section 4);
- the runtime does not use native sessions. Codex with `SupportsNativeSessions` keeps today's
  capture-and-retire path (open question 3);
- the Task is not a gateway conversation turn, has no Agent identity binding, carries no
  transaction context, and is not operator-tainted. The first two are open question 4.

### 2. Who owns the persisted session between turns

**The Session owns it, through a durable handoff record. Nothing holds the workspace attachment.**

- **The record.** A new status field, `RuntimeSessionControl.status.residentRuntime` (store type
  `SessionControl.ResidentRuntime *SessionResidentRuntime`), is mirrored in the sqlite and kube
  stores and `pkg/storage/v1`. It holds no credentials. It carries:
  - `handoffID`: a domain digest of the fields below;
  - the source: `sourceTaskUID`, `sourceAttempt`, `sourcePromptAttemptID`, `sourceTurnID`, and
    the released `sourceLeaseGeneration`;
  - `controllerEpochName` and `controllerEpoch`;
  - the workspace: name, UID, and the revoked attachment epoch;
  - the pool: RuntimePool name, UID and generation;
  - the runtime: `runtimeInstanceID`, `supervisorBootID`, and a digest of the pool's
    `orka.ai/external-runtime-served` record (allocation sequence and ID, instance ID, request
    revision, Pod UID, boot ID);
  - the session: `runtimeSessionUID`, `runtimeSessionGeneration`, profile digest, MCP digest,
    `RuntimeSessionWorkspaceDigest`, and the lineage configuration digest
    (`acpSessionLineageConfigurationDigest`);
  - `reuseDigest` (section 5);
  - `transcriptHead`: the last committed message ID and the message count;
  - `verifiedBaseline` (write intent);
  - `keptAt` and `expiresAt`. `expiresAt` is `keptAt` plus the workspace's frozen
    `acp.workspace.orka.ai/suspend-after-detach`, and is never extended;
  - `state`: `Resident`, `Claimed{taskUID, attempt, promptID, leaseGeneration}` or `Retiring`.
- **The in-memory binding becomes a cache.** On this path the `ACPDispatcher.runtimeSessions`
  binding is a cache of the record. A missing or disagreeing cache entry never authorizes reuse.
- **Written with turn N's settlement, in one write.** Turn N writes the record in the same fenced
  `FinalizeSessionTurn` write that finalizes its SessionTurn and releases its mutation lease
  (`releaseFinalizedSessionLease`). That write runs under the controller-epoch interlock (ADR 0058).
  The record therefore exists only if turn N's canonical result committed, and only from N's live
  settlement path. Recovery paths (`reconcileUnfinalizedTaskSession`,
  `settleRetainedTaskScopedRuntimeSession`, `retireRecoveredRuntimeSessionBinding`) never write
  one.
- **What turn N must prove first.** Its PromptAttempt is `Succeeded`. Its delivery state is
  `NotRequested` or `ReadValidated` (read intent), or `VerifiedExact` or `NoChange` with a rebase
  receipt (write intent, section 4). It did not park, and has no unresolved native-capture intent.
  The supervisor reports the session `idle` and quiescent.
- **The cleanup mark.** Once the record commits, turn N records its task-scoped cleanup as handed
  off (`markTaskScopedRuntimeSessionCleanupComplete` with reason `persistent_session_handoff`)
  instead of calling `cleanupRuntimeSession("task_finalized")`. If any write fails, turn N retires
  the session as today.
- **The attachment is revoked as today.** Turn N's settlement still revokes the attachment and
  opens the ADR 0055 window: `markACPWorkspaceRevocationStarted` stamps `suspend-at`, then
  `settleACPWorkspaceWarm` runs. The workspace is unattached between turns, so every attachment
  rule keyed on detached state is unchanged: retention, `workspaceRuntimePoolDemandHeld`, operator
  access, checkpoint export, and deletion. The runtime stays up because of the warm hold, not
  because of this ADR. A persisted session can never extend compute past the window.

### 3. How turn N+1 takes it over

Turn N+1 takes the session over in this order. Each step is fenced, and each fails toward the
ordinary path.

1. **Attach, still exclusive.** N+1 attaches through `WorkspaceAttachmentManager.attach` at epoch
   e+1, exactly as today. When the record is `Resident` and passes the static checks (section 5),
   the same optimistic attach patch also writes `acp.workspace.orka.ai/attached-from-warm: <task
   UID> <epoch> <suspend-at it observed> <last-settled-task-uid>`. That patch is the one that
   removes `suspend-at`, so the evidence is atomic with the observation. Once a suspension wins
   the race, the attach is refused, because a direct attach requires desired `Ready`.
2. **Queue once the attachment is held.** On the reuse path, `queueACPRuntimeTask` treats the
   workspace as *held* once all of these hold:
   - `spec.attachment.taskRef.uid` is this Task, at the Task's
     `acp.workspace.orka.ai/attachment-epoch`;
   - the Lease is held and fenced to that epoch;
   - desired state is `Ready`;
   - no revocation stamp stands (`acpWorkspaceRevocationStampMatchesCurrentEpoch`);
   - the workspace is not tainted, quarantined or `Failed`.

   It then runs the pool identity checks of `ensureACPRuntimePoolDemand`: frozen name and UID,
   workspace link, not deleting. It skips only the handshake, and persists the PromptAttempt with
   the record's `handoffID`. It marks the new milestone `workspaceAttachHeld` in place of
   `workspaceReady` and `runtimePoolReady`.

   A rival Task of the same Session does not hold the attachment, so it stays on the ordinary path
   and queues behind the holder, as today.
3. **Reserve.** `reserveTask` reserves with `residentSlots = 0`, as for any reused session. Unlike
   today's reuse, it also requires `AdmissionState == Accepting`. A pool that stops admitting is
   retiring its instance, and the turn falls back.
4. **Claim, in the lease write.** `bindAndOpenTaskSessionTurn` calls `AcquireSessionMutationLease`
   with `ClaimResidentRuntime{handoffID}`. The same CAS that acquires the lease moves the record
   from `Resident` to `Claimed{N+1}`. It succeeds only if all of these hold:
   - the `handoffID` matches;
   - `controllerEpoch` equals the write's fence;
   - `expiresAt` is in the future;
   - the Session is `Available` and not cleanup-fenced (`sessionCleanupFencedForUID`).

   The claim is exactly-once because the lease generation is monotonic and never reused. An
   ordinary acquisition that finds a `Resident` record moves it to `Retiring` in that same write,
   so no later turn can claim a runtime that this turn will delete or replace.
5. **Plan.** With a claimed record, `PlanACPRuntimeSession` returns `reuse-live-runtime-session`
   only if all of these hold:
   - the record matches the pool's current `ActiveInstance` (instance and boot);
   - generation *g*, the profile digest and the MCP digest match;
   - `bindACPRuntimeSessionWorkspace` reports no workspace change.

   Anything else plans `Recreate` at *g*+1 with its reason. Recreation first deletes the exact
   resident session (fenced `DeleteRuntimeSession`, reason `persistent_session_refused`). It
   requeues pre-submission on failure, exactly like today's `workspace_binding_changed` path. A
   missing or replaced instance is cleanup proof (ADR 0024).
6. **Gate before first contact.** The new turn step `residentSessionGate` runs before any runtime
   mutation:
   - **The ADR 0057 handshake.** `verifyACPWorkspaceReadyForPool` must hold. It requires current
     core admission, provider observation of the current generation, acknowledgement of exactly
     epoch e+1, and a current `Attached` condition. The gate waits for it within the pre-submission
     budget. Normally the acknowledgement has already arrived, because steps 2–5 take longer than
     the re-admission hops.
   - **Pool and runtime.** The pool is `Serving` and `Accepting`, with the record's active
     instance. The `external-runtime-served` digest is unchanged. No
     `external-runtime-retirement-requested` or `external-runtime-eviction` is set.
   - **Workspace.** `attached-from-warm` names this Task and epoch. Its `last-settled-task-uid`
     equals the record's source Task. Its observed `suspend-at` is later than `keptAt`. No
     `warm-window-drained-at`, `suspend-pending` or retirement is set.
   - **Session status.** An authenticated, operation-scoped `GET` on the RuntimeSession must
     return the exact UID and generation in state `idle`, with no prompt, no pending permission,
     no operator operation, and no running descendants. The supervisor gains a `quiescent` field
     in the session descriptor for this.

   If any check fails, the turn misses, releases the claim to `Retiring`, and continues at step 5's
   recreation. If the handshake does not arrive within budget, the turn requeues pre-submission.
   It is stamped `acp.workspace.orka.ai/persistent-session-refused: <reason>`, and its next attempt
   takes the ordinary order.
7. **Prompt.** A new fenced PromptAttempt and its own `StartPromptRequest`, with N+1's Task UID,
   attempt, prompt ID, lease and `PromptMCPAuthorization`. Turn N's prompt is never resent.

**Why N+1 can never run alongside N.** Each step depends on the one before it:

1. N writes the record only after its prompt settled `Succeeded`, delivery reached a reusable
   state, and (for write intent) the rebase receipt exists.
2. N releases its Session lease in that same write.
3. N+1 can claim only by acquiring that lease at a higher generation.
4. N+1 can hold the attachment only after N's revocation finished (`FinalizeRevocation`).
5. The pool runs one prompt at a time (`1 resident / 1 running`).
6. A settled Task is never dispatched. Its fences (prompt ID, attempt, lease generation) are stale
   to the supervisor.

**Crash and epoch analysis.** Every record write is a fenced control-store write, and the claim
compares the record's epoch with its own fence.

| Crash point | Durable state left | Outcome |
| --- | --- | --- |
| Before N's `FinalizeSessionTurn` | no record; session *g* resident | Recovery retires *g* as today. N+1 takes the ordinary path. |
| After the record, before N's cleanup mark | record `Resident` | Recovery may delete *g*. It must also move a record naming *g* to `Retiring`. Otherwise N+1's status check misses. |
| Restart or ADR 0058 takeover before the claim | record `Resident` at the old epoch | The claim is refused (`controller-epoch-changed`). The new epoch also retires the pool's published request (ADR 0057 §7). |
| After the claim, before `StartPrompt` | record `Claimed{N+1}`; lease held | `reconcileUnfinalizedTaskSession` treats the claim as consumed. A retry never reuses across an epoch change: it recreates at *g*+1. |
| After `StartPrompt` | record consumed | Existing prompt recovery applies (`OutcomeUnknown`, never replay). Only N+1's live settlement can write a new record. |
| During write rebase (section 4) | no record | Recovery retires *g*. |

ADR 0058's takeover cannot land between the claim check and its write, because the claim is one
interlocked write. After a takeover, the old holder's writes fail closed. The successor's fence
never equals a record's older epoch. `runtimePoolReservationAdmission` also requires
`ActiveInstance.ControllerEpoch` to equal the fence. So reuse across epochs is excluded twice. A
restart therefore always costs one cold turn, which is the intended fail-closed behavior.

### 4. Write intent: rebase instead of retire

A write session ends at `finalizing`. To be reused it needs one new fenced harness v2 mutation,
`POST /v2/runtime-sessions/{id}/rebase` (`RebaseRuntimeSessionRequest`). Its rules:

- **Allowed only after an exact, verified publication.** Turn N calls it after
  `FinalizeRuntimeSessionPublication` and before `finalizeTaskSessionResult`, and only when the
  delivery state is `VerifiedExact` or `NoChange`. `DeliveredSuperseded`, `DeliveryConflict`,
  `CredentialBlocked` and `PublicationOutcomeUnknown` never rebase. The session is retired as
  today.
- **The request** carries:
  - the publication finalization receipt digest;
  - the published delta digest;
  - the Session's new `VerifiedBranchBaseline`;
  - the expected next-baseline digest.
- **The supervisor** requires state `finalizing` with that exact receipt, and no prompt,
  descendants or operator operation. It derives the next delta baseline as the previous baseline
  with the published delta applied, and compares that with the expected digest. It does not
  re-capture the baseline from the live tree, which keeps ADR 0027's rule: a baseline comes from
  controller-verified state. It then returns the session to `idle` and records the receipt for
  idempotent replay. The live tree is untouched, so anything left unpublished appears in the next
  delta, as on a cold resume.
- **No new credential.** The new baseline is derived, not fetched. No repository read credential
  is minted, and the publication credential stays in the clean-room publisher.
- **Failure or ambiguity.** The turn retires the session as today and writes no record.

The `runtimeSessionRetirement` turn step becomes `runtimeSessionRebase` on this path. Its cost
moves to turn N's tail, before Task completion, and it is expected to take tens of ms. Read-intent
sessions skip the rebase.

### 5. Process memory, per-turn reset, and refusal

**Why a live process is compatible with "suspension preserves data only".** The rule is about
what crosses a suspension, and ADR 0030 is about restoring a checkpointed process. A persisted
session is never stopped, checkpointed or restored. It is the same process, with the same
supervisor boot ID and `RuntimeInstanceID`, continuously fenced by exact-instance checks, inside
one warm runtime lifetime. That is already true for read-only Sessions today, as ADR 0041 notes,
and for ADR 0055's warm supervisor.

Every event that would suspend or replace the runtime ends the process, and the next turn starts
fresh from canonical state:

- window expiry, which leads to the frozen Suspend;
- a cold Suspend at settlement;
- a drain, eviction or upgrade, which ends the window (`endACPWorkspaceWarmWindowForDrain`);
- ADR 0056 strong retirement;
- taint or quarantine;
- pool replacement.

The supervisor's drain treats an idle resident session like any other quiescent session and
deletes it. Data snapshots therefore still contain only `VOLUMES`, and no memory ever crosses a
suspension.

**ADR 0041 is unchanged.**

- A parked turn never writes a record (`turnRequestedPark`), and gets no warm window
  (`acpWorkspaceWarmDetachWindow` requires no `status.wait`). Its continuation always gets a fresh
  session.
- Approvals are never carried. The parked prompt is never replayed.
- The live process remembering turn N's exchange is not a replay. N+1 sends only its own prompt.

**Reset at every turn boundary.** A turn boundary is a prompt whose Task UID differs from the
session's last prompt.

| State | Rule |
| --- | --- |
| Approvals and permission grants | Permission requests are per prompt (`resolvePromptPermission`). `frozenMCPPermissionDecision` selects only `allow_once`. The supervisor refuses tool authority for any other option kind (`authorizePermissionTool`, keyed by prompt ID). The gate requires no pending permission. No grant survives its prompt. |
| Tool and transaction policy | `PromptMCPAuthorization` (tool policy, approval policy, MCP digests, Task UID and attempt, prompt ID, lease generation) is built per prompt from N+1's frozen policy. The session-scoped MCP server set must match (`mcpDigest`). |
| Cancellation | Cancel is per prompt ID (`CancelPromptRequest`). The previous prompt must have settled `Succeeded`. N+1 cancelled before contact sends nothing. Cancelled after contact, it follows today's settlement. An unproven cancel (`OutcomeUnknown`) deletes the session and writes no record. |
| Per-turn credentials | The provider-proxy session is activated per prompt lease (`providerProxySession.activatePrompt`) and deactivated on settlement, so an idle child has no model egress between turns. MCP upstream credentials are injected per prompt at the egress gateway (ADR 0045). `WorkspaceArtifactAuthorization` is consumed at create and never reissued on reuse. |
| TxTokens | TxTokens are not on the ACP path today. Transaction-bound Tasks are refused on both sides of a handoff anyway, and the record stores no token, digest of a token, or child-token Secret reference. Child-token Secrets stay owner-referenced to their own Task. |
| AgentConfiguration and profile digest | They must be byte-equal through `reuseDigest`. |
| Once-per-process context (`PromptDelivered`) | Carried in the record, so the toolbox note is not repeated to a process that already received it. |

**The snapshot test.** `reuseDigest` is a `acp-resident-runtime-reuse/v1` domain digest over
every frozen snapshot field that `CreateRuntimeSessionRequest` is built from:

- profile, image and profile digest;
- the rebuilt AgentConfiguration;
- MCP configuration;
- workspace intent, repository identity and sub-path;
- the `readCredentialRef` and `publicationCredentialRef` identities, and their frozen Secret
  versions from `PromptAttempt.CredentialBindings`;
- the class binding digest (class UID and generation, profile hash, provider identity, detach
  action, slot);
- Connections and the GatewayGrant;
- Agent UID and generation.

A unit test enforces coverage. It builds create requests from snapshots that differ in one field
at a time, with metadata, authorizations, expiry and native restore zeroed. Any field that changes
the request must change the digest.

N+1 computes its digest from its own frozen snapshot, so it needs no workspace preparation. Any
difference means a fresh session.

**History boundary.** A live process remembers everything since its creation, including tool
outputs that the canonical transcript does not carry. Reuse is refused unless N+1 would have
bootstrapped the whole history through turn N:

- `throughMessageID` is empty or equals `transcriptHead`;
- `maxMessages` is unset or at least the message count;
- `promptIncluded` is consistent;
- the Session's transcript head still equals `transcriptHead`, so nothing was appended or edited
  out of band.

**Refusal reasons.** Every refusal falls back to today's path. Each reason is a bounded enum, used
in logs and metrics. The previous-turn reasons appear on turn N's `retained` log, because such a
turn writes no record and turn N+1 then sees `no-record`:

| Group | Reasons |
| --- | --- |
| Configuration | `flag-disabled`, `ineligible-scope` (external, native, gateway, identity, transaction, tainted) |
| Record | `no-record`, `record-expired`, `controller-epoch-changed`, `claim-lost` |
| Snapshot | `snapshot-mismatch` (sub-reason: profile, image, agent-configuration, mcp, workspace-binding, class, repository, credential-ref, connections, agent) |
| Session | `history-boundary`, `transcript-changed`, `session-unavailable` (`ReconciliationBlocked`), `session-deleting` |
| Previous turn | `previous-turn-not-succeeded` (no record is written), `previous-turn-parked`, `previous-turn-cancelled`, `previous-turn-timed-out`, `rebase-failed` |
| Pool | `pool-replaced` (UID or generation), `pool-not-accepting` (Draining, Degraded, Ambiguous, Stopped) |
| Runtime | `runtime-replaced` (instance or boot), `provider-epoch-changed` (served-record digest: allocation sequence or ID, instance, request revision) |
| Workspace | `workspace-not-warm` (desired not `Ready`, suspended, suspend pending, retired, drained, evicted, ADR 0056 strong retirement, tainted, quarantined, `Failed`), `workspace-incarnation-changed` |
| Gate | `handshake-timeout`, `runtime-session-missing`, `runtime-session-not-quiescent` |
| Task | `task-cancelled`, `task-deadline` |

### 6. Cleanup and resource accounting

- **Lifetime.** A resident session lives no longer than its instance, and its instance lives no
  longer than the window, `maxLifetime`, or a drain. Nothing in this ADR holds replicas.
  `workspaceRuntimePoolDemandHeld` is unchanged.
- **Window expiry.** `reconcileWarmDetachWindow` applies the frozen Suspend. The pool drains
  through the authenticated supervisor drain and its persisted quiescence barriers, which delete
  the idle resident session. The instance then retires data-only. The stale record is refused on
  the next turn (`runtime-replaced`). Retention moves it to `Retiring` on a best-effort basis; it
  is inert metadata, and the next keep or the Session's deletion overwrites or removes it.
- **Session deletion.** `sessionCleanupFencedForUID` already blocks new turns.
  `CleanupSessionRuntime` also deletes the record's exact session, and the record goes with the
  `RuntimeSessionControl`. Sessions have no archived state today. Any future archive transition
  must move the record to `Retiring` in the same write.
- **Capacity.** Workspace pools are pinned to `MaxResidentSessions: 1, MaxRunningPrompts: 1`, and
  the pool belongs to one Session. The resident slot is the Session's own, so nothing else
  competes for it. The running-prompt slot is released at the end of every turn, as today. A
  fresh create on fallback first deletes the resident session, so the pool never needs two
  resident slots.
- **Admission.** Only `Serving` and `Accepting` admits new RuntimeSessions, unchanged. Persisted
  reuse additionally requires `Accepting`, so a draining or scaling-to-zero pool never takes a
  reused prompt.
- **Drain and finalization ordering.** Drain stops admission, waits for the running prompt, and
  deletes idle resident sessions before `Quiescent` is persisted. A rebase in flight counts as a
  running operation. Finalization never waits on a record.
- **Memory cost.** The ACP child stays resident for up to the window, inside the Actor's existing
  reservation. On shared workers (ADR 0056) that is one Actor's limits. Operators already size the
  window (ADR 0057).

### 7. External v2 `runtimeRef` dispatch is excluded

External dispatch (`AgentExecutionBackendExternalEndpoint`) skips `ensureACPClassWorkspace`, the
pool and the reservation lease. It receives no per-Task `AgentConfiguration`, and each Task
re-freezes and revalidates the AgentRuntime UID, generation, profile, endpoint, auth Secret
versions and observed instance. There is no warm window, no workspace evidence and no pool
`ActiveInstance` to anchor a record to. The scope check refuses it (`ineligible-scope`).

### 8. Observability

- **`ACP persistent session retained`.** Logged at N's settlement with `namespace`, `task`,
  `result=kept|retired`, `reason`, `runtimeSessionGeneration`, `rebaseMs`.
- **`ACP persistent session reuse`.** Logged once per N+1 attempt with `namespace`, `task`,
  `result=hit|miss`, `reason`, `stage=queue|claim|plan|gate`, `handoffAgeMs`, `gateWaitMs`,
  `runtimeSessionGeneration`.
- **Pre-dispatch timings** (`ACP pre-dispatch timings`) gain `workspaceAttachHeld`. On a hit,
  `workspaceReady` and `runtimePoolReady` are marked when the gate observes them, so the waterfall
  still shows when the acknowledgement arrived, off the critical path.
- **Turn timings** (`ACP turn timings`) gain `residentSessionGate` and `runtimeSessionRebase`. On
  a hit, `sessionCreate` is absent.
- **Metric.** `orka_acp_persistent_session_total{decision="kept|retired|hit|miss",reason}` with
  the bounded reason enum. No names or UIDs go into labels.

### 9. Rollout

- **The flag.** `--acp-workspace-persistent-session`, default **off**. It requires
  `--acp-workspace-dispatch-enabled` and `--enable-workspace-provider-api`. It is off by default
  because:
  - it reorders ADR 0057's queue handshake;
  - it adds a supervisor mutation;
  - ADR 0057's attachment-only serving rule, which this path depends on, still awaits its
    requested security review;
  - the per-turn reset and the history-boundary behavior need live adversarial coverage first.

  Turning the flag off makes the next settlement retire. Records already written are refused
  (`flag-disabled`), which costs one cold turn.
- **Stages.**
  1. The record, claim, gate and queue reorder, for read intent. The in-memory reuse becomes
     durable and gets the overlap saving.
  2. Write intent through `rebase`. This is the stage the round 8 benchmark needs.
  3. Codex native sessions, only if open question 3 is resolved.
- **Unit tests.**
  - `PlanACPRuntimeSession` with a claimed record, plus the full refusal table.
  - `reuseDigest` coverage, using the field-at-a-time property test.
  - Record write, claim, `Retiring` and epoch rules in both the kube and sqlite stores, including
    claim-exactly-once under concurrent acquisitions.
  - The history-boundary check.
  - The supervisor: `rebase` state machine (receipt mismatch, replay, a non-`finalizing` session,
    the derived baseline, unpublished residue appearing in the next delta), `quiescent`
    reporting, refusal of non-`allow_once` authority, and provider-proxy deactivation between
    prompts.
- **Envtest** (fake workspace provider and fake supervisor):
  - **Hit path.** No `CreateRuntimeSession`. The PromptAttempt is persisted before the epoch is
    acknowledged, and `StartPrompt` is sent only after it.
  - **Each fallback:**
    - window expiry, and a suspension that wins the attach race;
    - taint and quarantine;
    - pool drain, pool replacement, and eviction;
    - controller restart, and ADR 0058 takeover in the middle of the handoff;
    - Session deletion;
    - a parked, failed, cancelled, timed-out or `DeliveredSuperseded` turn N;
    - each snapshot field;
    - two concurrent continuations;
    - fault injection (`storagefault`) between the record and the cleanup mark, between the
      claim and the prompt, and during `rebase`.
- **Live kind latency harness** (Substrate on gVisor, Vekil, aimock):
  - a round 9 waterfall for warm follow-ups: median and p90, hit rate, and miss reasons;
  - adversarial cases: a pending permission at turn end, operator exec taint during the window,
    an eviction drain during the window, and strong retirement on shared workers;
  - the AGENTS.md ACP real-world validation list, including clean-room publication across three
    reused turns and controller restart between turns.

  Promote the flag's default only after the harness shows a hit rate of at least 90% on
  back-to-back follow-ups, and only after security review.

### Expected saving per step

| Step | Today Δ (ms) | Reuse path | Saving |
| --- | --- | --- | --- |
| informerAdded, executionBound | 877, 492 | unchanged | 0 |
| workspaceAttachRequested | 654 | unchanged; attachment still taken | 0 |
| workspaceReady | 1007 | overlapped with the attempt → plan chain | ~1.0 s |
| runtimePoolReady | 930 | identity checks only; the handshake moves to the gate | ~0.85 s |
| promptAttemptPersisted, reserved, runtimeClient | 375, 602, 423 | unchanged | 0 |
| sessionPlan | 969 | the claim rides on the lease CAS | ~0–0.1 s |
| residentSessionGate (new) | – | status GET, plus a wait only if the acknowledgement is late | −0.05 to −0.3 s |
| sessionCreate | 1189 | skipped | ~1.2 s |
| agentReceipt | 1473 | warm child, no ACP init, no transcript bootstrap | ~0.3–0.6 s (unmeasured) |
| **Total** | **8991** | **~5.3–5.9 s** | **~3.1–3.7 s** |

The overlap holds because attempt → plan takes about 2.4 s against about 1.9 s for acknowledgement
plus handshake. That is still about 1 s slower than kagent, so this does not reach parity on its
own. The rest is mostly the two re-admission hops ADR 0057 left in place, and the fixed per-write
costs ADR 0058 measured.

## Consequences

- A warm follow-up skips the RuntimePool handshake wait, RuntimeSession creation, and a fresh
  child's start. Process memory lives only inside one warm runtime lifetime. Every suspension,
  drain, retirement, taint, park, restart or configuration change still ends the process.
- **New durable surface.** It adds one `RuntimeSessionControl` status field, store methods in two
  backends, and one supervisor mutation (`rebase`) with conformance coverage. It also adds a
  queue-path branch and a dispatcher gate that must stay equivalent to
  `verifyACPWorkspaceReadyForPool`.
- **The reused agent knows more than a fresh one.** It remembers turn N's tool outputs and its
  reasoning, not only the canonical transcript. This is the point of the feature, but it is a
  behavior difference (open question 1).
- **Restart costs one cold turn.** Every controller restart or epoch takeover makes the next turn
  of each Session cold, by design.
- **The saving depends on ordering.** It assumes the internal chain stays longer than the
  re-admission hops. If later work shortens attempt → plan, the gate starts to wait, and the
  remaining saving is the create and child start (about 1.5–1.8 s).

## Rejected alternatives

- **Keep turn N's attachment through the window, and transfer it in place to N+1.**
  - A settled Task would remain the holder.
  - ADR 0055 settlement, retention, pool demand, operator access and checkpoint export are all
    keyed on detach, so each would need to change.
  - Deleting Task N would revoke under N+1.
  - The transfer is still a spec write, which needs core re-admission and acknowledgement.
- **A Session-held attachment** (`ExecutionWorkspaceAttachment.TaskRef` naming the
  `RuntimeSessionControl`). This contradicts the AGENTS.md rule that Task attachment is exclusive.
  It needs an `orka-workspace` API change and provider conformance, and it saves nothing over the
  overlap.
- **Skip the provider's epoch acknowledgement on an attachment-only lag** (extending ADR 0057's
  serving lag rule to the Task gate). This changes ADR 0023's "start only after status reports the
  enforced epoch", which ADR 0057 deliberately left in place. With the overlap it saves nothing
  extra.
- **Leave the in-memory binding as the only authority** (extending the read-only precedent as it
  is). It cannot make the claim atomic with the Session lease. Recovery paths could not tell a
  handed-off session from an orphan. It leaves no durable reason for audit.
- **Pre-create turn N+1's session at the end of turn N.** That would need N+1's
  AgentConfiguration and read credential before N+1 exists, or would carry N's into it.
- **Re-capture the write baseline from the live tree after publication.** ADR 0027 forbids it.
  The derived baseline is cheaper and controller-verified.
- **Keep Codex native processes alive and skip capture.** Capture stops the process by design.
  Skipping it leaves the Session's newest native state only in memory. A Session that depends on
  native continuity would then fail closed on the next fallback, instead of continuing.
- **Skip the Session mutation lease or the SessionTurn open on reuse.** They are the transcript's
  ordering and exactly-once authority, and the claim needs them.
- **Full-memory suspend and restore of the agent.** ADR 0030's gate is closed.

## Open questions for the maintainer

1. **Memory beyond the transcript.** Is it acceptable product behavior for a reused agent to
   remember turn N's tool outputs and reasoning? Should classes be able to opt out through a
   frozen workspace annotation such as `acp.workspace.orka.ai/persistent-session: disabled`,
   materialized like ADR 0057's default window?
2. **Write-intent `rebase`.** Do you accept the new supervisor mutation, and the rule that
   unpublished residue rolls into the next delta, as on a cold resume? Without it, the round 8
   Session gains almost nothing: it creates a session every turn, so it is write-intent or
   Codex-native.
3. **Codex native sessions.** Exclude them, as proposed? Or accept that a fallback after a reused
   turn bootstraps from the canonical transcript instead of a native capture? That changes
   ADR-level native continuity semantics.
4. **Gateway turns and Agent-identity Tasks.** These are excluded by default, following ADR 0041's
   precedent. Should gateway turns be admitted? `reply_in_conversation` re-authenticates per
   prompt.
5. **The flag default and the promotion bar.** The flag starts off. Is "≥90% hit rate plus a
   security review" the right bar to promote it?
6. **Review scope.** Moving the handshake from "before PromptAttempt" to "before first contact"
   amends ADR 0057's fencing argument 1. Should it go through the same security review that
   ADR 0057's serving rule is still waiting for?
7. **Re-admission hops.** Parity with kagent needs the two hops ADR 0057 left in place removed. Is
   changing ADR 0023's admission contract on the table, as a separate decision?
