# AgentRuntime adapter contract

`orka.harness.v1` is the stable Orka-facing contract for remote execution backends.

## Mandatory endpoints

- `GET /v1/health`
- `GET /v1/capabilities`
- `POST /v1/turns`
- `GET /v1/turns/{turnID}/events?afterSeq=N`
- `POST /v1/turns/{turnID}/cancel` when `supportsCancel=true`
- `POST /v1/turns/{turnID}/continue` when brokered profiles are advertised

## Capability profiles

| Profile | Capabilities | Required behavior |
| --- | --- | --- |
| observed | `toolExecutionModes: [observed]` | start, stream, terminal result/failure |
| brokered read | `brokered`, `brokeredToolClasses: [read]`, `supportsContinuation` | emit `ToolCallRequested`, accept `ToolCallResult`, complete |
| brokered write | `brokeredToolClasses: [write]` | request write intent and wait for Orka continuation after approval |
| coordination | `brokeredToolClasses: [coordination]` | request Orka coordination tools such as `delegate_task` and `wait_for_tasks` |

Adapters can be brokered-only when they pass the advertised brokered conformance profile.

## StartTurn safe tool schemas

When a Task exposes brokered tools, `StartTurnRequest.input.tools` carries safe definitions:

```json
{
  "name": "support-ticket-lookup",
  "description": "Look up sanitized support evidence",
  "brokeredClass": "read",
  "parameters": {"type": "object"}
}
```

Adapters must treat these definitions as requestable capabilities only. They are not execution instructions and intentionally omit URLs, credentials, headers, and Secret refs.

## Event rules

- Use `ToolCallRequested` to ask Orka for a tool call.
- Use `/continue` responses to receive Orka-owned `ToolCallResult` values.
- Do not create canonical approval state yourself. A runtime-originated `ApprovalRequested` frame is persisted only as a runtime diagnostic; Orka creates canonical approvals when policy requires them.
- Preserve `runtimeSessionID`, `turnID`, `correlationID`, and monotonically increasing `seq` on every frame.

## Structured results

Adapters may put structured fields on `TurnCompleted`:

```json
{
  "result": "investigation complete",
  "data": {"incident": "INC-1"},
  "artifacts": [{"filename": "evidence.json", "contentType": "application/json", "size": 42}]
}
```

Orka stores this as the standard structured result envelope so parent tasks can consume `data` and artifact metadata through `wait_for_tasks`.

## Local validation

Go adapters can import `github.com/orka-agents/orka/pkg/harness` and `github.com/orka-agents/orka/pkg/harness/conformance`. The generic HTTP fixture in `examples/harness/echo` is the reference implementation for observed and brokered read/write profiles. Provider-specific adapters, including the Foundry adapter, live in separate repositories.
