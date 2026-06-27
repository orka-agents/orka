# ADR 0009: Hand-roll GenAI semantic-convention constants

Date: 2026-06-23

## Status

Accepted for the OpenTelemetry GenAI instrumentation rollout.

## Context

The OpenTelemetry GenAI semantic conventions are Development-stage and have
moved across Go semconv versions. Orka needs stable, reviewable attribute and
metric names without tying controller and worker builds to a specific
experimental helper package shape.

## Decision

Keep the stable core resource attributes on the existing OpenTelemetry semconv
version, and define the GenAI subset Orka emits in `internal/tracing/genai`.
The package owns attribute keys, operation names, provider enum values, metric
names, histogram buckets, the content-capture parser, and the schema URL
`gen-ai-dev/1.42.0-dev`.

Provider identity is resolved through a lightweight LLM telemetry interface so
spans can emit `gen_ai.provider.name` for the concrete serving provider. In
particular, Azure OpenAI maps to `azure.ai.openai` even though the Go provider
implementation family remains `openai`.

## Consequences

- GenAI convention churn is isolated to one small package.
- Tests can assert exact strings and provider mappings without depending on an
  upstream generated constants package.
- Orka must periodically compare the local constants with upstream GenAI YAML
  before promoting or removing dual-emitted legacy attributes.
