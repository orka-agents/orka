# OpenAI-Compatible API

Mercan exposes an **OpenAI-compatible API** at `/v1/chat/completions` and `/v1/models`, allowing any OpenAI-compatible client to use Mercan as a provider. This includes tools like [Continue](https://continue.dev/), [Cursor](https://cursor.sh/), and others.

Mercan acts as a **proxy** to whichever LLM provider is configured in your cluster (Anthropic, OpenAI, Azure OpenAI, etc.), with credentials managed securely via Kubernetes Secrets and Provider CRDs.

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/v1/chat/completions` | Chat completions (streaming & non-streaming) |
| `GET` | `/v1/models` | List available models from configured providers |

Both endpoints require authentication via `Authorization: Bearer <token>` using a Kubernetes ServiceAccount token.

## Model Name Format

The `model` field supports two formats:

- **`provider/model`** — e.g., `anthropic/claude-sonnet-4-20250514`. The part before `/` matches a Provider CRD name, and the part after is the model name sent to that provider.
- **`model`** — e.g., `claude-sonnet-4-20250514`. Uses the default provider (from `--chat-provider` flag or a Provider CRD named `default`).

## Prerequisites

1. **Provider CRD** configured in the cluster:

```yaml
apiVersion: core.mercan.ai/v1alpha1
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
kubectl create serviceaccount mercan-client

# Bind it to the mercan viewer role (or a custom role)
kubectl create clusterrolebinding mercan-client-binding \
  --clusterrole=mercan-task-viewer \
  --serviceaccount=default:mercan-client

# Get a token
export MERCAN_TOKEN=$(kubectl create token mercan-client)
```

## Using with Continue

### Configuration

Configure Continue to use Mercan as an OpenAI-compatible provider. Add to your Continue configuration:

```json
{
  "models": [
    {
      "title": "Claude Sonnet 4 (via Mercan)",
      "provider": "openai",
      "model": "anthropic/claude-sonnet-4-20250514",
      "apiBase": "https://mercan.example.com/v1",
      "apiKey": "YOUR_MERCAN_TOKEN"
    }
  ]
}
```

### Environment

Set your Mercan API token:

```bash
export MERCAN_TOKEN=$(kubectl create token mercan-client)
```

## Using with curl

### Non-streaming

```bash
curl -X POST https://mercan.example.com/v1/chat/completions \
  -H "Authorization: Bearer $MERCAN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'
```

### Streaming

```bash
curl -X POST https://mercan.example.com/v1/chat/completions \
  -H "Authorization: Bearer $MERCAN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024,
    "stream": true
  }'
```

### List models

```bash
curl https://mercan.example.com/v1/models \
  -H "Authorization: Bearer $MERCAN_TOKEN"
```

## Supported Features

| Feature | Supported |
|---------|-----------|
| Chat completions | Yes |
| Streaming (SSE) | Yes |
| Tool/function calling | Yes |
| System messages | Yes |
| Multi-part content | Yes (text parts extracted) |
| `max_tokens` / `max_completion_tokens` | Yes |
| `temperature` | Yes |
| `stop` sequences | Yes |
| `stream_options.include_usage` | Yes |
| Image inputs | Not yet (text extracted from multi-part) |
| Embeddings | Not supported |
| Audio / Vision | Not supported |

## Architecture

```
┌──────────────┐     ┌─────────────────────────┐     ┌──────────────────┐
│  Continue    │────▶│  Mercan API Server       │────▶│  Anthropic API   │
│  (or any     │     │  /v1/chat/completions    │     │  OpenAI API      │
│   OAI client)│◀────│                          │◀────│  Azure OpenAI    │
└──────────────┘     │  Provider resolution:    │     └──────────────────┘
                     │  - Provider CRD lookup   │
                     │  - Secret-based API keys │
                     │  - Model routing         │
                     └─────────────────────────┘
```

Mercan transparently proxies requests to the backend LLM provider. The client manages its own tool execution loop — Mercan simply forwards the messages and tool definitions to the LLM and returns the response.
