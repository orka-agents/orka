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

---

## Frozen MVP contract (`orka.harness.v1`)

The implementation contract is now represented by `internal/harness` and is frozen for the first backend-neutral provider. The MVP transport is **HTTP control endpoints plus an SSE frame stream**:

| Route | Method | Purpose |
| --- | --- | --- |
| `/v1/health` | `GET` | Return `HealthResponse`. |
| `/v1/capabilities` | `GET` | Return `CapabilitiesResponse`. |
| `/v1/turns` | `POST` | Accept `StartTurnRequest` and return `StartTurnResponse`. |
| `/v1/turns/{turnID}/events?afterSeq=N` | `GET` | Stream `HarnessEventFrame` values as SSE `data:` records. |
| `/v1/turns/{turnID}/cancel` | `POST` | Accept `CancelTurnRequest` and return `CancelTurnResponse`. |

Every DTO carries `version: "orka.harness.v1"`. A missing or unsupported version is a deterministic validation error. Provider-specific fields, including Agent Substrate actor identifiers, are excluded from base request DTOs and belong only in provider capability metadata/status.

### Required turn fields

`StartTurnRequest` requires namespace, task name, session name, runtime session id, turn id, correlation id, deadline, and a verified auth identity subject or username. Tool and approval policies are safe object references; raw credentials and TxTokens are not valid DTO fields.

```json
{
  "version": "orka.harness.v1",
  "namespace": "default",
  "taskName": "task-a",
  "sessionName": "session-a",
  "runtimeSessionID": "runtime-a",
  "turnID": "turn-a",
  "correlationID": "corr-a",
  "deadline": "2026-06-11T12:00:00Z",
  "authIdentity": {"subject": "user:test"},
  "toolPolicyRef": {"name": "default-tools"},
  "approvalPolicyRef": {"name": "default-approvals"},
  "eventCursor": 7,
  "toolExecutionMode": "observed",
  "input": {"prompt": "summarize this repository"}
}
```

### Frame-to-event mapping

Harness frames are mapped to existing Orka execution event types so task/session streams and trace read models remain the source of history:

| Harness frame | Execution event type | Notes |
| --- | --- | --- |
| `TurnStarted` | `AgentRuntimeStarted` | Turn/session ids appear in event content metadata. |
| `RuntimeOutput` | `ModelMessage` | Runtime output is sanitized before persistence. |
| `ToolCallRequested` | `ToolCallStarted` | `toolName` and `toolCallID` are preserved. |
| `ToolResultReceived` | `ToolCallCompleted` | Failed tool results can carry safe error content. |
| `ApprovalRequested` | `ApprovalRequested` | Reuses durable approval event lifecycle. |
| `TurnCompleted` | `AgentRuntimeCompleted` | Terminal turn metadata is included in content. |
| `TurnFailed` | `AgentRuntimeFailed` | Severity is forced to `error`. |
| `TurnCancelled` | `AgentRuntimeCancelled` | Cancellation is terminal for the harness turn, but not controller-owned task cancellation. |
| `RuntimeLog` | `AgentRuntimeCommandStarted` | Used as a safe diagnostic/log event. |
| Unknown frame | `AgentRuntimeCommandStarted` warning | Does not panic; produces a safe diagnostic event. |

Redaction/truncation runs in the harness mapper and again at the event store boundary. Secret-looking fake frames in conformance tests must persist only redacted values.

### Tool execution modes

- `observed`: the harness executes tools itself and emits tool lifecycle frames. This is compatible with opaque runtimes but Orka cannot prevent side effects before observation.
- `brokered`: the harness requests tool execution through Orka. The idempotency key is `runtimeSessionID:turnID:toolCallID`; duplicate requests must return the same result or a deterministic conflict. Approval-required brokered calls emit the existing approval events and do not execute until approved.

### RuntimeSession lifecycle

`internal/harness` defines the backend-neutral state machine:

```text
Pending -> Booting -> Ready -> TurnRunning -> Idle -> Releasing -> Deleted
                                      |          |            +-> Retained
                                      |          |            +-> Suspended
                                      |          +-> Deleting -> Deleted
                                      +-> Failed/Unhealthy -> Deleting
```

Supported states are `Pending`, `Booting`, `Ready`, `TurnRunning`, `Idle`, `Releasing`, `Retained`, `Suspended`, `Deleting`, `Deleted`, `Failed`, and `Unhealthy`. Runtime sessions require namespace, session name, provider, cleanup policy, and owner metadata. Cleanup policies are `delete`, `retain`, and provider-capability-gated `suspend`.

### Security requirements

- Harness control calls are namespace-scoped and authenticated by Orka; per-turn credentials must be short-lived and scoped to the task/session.
- Raw secrets, raw TxTokens, environment dumps, cookies, API keys, and JWTs must not appear in request DTOs, status, events, logs, or trace output.
- Cross-namespace runtime reuse is denied by ownership validation.
- Lifecycle transitions and cleanup failures must be evented with safe metadata only.

### Conformance contract

The reusable conformance suite in `internal/harness/harnesstest` verifies health, capabilities, successful turns, failed turns, cancellation, invalid/unknown frames, redaction, and client timeout behavior against any provider factory. The fake harness server covers success, failure, delayed output, long-running turns, cancellation, invalid frames, and secret-looking output.
