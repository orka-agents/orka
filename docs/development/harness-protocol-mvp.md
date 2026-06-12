# Harness protocol MVP preparation

The harness protocol should build on the event model after task/session traces have burn-in. It must not require Agent Substrate.

## Requirements from current event streams

| Responsibility | Owner |
| --- | --- |
| Task CR creation, phase transitions, Job creation, terminal task status | Controller |
| Worker process start/complete/fail | Worker/harness |
| Model request lifecycle and model messages | Runtime harness |
| Tool call lifecycle | Runtime harness or tool executor |
| Workspace preparation lifecycle | Controller/worker boundary, depending on provider |
| Result submission and artifact upload | Worker/harness |
| Cancellation observed and turn completion | Controller plus harness |

## Protocol sketch

Messages:

- `StartTurn`: task/session metadata, safe prompt/context references, cancellation token, event append target.
- `StreamEvent`: one sanitized execution event payload matching the existing event DTO.
- `CancelTurn`: request cancellation for a running turn.
- `EndTurn`: terminal result metadata and final event sequence observed by the harness.
- `Health`: readiness/liveness and runtime capability surface.

Transport candidates:

- HTTP + SSE for lowest implementation cost and compatibility with current APIs;
- gRPC for typed bidirectional streaming once the contract stabilizes;
- stdin/stdout wrapper for local CLI runtimes.

Auth and namespace rules:

- Harnesses receive namespace-scoped, least-privilege credentials.
- TxTokens and provider credentials must never be placed in event payloads.
- Outbound scope exchanges remain fail-closed.

## Boring MVP

Use a non-Substrate backend first:

1. Wrap one existing agent CLI runtime with a local harness adapter.
2. Adapter emits `StreamEvent` messages to the existing internal event append API.
3. Controller still owns Task/Job lifecycle events.
4. Trace API validates model/tool/result grouping without a resident actor.
5. Only after this is stable should a resident/Substrate provider implement the same protocol.

Deferred:

- resident actor sessions;
- workspace snapshot-aware fork;
- remote harness-backed Agent CRDs;
- full graph layout UI.
