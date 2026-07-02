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

## Non-Substrate provider MVP

The first provider implementation is the boring Kubernetes Service HTTP provider. A Task can opt into a harness-backed
turn with the annotation:

```yaml
metadata:
  annotations:
    orka.ai/harness-endpoint: "http://harness.default.svc.cluster.local:8080"
```

The controller accepts only `http`/`https` Kubernetes Service DNS names in the Task namespace (for example,
`service.<namespace>.svc` or `service.<namespace>.svc.cluster.local`). It rejects raw IPs, user-info URLs, loopback,
link-local, and external hostnames so task authors cannot turn the controller into an arbitrary network client.

When this annotation is present, the Task controller skips Job creation and starts one `orka.harness.v1` turn through the
provider-neutral `TurnRunner`. The controller still owns Task phase transitions and terminal Task events; the harness
frames are mapped into the existing execution event stream, so task event list/stream/trace readers see
`AgentRuntimeStarted`, `ModelMessage`, tool/approval events, and `AgentRuntimeCompleted`/`AgentRuntimeFailed` from the
harness. This MVP is non-resident: each annotated Task runs one turn against the supplied service endpoint. RuntimeSession
reuse and resident daemon/workspace behavior are separate follow-up work.

This path does not require Agent Substrate. It deliberately uses an explicit endpoint annotation as the minimal feature
gate/configuration surface until provider CRD status and RuntimeSession cleanup semantics are stable.

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

`StartTurnRequest` requires namespace, task name, session name, runtime session id, turn id, correlation id, deadline, and a verified auth identity subject or username. Tool and approval policies are safe object references. Raw TxTokens are not valid DTO fields. Resolved literal credentials destined for the runtime subprocess (provider API keys, git tokens already read from a Secret by the controller) ARE permitted in `input.env` (`TurnEnvVar`): the request body is the controller-to-wrapper credential delivery channel, equivalent to mounting a Secret into the wrapper pod. The prohibition below scopes raw secrets out of observable/durable surfaces, not this in-memory request body.

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

Brokered tool calls are accepted at Orka's internal worker API:

```http
POST /internal/v1/harness/tools/{namespace}/{taskName}
```

The request body is `ToolCallRequest`. Orka validates the idempotency key, injects namespace/task context, routes the call
through the central tool registry, and appends `ToolCallStarted` plus terminal `ToolCallCompleted`/`ToolCallFailed` events
with low-cardinality safe metadata. Duplicate requests with the same idempotency key and body return the cached result while the broker cache is live. After controller/API restart, Orka derives the prior terminal tool event from the durable event stream and returns a deterministic `idempotency_already_processed` conflict instead of re-executing side effects or persisting raw tool output as a replay cache. The same key with different input returns an idempotency conflict. Approval-required requests append `ApprovalRequested`
and return `approval_required` without executing the tool.

Brokered tools are disabled by default. A harness-backed Task must explicitly list tools that may be brokered. The first controller-safe built-in brokered tool is `list_tools`:

```yaml
metadata:
  annotations:
    orka.ai/harness-brokered-tools: "list_tools"
```

The internal broker never treats the tool name in the request as authorization; it must match the Task allow-list. `list_tools` runs with the task namespace injected as both the watch namespace and namespace-isolation boundary, so cross-namespace input is denied at the tool layer. Filesystem, code-execution, and network-fetch tools are not brokerable from the controller/API process until a task-scoped workspace or egress proxy exists. The broker rejects `file_read`, `file_write`, `code_exec`, `web_fetch`, and `web_search` even if a Task allow-lists them; safe provider-specific tools can be enabled once they execute outside the controller trust boundary.

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

ADR 0008 is implemented as an internal SQLite-backed `RuntimeSessionStore` before exposing a CRD. The store persists
runtime session owner metadata, provider kind, state, cleanup policy, active task, idle timeout, max lifetime, and update
timestamps. It validates state transitions, reuses an existing runtime only within the same namespace/session/provider
owner tuple, denies cross-namespace lookups by key, and provides idle-timeout cleanup for inactive `Idle`, `Retained`, and `Suspended` sessions. Claimed runtime sessions default to a 30-minute idle timeout. The controller manager runs a runtime-session cleanup loop every five minutes by default (`--runtime-session-cleanup-interval`; set to `0` to disable) and logs cleanup failures without blocking task reconciliation. Public Kubernetes API surface for RuntimeSessions remains deferred until provider semantics and cleanup observability stabilize.

Harness-backed Tasks can opt into resident runtime reuse with:

```yaml
metadata:
  annotations:
    orka.ai/harness-reuse-policy: "retain"
```

When set, successful harness turns release the runtime session to `Retained` instead of deleting it. A later Task in the
same namespace/session/provider claims that retained runtime session and sends the same `runtimeSessionID` to the harness,
allowing a healthy non-Substrate harness daemon to keep session-local state/workspace warm. The fake provider test suite
proves this by writing state in turn 1 and reading it back in turn 2 through the same retained runtime session. Failed or
unhealthy runtime sessions are not selected for reuse; the next claim creates a replacement. This is still logical runtime
reuse for the Kubernetes Service provider. Physical workspace snapshot/clone semantics remain deferred.

### Security requirements

- Harness control calls are namespace-scoped and authenticated by Orka; per-turn credentials must be short-lived and scoped to the task/session.
- Raw secrets, raw TxTokens, environment dumps, cookies, API keys, and JWTs must not appear in **persisted or observable** surfaces: Task status, persisted annotations, execution events/frames, logs, or trace output. Resolved literal credentials MAY be carried in the in-memory `StartTurnRequest.input.env` solely as the controller-to-wrapper delivery channel (see "Required turn fields"); the wrapper must not log the request and should drop `input.env` from retained turn state once child env is materialized. Raw TxTokens are disallowed even on the delivery channel — use owner-referenced child Secrets and fail-closed TTS exchanges. Confidentiality of the delivery channel in transit (TLS/mTLS) is a deployment-posture concern.
- Cross-namespace runtime reuse is denied by ownership validation.
- Lifecycle transitions and cleanup failures must be evented with safe metadata only.

### Conformance contract

The reusable conformance suite in `internal/harness/harnesstest` verifies health, capabilities, successful turns, failed turns, cancellation, invalid/unknown frames, redaction, and client timeout behavior against any provider factory. The fake harness server covers success, failure, delayed output, long-running turns, cancellation, invalid frames, and secret-looking output.
