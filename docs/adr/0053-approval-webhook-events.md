# 53. Approval webhook events

Date: 2026-11-16

## Status

Accepted. Amends [ADR 0042](0042-signed-durable-task-webhooks.md), which left
non-terminal events open, with opt-in approval notices on the same signed,
durable delivery path. It answers, for Task webhooks, the approval notification
that [ADR 0032](0032-gateway-interim-delivery.md) deferred to a separate ADR.
Approval delivery through gateway adapters stays open.

## Context

A human approval is the one point where an agent Task waits on a person.
Reviewers find out by polling the approvals API or the UI, and review windows
are short: brokered ACP reviews expire after about ten minutes. A receiver that
already gets the signed terminal notice wants to be told when an approval is
requested and how it ended.

Approvals are event-sourced in the Task's execution event stream and derived by
`approvals.Derive`. Four producers write them:

- the AI worker's approval gate and `request_approval` tool (through the
  internal event API);
- the ACP MCP broker, for brokered tool calls;
- the wait controller, for a parked turn's sign-off (ADR 0041);
- the approval decision API.

The store keeps one terminal event per approval, so the first decision wins.
Only brokered reviews and parked sign-offs carry an enforced `expiresAt`. A
parked sign-off's expiry and cancellation are durable events. A brokered review
is expired by the clock: the API and the broker treat it as expired once its
`expiresAt` passes, and the broker records that only while it still owns the
call. AI-worker approvals have no expiry and resolve only by explicit decision.
A parked sign-off stays decidable after its Task reached `Succeeded`. Stream
names are reused by later Tasks with the same name. Runtime-native ACP
`session/request_permission` prompts are answered from frozen policy and never
reach a person, so they are not approvals here.

## Decision

**Opt in per Task.** `spec.webhookEvents` is a set of at most two values,
`task.approval_requested` and `task.approval_resolved`. Empty or absent keeps
ADR 0042's terminal-only behaviour. Terminal events cannot be listed, so they
cannot be removed. A non-empty set requires `spec.webhookURL` (CEL). The field
is an execution input: it is frozen with the rest of the spec once execution
authority is recorded, and `buildTaskContinuation` copies it with the spec.

**Lifecycle.** An approval has one `requested` transition, from its
`ApprovalRequested` event, and at most one resolution, from its first terminal
event: `approved`, `declined`, `expired`, or `cancelled`. Notices report only
durable events. The controller never invents a resolution. It does not cancel
open approvals when a Task finishes: a parked sign-off is still decidable, and
an AI-worker approval left pending by a finished Task never resolves.

**Expiry is made durable before it is notified.** For a brokered review that
is still pending five seconds after its `expiresAt`, the controller appends
`ApprovalExpired` once (actor `orka-controller`) and requeues the Task for that
moment. That matches what the API and broker already report. The store's
one-terminal-event rule makes this the linearization point: a decision that
committed first wins and the append is refused. The five seconds let a live
broker record its own expiry first. The append copies the request's Session
association, so, like the broker's recovery, it runs under the namespace
cleanup fence against a fresh read of the retained request. Timeline or
Session cleanup that commits after the read refuses the append. The read is
then repeated, so the controller never recreates a cleaned request or restores
a cleaned Session association. The controller records nothing for approvals
without an enforced expiry.

**Projection.** During Task reconcile, in every phase, the controller lists
the Task's approval events and keeps only approvals whose request names this
Task's UID. A transition whose stream sequence is above the cursor
`status.approvalWebhooks.projectedSeq` becomes a notice in
`status.approvalWebhooks.notices`, and the cursor advances. Notices and cursor
are written in one status update before any attempt. Each notice has:

- a deterministic delivery ID, UUIDv5 of the Task UID, the approval ID and the
  transition;
- `occurredAt` from the event that recorded the transition;
- the stream sequence;
- the safe identifiers the payload carries.

A transition at or below the cursor is never projected again, and a restarted
controller recomputes the same IDs. This relies on per-stream sequences being
visible in order, as the event store's other cursors do. It also relies on a
Task's stream never being emptied while the projection runs for it: only the
Task's deletion finalizer deletes the stream, and a Task being deleted is no
longer projected. A later Task with the same name has a new UID and an empty
cursor. A transition whose event is not selected still advances the cursor.

