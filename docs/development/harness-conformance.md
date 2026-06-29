# Harness conformance suite

`internal/harness/conformance` is the reusable, controller-friendly conformance library for `orka.harness.v1`. It returns condition-ready results: pass/fail, sanitized messages, and observed capabilities.

`internal/harness/harnesstest` remains the Go test fixture layer. It supplies fake harness behaviors and wraps the reusable checks for provider tests.

## How providers plug in

A provider test can continue to supply a factory:

```go
harnesstest.RunHarnessConformance(t, func(t *testing.T, behavior harnesstest.FakeBehavior) (string, func()) {
    server := startProviderHarnessForBehavior(t, behavior)
    return server.BaseURL, server.Close
})
```

Controller or CLI code should use the reusable runner directly:

```go
result := conformance.CheckReadiness(ctx, conformance.Target{
    BaseURL:     endpoint,
    BearerToken: token,
})
if !result.Passed {
    // publish result.Message into a Ready=False condition
}
```

## Required endpoint contract

The suite expects an HTTP+SSE harness endpoint implementing:

- `GET /v1/health` (unauthenticated)
- `GET /v1/capabilities` (unauthenticated)
- `POST /v1/turns` (bearer-authenticated)
- `GET /v1/turns/{turnID}/events?afterSeq=N` (bearer-authenticated)
- `POST /v1/turns/{turnID}/cancel` when `supportsCancel=true` (bearer-authenticated)

Orka's `internal/harness/protocol.go` is the source of truth for `orka.harness.v1`. External runtimes, including AgentKit services started with `AGENTKIT_PROTOCOL=orka`, must match these DTOs rather than expecting Orka to accept simplified compatibility JSON.

Required wire fields include:

- Health: `version`, `status`, `ready`, `checkedAt`.
- Capabilities: `version`, `protocolVersion`, `transport`, `runtimeName`, `providerKind`, and at least one `toolExecutionModes` entry.
- Start turn response: `version`, `accepted: true`, `runtimeSessionID`, `turnID`, `correlationID` when supplied, and `eventStreamPath`.
- Frames: `version`, known `type`, `runtimeSessionID`, `turnID`, `correlationID`, and positive `seq`.
- Terminal frames: exactly one of `TurnCompleted`, `TurnFailed`, or `TurnCancelled`; `TurnCompleted` must include `completed`, and `TurnFailed` must include `failed`.
- Auth: health/capabilities remain open; start, events, and cancel reject unauthenticated requests when auth is required.

## Covered behavior

- health and capabilities responses validate DTO versioning;
- readiness results expose observed capabilities for Kubernetes status;
- successful turns emit exactly one terminal frame;
- failed turns map to safe `AgentRuntimeFailed` events;
- cancellation invokes the cancel endpoint and emits `TurnCancelled` in fixture tests;
- the reusable readiness/conformance runner rejects unknown frame types; the legacy `harnesstest` mapper fixture still verifies unknown frames become warning diagnostics instead of panics at the event-mapping layer;
- secret-looking output is redacted before event persistence;
- client timeouts surface as sanitized typed client errors;
- broken fixtures such as unsupported protocol versions and omitted terminal frames fail with actionable messages.

Providers that cannot exercise every fake behavior directly should adapt the factory with a local fixture harness and run provider-specific smoke tests for the unavailable behavior.
