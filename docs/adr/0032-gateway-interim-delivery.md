# ADR 0032: Add bounded, capability-gated gateway interim deliveries

Date: 2026-09-17

## Status

Accepted (`orka.gateway.v1`), including the gateway-only native/ACP
`reply_in_conversation` tool. Extends the inbox/outbox decision in
[ADR 0012](0012-gateway-inbox-outbox-semantics.md), without changing canonical
terminal history ownership in [ADR 0020](0020-gateway-session-canonical-history.md).
Approvals are out of scope and require a separate ADR.

## Context

A gateway Task may need to send a useful intermediate message while it continues
working. Treating that message as a final result would prematurely complete the
turn, append terminal history, or unlock its Session. Sending directly from a
worker would bypass durable routing, receipts, retry bounds, and ordering.

Existing V1 controllers strictly reject unknown capability fields. A capability
extension can preserve behavior for old adapters, but cannot make a new adapter's
advertisement compatible with an old controller.

## Decision

### Add a delivery kind, not a status

Add optional `capabilities.interimDelivery: true` and `kind: "message"` under the
existing `orka.gateway.v1` discriminator. Absent or false means unsupported: the
controller and checker never send a message delivery to that adapter. `final`
and `error` retain their existing terminal meaning. Wire response statuses remain
`delivered`, `retryableError`, and `nonRetryableError`; durable delivery states
are unchanged. Interim messages use the ordinary outbox and stable provider
receipts, not a second delivery channel.

The controller derives all routing from the exact Task UID's admitted durable
event. Execution hosts supply content and a stable internal request ID; the model
supplies only content through the tool described below. The native
worker endpoint uses the existing TokenReview and current Pod/Job/Task UID
fences, with no controller-ServiceAccount or runtime impersonation exception.
Live Task/Gateway/capability checks happen before the authorized SQLite writer.
Live identity failures remain immediate; readiness/capability denial permits only
receipt recovery for the same authorized Running Task. The writer checks exact
event identity and deduplicates before rejecting receipt-only misses with the
original admission error, or checking new-admission eligibility and quota. Job
revocation is checked inside the writer even for receipt recovery. No receipt is
requeued or modified, and changed sanitized text still conflicts.
This is not a transaction spanning Kubernetes and SQLite. Dispatch rechecks
identity and capability before sending.

Idempotency compares sanitized delivery content, not raw request bytes. Reusing
the request ID with content that sanitizes to the same text returns the same
delivery ID and its current status without a second enqueue or quota charge.
“Different content” means different sanitized text and is a conflict. Only the
sanitized text is retained, with no raw-content digest or additional persisted
identity. The HTTP receipt acknowledges durable admission, not provider send.

### Expose a content-only tool, not a general send API

The production native worker and ACP broker register `reply_in_conversation`.
Its only argument is `content`: a required string with schema `minLength: 1`,
`maxLength: 16384`, and `additionalProperties: false`. Execute also enforces the
strict object shape, valid Unicode, nonempty sanitized text, and the stronger
16 KiB UTF-8 **byte** bound. There is no target, recipient, request ID, quota
configuration, or approval argument.

The controller proves durable origin by exact admitted event/Task UID linkage,
not prompt inspection or provenance metadata alone. Pending linkage defers
Job/session configuration. Readiness and interim capability are admission gates,
not origin evidence: their withdrawal does not permanently remove the tool from
an authenticated execution. Explicit tool denials, closed allowlists, and
transaction scopes remain authoritative. Ordinary, delegated, container, and
compatibility-proxy callers do not gain the tool.

Native bootstrap uses an authenticated, content-free origin read, then Execute
uses a read-only budget/replay snapshot before enqueueing. All three native
routes retain current Pod/Job/Task authentication and revocation fences. A
transient origin-service failure can omit the optional tool while normal work
continues; no unavailable response grants identity. Successful origin proof is
not a quota reservation or permission to bypass later admission.

ACP policy and descriptors are frozen before session creation. Built-in
providers preserve their implicit native defaults when adding reply. External
runtime profiles must explicitly opt in and exactly match their registered,
conformed policy; profiles without reply remain final-only. Restoring an existing
frozen session does not add the tool. ACP execution uses the signed broker call's
active prompt/session guard and authorized task-data transaction, not a native
HTTP endpoint or a synthetic Job. Live preparation happens outside the SQLite
writer; durable admission and mutation fences run inside the authorized writer.

The host supplies a stable logical operation identity. The tool derives a bounded,
domain-separated request ID from that identity, namespace, and Task UID, never
from content. Native identity is scoped to the worker execution/model turn/call;
ACP uses the sealed operation ID, not a raw JSON-RPC ID. Same-operation retries
reuse the receipt, while distinct calls with identical text remain distinct.
Regeneration after restart is not an exactly-once guarantee. The authoritative
budget counts all accepted messages and recognizes retained receipts even at
exhaustion; final atomic admission still arbitrates concurrent callers.

