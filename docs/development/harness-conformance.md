# Harness conformance suite

`internal/harness/harnesstest` contains the provider-agnostic conformance suite for `orka.harness.v1`.

## How providers plug in

A provider test supplies a factory:

```go
harnesstest.RunHarnessConformance(t, func(t *testing.T, behavior harnesstest.FakeBehavior) (string, func()) {
    server := startProviderHarnessForBehavior(t, behavior)
    return server.BaseURL, server.Close
})
```

The suite expects an HTTP+SSE harness endpoint implementing:

- `GET /v1/health`
- `GET /v1/capabilities`
- `POST /v1/turns`
- `GET /v1/turns/{turnID}/events?afterSeq=N`
- `POST /v1/turns/{turnID}/cancel`

## Covered behavior

- health and capabilities responses validate DTO versioning;
- successful turns emit start/output/tool/result/completed frames;
- failed turns map to safe `AgentRuntimeFailed` events;
- cancellation invokes the cancel endpoint and emits `TurnCancelled`;
- unknown frames become warning diagnostics instead of panicking;
- secret-looking output is redacted before event persistence;
- client timeouts surface as sanitized typed client errors.

Providers that cannot exercise every fake behavior directly should adapt the factory with a local fixture harness and run provider-specific smoke tests for the unavailable behavior.
