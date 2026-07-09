# Foundry hosted Responses validation matrix

This document maps the brokered Foundry hosted Responses plan to deterministic
local evidence and the remaining live gates. It intentionally contains no live
endpoints, credentials, tokens, or Foundry project identifiers.

## Local deterministic validation

Run the bundled deterministic validator from the Orka repository root:

```bash
examples/harness/foundry-responses/validate.sh
```

Or run the component commands manually:

```bash
go test ./examples/harness/foundry-responses -count=1

go test \
  ./examples/harness/foundry \
  ./examples/harness/foundry-responses \
  ./examples/harness/echo \
  ./internal/harness \
  ./internal/harness/conformance

go test ./internal/controller -run 'Test.*(AgentRuntime|Harness|Brokered|Runtime)'

find examples -type f -name '*.sh' -print0 | sort -z | \
  xargs -0 -n1 bash -n
```

Full non-e2e validation:

```bash
make test
```

When a clean AgentKit checkout is available, run the shared deterministic
Foundry brokered protocol suite explicitly:

```bash
examples/harness/foundry-responses/validate.sh --agentkit ../agentkit
```

Equivalent manual command from the AgentKit checkout's `runtimes/common` directory:

```bash
uv run --extra dev pytest -q \
  tests/test_foundry_brokered_protocol.py \
  tests/test_brokered_schema.py \
  tests/test_foundry_protocol.py
```

## Requirement evidence

| Plan requirement | Local evidence |
| --- | --- |
| Separate hosted Responses adapter path | `examples/harness/foundry-responses/{main.go,main_test.go,README.md,Dockerfile}` |
| Existing Assistants adapter remains Assistants-only | `examples/harness/foundry/README.md` distinguishes Assistants/threads from hosted Responses. |
| Hosted `/responses` initial request does not send request-level `tools` | `TestResponsesAdapterObservedTurnCompletes`, `TestResponsesAdapterBrokeredReadContinuationAndGoldenFixtures`, and conformance tests assert no `tools` field reaches the fake hosted endpoint. |
| Endpoint URL safety | `TestResponsesEndpointSafety`, `TestResponsesAdapterDoesNotFollowCredentialedRedirects`, and `TestSanitizeEndpointDoesNotReturnRawMalformedURL`. |
| Mutating/streaming harness endpoints require bearer auth | `TestResponsesAdapterPassesObservedConformanceByDefault`, `TestResponsesAdapterPassesBrokeredReadConformance`, and `TestResponsesAdapterPassesBrokeredWriteConformance` run harness conformance with `RequireAuth`. |
| Observed hosted response maps to `TurnCompleted` | `TestResponsesAdapterObservedTurnCompletes` and observed conformance. |
| Responses `function_call` maps to `ToolCallRequested` | `TestResponsesAdapterBrokeredReadContinuationAndGoldenFixtures`, `TestResponsesConsumesAgentKitBrokeredFixtures`, and brokered read/write conformance. |
| Orka `ToolCallResult` maps to `function_call_output` | `TestResponsesAdapterBrokeredReadContinuationAndGoldenFixtures`, `TestResponsesAdapterWriteParksUntilDeclinedApprovalContinue`, `TestResponsesAdapterContinuesToolExecutionFailurePayload`, golden fixtures, and AgentKit fixture tests. |
| Canonical error/decline encoding is stable | `TestCanonicalErrorAndDeclineOutputFixtures` and `TestResponsesAgentKitErrorPayloadFixturesMatchCanonicalEncoding`. |
| Unknown tool and malformed arguments are rejected before Orka execution | `TestResponsesAdapterRejectsUnknownToolBeforeOrkaExecution` and `TestResponsesAdapterRejectsMalformedArguments`. |
| Multiple hosted calls are buffered until all results arrive | `TestResponsesAdapterMultipleFunctionCallsBufferedAndContinued`. |
| Duplicate/replayed function calls fail closed | `TestResponsesRepeatedSubmittedFunctionCallFailsTurn` and `TestResponsesMixedRepeatedFunctionCallFailsTurn`. |
| Duplicate identical `/continue` is idempotent and conflicting duplicates reject | `TestResponsesAdapterDuplicateContinueIsIdempotentAndConflictsReject`, `TestResponsesAdapterAlreadySubmittedContinueDoesNotResubmit`, and `TestResponsesAdapterAlreadySubmittedPendingResultIsNoop`. |
| Continuation failures fail closed rather than duplicating hosted progress | `TestResponsesAdapterContinuationFailureFailsClosedWithoutDuplicatePost`. |
| Pending approval/tool waits are bounded | `TestResponsesAdapterPendingToolTimesOutWithoutContinuation`, `TestResponsesAdapterPendingTimeoutSkipsSubmittedCall`, and `TestResponsesAdapterBrokeredMaxTurnIncludesApprovalWait`. |
| Restart/state loss fails safely without hosted continuation | `TestResponsesAdapterStateLossContinueFailsSafely`. |
| Runtime session continuity | `TestResponsesAdapterRuntimeSessionHeaderReuse` and `TestResponsesAdapterContinuationUsesTurnSessionAfterSessionMapCleanup`. |
| Hosted response status handling is fail-closed | `TestResponsesFailureStatusDoesNotCompleteWithPartialText`, `TestResponsesFailureStatusWithFunctionCallFailsBeforeToolRequest`, `TestResponsesNonTerminalStatusDoesNotCompleteWithPartialText`, and `TestResponsesMissingStatusDoesNotCompleteWithPartialText`. |
| Large hosted output and platform failures are safe | `TestResponsesLargeOutputFails` and `TestResponsesInitialPlatformErrorDoesNotRetainTurn`. |
| No secrets or endpoint URLs in golden fixtures | `TestResponsesGoldenFixturesDoNotContainEndpointsOrSecrets`. |
| Existing Orka broker approval/idempotency/write ledger behavior | `go test ./internal/controller -run 'Test.*(AgentRuntime|Harness|Brokered|Runtime)'`, especially brokered write approval, decline, replay, and unresolved-ledger tests in `internal/controller/harness_wrapper_test.go`. |
| Kubernetes smoke skeleton is credentials-free | `examples/harness/foundry-responses/kubernetes.example.yaml` uses `REDACTED` placeholders and a read-only advertised class by default. |

