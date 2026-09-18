---
slug: /openai-compat
description: "Use Orka with OpenAI Chat Completions and stateless Responses clients."
---

# OpenAI-compatible API

Orka serves `/openai/v1/chat/completions`, stateless `/openai/v1/responses`, and `/openai/v1/models`,
so clients like [Continue](https://continue.dev/) and [Cursor](https://cursor.sh/) can point
at Orka instead of at a model vendor. Your cluster holds the API keys; the client holds a
ServiceAccount token.

For teams using separate namespaces, the optional
[shared compatibility router](../operations/shared-compatibility-api.md) selects
the installation from the validated ServiceAccount token. Clients use one base
URL and omit the namespace parameter. Direct installation endpoints keep their
existing namespace rules.

:::warning[This is not a transparent proxy by default]
Orka rewrites your request before sending it upstream: it **discards the tools your client
sent**, injects its own, and prepends its own system prompt. Read
[Coordinator mode](#coordinator-mode) before wiring up a client that relies on its own
tools. One header turns it off.
:::

:::info[Endpoints moved]
These used to live at `/v1/`. They are now at `/openai/v1/`. See
[Anthropic compatibility](anthropic-compat.md) for the Anthropic-native equivalent.
:::

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/openai/v1/chat/completions` | Chat completions (streaming & non-streaming) |
| `POST` | `/openai/v1/responses` | Stateless Responses (streaming & non-streaming) |
| `GET` | `/openai/v1/models` | List available models from configured providers |

All endpoints require authentication. Send a Kubernetes ServiceAccount token in
`Authorization: Bearer <token>`. OIDC tokens use the same header when OIDC is configured.

When transaction-token authentication is configured, send TxTokens in `Txn-Token: <token>`
by default. To accept TxTokens as Bearer tokens, the operator must explicitly include
`Authorization:Bearer` in `--context-token-headers`, for example
`--context-token-headers=Txn-Token,Authorization:Bearer`.
See [Authentication](./api-reference.md#authentication).

## Responses API

`POST /openai/v1/responses` requires **`store:false` on every request**. Orka
forwards it to upstream OpenAI Responses and Chat Completions requests, including
API-mode probes and coordinator tool rounds. Response IDs are transient labels;
Orka does not save Responses objects or conversation history.

```bash
curl https://orka.example.com/openai/v1/responses \
  -H "Authorization: Bearer $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -H "X-Orka-Tools: disabled" \
  -d '{"model":"openai/gpt-4.1","store":false,"input":"Hello!","stream":true}'
```

Provider refusal items are not supported in this first version. They return HTTP
422 with error code `unsupported_provider_outcome`, or a streaming `error` event
with that code followed by `response.failed`. The nested response error retains
the schema-defined `server_error` code and an explicit unsupported-refusal
message; upstream refusal details are not forwarded.

The first version supports:

- String input, text message arrays, `instructions`, and system/developer messages.
- `function` tools and `function_call` / `function_call_output` history. In client
  tool mode, append the returned output items and a result with the same `call_id`
  to your next request's `input`. Include the preceding history on every request.
  Responses upstreams receive text and function items in their supplied order.
  Native Responses output retains message boundaries and text/function order,
  including SSE calls that finish out of order. Missing stream indices are
  recovered from known item IDs; ambiguous or contradictory ordering metadata
  fails explicitly. Entirely unindexed text-only streams remain supported.
  Coordinator continuation keeps
  those items together as one turn for context truncation.
  Chat Completions fallback represents consecutive assistant items as one turn,
  combining their text and function calls in that API's format.
- `text.format` with `text`, `json_object`, or `json_schema` (including schema name
  and strictness). Structured output requires an OpenAI-compatible upstream;
  Anthropic structured output returns an explicit error in this version.
- `temperature`, positive `max_output_tokens`, `tool_choice:"auto"`, and the
  default allowance for multiple function calls.
- Responses SSE events: creation/progress, output items, text and function
  argument deltas, item completion, and response completion. Token-budget
  truncation returns `response.incomplete`, including an empty `output` when no
  text was produced; provider failures emit `error` and
  `response.failed`. Disconnects and the configured duration limit cancel work.
  A client that stops reading is disconnected after the duration limit plus a
  one-second grace period for terminal events.

Coordinator mode is the default here too. Orka replaces client tools and executes
its own tools on the server. Streaming emits text progress between coordinator
rounds; function arguments are emitted as complete deltas once the upstream
provider has assembled a call. Internal coordinator calls are not returned as
client-executable function items. Use `X-Orka-Tools: disabled` for client tools.

Unsupported fields and inputs produce HTTP 400 errors. These include missing or
true `store`, `previous_response_id` (even null), `conversation` (even null),
background execution, saved item references, reasoning items/options, non-empty
`include`, OpenAI-hosted tools, images/audio, non-auto `tool_choice`, and
`parallel_tool_calls:false`. Retrieval, deletion, and saved-conversation APIs
are not implemented. A base URL change alone does not configure a compatible
client.

### Agent Framework

The executable interoperability fixture pins `agent-framework-core==1.18.0`,
`agent-framework-openai==1.14.3`, and `openai==3.14.0`. Its `OpenAIChatClient`
uses Responses. Set `store=False`; set the disabled header for client-managed
tools. This pinned client also automatically requests encrypted reasoning, so
use its public SDK `extra_body` option to send `include:[]`:

```python
import os
from agent_framework import Agent
from agent_framework.openai import OpenAIChatClient

client = OpenAIChatClient(
    model="openai/gpt-4.1",
    base_url="https://orka.example.com/openai/v1",
    api_key=os.environ["ORKA_TOKEN"],
    default_headers={"X-Orka-Tools": "disabled"},
)
agent = Agent(
    client=client,
    default_options={"store": False, "extra_body": {"include": []}},
)
session = agent.create_session()  # The client owns and resends history.
first = await agent.run("Hello!", session=session)
followup = await agent.run("Summarize our exchange.", session=session)
```

Omit the disabled header for coordinator mode. Run the deterministic test from
a checkout to exercise the real client against Orka's production server routes
and JWT authentication,
including an actual tool and follow-up in both modes, with and without streaming:

```bash
python3 -m venv bin/responses-client
bin/responses-client/bin/pip install -r scripts/fixtures/agent-framework-responses/requirements.txt
make ensure-ui-embed
ORKA_RESPONSES_INTEROP_PYTHON="$PWD/bin/responses-client/bin/python" \
  go test ./internal/api -run 'TestAgentFrameworkResponsesInterop|TestResponsesPinnedSDKSchema' -count=1 -timeout=120s -v
```

This CPU fixture uses local HTTP servers and ephemeral OIDC credentials. Model
output and Kubernetes object storage are fixtures; it does not test live model
inference or a deployed controller. These Python checks are opt-in and are
skipped by ordinary Go test runs unless the environment variable above is set.
The executable client lives in `scripts/fixtures/agent-framework-responses/client.py`.
It verifies client tool execution, coordinator tool execution, and client-owned
follow-up history; it does not establish behavior of a particular hosted model.

## Model name format

The `model` field supports two formats:

- **`provider/model`** — e.g., `anthropic/claude-sonnet-4-20250514`. The part before `/` matches a Provider CRD name, and the part after is the model name sent to that provider.
- **`model`** — e.g., `claude-sonnet-4-20250514`. Uses the default provider (from `--chat-provider` flag or a Provider CRD named `default`).

## Prerequisites

1. **Provider CRD** configured in the cluster:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic
  namespace: orka-system
spec:
  type: anthropic
  secretRef:
    name: anthropic-secret
    key: api-key
  defaultModel: claude-sonnet-4-20250514
```

2. **Secret** with the API key:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: anthropic-secret
  namespace: orka-system
type: Opaque
stringData:
  api-key: sk-ant-...
```

### Azure OpenAI provider example

If you use Azure OpenAI, configure a Provider with `type: azure-openai`:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: azure-openai
  namespace: orka-system
spec:
  type: azure-openai
  secretRef:
    name: azure-openai-secret
    key: api-key
  baseURL: https://<resource>.openai.azure.com
  defaultModel: gpt-4o-deployment
  azure:
    deploymentName: gpt-4o-deployment
    apiVersion: "2024-02-15-preview"
```

3. **ServiceAccount token and RBAC** for authentication and coordinator tools:

Follow the [API-client setup](../getting-started.md#connect-to-the-api) to
configure `orka-client` and its Task permissions. Coordinator Task creation requires
`tasks/create`. Agent creation currently does not check a ServiceAccount caller's
`agents/create` permission; see the [API authorization limitation](../operations/troubleshooting.md#i-get-403-from-the-api).
Then create a token:

```bash
export ORKA_TOKEN="$(kubectl -n orka-system create token orka-client)"
```

## Using with Continue

### Configuration

Configure Continue to use Orka as an OpenAI-compatible provider. Add to your Continue configuration:

```json
{
  "models": [
    {
      "title": "Claude Sonnet 4 (via Orka)",
      "provider": "openai",
      "model": "anthropic/claude-sonnet-4-20250514",
      "apiBase": "https://orka.example.com/openai/v1",
      "apiKey": "YOUR_ORKA_TOKEN"
    }
  ]
}
```

### Environment

Set your Orka API token:

```bash
export ORKA_TOKEN=$(kubectl -n orka-system create token orka-client)
```

## Using with curl

### Non-streaming

```bash
curl -X POST https://orka.example.com/openai/v1/chat/completions \
  -H "Authorization: Bearer $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'
```

### List models

```bash
curl https://orka.example.com/openai/v1/models \
  -H "Authorization: Bearer $ORKA_TOKEN"
```

## Supported features

| Feature | Supported |
|---------|-----------|
| Chat completions | Yes |
| Streaming (SSE) | Yes |
| Tool/function calling | Server-side only by default — see [Coordinator mode](#coordinator-mode) |
| System messages | Yes, but Orka's own prompt is prepended in coordinator mode |
| Multi-part content | Yes (text parts extracted) |
| `max_tokens` / `max_completion_tokens` | Yes |
| `temperature` | Yes |
| `stop` sequences | Yes |
| `stream_options.include_usage` | Yes |
| Image inputs | Not yet (text extracted from multi-part) |
| Embeddings | Not supported |
| Audio / Vision | Not supported |

## Coordinator mode

Both compatibility endpoints — this one and
[Anthropic](anthropic-compat.md) — default to **coordinator mode**. The idea is that your
editor's chat window becomes a way to drive the cluster: ask for a change, and the model
creates Agents and Tasks, waits for them, and opens a pull request. It is not a pass-through
proxy.

Concretely, Orka does five things to every request before it reaches the model:

| # | What happens | Consequence for you |
| --- | --- | --- |
| 1 | **Your `tools` array is discarded.** Not merged — replaced. | Your client's own tools never run. |
| 2 | 18 built-in Orka tools are injected. | The model can act on your cluster with the tools listed below. |
| 3 | The tool list is filtered against your context token's allowed tools, if you use [transaction tokens](../concepts/transaction-tokens.md). | Denied tools disappear rather than failing at call time. |
| 4 | A large Orka system prompt is **prepended** to yours. | Your system prompt still applies, but it is no longer first. |
| 5 | Tool history is stripped from your messages. `role: tool` messages are dropped; assistant messages keep their text but lose their tool calls; consecutive same-role messages are merged. | Sending back a conversation that contains client-side tool use loses that structure. |

Orka then runs the tool loop itself and returns the final answer.

The 18 injected tools:

| Group | Tools |
| --- | --- |
| Built-in | `web_search`, `web_fetch`, `code_exec`, `file_read`, `file_write` |
| Create work | `create_agent`, `create_agent_task`, `create_ai_task`, `create_container_task`, `create_pr_monitor` |
| Track work | `check_task_progress`, `fetch_task_output`, `wait_for_task`, `cancel_task`, `list_agents`, `list_tasks` |
| Pull requests | `create_pull_request`, `check_pull_request_ci` |

Orka advertises only the tools listed above that are registered for server-side execution.
Custom Kubernetes `Tool` resources are not advertised or executed by either compatibility
endpoint. A custom `Tool` with a built-in name cannot replace or duplicate its definition.

### Turning it off

```
X-Orka-Tools: disabled
```

This header disables the coordinator rewrite and Orka's tool loop. Your client's tool
definitions pass through unchanged, and your client manages its own tool execution.
Requests still pass through Orka's OpenAI request conversion;
for example, `top_p`, `frequency_penalty`, and `presence_penalty` are not forwarded to the
provider. Provider resolution and credential handling are unchanged.

Use `disabled` when you want Orka only for centralized credentials and model routing. Leave
it on when you want the model to be able to do things in the cluster.

## Request path

```
┌─────────────┐     ┌─────────────────────────────┐     ┌───────────────┐
│ Continue    │────▶│ Orka API server             │────▶│ Anthropic API │
│ (or any     │◀────│ /openai/v1/chat/completions │◀────│ OpenAI API    │
│ OAI client) │     │                             │     │ Azure OpenAI  │
└─────────────┘     │ 1. Resolve Provider CRD     │     └───────────────┘
                    │ 2. Read API key from Secret │
                    │ 3. Coordinator rewrite      │
                    │    (unless X-Orka-Tools:    │
                    │     disabled)               │
                    │ 4. Server-side tool loop    │
                    └─────────────────────────────┘
```

## Token budgets and "incomplete" errors

`max_tokens` caps the model's *entire* output for a turn, and for reasoning
models that budget is shared with hidden reasoning tokens. Two shapes come
back when the budget runs out:

- **Partial text** — the response was cut off mid-answer. Orka returns the
  partial text with `finish_reason: "length"`, matching upstream behavior; raise the budget
  and retry.
- **Nothing usable** — the model spent the whole budget before emitting any
  text (common when a reasoning model gets a small `max_tokens`), or the
  cutoff truncated a tool call, whose arguments would be unsafe to execute.
  Orka fails the request with an error describing an *incomplete completion
  outcome* instead of returning an empty or corrupt message.

If you see the incomplete-outcome error, it is not a proxy fault: give the
request a substantially larger `max_tokens` (reasoning models often need
thousands of tokens of headroom), or use a non-reasoning model for short
completions.