**Selection boundary.** The spec stays mutable for Tasks that record no
execution authority, such as native AI Tasks without a soul binding, so
approval events can be selected after approvals already happened.
`status.approvalWebhooks.tracking` records that the cursor has followed the
stream since the Task selected an approval event. The controller sets it when
it initializes the status of a Task that already selects one: nothing has run
yet, so every transition of the Task is projected. A Task that selects
approval events without it, because it selected them later, has its cursor
raised to the stream's high-water mark at its first projection, which then
sets `tracking`. Earlier history is never notified and cannot latch the
overflow. Observing a Task that selects no approval event clears `tracking`,
so selecting again also starts at the high-water mark and does not replay the
opted-out period. The boundary follows what the controller observes: an
opt-out and re-enable that no reconcile saw in between are treated as
continuous selection.

**Overflow.** The queue holds at most 16 notices, one slot reserved for
overflow. A pending notice is never removed. Settled notices (`Delivered` or
`Failed`) are pruned oldest first to make room, and `pruned` counts them. If 15
pending notices fill the queue, the next transition records one
`task.approval_overflow` notice in the reserved slot, with the stream sequence
of the first transition not notified, and sets `overflowed`. From then on no
transition is notified, `omitted` counts them, and the cursor keeps advancing.
A receiver that gets the overflow notice reads the approvals API. Draining
needs 15 undelivered notices inside one retry window, so the latch is meant to
be rare, not a throttle. A signing outage is held to the same rule by the
signing deadline below: it overflows the queue only when 15 notices are
queued within one signing window.

**Payload.** The body is separate from the terminal body. It carries
`schemaVersion: orka.ai/task-approval-webhook/v1` and only these fields:

- `deliveryId`, `eventType`, `occurredAt`, `streamSeq`;
- `taskUID`, `taskName`, `taskNamespace`;
- `approvalId`, and when present `targetTool`, `severity` and `expiresAt`;
- `resolution` for a resolved notice;
- `parked`.

Identifiers that do not match a fixed safe shape are left out. A notice whose
approval ID does not match is not sent. `expiresAt` appears only when the
approval model enforces it. The body never carries:

- argument previews or digests;
- action, risk summary or reason text;
- bindings or summaries;
- who decided.

The body is rendered from the status record alone. Its digest is checked before
every attempt, and a mismatch retires the notice unsent.

**Delivery.** Approval notices use ADR 0042's machinery unchanged: the
dial-time destination policy, the Ed25519 JWS with `event` set to the notice's
event type and `payload_sha256` over the body, the same headers, the
status-persisted attempt lease, and sanitized errors. Notices are sent in
stream order: a notice waits until every earlier one has settled. That puts a
resolution after its request unless the request's notice failed. One attempt
runs per reconcile, as for the terminal notice. Task reconciles share one
worker, so a slow receiver holds it no longer than a terminal notice does.
Projection runs before the Task's phase work, which the expiry recording and
the history hold rely on; the attempt runs after it, even when the phase work
failed, against an uncached read of the Task. A slow or unreachable receiver
therefore never delays the Task's own scheduling, monitoring or finalization.
The reconcile requeues for the earlier of the phase's requeue and the head
notice's next attempt.

**Retry cap.** Five attempts at 10, 20, 40 and 60 seconds settle within about
three minutes, against eight attempts capped at 15 minutes for terminal
notices. A notice that arrives after a ten-minute review window closed has lost
its value, and stream-order delivery means one failing notice holds back the
Task's later ones only this long.

**Signing outage.** ADR 0042 keeps a notice pending without consuming an
attempt while no signing key is available, so a configuration fix resumes
delivery. For an approval notice that alone would let one unsignable notice
hold back every later one until the queue overflows. Each approval notice
therefore records `signingDeadline` when it is queued: five minutes, the retry
window plus two minutes of grace, rounded up to a whole second. The deadline
lives in the status record, so a restarted controller enforces the same one,
and it is not part of the body. Whenever the keys are unavailable, every
pending approval notice at or past its deadline is retired `Failed` unsent,
with no attempt consumed and the bounded `lastError` "webhook signing key
unavailable past the approval notice window", and the next notice goes ahead.
This also runs before projection, because overflow cannot be undone: a
controller that was stopped or reconciled rarely during the outage never counts
an expired notice toward the 15.
Only the head notice is ever attempted, so no retired notice holds a lease.
A retired notice is never regenerated: the cursor already covers its
transition, and the retained record keeps its delivery ID. A notice recorded
without a deadline, by an earlier controller, gets one from the first pass
that finds the keys unavailable. Terminal notices keep ADR 0042's behaviour
and wait until signing is fixed.

