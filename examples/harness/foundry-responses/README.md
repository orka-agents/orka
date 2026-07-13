# Foundry hosted Responses AgentRuntime adapter

This adapter presents an Azure AI Foundry **hosted AgentKit agent** endpoint as an `orka.harness.v1` runtime. It targets endpoint-scoped hosted Responses:

```text
POST /agents/<agent-name>/endpoint/protocols/openai/responses?api-version=...
```

Use this adapter for AgentKit agents deployed as Foundry hosted agents. Use `examples/harness/foundry` only for the older Assistants/threads/run protocol.

## Security model

- The adapter never sends request-level `tools` to hosted `/responses`; hosted AgentKit must be statically configured with the safe function schemas it is allowed to request.
- Orka remains authoritative. Hosted AgentKit can request a function by name, but Orka still validates the request against the Task policy and Tool CRDs, performs approval checks, injects idempotency keys, executes/brokers the tool, and audits the result.
- Orka Tool URLs, auth refs, headers, and production tool credentials are not sent to Foundry.
- Adapter bearer tokens and Foundry credentials must live in Kubernetes Secrets or environment variables. Do not put them in `AgentRuntime` specs or logs.

## Configuration

| Env var | Purpose |
| --- | --- |
| `ORKA_FOUNDRY_RESPONSES_ADAPTER_ADDR` | HTTP listen address, default `:8090`. |
| `ORKA_FOUNDRY_RESPONSES_RUNTIME_NAME` | Runtime name advertised in `/v1/capabilities`, default `foundry-agentkit-responses`. |
| `ORKA_FOUNDRY_RESPONSES_ADAPTER_BEARER_TOKEN` | Bearer token Orka uses for mutating/streaming harness endpoints. |
| `ORKA_FOUNDRY_RESPONSES_ENDPOINT` | Preferred full hosted Responses endpoint URL, including `/agents/<name>/endpoint/protocols/openai/responses`. The adapter appends `api-version` from `ORKA_FOUNDRY_RESPONSES_API_VERSION` when missing. |
| `ORKA_FOUNDRY_RESPONSES_PROJECT_ENDPOINT` + `ORKA_FOUNDRY_RESPONSES_AGENT_NAME` | Optional alternative to build the hosted Responses endpoint from a project endpoint and agent name. |
| `ORKA_FOUNDRY_RESPONSES_API_VERSION` | API-version query value, default `v1`, matching the current `AIProjectClient.get_openai_client(agent_name=...)` default query. Override it for deployments pinned to another preview version. |
| `ORKA_FOUNDRY_RESPONSES_API_KEY` | Static API-key auth mode. Tests/demo only unless your deployment standard permits it. |
| `ORKA_FOUNDRY_RESPONSES_AUTH_BEARER` | Static bearer auth mode. Tests/demo only unless supplied by a production token refresher sidecar. |
| `ORKA_FOUNDRY_RESPONSES_TOKEN_AUDIENCE` | Reserved for future workload-identity token refresh support; currently not used. |
| `ORKA_FOUNDRY_RESPONSES_BROKERED_CONTINUATION_PROOF` | Optional Orka-only proof value sent on hosted Responses continuations in both the `X-AgentKit-Brokered-Continuation-Proof` header and `brokered_continuation_proof` request-body field, so gateways that strip custom headers can still forward it. Set it to match AgentKit's `AGENTKIT_FOUNDRY_BROKERED_CONTINUATION_PROOF` when that guard is enabled. |
| `ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES` | Comma-separated static classes the hosted AgentKit deployment has been configured and conformance-tested to request, e.g. `read` or `read,write`. Empty means observed-only. |
| `ORKA_FOUNDRY_RESPONSES_POLL_TIMEOUT` | Per-request timeout for hosted Responses calls, default `20s`. |
| `ORKA_FOUNDRY_RESPONSES_STATE_RETENTION` | How long terminal in-memory turn/session state is retained, default `10m`. |
| `ORKA_FOUNDRY_RESPONSES_MAX_APPROVAL_WAIT` | Maximum time a pending brokered call may wait before a late continuation fails safely, default `30m`. |

Exactly one Foundry auth mode (`API_KEY` or `AUTH_BEARER`) must be set.

## Endpoint safety rules

The hosted Responses endpoint must:

- use HTTPS in production;
- use HTTP only for loopback tests (`localhost`, `127.0.0.1`, or `::1`);
- end in `/responses`;
- not include username/password, fragments, or query parameters other than `api-version`.

The adapter returns degraded health and rejects starts when the endpoint is unsafe. `GET /v1/health` always returns a harness health body; `GET /v1/ready` is a Kubernetes readiness helper that returns HTTP 503 until the same configuration is ready.

## Capability discipline

Capabilities must reflect the **static schemas actually deployed in AgentKit**:

- If `ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES` is empty, the adapter advertises only `observed` and `supportsContinuation=false`.
- If it is `read`, the adapter advertises brokered read only.
- Advertise `write` only after the hosted AgentKit deployment has a static write schema and passes write conformance. Orka will still gate the write with approval/idempotency, but the hosted model must not be told it can request writes unless that path is intentionally enabled.

AgentRuntime readiness deliberately deep-probes every advertised brokered class. Because this adapter never sends request-level `tools`, the hosted AgentKit deployment must statically expose the probe-only `conformance_read` and/or `conformance_write` schemas in addition to its real tools. These schemas take an empty object and must be safe to call: Orka's conformance client supplies the synthetic result and no production tool credential is sent to Foundry. Fake-server tests alone are not sufficient; if the hosted deployment cannot request the matching probe tool, leave that brokered class unadvertised and readiness will fail closed.

