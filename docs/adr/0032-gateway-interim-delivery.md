# ADR 0032: Add bounded, capability-gated gateway interim deliveries

Date: 2026-09-17

## Status

Accepted for W76 PR1 (`orka.gateway.v1`). Extends the inbox/outbox decision in
[ADR 0012](0012-gateway-inbox-outbox-semantics.md), without changing canonical
terminal history ownership in [ADR 0020](0020-gateway-session-canonical-history.md).
Production native/ACP agent-facing tools are a separate PR2, blocked on PR1 merge.
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
event. Callers supply only content and a stable internal request ID. The native
worker endpoint uses the existing TokenReview and current Pod/Job/Task UID
fences, with no controller-ServiceAccount or runtime impersonation exception.
Live Task/Gateway/capability checks happen before the authorized SQLite writer;
the writer atomically checks event eligibility, deduplicates, and enforces quota.
This is not a transaction spanning Kubernetes and SQLite. Dispatch rechecks
identity and capability before sending.

Idempotency compares sanitized delivery content, not raw request bytes. Reusing
the request ID with content that sanitizes to the same text returns the same
delivery ID and its current status without a second enqueue or quota charge.
“Different content” means different sanitized text and is a conflict. Only the
sanitized text is retained, with no raw-content digest or additional persisted
identity. The HTTP receipt acknowledges durable admission, not provider send.

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
- Live E2E retains external-v2 coverage and uses a deterministic **test-only**
  native worker image with genuine controller-created identities. The fixture
  holds final behind a local release fence so the suite can observe a delivered
  interim receipt while the Task is still Running. It is not the PR2 tool.
