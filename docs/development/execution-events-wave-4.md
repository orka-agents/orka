# Execution events P0 operator guide

Wave 4 P0 eventing is task-scoped and does **not** require Agent Substrate. Normal Kubernetes Job-backed tasks, AI workers, and agent CLI workers emit best-effort events to the controller; the controller persists those events and exposes list and SSE stream APIs.

## Endpoints

### Internal worker event submit

Workers append events through the internal API:

```http
POST /internal/v1/events/{namespace}/task/{taskName}
Content-Type: application/json
```

Example payload:

```json
{
  "type": "WorkerStarted",
  "severity": "info",
  "taskName": "my-task",
  "summary": "worker started",
  "content": {"runtime": "codex"}
}
```

The route accepts only the P0 `task` stream type. The caller must be authorized for the target namespace; service-account callers from a different namespace are rejected.

### Public task event list

Clients query durable events for a task with:

```bash
curl '/api/v1/tasks/my-task/events?namespace=default&after=0'
```

Response shape:

```json
{
  "namespace": "default",
  "streamType": "task",
  "streamID": "my-task",
  "afterSeq": 0,
  "latestSeq": 3,
  "events": [
    {
      "id": "default/task/my-task/1",
      "namespace": "default",
      "streamType": "task",
      "streamID": "my-task",
      "seq": 1,
      "type": "TaskJobCreated",
      "severity": "info",
      "taskName": "my-task",
      "summary": "Job orka-task-my-task created",
      "createdAt": "2026-06-11T10:00:00Z"
    }
  ]
}
```

### Public task SSE stream

Clients stream replay and live events with:

```bash
curl -N '/api/v1/tasks/my-task/stream?namespace=default&after=42'
```

Each execution event frame uses the event sequence as the SSE `id`:

```text
id: 43
event: execution_event
data: {"seq":43,"type":"WorkerCompleted",...}
```

When a terminal task event is observed (`TaskSucceeded`, `TaskFailed`, or `TaskCancelled`), the stream sends a final completion frame and closes:

```text
id: 44
event: stream_complete
data: {"lastSeq":44,"type":"TaskSucceeded"}
```

Idle streams emit heartbeat comments (`: heartbeat`) so clients can detect an open connection.

## Ordering and resume semantics

- `seq` is monotonically increasing per `(namespace, streamType, streamID)`.
- `after=N` returns or streams only events where `seq > N`.
- To resume without duplicates, clients should store the largest execution-event `seq` they processed and reconnect with `after=<lastSeq>`.
- The list endpoint returns `latestSeq` so pollers can checkpoint even when the returned page is empty.

## Worker event semantics

Worker event emission is best-effort. Recorder failures are warning-only and must not change task result behavior. Controller lifecycle events are also best-effort; event write failures are logged and do not block status transitions.

Minimum P0 producers:

- Controller lifecycle: `TaskJobCreated`, `TaskStarted`, and terminal `TaskSucceeded`/`TaskFailed`/`TaskCancelled`.
- AI worker: `WorkerStarted`, model request started/completed or failed, tool call started/completed or failed when used, `ResultSubmitted`, and worker completed/failed.
- Agent CLI runtime: `WorkerStarted`, workspace preparation started/completed or failed, `AgentRuntimeStarted`, `AgentRuntimeCommandStarted`, runtime completed/failed, `ResultSubmitted`, and worker completed/failed.

## Retention and deletion behavior

P0 chooses bounded task-coupled retention: execution events are deleted when the owning Task is deleted. This matches existing result/artifact cleanup behavior and keeps test and local SQLite stores from growing without bound.

Configurable audit retention is deferred. Follow-up note: **owner:** controller/API maintainers; **risk:** deleting a Task also deletes its event audit trail, so operators needing durable event audit should export or retain Task objects until configurable retention lands.

Tests can clean event data with the store `DeleteExecutionEvents(namespace, "task", taskName)` API or by deleting the Task through the controller cleanup path.

## Security and redaction caveats

Event payloads are redacted and size-bounded before persistence and again at worker/API boundaries. The redactor covers obvious bearer tokens, JWT-like tokens, API-key fields, cookie headers, transaction-token headers, and GitHub/OpenAI/Anthropic-looking token prefixes. Redacted values use the stable marker `[REDACTED]`; oversized fields preserve truncation metadata without exposing raw secret lengths.

Operators should still treat event summaries and payload metadata as public to authorized task readers. Do not intentionally place credentials, raw transcripts, transaction tokens, cookies, or API keys in event payloads.
