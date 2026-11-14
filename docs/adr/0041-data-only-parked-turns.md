# 41. Release runtime capacity while an agent waits: data-only parked turns

Date: 2026-11-06

## Status

Accepted. Implements parked turns for built-in ACP runtimes, resumed by approval
decisions, child-Task completion, and backoff timers, plus a distinct WaitingForCapacity
admission reason. It builds on ADR 0024 (RuntimePools), ADR 0027/0031 (data-only
suspension), and ADR 0030 (the closed full-memory gate), and changes none of them. Waking
on pull request reviews and CI results, and parking a pending `approvalRequiredTools` call,
are deferred (see Consequences).

## Context

ACP agents spend much of their wall-clock time waiting on things outside the runtime: a
person's sign-off, child Tasks they delegated, a rate limit. A waiting turn still holds a
RuntimePool prompt slot and, for a read-only Session, a resident RuntimeSession that blocks
scale-to-zero. `wait_for_tasks` is capped at 30 seconds per brokered call, so a coordinator
burns a live turn polling. Workspace-backed Sessions keep their workspace attached.

Snapshot-based systems solve this by freezing the whole agent process and thawing it later.
Orka cannot: ADR 0030 keeps full-memory restore closed, because a restored process would
carry live credentials, stale runtime and boot identities, and in-flight prompts, tool
calls, and network requests whose outcomes are unknown. AGENTS.md states the rule plainly:
suspension preserves data only, never process memory.

Orka already has a data-only way to continue work. A new Task on the same `sessionRef`
re-freezes its execution snapshot, revalidates Session lineage, recreates or reuses the
RuntimeSession from the canonical transcript, and cold-resumes a suspended workspace. A
parked turn is that continuation, created by Orka when the awaited event arrives.

## Decision

### Request

- The agent calls the brokered built-in tool `park_until` with a kind (`approval`, `tasks`,
  or `timer`), a reason, and kind-specific references. It is offered only when the Agent
  or Task allowlist names it. It executes only inside the controller broker, and only for
  the authenticated Task's own running turn (`Accepted`/`Running`), on a built-in runtime.
- The tool writes `status.wait` in state `Requested`, bound to the exact attempt, prompt ID,
  and a digest of the brokered operation. It is consequential, so it runs under the
  external-effect ledger. An exact replay returns the recorded wait; a second request in
  the same turn is rejected.
- Child-Task references must pass the same authorization as `wait_for_tasks`. An approval
  wait appends one unbound `ApprovalRequested` event whose ID is derived from the Task UID,
  attempt, prompt, and operation digest.
- Parking is refused for Tasks without an appending `sessionRef`, gateway conversation
  turns, transaction-bound Tasks (a continuation cannot safely inherit a TxToken),
  scheduled or prior-diff Tasks, Tasks bound to an Agent identity (the controller creates
  the continuation, so it cannot re-prove the delegation, and admission refuses a
  continuation whose Agent gained an identity while it waited), and chains already 32
  continuations long.

### Arm only at an idle boundary

- The wait reconciler arms (`Waiting`) only when the Task phase is `Succeeded` and the
  execution state and outcome are `Succeeded` for that exact attempt and prompt. At that
  point the prompt has settled and delivery has finished, so no prompt, brokered tool call,
  or publication is in flight.
- Any other end (`Failed`, `Cancelled`, `OutcomeUnknown`, a retry to a new attempt) moves
  the wait to `Abandoned` and closes an undecided sign-off as cancelled. Orka never
  "resumes" a turn whose outcome it cannot prove.

### Release capacity

- The prompt slot and pool reservation are released at turn end, as always. Write-intent,
  task-scoped, and native-session RuntimeSessions are already retired after every turn.
- For a read-only Session, whose RuntimeSession normally stays resident, the dispatcher
  checks for a `Requested` wait on the settling turn. If there is one, it marks the in-memory
  binding `RecreationRequired` while the Session mutation lease is still held. After the
  Session result is finalized, it deletes that exact RuntimeSession.
  - No later turn can adopt the runtime being deleted.
  - Deletion is best effort: on failure, the old resident capacity remains, exactly as it did
    before parking. The next turn recreates the session either way.
- A workspace-backed Session follows its frozen `onDetach` policy. Parking never overrides
  it. With `Suspend`, ADR 0027/0031 data-only suspension applies; full memory is never
  captured.

### Resume exactly once, never replay

- When the event arrives, the reconciler first writes `Resuming` with a deterministic
  continuation name derived from the parked Task's UID. Only then does it create the Task.
- A restart adopts an existing Task of that name only if it carries the parked Task's UID
  annotation and Task provenance admission is enabled. Admission reserves the continuation
  lineage annotations to the controller. Without it the annotation could be forged, so the
  wait is abandoned rather than linked, and the Task is left alone rather than deleted on
  that forgeable claim. Without admission, only a continuation this controller process
  remembers creating can still be linked. An unrelated holder also abandons the wait.
