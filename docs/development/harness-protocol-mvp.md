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
| `/v1/turns/{turnID}/continue` | `POST` | Accept `ContinueTurnRequest` carrying Orka-brokered tool results and return `ContinueTurnResponse` when the continuation profile is advertised. |
| `/v1/turns/{turnID}/cancel` | `POST` | Accept `CancelTurnRequest` and return `CancelTurnResponse`. |

Every DTO carries `version: "orka.harness.v1"`. A missing or unsupported version is a deterministic validation error. Provider-specific fields, including Agent Substrate actor identifiers, are excluded from base request DTOs and belong only in provider capability metadata/status.

### Required turn fields

`StartTurnRequest` requires namespace, task name, session name, runtime session id, turn id, correlation id, deadline, and a verified auth identity subject or username. Tool and approval policies are safe object references. Raw TxTokens are not valid DTO fields. Resolved literal credentials destined for a controller-managed local runtime subprocess (provider API keys, git tokens already read from a Secret by the controller) ARE permitted in `input.env` (`TurnEnvVar`): that request body is equivalent to mounting a Secret into the wrapper pod. Remote execution backends must not receive production Orka Tool credentials; governed tool access is through brokered Orka calls. When brokered tools are allowed, `input.tools` carries only safe schema metadata: tool name, description, brokered class, and JSON parameters. It intentionally omits downstream URLs, headers, Secret refs, and credentials.

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
  "toolExecutionMode": "brokered",
  "input": {
    "prompt": "summarize this incident",
    "tools": [{
      "name": "support-ticket-lookup",
      "description": "Look up sanitized support evidence",
      "brokeredClass": "read",
      "parameters": {"type": "object"}
    }]
  }
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
| `ApprovalRequested` | `AgentRuntimeCommandStarted` | Runtime-originated approval frames are diagnostics only; Orka creates canonical `ApprovalRequested` events when broker policy requires approval. |
| `TurnCompleted` | `AgentRuntimeCompleted` | Terminal turn metadata is included in content. |
| `TurnFailed` | `AgentRuntimeFailed` | Severity is forced to `error`. |
| `TurnCancelled` | `AgentRuntimeCancelled` | Cancellation is terminal for the harness turn, but not controller-owned task cancellation. |
| `RuntimeLog` | `AgentRuntimeCommandStarted` | Used as a safe diagnostic/log event. |
| Unknown frame | `AgentRuntimeCommandStarted` warning | Does not panic; produces a safe diagnostic event. |

Redaction/truncation runs in the harness mapper and again at the event store boundary. Secret-looking fake frames in conformance tests must persist only redacted values.

### Tool execution modes

- `observed`: the remote execution backend may use its own internal tools; Orka records lifecycle, output, and terminal results but cannot govern backend-internal side effects.
- `brokered`: the backend requests Orka Tool execution through Orka. Orka owns authorization, approval, idempotency, credential resolution, execution/brokering, and audit. The idempotency key is `runtimeSessionID:turnID:toolCallID`; duplicate requests must return the same result or a deterministic conflict. Approval-required brokered calls emit approval events and do not execute until approved.

Capabilities use a small mandatory core plus optional profiles. `toolExecutionModes` advertises `observed` and/or `brokered`. Brokered runtimes may additionally advertise `brokeredToolClasses` (`read`, `write`, `coordination`), `supportsContinuation`, `supportsArtifacts`, `maxTurnSeconds`, and `maxOutputBytes`. Foundry-, AgentKit-, or backend-native identifiers belong in adapter-owned metadata, not in the Orka-facing contract.

Structured task results may use the `workers/common.StructuredResult` JSON envelope. In addition to summary/verdict/diff metadata, the envelope supports a generic `data` object for machine-readable payloads and `artifacts` references for larger outputs. `wait_for_tasks` preserves `data` when it fits the inline bound and propagates artifact references; oversized data is replaced with an explicit truncation marker so large payloads can move to artifacts instead of task summaries.

Brokered coordination tools follow the same governance path as Tool CRDs. `delegate_task` supports explicit `agentNamespace` and `taskNamespace` fields: child tasks stay in the parent/task namespace by default, while the target Agent may live in a namespace-local facade or an allowed catalog namespace. The legacy `namespace` field remains a compatibility shortcut for callers that intentionally want both lookup and child task creation in the same namespace.

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
- Raw secrets, raw TxTokens, environment dumps, cookies, API keys, and JWTs must not appear in **persisted or observable** surfaces: Task status, persisted annotations, execution events/frames, logs, or trace output. Resolved literal credentials MAY be carried in the in-memory `StartTurnRequest.input.env` solely as the controller-to-wrapper delivery channel (see "Required turn fields"); the wrapper must not log the request and should drop `input.env` from retained turn state once child env is materialized. Raw TxTokens are disallowed even on the delivery channel — use owner-referenced child Secrets and fail-closed TTS exchanges. Confidentiality of the delivery channel in transit (TLS/mTLS) is a deployment-posture concern.
- Cross-namespace runtime reuse is denied by ownership validation.
- Lifecycle transitions and cleanup failures must be evented with safe metadata only.

### Conformance contract

The reusable conformance suite in `internal/harness/harnesstest` verifies health, capabilities, successful turns, failed turns, cancellation, invalid/unknown frames, redaction, and client timeout behavior against any provider factory. The fake harness server covers success, failure, delayed output, long-running turns, cancellation, invalid frames, and secret-looking output.
