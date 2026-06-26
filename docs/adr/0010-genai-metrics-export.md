# ADR 0010: Export GenAI metrics through OTLP push

Date: 2026-06-23

## Status

Accepted for the initial GenAI metrics implementation.

## Context

Orka already exports traces to an OTLP gRPC endpoint when telemetry is enabled.
GenAI client instrumentation requires histograms such as
`gen_ai.client.operation.duration` and `gen_ai.client.token.usage`; those metrics
must be available in the same backend as traces while preserving the existing
controller-runtime Prometheus scrape endpoint.

## Decision

Initialize an OpenTelemetry `MeterProvider` alongside the existing
`TracerProvider` in `internal/tracing.Init`. Metrics use OTLP gRPC push via
`OTEL_EXPORTER_OTLP_ENDPOINT` and share the trace resource. The shutdown closure
flushes both providers. Existing Prometheus metrics remain unchanged and continue
to be served by controller-runtime.

The controller reuses `--enable-tracing` for the combined traces+metrics
backend and also accepts `--enable-telemetry` as an alias. When telemetry is
enabled, the Job builder injects `ORKA_ENABLE_TELEMETRY` and the non-secret standard OTLP
environment into worker Jobs so worker model calls can emit the same signals.

## Consequences

- Operators can route traces and GenAI metrics through the same OpenTelemetry
  Collector configuration.
- Prometheus scrape behavior is unchanged for existing dashboards.
- Worker telemetry remains opt-in unless the worker sees
  `ORKA_ENABLE_TELEMETRY=true`; OTLP endpoint variables alone do not enable
  Kubernetes task workload telemetry.
