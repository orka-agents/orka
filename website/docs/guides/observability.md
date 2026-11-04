---
slug: /observability
description: "OpenTelemetry traces, GenAI metrics, and how to follow one task through them."
---

# Observability

Orka emits OpenTelemetry traces and metrics for controller, chat, tool, native AI
worker, and Orka harness v2 controller/supervisor paths when telemetry is enabled. ACP runtime
internals remain observable primarily through durable Task execution/delivery
status, RuntimePool status, bounded events, and structured logs. The GenAI
signals are backend instrumentation: they are exported over OTLP to your
collector/backend and are separate from the Orka React UI.

Telemetry is disabled by default. Disabled mode keeps the hot path on the global
OpenTelemetry no-op providers and does not configure OTLP exporters.

For retained team usage and tokens per merged PR in the dashboard, see
[Usage and PR outcomes](usage-and-pr-outcomes.md). That report records counts
independently of telemetry and includes measurement gaps.

## Enable telemetry

Start the controller with telemetry enabled and point it at an OTLP endpoint.
gRPC is the default exporter protocol:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector.otel.svc:4317 \
  orka-controller --enable-telemetry
```

`--enable-tracing` is kept as a compatible alias and enables the same traces and
metrics. Existing Prometheus metrics on `--metrics-bind-address` continue to
work independently.

For an existing Kubernetes Deployment, set both the controller flag and the
collector environment. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` alone does not
enable telemetry:

