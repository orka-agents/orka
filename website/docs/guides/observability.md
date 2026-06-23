---
slug: /observability
---

# Observability

Orka emits OpenTelemetry traces and metrics for controller, chat, tool, and AI
worker paths when telemetry is enabled. The GenAI signals are backend
instrumentation: they are exported over OTLP to your collector/backend and are
separate from the Orka React UI.

## Enable telemetry

Start the controller with telemetry enabled and point it at an OTLP gRPC
endpoint:

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=otel-collector.otel.svc:4317 \
  orka-controller --enable-telemetry
```

`--enable-tracing` is kept as a compatible alias and enables the same traces and
metrics. Existing Prometheus metrics on `--metrics-bind-address` continue to
work independently.

When controller telemetry is enabled, worker Jobs receive:

- `ORKA_ENABLE_TELEMETRY=true`
- `OTEL_EXPORTER_OTLP_ENDPOINT` and related standard OTLP environment variables
- `ORKA_TRACEPARENT` when a Task was created from an already-traced API/chat/tool
  request

Credential-bearing OTLP header environment variables are not copied from the
controller into task workloads. Use an in-cluster collector endpoint or a
worker-scoped credential mechanism if your collector requires authentication.

The AI worker also auto-enables telemetry when `OTEL_EXPORTER_OTLP_ENDPOINT` is
present, which is useful for direct/local worker runs.

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

Legacy `llm.*` attributes are dual-emitted during the migration window.

Tool calls executed through the built-in registry are emitted as internal spans
named `execute_tool {tool.name}` with `gen_ai.tool.*` attributes and a duration
metric.

## GenAI metrics

Orka records these OTLP histograms when model/tool calls run:

| Metric | Unit | Notes |
|---|---|---|
| `gen_ai.client.operation.duration` | `s` | One datapoint per model call |
| `gen_ai.client.token.usage` | `{token}` | Separate datapoints for `gen_ai.token.type=input` and `output` |
| `gen_ai.client.operation.time_to_first_chunk` | `s` | Streaming calls |
| `gen_ai.execute_tool.duration` | `s` | Built-in registry tool calls |

Dimensions are intentionally low-cardinality: operation, provider, model, token
type, tool name/type, and error type when applicable.

## Distributed traces across pods

Task creation stamps the current W3C trace context into Task annotations. The
controller extracts that context when reconciling the Task and injects the
`ORKA_TRACEPARENT` environment variable into worker Jobs.

```text
HTTP/API span
  └─ chat.request
      ├─ chat {model}
      └─ execute_tool create_agent_task
          └─ Task annotation: orka.ai/traceparent
              ├─ controller task.reconcile
              └─ worker chat {model}
```

This preserves TraceID continuity across the API process, controller reconcile,
and worker Pod without adding CRD fields.

## Content capture and privacy

Prompt/completion content capture is fail-closed and defaults to `none`. The
current rollout emits metadata, token counts, model/provider identity, tool
names, and durations, but not raw prompt or completion text. Future opt-in
content capture must pass through Orka redaction and size caps before any span
attribute or event is emitted.
