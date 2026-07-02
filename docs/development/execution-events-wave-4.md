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

## Health and failure modes

The API `/readyz` response includes an `executionEvents` check when an execution event store is configured. SQLite-backed
deployments use the same store health query as the rest of the API persistence layer, so event-store database failures are
visible as readiness failures. If event storage is not configured, readiness reports `executionEvents: not_configured` and
event list/stream/write endpoints return `501 Not Implemented`.

Worker event writes are best-effort for lifecycle telemetry, but approval-gated tools fail closed when they cannot attach
an approval request to a task event stream. Event append/list/stream failures are logged with namespace, task/session, and
event type metadata where available; raw event payloads are not logged.

## Worker event semantics

Worker event emission is best-effort. Recorder failures are warning-only and must not change task result behavior. Controller lifecycle events are also best-effort; event write failures are logged and do not block status transitions.

Minimum P0 producers:

- Controller lifecycle: `TaskJobCreated`, `TaskStarted`, and terminal `TaskSucceeded`/`TaskFailed`/`TaskCancelled`.
- Managed general/container worker: `WorkerStarted`, `ContainerCommandStarted`, `ContainerCommandCompleted` or
  `ContainerCommandFailed`, `ResultSubmitted`, and worker completed/failed. Command events contain safe metadata only:
  executable basename, argument count, duration, exit code, and stdout/stderr byte counts. They intentionally omit raw
  command output and full argument values because both can contain credentials.
- AI worker: `WorkerStarted`, model request started/completed or failed, tool call started/completed or failed when used, `ResultSubmitted`, and worker completed/failed.
- Agent CLI runtime: `WorkerStarted`, workspace preparation started/completed or failed, `AgentRuntimeStarted`, `AgentRuntimeCommandStarted`, runtime completed/failed, `ResultSubmitted`, and worker completed/failed.

For custom-image tasks where the image entrypoint replaces Orka's managed general worker process, Orka can still emit
controller lifecycle events, but it cannot observe the container's internal command start/completion boundary unless the
image calls the worker event API itself or runs through the managed wrapper.

## Retention and deletion behavior

P0 chooses bounded task-coupled retention: execution events are deleted when the owning Task is deleted. This matches existing result/artifact cleanup behavior and keeps test and local SQLite stores from growing without bound.

Configurable audit retention is deferred. Follow-up note: **owner:** controller/API maintainers; **risk:** deleting a Task also deletes its event audit trail, so operators needing durable event audit should export or retain Task objects until configurable retention lands. Tracked by GitHub issue #195 before this behavior is promoted beyond the bounded local-store default.

Tests can clean event data with the store `DeleteExecutionEvents(namespace, "task", taskName)` API or by deleting the Task through the controller cleanup path.

## Deleted-session event semantics

SQLite stores keep a small tombstone read model for tasks that were attached to a session at deletion time. Deleting a
session removes the transcript and clears that session from existing task events so the public session timeline disappears.
If an old task emits late lifecycle events after deletion, the store strips the deleted session name before assigning a
session cursor. If a new session is later created with the same name, late events from the old task remain excluded while
events from new task names can populate the recreated session timeline. Reusing the same task name after deleting the old
session remains excluded until a future generation-aware task identity is introduced.

The tombstone stores only namespace, session name, task name, and deletion time. It does not retain raw transcripts,
event payloads, or credentials.

## Session fork semantics

Session fork is a logical/context fork, not a workspace snapshot. `POST /api/v1/sessions/{id}/fork` validates a session
checkpoint (`afterSeq`), creates a new session in the same namespace, and records a system provenance message containing
bounded, sanitized session event summaries up to that checkpoint. The API can optionally create a seed task that points at
the forked session; seed tasks are annotated with the source session and checkpoint. Cross-namespace forks are rejected by
the normal namespace resolution rules. Physical workspace clone/snapshot support is intentionally deferred.

## Event-derived SLO metrics

The execution event store derives low-cardinality SLO metrics from durable start/end pairs as events are appended. Current
measurements are `task_to_worker_start`, `model_request`, `tool_call`, `workspace_preparation`, and `agent_runtime`.
Latency observations are labelled only by measurement and result (`success` or `failure`); failure counters are labelled
by measurement and terminal failure event type. Task names, session names, event IDs, and tool call IDs are used only for
in-process de-duplication/correlation and are not Prometheus labels.

## Security and redaction caveats

Event payloads are redacted and size-bounded before persistence and again at worker/API boundaries. The redactor covers obvious bearer tokens, JWT-like tokens, API-key fields, cookie headers, transaction-token headers, and GitHub/OpenAI/Anthropic-looking token prefixes. Redacted values use the stable marker `[REDACTED]`; oversized fields preserve truncation metadata without exposing raw secret lengths.

Operators should still treat event summaries and payload metadata as public to authorized task readers. Do not intentionally place credentials, raw transcripts, transaction tokens, cookies, or API keys in event payloads.

## Approval-gated high-risk tools

The first approval-gated high-risk action is `create_pull_request`. When the tool is executed with an approval-capable
task context, it emits `ApprovalRequested` with a deterministic approval ID before reading GitHub credentials or calling
the GitHub mutation API. The request metadata includes the target task, resolved non-secret repository identity, GitHub API base URL, head/base branches, title, timeout, tool call ID,
and a safe risk summary; it intentionally omits PR body text and credentials.

Retries with the same tool call, arguments, and resolved repository target reuse the same pending approval instead of appending duplicate requests.
If the task workspace resolves to a different repository/API target, Orka creates a fresh approval request and does not call GitHub with the stale approval. Approved requests proceed with PR creation. Declined, expired, or cancelled approvals return a structured denial and do
not call GitHub. Approval terminal events (`ApprovalApproved`, `ApprovalDeclined`, etc.) are still written only by the
approval decision API.
