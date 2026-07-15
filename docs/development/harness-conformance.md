# Harness conformance suite

`internal/harness/conformance` is the controller implementation of the reusable `orka.harness.v1` conformance library. Its public facade at `pkg/harness/conformance` makes the same checks available to external Go adapters. Both return condition-ready results: pass/fail, sanitized messages, and observed capabilities.

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
- `POST /v1/turns/{turnID}/continue` for brokered profiles (bearer-authenticated)
- `POST /v1/turns/{turnID}/cancel` when `supportsCancel=true` (bearer-authenticated)

Orka's `internal/harness/protocol.go` is the source of truth for `orka.harness.v1`. External Go adapters can consume the alias-only public surface in `pkg/harness`; adapters implemented independently must keep their wire DTOs compatible with it. External runtimes — including generic HTTP adapters, AgentKit Serve, and Foundry adapters — must match these DTOs rather than expecting Orka to accept backend-specific compatibility JSON.

Required wire fields include:

- Health: `version`, `status`, `ready`, `checkedAt`.
- Capabilities: `version`, `protocolVersion`, `transport`, `runtimeName`, `providerKind`, and at least one `toolExecutionModes` entry. Brokered capability profiles also advertise `brokeredToolClasses`, `supportsContinuation`, and artifact/limit fields when supported.
- Observed runtimeRef execution requires observed-mode turn conformance. Brokered-only runtimes are accepted when they advertise brokered classes, support continuation, and pass the advertised brokered profile probes. Brokered read/write/coordination probes validate `brokeredToolClasses`, `supportsContinuation`, tool-call frames, `/continue`, tool-result acknowledgement, and terminal completion.
- Start turn response: `version`, `accepted: true`, `runtimeSessionID`, `turnID`, `correlationID` when supplied, and `eventStreamPath`.
- Frames: `version`, known `type`, `runtimeSessionID`, `turnID`, `correlationID`, and positive `seq`.
- Terminal frames: exactly one of `TurnCompleted`, `TurnFailed`, or `TurnCancelled`; `TurnCompleted` must include `completed`, and `TurnFailed` must include `failed`.
- Auth: health/capabilities remain open; start, events, and cancel reject unauthenticated requests when auth is required.

## Covered behavior

- health and capabilities responses validate DTO versioning;
- readiness results expose observed capabilities for Kubernetes status;
- successful turns emit exactly one terminal frame;
- failed turns map to safe `AgentRuntimeFailed` events;
- brokered read, write, and coordination profile probes start brokered turns, require a `ToolCallRequested` frame, send a synthetic Orka `ToolCallResult` through `/continue`, and require a matching `ToolResultReceived` plus terminal completion;
- cancellation invokes the cancel endpoint and emits `TurnCancelled` in fixture tests;
- the reusable readiness/conformance runner rejects unknown frame types; the legacy `harnesstest` mapper fixture still verifies unknown frames become warning diagnostics instead of panics at the event-mapping layer;
- secret-looking output is redacted before event persistence;
- client timeouts surface as sanitized typed client errors;
- broken fixtures such as unsupported protocol versions and omitted terminal frames fail with actionable messages.

The generic HTTP fixture in `examples/harness/echo` is the reference runtime for observed and brokered read/write conformance. Task start requests include safe `input.tools` schemas for allowed brokered tools; these schemas omit execution URLs and credentials. Providers that cannot exercise every fake behavior directly should adapt the factory with a local fixture harness and run provider-specific smoke tests for the unavailable behavior.

## AgentKit Serve observed conformance

Current AgentKit Serve Orka support is observed mode by default; brokered read/write/coordination are available only behind AgentKit's conformance gates. To prove the Orka
protocol skin against Orka's reusable conformance runner without model
credentials, run the path-gated AgentKit source fixture test from an Orka
checkout that can also see an AgentKit Serve checkout:

```bash
AGENTKIT_SERVE_ROOT=/path/to/agentkit.serve \
  go test ./internal/harness/conformance -run 'TestExternalAgentKitServe(Observed|BrokeredRead|BrokeredWrite|BrokeredCoordination)Conformance' -count=1 -v
```

The observed test starts `agentkit_serve_common.orka.create_orka_app` with an
offline echo `RuntimeSession`, requires bearer auth on turn endpoints, starts a
native Orka `StartTurnRequest`, verifies duplicate-start behavior, streams Orka
`HarnessEventFrame` values, and asserts AgentKit's default capabilities remain
observed only. The brokered-read, brokered-write, and brokered-coordination tests start the same
AgentKit Orka skin with the corresponding conformance gate enabled and run Orka
`ProbeBrokeredRead`, `ProbeBrokeredWrite`, or `ProbeBrokeredCoordination`, proving
`ToolCallRequested`, `/continue`, `ToolResultReceived`, and terminal completion.
These tests are skipped by default so ordinary Orka-only CI does not need a
sibling AgentKit checkout. Coordination policy, quotas, lineage, and namespace
checks remain Orka-owned.
