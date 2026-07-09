# Foundry Assistants/threads AgentRuntime adapter

This example adapter presents the Azure AI Foundry/OpenAI **Assistants threads/runs** protocol as an `orka.harness.v1` AgentRuntime endpoint.

It is intentionally **not** the adapter for AgentKit agents deployed as Foundry hosted agents. Hosted AgentKit uses endpoint-scoped Responses and must be driven by `examples/harness/foundry-responses` instead.

## Protocol target

This adapter uses the Assistants-style API shape:

```text
POST /threads
POST /threads/{threadID}/runs with assistant_id
GET  /threads/{threadID}/runs/{runID}
POST /threads/{threadID}/runs/{runID}/submit_tool_outputs
```

Because it targets Assistants/runs, it may send safe Orka tool schemas in the run request and it requires a real Assistants `assistant_id`. Do not set `ORKA_FOUNDRY_AGENT_ID` to a hosted AgentKit agent name; hosted agents are not `asst_*` Assistants resources.

For Foundry hosted AgentKit over Responses:

- use `examples/harness/foundry-responses`;
- do not send request-level `tools`;
- statically configure safe brokered schemas in AgentKit;
- resume with `function_call_output` and `previous_response_id`.

## Configuration

| Env var | Purpose |
| --- | --- |
| `ORKA_FOUNDRY_ADAPTER_ADDR` | HTTP listen address, default `:8090`. |
| `ORKA_FOUNDRY_RUNTIME_NAME` | Runtime name advertised in `/v1/capabilities`. |
| `ORKA_FOUNDRY_ADAPTER_BEARER_TOKEN` | Bearer token Orka uses for mutating harness endpoints. |
| `ORKA_FOUNDRY_ENDPOINT` | Foundry Assistants endpoint base URL. Must be HTTPS in production; plain HTTP is accepted only for loopback/local tests. Do not include userinfo, query strings, or fragments. Put API versions in `ORKA_FOUNDRY_API_VERSION`. |
| `ORKA_FOUNDRY_AGENT_ID` | Assistants `assistant_id` for the thread run. |
| `ORKA_FOUNDRY_API_KEY` | Foundry API key, sent as `api-key`. |
| `ORKA_FOUNDRY_AUTH_BEARER` | Optional bearer auth alternative. |
| `ORKA_FOUNDRY_API_VERSION` | API version query parameter, default `v1`. |

Use Kubernetes Secrets for every token/key value. Do not put Foundry credentials in `AgentRuntime` CRDs.

For `AgentRuntime` readiness/conformance, the Assistants runtime must follow Orka brokered-tool probe prompts by calling exactly one provided function tool (for example `conformance_read` or `conformance_write`) and completing after Orka returns the brokered result. The adapter enforces that Foundry can only request tools supplied in the current `StartTurnRequest.input.tools` payload.

## Protocol mapping

- `StartTurnRequest` creates a Foundry thread and run.
- In brokered mode, Orka safe `input.tools` schemas are passed to the Assistants run as function definitions only; Orka Tool URLs and credentials are never sent.
- Foundry `requires_action.submit_tool_outputs.tool_calls[]` is mapped to `ToolCallRequested` frames.
- `/v1/turns/{turnID}/continue` submits Orka-brokered tool outputs back to Foundry.
- Foundry completion messages are mapped to `TurnCompleted`.
- Cancellation calls the Foundry run cancel endpoint when a run exists.

## Local build

```bash
docker build -t ghcr.io/orka-agents/orka/foundry-harness-adapter:latest -f examples/harness/foundry/Dockerfile .
```

The unit tests use a fake Foundry-compatible HTTP server and run observed plus brokered read/write conformance probes.