## Protocol mapping

Initial turn:

```json
{"input":"Investigate incident"}
```

No `tools` field is sent.

Hosted AgentKit function call:

```json
{"type":"function_call","call_id":"call_1","name":"check-network-telemetry","arguments":"{\"site\":\"quincy-north\"}"}
```

Adapter emits `ToolCallRequested` with the exact `call_id`, function name, and compact JSON object arguments. If a hosted response returns multiple `function_call` items, the adapter emits one `ToolCallRequested` frame per call and waits until Orka returns every pending result before continuing.

Orka continuation:

```json
{"type":"function_call_output","call_id":"call_1","output":"{\"approved\":true,\"output\":{\"success\":true}}","status":"completed"}
```

The hosted continuation request includes `previous_response_id`, `agent_session_id` when a Foundry session is known, and one or more `function_call_output` items. Raw REST calls include the hosted-agent feature header required by Foundry hosted-agent endpoints.

## Output and error encoding

`function_call_output.output` is always a compact JSON string:

- successful tool result: `{"approved":true,"output":<ToolCallResult.Output>}`
- declined approval or policy/execution error: `{"approved":false,"error":<ErrorInfo>}`

Approval decline, tool policy rejection, and tool execution failure fixtures live under `testdata/golden/`.

## State, restart, and sessions

This MVP stores turn state in memory. That is intentionally fail-safe:

- duplicate identical `/continue` calls for a submitted call are accepted without a second hosted continuation;
- conflicting duplicate `/continue` calls are rejected;
- if the adapter restarts while a tool approval is pending, `/continue` returns `turn not found` and does not call Foundry, so the adapter itself does not duplicate a side effect;
- Orka's broker/idempotency ledger remains the source of truth for actual write execution.

The adapter captures Foundry session identifiers from the hosted response body (`agent_session_id`) and compatibility headers such as `x-agent-session-id`, then reuses the session in the `agent_session_id` request field for later calls with the same Orka `runtimeSessionID`. Session/auth values are stored only in memory and are not logged.

## Local build

```bash
docker build -t ghcr.io/orka-agents/orka/foundry-responses-harness-adapter:latest -f examples/harness/foundry-responses/Dockerfile .
```


## Kubernetes smoke skeleton

`kubernetes.example.yaml` contains a credentials-free Deployment, Service, Secret placeholders, and matching `AgentRuntime` facade for a read-profile hosted Responses smoke. Replace the `REDACTED` values through your secret-management flow, set the hosted Responses endpoint or project/agent-name pair, ensure the hosted agent statically exposes the probe-only `conformance_read` schema, and keep `ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES` narrowed to classes whose live AgentRuntime conformance passed.

```bash
kubectl apply -f examples/harness/foundry-responses/kubernetes.example.yaml
kubectl wait --for=condition=Ready agentruntime/sample-foundry-responses-runtime --timeout=60s
```

For write-profile smoke, first prove the hosted AgentKit deployment has a static write schema, then add `write` to both the adapter env and the `AgentRuntime.spec.capabilities.brokeredToolClasses`. Orka still performs approval and idempotent write execution; the hosted endpoint receives only `function_call_output` continuations.

## Tests

```bash
go test ./examples/harness/foundry-responses
```

For the full deterministic local validation bundle, including the focused Orka harness/controller suites and explicit AgentKit fixture tests when --agentkit is provided:

```bash
examples/harness/foundry-responses/validate.sh
```

For the live Foundry hosted AgentKit smoke gate, first run the credentials-safe preflight and then apply only when your current Kubernetes context is the intended Orka cluster:

```bash
examples/harness/foundry-responses/live-smoke.sh
examples/harness/foundry-responses/live-smoke.sh --apply --wait

# After the live task completes, capture redacted evidence.
examples/harness/foundry-responses/live-evidence.sh \
  --namespace <namespace> \
  --runtime fibey-agentkit-foundry-responses \
  --task fibey-foundry-responses-quincy-north-alert
```

The tests use a fake hosted Responses server and golden fixtures for initial requests, function calls, `ToolCallRequested`, continuations, final messages, error encoding, and buffered multiple-call behavior.

See [`VALIDATION.md`](VALIDATION.md) for the brokered-plan evidence matrix, local commands, and remaining live Foundry/Fibey gates.

## Troubleshooting

| Symptom | Likely cause | Fix |
| --- | --- | --- |
| Health is degraded | Missing adapter bearer, missing/unsafe endpoint, bad brokered class config, or missing Foundry auth | Check env vars and endpoint safety. |
| `tools` rejected by hosted endpoint | You are using the wrong adapter or adding request-level tools | This adapter intentionally never sends `tools`; configure AgentKit static schemas instead. |
| Brokered start rejected | Adapter does not advertise the requested tool class | Set `ORKA_FOUNDRY_RESPONSES_BROKERED_TOOL_CLASSES` only after AgentKit static schema/conformance is ready. |
| Turn fails with unknown tool | Hosted AgentKit requested a function name not present in Orka's safe `StartTurnRequest.input.tools` | Fix the AgentKit static schema or Task `allowedTools`. |
| Turn fails with malformed arguments | Hosted AgentKit emitted non-object or invalid JSON arguments | Fix the hosted schema/prompting; Orka rejects before tool execution. |
| Continue returns `turn not found` after restart | In-memory state was lost while waiting for approval | Re-run/fail the Task safely; no hosted continuation was sent by the restarted adapter. |
