---
slug: /observability
---

# Observability

Orka emits OpenTelemetry traces and metrics for controller, chat, tool, AI
worker, and harness-backed agent runtime paths when telemetry is enabled. The
GenAI signals are backend instrumentation: they are exported over OTLP to your
collector/backend and are separate from the Orka React UI.

Telemetry is disabled by default. Disabled mode keeps the hot path on the global
OpenTelemetry no-op providers and does not configure OTLP exporters.

## Enable telemetry

Start the controller with telemetry enabled and point it at an OTLP endpoint.
gRPC is the default exporter protocol:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector.otel.svc:4317 \
  orka-controller --enable-telemetry
```

`--enable-tracing` is kept as a compatible alias and enables the same traces and
metrics. Existing Prometheus metrics on `--metrics-bind-address` continue to
work independently.

For an existing Kubernetes Deployment, set both the controller flag and the
collector environment. Setting `OTEL_EXPORTER_OTLP_ENDPOINT` alone does not
enable telemetry:

```bash
kubectl patch deployment orka-controller -n orka-system --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-telemetry"},
  {"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"OTEL_EXPORTER_OTLP_ENDPOINT","value":"otel-collector.otel.svc:4317"}},
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

Harness-wrapper and agent-runtime worker telemetry is explicit opt-in: set
`ORKA_ENABLE_TELEMETRY=true` and OTLP exporter configuration on those workloads
when you want their process-local spans exported.

For the singleton CLI harness wrapper Deployment, opt in explicitly:

```bash
kubectl set env deployment/orka-agent-harness-wrapper -n orka-system \
  ORKA_ENABLE_TELEMETRY=true \
  OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector.otel.svc:4317 \
  OTEL_EXPORTER_OTLP_INSECURE=true
```

For per-Task agent runtime Jobs, set telemetry env in the Task `spec.env` where
you intentionally want that runtime process to export spans. Orka reserves and
overwrites trace-context env (`ORKA_TRACEPARENT`, `ORKA_TRACESTATE`,
`ORKA_BAGGAGE`) so user-supplied values cannot forge parentage.

Credential-bearing OTLP header environment variables are not copied from the
controller into task workloads. Use an in-cluster collector endpoint or a
worker-scoped credential mechanism if your collector requires authentication.

Kubernetes task workloads require `ORKA_ENABLE_TELEMETRY=true`; OTLP endpoint variables alone
do not enable telemetry. Standard SDK sampling is
available through `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG` (for
example `always_off`, `always_on`, or `parentbased_traceidratio`).

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

Model client spans measure provider-call latency only. Tool spans are siblings
of the model client span under the same agent/chat step, not children of the
model client span.

Task creation stamps the current W3C trace context into Task annotations. The
controller extracts that context when reconciling the Task and injects
`ORKA_TRACEPARENT` into worker Jobs or harness-wrapper turn metadata. Delegation
stamps the active `execute_tool delegate_task` span context onto the child Task,
so the child `task.run` span is linked to the parent tool call.

Outbound HTTP and MCP Tool CRD requests receive W3C `traceparent` headers. If a
Tool config supplies its own `traceparent` header, the active Orka trace context
wins.

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
kubectl set env deployment/orka-controller \
  OTEL_EXPORTER_OTLP_ENDPOINT=jaeger-collector.observability.svc:4317
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
