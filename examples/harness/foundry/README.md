# Foundry AgentRuntime adapter

This example adapter presents Azure AI Foundry hosted agents as an `orka.harness.v1` AgentRuntime endpoint. It keeps Foundry-specific IDs and credentials inside the adapter deployment while Orka keeps owning task lifecycle, brokered tool policy, approvals, idempotency, and result storage.

## Configuration

| Env var | Purpose |
| --- | --- |
| `ORKA_FOUNDRY_ADAPTER_ADDR` | HTTP listen address, default `:8090`. |
| `ORKA_FOUNDRY_RUNTIME_NAME` | Runtime name advertised in `/v1/capabilities`. |
| `ORKA_FOUNDRY_ADAPTER_BEARER_TOKEN` | Bearer token Orka uses for mutating harness endpoints. |
| `ORKA_FOUNDRY_ENDPOINT` | Foundry agents endpoint base URL. Must be HTTPS in production; plain HTTP is accepted only for loopback/local tests. Do not include userinfo, query strings, or fragments. Put API versions in `ORKA_FOUNDRY_API_VERSION`. |
| `ORKA_FOUNDRY_AGENT_ID` | Foundry hosted-agent ID. |
| `ORKA_FOUNDRY_API_KEY` | Foundry API key, sent as `api-key`. |
| `ORKA_FOUNDRY_AUTH_BEARER` | Optional bearer auth alternative. |
| `ORKA_FOUNDRY_API_VERSION` | API version query parameter, default `v1`. |

Use Kubernetes Secrets for every token/key value. Do not put Foundry credentials in `AgentRuntime` CRDs.

For `AgentRuntime` readiness/conformance, the hosted Foundry agent must follow Orka brokered-tool probe prompts by calling exactly one provided function tool (for example `conformance_read` or `conformance_write`) and completing after Orka returns the brokered result. The adapter enforces that Foundry can only request tools supplied in the current `StartTurnRequest.input.tools` payload.

## Protocol mapping

- `StartTurnRequest` creates a Foundry thread and run.
- Orka safe `input.tools` schemas are passed to Foundry as function definitions only; Orka Tool URLs and credentials are never sent.
- Foundry `requires_action.submit_tool_outputs.tool_calls[]` is mapped to `ToolCallRequested` frames.
- `/v1/turns/{turnID}/continue` submits Orka-brokered tool outputs back to Foundry.
- Foundry completion messages are mapped to `TurnCompleted`.
- Cancellation calls the Foundry run cancel endpoint when a run exists.

## Local build

```bash
docker build -t ghcr.io/orka-agents/orka/foundry-harness-adapter:latest -f examples/harness/foundry/Dockerfile .
```

The unit tests use a fake Foundry-compatible HTTP server and run observed plus brokered read/write conformance probes.