**Prompt reconcile.** Each producer already causes a reconcile:

- the decision API patches the Task's annotations;
- the wait controller's appends are followed by its status write;
- running ACP Tasks reconcile every second;
- running native Tasks reconcile at least every five seconds and on Job events;
- a pending brokered review requeues the Task for its expiry.

**Ordering and lifecycle.** Approval and terminal notices are independent
records. ADR 0042's terminal notice, including the parked-turn deferral, is
unchanged, and a receiver may see it before a late parked resolution.
Removing an event from `spec.webhookEvents`, where the spec is still
mutable, retires its pending notices unsent. Deleting a Task drops its pending
notices, as for the terminal notice.

**History retention.** Scheduled-run history pruning, from both the finished
run and the scheduled parent, keeps a run beyond its history limit while any
Task in its continuation chain has a pending approval notice, or a selected
approval transition above its cursor that projection has not recorded yet.
Only the Task's own reconcile projects and sends its notices, and deleting
the run deletes its stream, so deleting the run after its request notice
alone, as `successfulRunsHistoryLimit: 0` did, lost the resolution. So did a
decision that committed after the reconcile's projection read but before its
pruning, or before the parent's. The unprojected check applies only to a
tracking Task that selects approval events and has not overflowed. It is one
indexed read of the Task's request and terminal approval events past the
cursor, limited to one. Only when that finds one is the stream derived as
projection derives it, so an event that never becomes a transition, such as
a duplicate request or another incarnation's, does not hold the run for good.
The check reads the run's head and follows its verified continuation links
with uncached reads, bounded by the chain limit, and keeps the run when any
Task read fails. A failed event store read keeps it only within the five-minute
approval notice window after the chain's tail completed: projection cannot
read an unavailable store either, and holding beyond that would let an outage
keep every run. A held run still counts toward the limit, so no newer run is
pruned in its place. Retention is retried without polling: the attempt that
settles a Task's last pending notice requeues the Task promptly, and that
reconcile reaches its parent's history enforcement again. A decision lands
with a reconcile of its own (see Prompt reconcile), which projects it, and the
parent's next schedule enforces history again in any case. The hold is
bounded, since an unprojected transition is projected by the Task's next
reconcile and a pending approval notice settles within its retry window or
signing deadline. Terminal notices do not hold back parent-triggered
pruning: the finished run already prunes only after its own terminal notice
settles, and ADR 0042 lets a terminal notice wait indefinitely for signing
keys, so holding on it would let an outage keep every run.

## Consequences

- Receivers must deduplicate on `deliveryId`. Delivery is at-least-once, not
  exactly-once: a crash between send and record repeats an attempt under the
  same ID.
- Receivers must tolerate gaps and order by `streamSeq` within a Task. Gaps
  come from a failed notice, the overflow latch, or a later selection. A
  resolution can arrive without its request when the request predates the
  selection.
- An approval notice is an alert, not authority. Receivers act through the
  approvals API, which revalidates everything.
- The controller now writes `ApprovalExpired` for an expired brokered review
  of an opted-in Task when nothing else did. The approval API already reports
  that state.
- Opted-in Tasks read their approval events on each reconcile and cost two
  status writes per attempt. Legacy Tasks do no new work.
- Task status grows by at most 16 small records.
- An approval notice queued during a signing outage longer than five minutes
  is never sent. The receiver sees a gap and reads the approvals API.
- `approvals.Approval` gains an unserialized `RequestSeq`.
- 2026-11-16: Approval notice attempts run in ADR 0042's delivery runnable,
  not in the Task reconcile worker. Projection still runs before the phase
  work, and the notice is handed off after it. The statement under Delivery
  that Task reconciles share one worker no longer describes delivery: a slow
  receiver holds one delivery worker per attempt, and stream order still holds
  because a Task is never in two workers at once. The settling attempt's status
  write, rather than a one-second requeue, reconciles the Task again for its
  history enforcement.
