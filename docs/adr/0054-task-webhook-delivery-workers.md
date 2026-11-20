# 54. Deliver Task webhooks from a worker pool, not the Task reconcile

Date: 2026-11-16

## Status

Accepted. Supersedes the parts of [ADR 0042](0042-signed-durable-task-webhooks.md)
and [ADR 0053](0053-approval-webhook-events.md) that run delivery attempts inside
the Task reconcile. Everything else those records decide (signing, the durable
claim and lease, attempt bounds, dial-time checks, supersession, approval
projection, stream order and overflow) is unchanged.

## Context

ADR 0042 sends a terminal notice from the Task reconcile, and ADR 0053 sends
approval notices the same way, one attempt per reconcile. The Task controller
runs a single reconcile worker that every Task shares, and an attempt may take
the full 30-second request timeout. A receiver that hangs therefore stalls all
Task processing: in a live repro, three Tasks pointing at a hanging receiver
delayed three unrelated Tasks' start by 5m15s, until their attempts ran out.
Running the Task reconciler concurrently would remove the stall only by making
that large reconciler parallel for the first time, and N hanging receivers
would still hold N of its workers.

## Decision

- The Task reconcile never sends. It projects approval events and decides that
  a notice is due, then hands the Task to a leader-elected delivery runnable.
- The runnable has a bounded worker pool, `--task-webhook-delivery-workers`
  (default 8, 1–256). A worker re-reads the Task uncached and runs the same
  claim, send and record steps as before, so the persisted lease prevents a
  second replica from sending the same attempt.
- The work queue never gives one Task to two workers, so a Task's notices go one
  attempt at a time and approval notices keep stream order.
- Because the reconciler and other controllers can write the Task's status while
  an attempt is in flight, a conflicting result write is applied again to a
  fresh read only while the record still holds that attempt's claim (same
  attempt count and lease), the event is still selected, the fresh Task names
  the same webhook URL, and it still renders the sent body. Otherwise nothing is
  recorded and the lease governs the next attempt, as after a crash.
- Deselected approval notices are retired by the reconcile that observes the
  opt-out, before any handoff, so a later re-selection never revives them.
- Parent scheduled-run history keeps a run while its terminal notice or any
  approval notice is outstanding, since deleting the run drops its notices. The
  runnable's status writes reconcile the run again once a notice settles.

## Consequences

- A slow or hanging receiver holds one delivery worker per attempt and never
  delays Task reconciles.
- There is no fairness between receivers. When every worker is held by hanging
  receivers, other notices, including approval notices, wait first-come
  first-served; waiting consumes none of their attempts but does use their
  signing window.
- Delivery happens later than the reconcile that made a notice due, so code that
  needs a notice settled must read its status rather than assume the reconcile
  sent it.
- One more controller flag, which the Helm chart does not set; it uses the
  default.
