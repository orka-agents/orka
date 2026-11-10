---
slug: /observability
description: "OpenTelemetry traces, GenAI metrics, and how to follow one task through them."
---

# Observability

Orka emits OpenTelemetry traces and metrics for controller, chat, tool, native AI
worker, Orka harness v2 controller/supervisor, ACP runtime GenAI, and Agent
Substrate client paths when telemetry is enabled. ACP runtime internals remain
observable primarily through durable Task execution/delivery status, RuntimePool
status, bounded events, and structured logs. The GenAI
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
endpoint, protocol, insecure and compression settings for traces and for
[ACP GenAI metrics](#genai-telemetry-for-acp-runtimes). Generic and signal-specific
endpoints retain their normal SDK semantics; signal-specific settings take precedence.
A missing or invalid trace endpoint disables supervisor export. Metrics-only
configuration enables neither supervisor tracing nor supervisor metrics. A
metrics-specific endpoint that is not worker-reachable drops supervisor metrics
rather than falling back to the generic collector; so does a metrics setting the
supervisor would reject. When metrics are dropped, the supervisor also receives
`OTEL_METRICS_EXPORTER=none` if it keeps a generic endpoint for traces, so that
endpoint is not used for metrics. A protocol, insecure or compression value that
traces and metrics share is passed once, under its generic name. Substrate native
runtimes accept 32 environment entries per container and Orka's own settings use
most of them: a native runtime has room for the enable flag, one endpoint and one
shared setting, or for one endpoint per signal. More settings than that, or
telemetry on a native runtime whose workspace can suspend, exceed the limit and
the provider rejects the workload. Headers, certificate paths,
resource attributes and content-capture settings are never copied. Adding metrics
settings changes the supervisor Pod template, so existing RuntimePools roll through
their normal drained replacement the first time a controller with these settings runs.

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
Agent Substrate runtimes have no Pod network policy to extend; they can opt in to
an explicit [collector egress rule](#collector-egress-for-agent-substrate-runtimes).
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
  │   ├─ substrate.router POST          # Substrate-routed runtimes only
  │   ├─ acp.supervisor.prompt
  │   │   └─ invoke_agent {runtime}
  │   │       ├─ chat {model}           # one per model call through the provider proxy
  │   │       └─ execute_tool {tool}    # one per ACP tool call
  │   └─ acp.publication.reconcile  # live write-workspace delivery
  └─ acp.publication.reconcile      # recovery after the prompt span is gone
```

For Substrate-routed runtimes, `substrate.router` carries the Task trace into
`atenet-router`, so Substrate's routing spans, including a resume the router
triggers for a suspended Actor, join the Task trace when Substrate exports
traces. See [Agent Substrate](#agent-substrate).

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
| `orka.gen_ai` (ACP supervisor) | `invoke_agent {runtime}`, `chat {model}`, `execute_tool {tool}` | `gen_ai.*` agent/provider/model/usage/tool metadata, `orka.task.uid`, `orka.acp.prompt.id`; see [GenAI telemetry for ACP runtimes](#genai-telemetry-for-acp-runtimes) |
| `orka.substrate` | `substrate.router {method}` | Router method, authority and response status only |
| `go.opentelemetry.io/contrib/.../otelgrpc` | `substrate.ateapi.v1.Control/{Method}` client spans | gRPC method and status; no metadata or message events |
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

## GenAI telemetry for ACP runtimes

Built-in ACP runtimes (`codex`, `claude`, `copilot`, `opencode`) run provider CLIs
that Orka does not instrument. The supervisor derives GenAI spans and metrics
from what it already observes: the ACP session update stream and the
supervisor's provider proxy, which every model call passes through. Telemetry
needs the supervisor trace endpoint described above; metrics also need a metrics
endpoint (`OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` or the generic endpoint).

| Span | Kind | Derived from | Key attributes |
|---|---|---|---|
| `invoke_agent {runtime}` | client | One authenticated prompt, from admission to settlement | `gen_ai.agent.name` (runtime), `gen_ai.provider.name`, `gen_ai.request.model`, `gen_ai.conversation.id` (RuntimeSession ID), `gen_ai.response.finish_reasons` (ACP stop reason), `gen_ai.usage.*` when the agent reports ACP turn usage, `orka.task.uid`, `orka.acp.prompt.id`, `error.type` |
| `chat {model}` | client | One admitted inference request relayed by the provider proxy | `gen_ai.provider.name`, `gen_ai.request.model`, `error.type` (upstream HTTP status, `cancelled`, or `_OTHER`) |
| `execute_tool {tool}` | internal | ACP `tool_call` / `tool_call_update` until `completed` or `failed` | `gen_ai.tool.name`, `gen_ai.tool.call.id` (canonical ACP call ID), `gen_ai.tool.type`, `orka.tool.name`, `orka.acp.tool.kind`, `error.type` (`tool_error`, `incomplete`, `cancelled`) |

`invoke_agent` is a child of `acp.supervisor.prompt`; `chat` and `execute_tool` are
its children, so model calls and tool calls appear as siblings, as they do for
native workers. Spans are written when their outcome is known, with the observed
start time. `gen_ai.provider.name` names the model API the runtime speaks through
the proxy: `anthropic` for Claude and `openai` for Codex, Copilot and OpenCode.
OpenCode pins `provider/model` but always speaks OpenAI chat completions, so its
prefix does not change the label and `gen_ai.request.model` is the model part.
With a gateway such as Vekil in front, that is the wire API, not the gateway's
upstream.

Tool names come only from the adapter's structured tool identity when it is a
plain identifier (letters, digits, `_ . : / -`, up to 128 characters); otherwise
the span uses the closed ACP tool kind (`read`, `edit`, `execute`, ...) or
`other`. Tool titles are free text and never become names. A tool call still open
when its prompt settles is recorded with `error.type=incomplete`, or `cancelled`
for cancelled prompts.

Per-call token counts are not available: provider responses stream through the
proxy unparsed. Token usage is recorded once per prompt on `invoke_agent` and in
`gen_ai.client.token.usage` with `gen_ai.operation.name=invoke_agent`, and only
when the ACP agent includes `usage` in its `session/prompt` response. A malformed
usage report is ignored and never fails a prompt.

ACP runtime metrics:

| Metric | Unit | Datapoints |
|---|---|---|
| `gen_ai.client.operation.duration` | `s` | One per `chat` model call (`gen_ai.operation.name=chat`) |
| `gen_ai.client.token.usage` | `{token}` | Input and output tokens per prompt, when reported (`gen_ai.operation.name=invoke_agent`) |
| `gen_ai.execute_tool.duration` | `s` | One per ACP tool call |
| `gen_ai.invoke_agent.duration` | `s` | One per prompt |

Metric attributes are limited to operation, agent/runtime name, provider, model,
token type, tool name/type and `error.type`. Task, prompt and session identities
stay on spans. Supervisor metrics use the supervisor's fixed `service.name`
resource (`orka-acp-supervisor`), like its spans, plus a `service.instance.id`:
a random UUID drawn each time a supervisor starts. It keeps supervisors that
report the same runtime and model as separate series, names no Task, user or
Pod, and changes on every supervisor restart, so aggregate across it for
fleet-wide totals.

**Content capture stays off for ACP runtimes.** The supervisor never records
prompts, completions, tool titles, tool arguments, tool results, or provider
error bodies in span attributes, events or metrics, and ignores
`OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT`.

## Collector egress for Agent Substrate runtimes

Agent Substrate v1 Actors have no direct network egress: the egress gateway
refuses any destination the runtime's allowlist does not name. By default core's
allowlist names only the provider proxy and controller API origins, so a
supervisor configured to export OTLP cannot reach the collector. Opt in
explicitly:

```bash
orka-controller --enable-telemetry --runtime-telemetry-collector-egress
# or ORKA_RUNTIME_TELEMETRY_COLLECTOR_EGRESS=true
```

When both are set, core adds the origin of each OTLP endpoint it projects into
the supervisor (the trace endpoint, and the metrics endpoint when it differs) to
the allowlist of every new Substrate runtime workload request:

- `http://` endpoints become `HTTP` rules, which admit cleartext HTTP/1.1 and
  h2c, so both OTLP/HTTP and insecure OTLP/gRPC work.
- `https://` endpoints become `TLSPassthrough` rules, so the supervisor verifies
  the collector's own certificate.
- The rule names exactly the endpoint host and port (default `80`/`443`).

Validation fails closed. The controller refuses to start, and admission reports
the pool `Degraded`, when an endpoint is not an absolute `http(s)` URL, names an
IP address, a single-label or wildcard host, or conflicts with a core origin on
the same host and port. Use a fully qualified in-cluster name such as
`http://otel-collector.otel.svc:4317`. Scheme-less gRPC targets
(`collector:4317`) are rejected because their transport cannot be determined.

The flag does not change Kubernetes NetworkPolicies for Pod-backed runtimes, and
it is fixed per workload request: a running workload keeps its admitted
allowlist until its next request. The collector sees connections from the
Substrate egress gateway, not the Actor, so collector-side `k8sattributes`
enrichment labels supervisor telemetry with the gateway Pod; rely on the
supervisor's `service.name` and the Orka span attributes instead. Every process
in the Actor, including provider CLIs, can reach the allowlisted collector, so
treat its OTLP ports as reachable by agent code. MCP Tool Actors keep their
explicit `spec.mcp.substrateActor.egress` and are not changed by this flag.

## Agent Substrate

With `--enable-telemetry`, Orka's Agent Substrate clients propagate W3C trace
context (`traceparent`/`tracestate`, never baggage) and record client spans for
calls made inside an existing trace. Calls outside a trace pass through
unchanged, so background reconciles do not start one trace per Substrate call.

- **Router (HTTPS):** each traced request through `atenet-router` records a
  `substrate.router {method}` client span (time to response headers) and carries
  its context to the routed runtime or MCP Actor. This covers ACP harness v2
  calls to Substrate-routed supervisors (under the Task's `acp.*` spans) and MCP
  Tool calls proxied by `orka-api` (under the worker's `execute_tool` span).
- **Control API (gRPC):** `substrate.ateapi.v1.Control` calls record an
  `otelgrpc` client span and carry the active trace, which `ateapi` extracts for
  its lifecycle spans and the `atelet`/`ateworker` spans beneath them. In core
  these calls come from the MCP Tool, SubstrateActorPool and template-validation
  reconcilers, which do not start traces today, so they produce spans only when
  invoked from a traced context.

ACP execution workspace lifecycle operations (Actor resume, suspend, delete and
checkpoint) are performed by the separately deployed `orka-workspace` provider,
not by core. Core's instrumentation does not cover those calls; correlate them
by time, Actor and template as described below.

Substrate samples with parent-based samplers, so a sampled Orka trace is sampled
end to end; its default ratios (10% control plane, 1% router) only apply to
requests Orka does not trace. Client spans contain no request paths, queries,
headers, gRPC metadata, bearer tokens or message bodies; a failed gRPC call's
span status carries Substrate's status message. Disabled telemetry adds no
instrumentation.

### Substrate metrics relevant to Orka operators

Substrate exports these instruments over OTLP from its own components (see the
upstream Agent Substrate observability guide and metric registry for all
labels). They explain latency and failures that surface in Orka as slow or
`Degraded` RuntimePools, slow cold resumes, or failed MCP Tool calls:

| Substrate metric | What it tells an Orka operator |
|---|---|
| `ate.actor.lifecycle.operation.duration` (ateapi) | Duration and failure (`error.type`) of Actor create/resume/suspend/delete. Resume with `error.type=ResourceExhausted` means no worker capacity, the usual cause of a workspace that cannot start or resume. Already-running resumes are excluded. |
| `ate.scheduler.assignment.duration` (ateapi) | Worker assignment outcome (`assigned`, `no_capacity`, `error`) per pool and sandbox class. |
| `ate.actor.restore.duration` / `ate.actor.checkpoint.duration` (atelet) | Per-phase cold-resume and checkpoint latency, by snapshot kind/fidelity and template. Orka's data-only suspension uses volume-fidelity snapshots. |
| `atelet.snapshot.upload.duration`, `atelet.snapshot.undurable.age.max`, `atelet.snapshot.upload.overdue.actors` (atelet) | Whether suspended workspace data has reached durable storage; a growing undurable age puts later cold resumes at risk. |
| `ate.workerpool.desired_workers` / `ate.workerpool.ready_workers` / `ate.workerpool.workers` (atecontroller, ateapi) | Capacity of the private single-replica WorkerPools the provider creates per allocation. |
| `atenet.router.route.duration` (atenet-router) | Router outcome and whether a request woke a suspended Actor (`ate.router.resume`); covers Orka's routed supervisor and MCP Tool calls. |
| `ate.actor.crashes` (ateapi) | Actors that crashed, for example a supervisor that exited. |
| `rpc.server.call.duration` (all gRPC servers) | Per-method availability and latency of the control API Orka calls. |

Substrate's SLO guide suggests alerting on resume capacity (the
`ResourceExhausted` share of `ate.actor.lifecycle.operation.duration` resumes,
with multi-window burn-rate alerts), on `ateapi` RPC availability (5xx-equivalent
gRPC codes from `rpc.server.call.duration`), and on WorkerPools whose
`ate.workerpool.workers` stays below `ate.workerpool.desired_workers`. Its
example Prometheus rules (`docs/examples/gmp-alerting-rules.yaml`) implement
these as `AteResumeActorCapacityBurnPage`/`Ticket`, `AteApiRpcErrorBudgetBurnPage`/`Ticket`
and `SubstrateWorkerPoolCapacityNotDelivered`. For Orka, pair them with the
RuntimePool metrics on the controller's Prometheus endpoint: rising
`ResourceExhausted` resumes alongside Orka queued Tasks or a closed RuntimePool
admission state indicates Substrate capacity, not Orka, is the bottleneck.

### Correlating Substrate with Orka Tasks

1. Find the Task trace by `orka.task.id` (controller) or `orka.task.uid`
   (supervisor and GenAI spans).
2. Under `acp.prompt` (or a worker's `execute_tool` for MCP Tools), open the
   `substrate.router` client span. Substrate's router spans beneath it show
   routing time and any resume of a suspended Actor before the supervisor's
   `acp.supervisor.*` spans continue the trace.
3. Substrate component logs and Actor lifecycle records carry top-level
   `trace_id`/`span_id`, so the same trace ID finds router-driven resume log
   lines in your log backend.
4. Substrate metrics carry template and pool labels, not Task identity. Use the
   time window and the workspace's Actor, template and pool (from the
   ExecutionWorkspace and provider status) to slice
   `ate.actor.lifecycle.operation.duration`, `ate.actor.restore.duration` and
   `atenet.router.route.duration`, including provider-driven resume, suspend and
   checkpoint operations that are not in the Task trace.

Substrate components export to their own configured collector (`ate-otel-config`);
point both installations at the same backend to see one trace. Spans are only
joined when both sides sample the trace.

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