:::note[These commands assume a Helm install named `orka`]
That makes the Deployment `orka-controller`. If you installed with
`kubectl apply -f .../deploy/orka.yaml` it is `orka-controller-manager` instead. See
[Troubleshooting](../operations/troubleshooting.md#what-is-my-controller-deployment-called).
:::


```bash
kubectl patch deployment orka-controller -n orka-system --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-telemetry"},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"http://otel-collector.otel.svc:4317"}},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"OTEL_EXPORTER_OTLP_INSECURE","value":"true"}}
]'
```

Use `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` (or signal-specific
`OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` / `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL`)
for HTTP/protobuf collectors. Signal-specific endpoints are also supported:
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` and
`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT`.

When controller telemetry is enabled and a worker-reachable OTLP endpoint is configured, AI worker Jobs receive:

- `ORKA_ENABLE_TELEMETRY=true`
- `OTEL_EXPORTER_OTLP_ENDPOINT` and related standard non-secret OTLP environment variables
- `ORKA_TRACEPARENT` when a Task was created from an already-traced API/chat/tool request

The controller copies only worker-safe OTLP settings into AI worker Pods:

| Copied when set | Not copied |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `OTEL_EXPORTER_OTLP_HEADERS` |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` / `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | `OTEL_EXPORTER_OTLP_TRACES_HEADERS` / `OTEL_EXPORTER_OTLP_METRICS_HEADERS` |
| `OTEL_EXPORTER_OTLP_PROTOCOL` and signal-specific protocol vars | OTLP certificate and client-key env vars |
| `OTEL_EXPORTER_OTLP_INSECURE` and signal-specific insecure vars | `OTEL_RESOURCE_ATTRIBUTES` |
| `OTEL_EXPORTER_OTLP_TIMEOUT` and signal-specific timeout vars | `ORKA_BAGGAGE` |
| `OTEL_EXPORTER_OTLP_COMPRESSION` and signal-specific compression vars | |

Worker endpoint values must be reachable from the worker Pod. Empty,
loopback, and unspecified hosts such as `localhost`, `127.0.0.1`, `::1`, and
`::` are not copied into worker Jobs. If a signal-specific endpoint is
unreachable, its signal-specific overrides are dropped instead of sending a
broken per-signal configuration to the worker.

Telemetry environment for AI workers is controller-owned: task-supplied
`ORKA_ENABLE_TELEMETRY`, `ORKA_TRACEPARENT`, `ORKA_TRACESTATE`, and
`OTEL_EXPORTER_OTLP*` values in `spec.env` are ignored. Generic container Tasks
preserve user-supplied telemetry env, because Orka does not instrument arbitrary
container processes.

Managed ACP RuntimePools are not per-Task Jobs and do not inherit `Task.spec.env`.
The controller emits `acp.prompt`, `acp.session.create`,
`acp.session.continue`, and `acp.publication.reconcile` spans from the Task's
trace annotations. It also exports RuntimePool desired/ready replica,
resident-session, active-prompt, queued-Task, admission-state, and completed
scale-to-zero metrics on the Prometheus metrics endpoint.

With controller telemetry enabled and a worker-reachable trace endpoint,
managed supervisors receive `ORKA_ENABLE_TELEMETRY=true` and non-secret OTLP
endpoint, protocol, insecure and compression settings. Generic and trace-specific
endpoints retain their normal SDK semantics; trace-specific settings take precedence.
A missing or invalid trace endpoint disables supervisor export. Metrics-only
configuration does not enable supervisor tracing. Headers, certificate paths,
resource attributes and content-capture settings are never copied.

The v2 mutation client sends W3C `traceparent`/`tracestate` HTTP headers outside canonical
request bodies and operation capabilities. Authenticated supervisor operations
emit `acp.supervisor.*` spans under the current controller operation, with Task
UID/attempt, prompt/operation IDs and runtime pool/session identity. Context is
request-local, including for reused sessions. Missing or invalid context starts
a separate trace; disabled telemetry does not affect execution. Provider CLI
children receive no tracing environment variables or instrumentation. Supervisor
spans never capture prompts, completions, raw tool arguments or error text, even
if a content-capture environment variable is set.

**Collector routing is an operator prerequisite.** Managed RuntimePool network
policies permit DNS, controller API and provider proxy traffic; enabling telemetry
does not add collector egress. Under an enforcing CNI, configure a separate,
narrowly selected collector route/policy before expecting exports. Pod-level
collector access also permits provider children to reach that collector, because
they share the Pod network. Choose a collector and authorization boundary
accordingly. This feature does not install a collector or alter managed policies.
External supervisors can opt in with the same environment settings. Supervisor
exporters require `http://` or `https://` endpoint URLs and the scalar settings
listed above. Unsupported ambient SDK settings (including OTLP headers, certificate
paths and resource attributes) disable supervisor telemetry before SDK initialization,
so malformed values cannot enter SDK diagnostics. The content-capture flag is ignored.
Use an authorized local collector.

Export is asynchronous and bounded. Initialization/export failure does not fail
a Task, and supervisor shutdown spends at most two additional seconds flushing
traces. Unavailable collectors can result in dropped spans. Use these sources of
truth for runtime state:

- `Task.status.execution` for fenced attempt, RuntimePool, RuntimeSession, prompt, and terminal outcome;
- `Task.status.delivery` for workspace validation/publication state and non-secret receipts;
- `RuntimePool.status` for lifecycle, admission, exact Pod/boot identity, capacity, and pressure;
- Task execution events and controller/runtime/publisher structured logs.

Do not add credential-bearing OTLP headers to runtime images or provider child environments.
The provider child-environment allowlist remains unchanged.

Credential-bearing OTLP header environment variables are not copied from the
controller into task workloads. Use an in-cluster collector endpoint or a
worker-scoped credential mechanism if your collector requires authentication.

Kubernetes task workloads require `ORKA_ENABLE_TELEMETRY=true`; OTLP endpoint variables alone
do not enable telemetry. Standard SDK sampling is
available through `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` (for
example `always_off`, `always_on`, or `parentbased_traceidratio`).

## Approval decision duration

The Prometheus endpoint on `--metrics-bind-address` exports
`orka_approval_decision_duration_seconds`, independently of OpenTelemetry enablement.
It measures seconds between the stored approval request timestamp and the first
committed terminal decision timestamp. Buckets cover one second through one day,
with longer waits included in the `+Inf` bucket.

The only labels are:

| Label | Values |
|---|---|
| `outcome` | `approved`, `denied`, `cancelled`, `expired` |
| `path` | `acp` for broker-owned approvals with a call binding; `worker` for built-in AI worker approvals |

Human approvals and declines record `approved` and `denied`. ACP wait/deadline
expiry records `expired`; Task cancellation or loss of the original tool/prompt
authority records `cancelled`. Recovery records a sample when it commits a missing
terminal decision from a verified execution receipt. Revocation after a human
decision preserves that original decision and adds no second sample. Polling,
receipt replay, rejected decisions and rolled-back writes add no samples.

Worker approvals currently have human decisions only: passive timeout metadata,
Task termination and cancellation do not create worker terminal approval events.
This metric does not introduce worker expiry or agent parking. A pending approval
whose expiry is derived for display is counted only when a terminal event commits.
Missing or backwards timestamps are omitted. Labels contain no Task names,
approval IDs, users or request text.

Histograms are in memory and reset on process restart. Existing terminal events
are not replayed into the histogram. A crash between committing a decision and
observing it, or between scrapes, can lose a sample; use durable approval events
for audits. Pending requests retained across a restart still use their original
request timestamps when a new decision commits.

## Capability-gap signals

Capability-gap signals count requests Orka could not serve. They are off by
default: start the controller with `--capability-gap-signals-enabled` (or
`ORKA_CAPABILITY_GAP_SIGNALS_ENABLED=true`). They do not need OpenTelemetry.

### What Orka already recorded

Before these signals, each kind of gap was either recorded as free text or not
recorded at all:

| Signal | What Orka already had | Where |
|---|---|---|
| Admission rejected as unsupported | Free-text denials with no metric. Webhook denials and CEL rules leave no Orka record. Planning rejections end the Task as `Failed` with the text in `status.message`; a few set `status.execution.reason` (`WorkspaceUnsupported`, `NativeSessionRuntimeUnsupported`). | `internal/admission/*.go`, `api/v1alpha1/*_types.go` (`XValidation`), `internal/controller/agent_execution_plan.go`, `internal/controller/acp_task_queue.go`, `internal/controller/acp_native_session.go` |
| Missing or denied tool | OpenTelemetry only, behind `--enable-telemetry`: `error.type=tool_not_found` with the tool name replaced by `unknown_tool`, and `RecordRejectedToolCall` with the name replaced by `rejected_tool`. Built-in AI worker `ToolCallFailed` execution events carried the tool name and error text but no typed reason. ACP permission requests denied by frozen tool policy, and supervisor rejections of unlisted MCP tools, were not recorded. | `internal/tools/registry.go`, `workers/ai/main.go`, `internal/controller/acp_dispatcher.go` (`frozenMCPPermissionDecision`), `workers/acp/supervisor/mcp_proxy.go` (`authorizeCall`) |
| Agent says it can't | Not recorded. An "I can't do that" reply ends the Task as `Succeeded`. Model refusals (`refused`, ACP `stopReason`) are a different thing and are kept only on execution events. | `internal/llm/provider.go`, `internal/harness/v2/eventjournal/mapper.go` |
| Repeated workaround | Per-Task tool events only (`code_exec`, or the ACP tool kind such as `execute`). Nothing aggregates them. | `workers/ai/main.go`, `internal/harness/v2/eventjournal/mapper.go` |
| Repeated failed request | Not recorded. `orka_execution_events_appended_total{event_type="TaskFailed"}` counts failures without a reason. | `internal/metrics/metrics.go` |

### What is recorded

| Category | Reason | Source |
|---|---|---|
| `unsupported_request` | `runtime`, `contract_version`, `execution_workspace`, `toolboxes`, `prior_task_ref`, `max_turns`, `retry_policy`, `resources`, `placement`, `autonomous_coordination`, `transaction_delegation`, `native_session` | The controller refused an agent Task during execution planning, at the workspace dispatch gate, or at native Session restore. Validation errors and missing configuration are not counted. |
| `missing_tool` | `not_enabled`, `not_registered` | The built-in AI worker rejected a model's tool call because the Task did not enable it, or no tool had that name. The worker marks the `ToolCallFailed` event with a closed `errorType`; the controller counts it when the event arrives from a managed AI worker Pod. The same event from any other Task is journaled but not counted. |
| `missing_tool` | `denied_by_policy` | An ACP runtime asked permission to use a tool its frozen tool policy does not allow. |

The Prometheus counter is `orka_capability_gap_signals_total` with labels
`category`, `reason`, `runtime` and `tool`:

- `runtime` is `native`, `codex`, `claude`, `copilot`, `opencode`, `external` or `unknown`.
- `tool` is a name from the tool catalog fixed at controller startup (built-in,
  coordination and brokered tools, plus built-in runtime native tools), `unknown`
  for any other name, or `none` for `unsupported_request`.

Labels never contain Task names, prompts, completions, tool arguments, error
messages or user text. Values outside these sets fall back to `other` or `unknown`.

### Weekly summary

`GET /api/v1/capability-gaps?namespace=<ns>` returns the namespace's signals for
the current UTC day and the six before it. It needs `list` on `tasks` in that
namespace, like `GET /api/v1/tasks`, and returns `404` while the flag is off.
A context token must not be limited to particular objects: counts outlive their
Tasks, so they cannot be filtered per Task.

```json
{
  "namespace": "team-a",
  "from": "2026-10-26T00:00:00Z",
  "to": "2026-11-01T12:00:00Z",
  "recordingSince": "2026-10-28T09:14:03Z",
  "partial": true,
  "signals": [
    {"category": "missing_tool", "reason": "denied_by_policy", "runtime": "codex",
     "tool": "WebFetch", "count": 4, "tasks": ["nightly-triage-x7k2", "nightly-triage-p9q1"]}
  ]
}
```

Each row lists at most five Task names, most recently observed first. A name is
shown only when the caller could list that Task now; deleted Tasks drop out but
keep their counts. Signals from a Gateway-owned Task count only for callers who
can read that Gateway, the same check that guards the Task. Counts belong to
one namespace incarnation: if a namespace is deleted and recreated with the same
name, the new one starts empty. Prometheus counts every signal.

The summary is held in controller memory, so it starts empty after a restart or
leader change. `partial` is `true` when `recordingSince` is after `from`, so a
reader can tell the window is incomplete. Each day keeps at most 64 namespaces,
and each namespace keeps at most 16 Gateways per day; a further Gateway replaces
the least recently seen one. Signals beyond these bounds still count in
Prometheus. For history across restarts,
query the counter in your metrics backend, for example
`increase(orka_capability_gap_signals_total[7d])`.

### Not recorded yet

- **Agent says it can't.** Detecting this needs either reading the agent's reply
  or a structured way for agents to report a missing capability. Orka has
  neither yet.
- **Repeated workaround.** Telling whether a shell command could have been a
  built-in tool needs the command text, which these signals never read.
- **Repeated failed request.** Grouping similar requests needs request text.
- Tool calls rejected inside an ACP runtime Pod before they reach the
  controller, such as an unlisted MCP tool name, and tool rejections in chat or
  the compatibility endpoints, which are not Tasks.

## Trace topology

A delegated Task should appear as one distributed trace:

```text
task.run
  └─ agent.step / chat.tool_loop.iteration
      ├─ chat {model}
      ├─ execute_tool {tool.name}
      └─ execute_tool delegate_task
          └─ child task.run
              └─ child agent.step / chat.tool_loop.iteration
                  ├─ chat {model}
                  └─ execute_tool {tool.name}
```

An Orka harness v2 Task continues the same Task-carried trace across controller and supervisor:

```text
<Task-carried parent span>
  ├─ acp.session.create / acp.session.continue
  │   └─ acp.supervisor.session.create  # when a runtime session is created
  ├─ acp.prompt
  │   ├─ acp.supervisor.prompt
  │   └─ acp.publication.reconcile  # live write-workspace delivery
  └─ acp.publication.reconcile      # recovery after the prompt span is gone
```

Model client spans measure provider-call latency only. Tool spans are siblings
of the model client span under the same agent/chat step, not children of the
model client span.

Task creation stamps the current W3C trace context into Task annotations. The
controller extracts that context for `task.reconcile` and controller-side ACP
spans. Supported native worker Tasks also receive `ORKA_TRACEPARENT` in their
Jobs. Orka harness v2 runtime requests carry `traceparent` and `tracestate` headers to
authenticated supervisor operations; provider CLI children remain outside this
instrumentation. Delegation stamps the active `execute_tool delegate_task` span
context onto the child Task so child controller, supervisor and native-worker
spans remain linked.

Outbound HTTP and MCP Tool CRD requests receive W3C `traceparent` headers. If a
Tool config supplies its own `traceparent` header, the active Orka trace context
wins.

### Instrumented components

| Tracer | Span | Attributes |
|--------|------|------------|
| `orka.api` | HTTP/API middleware spans | HTTP request/route/status metadata |
| `orka.chat` | `chat.request`, `chat.tool_loop.iteration` | session metadata; `chat.iteration`, `orka.tenant`, requested model, tool-call count |
| `orka.worker` | `task.run` | `orka.task.id`, namespace, and agent name when known |
| `orka.acp` | `acp.prompt`, `acp.session.create`, `acp.session.continue`, `acp.publication.reconcile` | `orka.task.id`, namespace, attempt/prompt identity, RuntimePool/RuntimeSession identity, prompt/session outcome, publication identity, and agent name when known |
| `orka.acp.supervisor` | `acp.supervisor.*` authenticated runtime operations | Task UID/attempt, prompt/operation IDs and runtime pool/session identity; no request content |
| `orka.agent` | `agent.step` | iteration, requested model/provider, tool-call count, Orka task metadata |
| `orka.gen_ai` | `chat {model}` | `gen_ai.*` provider/model/token metadata and `error.type` |
| `orka.gen_ai` | `execute_tool {tool.name}` | `gen_ai.tool.*`, `orka.tool.name`, `orka.tool.kind`, `orka.tool.result.size_bytes`, parent/child task fields for delegation |
| `orka.controller` | `task.reconcile` | task name, namespace, type, and propagated trace context |

Use `orka.task.id` to find a Task trace, `orka.tool.name` to find specific tool
executions, and `orka.parent_task.id` / `orka.child_task.id` to follow delegated
children. Tool spans do not include raw arguments or complete result bodies, but a structured tool
error message can appear in the span status description.

### OpenTelemetry traces vs Task trace read model

OpenTelemetry traces are exported to your collector/backend and are queried in
that backend. Orka's Task trace API (`GET /api/v1/tasks/:id/trace`) and CLI
(`orka task trace`) are different: they build an execution read model from
stored Orka events for UI/CLI troubleshooting. The two systems share task and
tool terminology, but the Task trace API does not read from the OTel backend.

## Orka query attributes

Orka emits low-cardinality, content-safe attributes alongside GenAI attributes:

| Attribute | Meaning |
|---|---|
| `orka.task.id` | Task name |
| `orka.task.namespace` | Kubernetes namespace |
| `orka.tenant` | Tenant; currently the namespace fallback |
| `orka.agent.name` | Agent name/runtime when known |
| `orka.parent_task.id` | Parent Task name on delegation spans |
| `orka.child_task.id` | Child Task name after creation |
| `orka.tool.name` | Tool name |
| `orka.tool.kind` | `builtin`, `delegate`, or `http` |
| `orka.tool.result.size_bytes` | Tool result size only; never the body |

Useful backend queries:

- Find a Task trace: filter spans by `orka.task.id = "<task-name>"`.
- Find tool calls: filter by `orka.tool.name = "delegate_task"` or another tool name.
- Locate child work: filter by `orka.parent_task.id` or `orka.child_task.id`.
- Split built-in vs external tools: group by `orka.tool.kind`.

## GenAI traces

Model calls are emitted as client spans named:

```text
chat {model}
```

Key attributes include:

| Attribute | Meaning |
|---|---|
| `gen_ai.operation.name=chat` | GenAI operation |
| `gen_ai.provider.name` | Concrete serving provider, for example `anthropic`, `openai`, or `azure.ai.openai` |
| `gen_ai.request.model` | Requested model |
| `gen_ai.request.max_tokens` / `gen_ai.request.temperature` | Request settings when present |
| `gen_ai.output.type` | Requested output format when present |
| `gen_ai.usage.input_tokens` / `gen_ai.usage.output_tokens` | Token usage |
| `gen_ai.response.model` / `gen_ai.response.finish_reasons` / `gen_ai.response.id` | Response metadata when available |
| `error.type` | Provider status code or Go error type on failed calls |

Tool calls executed through the built-in registry are emitted as spans named
`execute_tool {tool.name}` with `gen_ai.tool.*` and `orka.tool.*` attributes and
a duration metric. External HTTP/MCP tools use the same span name and
`orka.tool.kind=http`. OpenAI-compatible and Anthropic-compatible API requests
emit the same GenAI model-call spans as native Orka chat and AI worker calls.

## GenAI metrics

Orka records these OTLP histograms when model/tool calls run:

| Metric | Unit | Notes |
|---|---|---|
| `gen_ai.client.operation.duration` | `s` | One datapoint per model call |
| `gen_ai.client.token.usage` | `{token}` | Separate datapoints for `gen_ai.token.type=input` and `output` |
| `gen_ai.client.operation.time_to_first_chunk` | `s` | Streaming calls |
| `gen_ai.execute_tool.duration` | `s` | Built-in registry and external HTTP/MCP tool calls |

Metric dimensions are intentionally low-cardinality: operation, provider, model, token
type, tool name/type, and error type when applicable. High-cardinality fields such
as task IDs and result sizes stay on spans, not metric labels.

## Example collector and backend

A development collector can export traces to Jaeger or Tempo. Example collector
pipeline:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
exporters:
  otlp/tempo:
    endpoint: tempo.observability.svc:4317
    tls:
      insecure: true
service:
  pipelines:
    traces:
      receivers: [otlp]
      exporters: [otlp/tempo]
```

For local Jaeger all-in-one, expose its OTLP gRPC endpoint and set:

```bash
kubectl -n orka-system set env deployment/orka-controller \
  OTEL_EXPORTER_OTLP_ENDPOINT=http://jaeger-collector.observability.svc:4317
```

## Content capture and privacy

Prompt/completion content capture is fail-closed and defaults to `none`. The
current rollout emits metadata, token counts, model/provider identity, tool
names, result sizes, and durations, but not raw prompt or completion text.

Telemetry must not include:

- raw prompts or completion content,
- raw tool arguments or tool result bodies,
- API keys, auth headers, TxTokens, context tokens, JWTs, cookies, or credentials,
- raw transcripts or broad local filesystem paths.

Future opt-in content capture must pass through Orka redaction and size caps
before any span attribute or event is emitted.