## Live gates that cannot be satisfied by local fixtures

Use the credentials-safe live smoke helper as the first live preflight/deploy step:

```bash
examples/harness/foundry-responses/live-smoke.sh
examples/harness/foundry-responses/live-smoke.sh --apply --wait
```

The helper validates required environment without printing secret values and can
deploy the adapter `Deployment`, `Service`, `Secret` placeholders, and matching
`AgentRuntime` facade into the selected namespace. It does not replace the
required real hosted AgentKit deployment, downstream tools, human approvals, or
Fibey scenario verification.

The following remain required before declaring the full hosted Foundry/Fibey plan
complete:

1. Deploy an AgentKit prototype as a real Foundry hosted agent with static safe
   brokered schemas and a configured brokered continuation proof.
2. Deploy the Orka `foundry-responses` adapter with real Foundry auth and a real
   hosted `/responses` endpoint.
3. Verify `AgentRuntime` readiness for the read profile.
4. Run a read task and verify `ToolCallRequested` then successful completion.
5. Enable write only after the hosted AgentKit deployment has a static write
   schema and passes write conformance.
6. Run a write task and verify `ApprovalRequested`, no downstream execution
   before approval, approved continuation, idempotency key delivery, and no
   credential/tool URL leakage in logs.
7. Run the Fibey read/write scenario with `check-network-telemetry`,
   `get-active-incidents`, `dispatch-work-order`, and optional
   `escalate-incident`, then verify replay produces no second dispatch. Capture
   Orka task events and run:

   ```bash
   examples/fibey-custom-agent-demo/verify-foundry-responses.sh \
     --task fibey-foundry-responses-quincy-north-alert \
     --namespace <namespace>
   ```