Tool success contains only `deliveryID`, current `status`, and `created` in the
normal success envelope, not the submitted text or destination. Known admission
rejections are safe model-visible failures. Uncertain transport/backend outcomes
must not be treated as definite rejection or retried with a fresh operation ID.
ACP retains both the gateway receipt ledger and the consequential external-effect
ledger, including its `OutcomeUnknown` fence. No approval stop or terminal
lifecycle transition is added.

### Bound storage and preserve terminal ownership

The default is **10 distinct accepted messages per Task**, configured by
`--gateway-interim-messages-per-task`. Failed and expired messages still consume
the lifetime quota. Each message is at most **16 KiB of UTF-8 text**, checked
before sanitization without silent truncation. The **64 KiB terminal text bound**
is unchanged.

Interim text stays in delivery records. It is not written to the Task CR, final
result, or canonical Session transcript. Admission and receipts do not complete
the event or Task, add terminal Task correlation annotations, or unlock the
Session. `spec.prompt` remains empty. Only normal terminal projection appends
final/error history and releases the turn reservation.

No SQLite schema or Task/Session lifecycle changes are needed. The existing
`created_at` field is advanced monotonically within the event when necessary,
under the writer, to preserve admission order even with tied or backwards clock
values. Accepted message rows are retained for the owning event's lifetime, so
retention cannot reset quota or erase ordering evidence.

### Drain or abandon; never revive behind later work

Messages drain in admission order before the event's final/error delivery. A
live predecessor blocks the next delivery. A permanently rejected, exhausted,
or expired message is abandoned so the terminal response can proceed; failed
progress must not suppress the final answer forever. Withdrawing capability
also prevents a queued message from being posted.

Manual retry of a message is forbidden once **any later delivery has started**,
including a Sending or uncertain attempt and a subsequently expired/abandoned
attempt. An operator cannot revive an earlier message behind a later send or
final. Terminal manual-retry behavior remains unchanged. At-least-once delivery
still requires adapter deduplication: replaying an old message ID after terminal
returns the original receipt without a new provider send. The reference fixture
rejects a *new* message after terminal and records actual send order in-process.

### Controller-first compatibility

Apply CRDs, upgrade controllers, then enable interim advertisement on adapters.
Old adapters continue final/error-only operation with the new controller.
New adapters advertising this field are **incompatible with older strict V1
controllers**, even though the discriminator stays the same. This is a
coordinated additive kind/capability rollout, not general V1 forward compatibility
or reverse-skew support.

Before rolling back a controller, pause new work, settle or abandon outstanding
interim deliveries using the new controller, and disable the adapter capability
advertisement. The reference adapter advertises true by default; use
`--interim-delivery=false` to omit it before an older-controller rollback.
Disabling advertisement alone does not make an old dispatcher safe for retained
pending message rows. Normal database/CRD rollback compatibility checks still
apply; the unchanged schema is not proof of behavioral downgrade safety.

## Consequences

- Long-running turns can send bounded progress without changing terminal state
  or canonical history. Delivery records remain the operator view of progress.
- An interim message may never reach the provider if abandoned; terminal work
  progresses rather than waiting indefinitely. Manual recovery is deliberately
  narrower for messages than for terminal deliveries.
- Capability-aware conformance sends two distinct messages before its final,
  checks duplicate receipts including replay after final, and tests the smaller
  message bound. A private delivery fixture can therefore produce **three real
  visible messages**, not just one, and must use an authorized test event that
  has not already received terminal delivery. Routing/privacy protections remain
  unchanged; non-capable adapters receive no message probes.
- The three-spec Gateway live E2E suite retains external-v2 final-only/no-Job
  coverage and uses a deterministic **test-only** native worker with genuine
  controller-created identities. That fixture authenticates origin and executes
  the production reply tool and reusable native client, including budget and
  replay, before releasing final. It requires an adapter receipt while the Task
  is still Running; the unsupported-capability case sends no message and still
  delivers final.
- The fixture does not exercise model-driven registration or a live ACP tool
  call. Native worker unit/integration tests cover actual registration,
  advertisement, dispatch, and normal-loop continuation. ACP integration tests
  use real SQLite, a signed broker capability, an active prompt, the retained
  prompt/session guard, and no Job. Kubernetes resources and broker credential
  resolution use test fixtures, not a live RuntimePool/provider. Tagged
  compilation and local integration passes are not a live-cluster E2E result;
  the Gateway workflow supplies that proof when its three specs actually run.
