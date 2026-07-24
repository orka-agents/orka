# ADR 0014: Separate immutable Task execution outcome from workspace finalization

Date: 2026-07-16

## Status

Accepted.

## Context

The legacy wrapper-first path ties workspace cleanup to Task success. A successful command followed by scrub, suspend, or provider deletion failure can therefore look like a failed attempt and be replayed. Replaying an already-successful side effect is unsafe.

## Decision

`TaskPhaseFinalizing` is a non-terminal phase between workload execution and terminal Task state. `status.executionOutcome` records the workload outcome exactly once and is immutable thereafter. It includes the terminal execution phase, producing attempt, result reference, timestamp, and sanitized message.

The Task state machine is:

- `Pending -> Running` when execution starts.
- `Running -> Finalizing` immediately after Orka durably records `executionOutcome` or when cancellation begins revocation.
- `Finalizing -> Succeeded|Failed|Cancelled` after the attachment is revoked, or after the detach timeout has quarantined the workspace and removed worker credentials.

Full scrub, reset, suspend, delete, retained-data disposition, and provider cleanup continue on `ExecutionWorkspace` after the Task becomes terminal. Their success or failure updates workspace disposition and degraded Task conditions but never changes an existing execution outcome.

Retry rules are outcome-first:

- A recorded `Succeeded` outcome permanently disables Task retry.
- A recorded `Cancelled` outcome is terminal.
- A failed attempt may retry only when policy permits and the old attachment has been revoked; retryable failures are retried before `executionOutcome` is recorded, and a recorded `Failed` outcome is final.
- An operation timeout is not automatically a failed outcome. The controller queries the caller-supplied operation ID. If the outcome remains ambiguous, the workspace is quarantined and the attempt is not blindly replayed in that workspace.
- Controller, worker, or adapter restarts reuse the recorded outcome and operation identity.

Cancellation becomes terminal only after revocation or quarantine-on-timeout. `wait_for_task(s)`, webhooks, events, API views, and UI treat `Finalizing` as non-terminal and may expose degraded finalization metadata with a terminal result.

Status writer ownership remains strict: worker result ingestion or the Task controller records execution outcome; only the Task controller changes Task phase and finalization conditions; adapters write only workspace status.

## Consequences

- A successful Task is never replayed because cleanup failed.
- Task latency waits for authority revocation, not provider disposal.
- Operators can distinguish business execution failure from cleanup/isolation failure.
- Quarantine is the fail-closed resolution for uncertain revocation or cleanup.