- A continuation does not execute until its predecessor's `Resumed` wait records its exact
  UID. Queueing holds it while the predecessor still reserves its name, and cancels it
  without a prompt attempt once nothing can link it. Abandoning or cancelling an unlinked
  continuation therefore never discards work that ran.
- The continuation is created with the `orka.ai/continuation-link` finalizer. That
  finalizer is removed only after the predecessor records the continuation's UID, ends its
  wait without it, or is gone, and Task provenance admission lets only the controller
  remove it. A continuation deleted while the wait is still `Resuming`
  therefore stays visible and is adopted. Name absence alone never leads Orka to start the
  work a second time. `Resumed` records the continuation's
  UID, and readers verify that UID before following the chain.
- The continuation copies the parked spec, including agent, runtime overrides, workspace,
  and requester, and changes only three things:
  - a controller-authored prompt describing the event, which never contains the parked
    prompt;
  - `sessionRef.create: false`;
  - `timeout` equal to the time left before the parked Task's deadline. The absolute
    chain deadline is also recorded in the controller-reserved
    `orka.ai/continuation-chain-deadline` annotation, and enforcement uses the earlier of
    the two, so creation latency or clock skew never extends the chain.

  It inherits only allowlisted labels and annotations needed for an identical frozen policy.
  It gets a non-controller owner reference to the parked Task, so deleting the parked Task
  cascades without making the continuation a coordination child. Its requester stamp is
  re-sealed against its own UID.
- The continuation goes through normal admission. Snapshot freezing, MCP policy, Session
  lineage checks, credential rotation, and cold workspace resume all apply unchanged.
  Approvals are never carried across turns: a sign-off decision is information delivered
  to the next turn, not authorization of a tool call.

### Bounds and cancellation

- Every wait is bounded by the parked Task's deadline: `spec.timeout`, or the 30-minute
  harness default, measured from creation. The continuation receives only the remainder.
  So the whole chain shares the original deadline and is capped at 32 continuations.
- If the requested wait timeout passes and at least a minute of the Task deadline remains,
  the agent resumes and is told the wait timed out. Otherwise the wait is `Expired` and
  nothing resumes.
- Deleting the parked Task, or setting `orka.ai/cancel-wait`, cancels a non-terminal wait.
  A continuation created in the same window is deleted.
- An undecided sign-off is closed as cancelled or expired. The store allows one terminal
  event per approval, so a racing human decision either wins (and is delivered) or is
  rejected with a conflict.

### WaitingForCapacity

- Admission blocked only by exhausted capacity is reported as the execution reason
  `WaitingForCapacity` and a `WaitingForCapacity` Task condition. The causes are a full or
  non-admitting RuntimePool, a runtime 429 rate limit, or an execution-workspace pool
  without admission capacity.
- The generic `AtCapacity` reason remains for other bounded admission retries.
- Substrate `codes.ResourceExhausted` maps to the new retryable workspace error kind
  `ResourceExhausted`. Substrate actor pools report it as `Pending`/`WaitingForCapacity`
  instead of `Failed`.
- All of these remain retryable and bounded by the Task deadline. The
  `orka_waiting_for_capacity_total{resource,reason}` counter records transitions into the
  wait.

## Consequences

- A parked Task's phase is the honest outcome of its own turn (`Succeeded`). The overall
  result lives at the chain's tail. `wait_for_tasks` and the wait reconciler follow verified
  continuation links. Other consumers that read a single Task's phase see the parked turn
  only, and must follow `status.wait` to learn more.
- Scheduled-run history pruning treats a parked run and its continuations as one run: it
  skips the chain while it is unfinished, counts it by its tail's outcome, and prunes it
  through its head. A parked run counts as active for
  `concurrencyPolicy: Forbid`.
- Continuations of delegated children copy the parent label so their tool policy matches,
  but they are not controller-owned by the coordinator. Coordinator operations on them go
  through the parked child's chain.
- An unsealable requester leaves the continuation's connector tools failing closed, like
  an unsealed scheduled run.
- The read-only RuntimeSession release relies on the dispatcher's in-memory binding cache.
  After a controller restart, an undeleted resident runtime from a parked turn stays until
  pool drain, as it would have without parking.
- Deferred:
  - **Parking a pending `approvalRequiredTools` call.** That approval binds the exact call,
    attempt, prompt, runtime fences, and controller epoch, and its approved execution must
    run under the frozen policy of that live call. Moving it to a later turn needs a new
    authorization model.
  - **PR review and CI wakeups.** The GitHub webhook receiver accepts only `issues`,
    `pull_request`, and `ping`, and routes them to repository monitors. Review and check
    events, and routing them to parked Tasks with repository authorization, are a separate
    change. Timer waits plus `check_pull_request_ci` cover the gap.
  - **Gateway conversations, transaction-bound Tasks, and external `runtimeRef` runtimes.**
    These are refused.
