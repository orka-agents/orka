---
slug: /anthropic-compat
---

# Anthropic-Compatible API

Orka exposes an **Anthropic-compatible Messages API** at `/anthropic/v1/messages`, enabling Anthropic-compatible clients (Claude Code, etc.) to use Orka as a transparent proxy.

Orka acts as a **proxy** to whichever LLM provider is configured in your cluster, with credentials managed securely via Kubernetes Secrets and Provider CRDs. See also [OpenAI Compatibility](openai-compat.md) for the OpenAI-compatible proxy.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/anthropic/v1/messages` | Create a message (streaming & non-streaming) |
| `GET` | `/anthropic/v1/models` | List available models from configured providers |

PR-blocking live CI exercises this API directly against a live Claude-family backend by checking `/anthropic/v1/models` and both non-streaming and streaming `/anthropic/v1/messages` requests. Those live checks keep the transparent-proxy default; server-side Orka tool-loop behavior is exercised only when the client explicitly sets `X-Orka-Tools: enabled`.

## Authentication

Two authentication methods are supported:

- **`x-api-key: <orka-token>`** — Anthropic convention (recommended for Anthropic clients)
- **`Authorization: Bearer <orka-token>`** — Standard Bearer token

Both use a Kubernetes ServiceAccount token as the value.

## Model Name Format

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
  namespace: default
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
  namespace: default
type: Opaque
stringData:
  api-key: sk-ant-...
```

3. **ServiceAccount token** for authentication:

```bash
# Create a service account
kubectl create serviceaccount orka-client

# Bind it to the orka viewer role (or a custom role)
kubectl create clusterrolebinding orka-client-binding \
  --clusterrole=orka-task-viewer \
  --serviceaccount=default:orka-client

# Get a token
export ORKA_TOKEN=$(kubectl create token orka-client)
```

## Using with Claude Code

Configure Claude Code to route all API calls through Orka:

```bash
export ANTHROPIC_BASE_URL=https://orka.example.com/anthropic
export ANTHROPIC_API_KEY=$(kubectl create token orka-client)
# Claude Code will now route all API calls through Orka
```

## Using with curl

### Non-streaming

```bash
curl -X POST https://orka.example.com/anthropic/v1/messages \
  -H "x-api-key: $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Streaming

```bash
curl -X POST https://orka.example.com/anthropic/v1/messages \
  -H "x-api-key: $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

### List models

```bash
curl https://orka.example.com/anthropic/v1/models \
  -H "x-api-key: $ORKA_TOKEN"
```

## Supported Features

| Feature | Supported |
|---------|-----------|
| Messages API | Yes |
| Streaming (SSE) | Yes |
| Tool use (function calling) | Yes |
| Extended thinking (`thinking` content blocks with `budget_tokens`) | Yes |
| System messages (string format) | Yes |
| System messages (content block array format) | Yes |
| `max_tokens` | Yes |
| `temperature` | Yes |
| `stop_sequences` | Yes |
| Image inputs | Not yet |
| PDF inputs | Not supported |

## Server-Side Tool Execution

By default, the Anthropic endpoint is a **transparent proxy**: requests are forwarded to the LLM and responses are returned without intercepting tool calls. The client manages its own tool execution loop.

To opt in to **server-side tool execution**, set the `X-Orka-Tools: enabled` header:

```
X-Orka-Tools: enabled
```

When this header is set, Orka injects its built-in tools into the request and runs an autonomous tool loop. When the LLM returns `tool_use` content blocks, the proxy intercepts them, executes the tools, feeds results back to the LLM, and repeats until a final text response is produced.

### Available Tools

When `X-Orka-Tools: enabled` is set, the proxy automatically injects these built-in tools into the request:

| Tool | Description |
|------|-------------|
| `web_search` | Search the web for information |
| `code_exec` | Execute code snippets in a sandbox |
| `file_read` | Read file contents from workspace |
| `file_write` | Write files to workspace |
| `web_fetch` | Fetch and extract content from URLs |

Additionally, any [Tool CRDs](../concepts/configuration.md) defined in the user's namespace are automatically included as custom HTTP tools.

Client-provided tools in the request are preserved and merged with the injected tools.

### How It Works

1. Client sends a `POST /anthropic/v1/messages` request (tools are injected automatically)
2. Proxy injects Orka tools into the request and forwards to the LLM
3. If the LLM returns `tool_use` blocks:
   - Proxy executes each tool server-side
   - Tool results are appended to the conversation
   - Proxy calls the LLM again with updated context
4. Steps 2-3 repeat until the LLM returns a text-only response
5. Final response is returned to the client

### Streaming Behavior

When `stream: true`, the proxy streams Anthropic SSE events throughout the entire tool loop:

- **`message_start`**: Emitted once at the beginning
- **`content_block_start/delta/stop`**: Streamed for each text and `tool_use` block from the LLM
- **Tool result blocks**: After executing each tool, the result is streamed as a text content block (e.g., `[Tool web_search result]: ...`)
- **`message_delta` + `message_stop`**: Emitted once at the end

This means clients see real-time progress as tools are called and results are produced, even across multiple LLM round-trips.

### Limits and Timeouts

| Setting | Default | Description |
|---------|---------|-------------|
| Max iterations | 50 | Maximum number of LLM calls per request |
| Max duration | 30 minutes | Overall request timeout |
| Tool timeout | 60 seconds | Per-tool execution timeout |
| Max session size | 500 KB | Conversation size budget (triggers truncation) |

These values come from the chat configuration and apply to both streaming and non-streaming requests.

When the iteration limit is reached, the proxy injects a summary prompt and makes one final LLM call without tools to produce a closing response.

### Repetition Detection

If the LLM calls the same tool with identical arguments 3 or more times, the proxy injects a warning message asking it to try a different approach. This prevents infinite loops where the LLM repeatedly calls a failing tool.

### Error Handling

- **Tool execution errors**: Wrapped as JSON results (`{"success": false, "error": "..."}`) and fed back to the LLM, which can decide how to recover
- **LLM errors**: If the LLM returns a context-too-long error, the proxy truncates the conversation to ~50% and retries once. Other LLM errors terminate the loop and return an Anthropic error response
- **Timeout**: If the overall request timeout is reached, the proxy returns whatever progress has been made

### Example: curl with Server-Side Tools

Server-side tool execution is enabled by default — no special header needed:

```bash
curl -X POST https://orka.example.com/anthropic/v1/messages \
  -H "x-api-key: $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "max_tokens": 4096,
    "messages": [{"role": "user", "content": "Search the web for Kubernetes 1.32 release highlights and summarize them."}],
    "stream": true
  }'
```

By default, the endpoint is a transparent proxy where the client manages tools. Add `X-Orka-Tools: enabled` only when you want Orka to manage server-side tools.

## Architecture

```
┌─────────────┐     ┌──────────────────────────────┐     ┌───────────────┐
│ Claude Code │────▶│ Orka API Server              │────▶│ Anthropic API │
│ (or any     │◀────│ /anthropic/v1/messages       │◀────│ OpenAI API    │
│ Anthropic   │     │                              │     │ Azure OpenAI  │
│ client)     │     │ Provider resolution:         │     └───────────────┘
└─────────────┘     │ - Provider CRD lookup        │
                    │ - Secret-based API keys      │
                    │ - Model routing              │
                    │ - Server-side tool execution │
                    └──────────────────────────────┘
```

Orka uses transparent proxy mode by default. Set `X-Orka-Tools: enabled` to inject built-in tools and run server-side tool execution — see [Server-Side Tool Execution](#server-side-tool-execution) above.
