# OpenAI-Compatible API

Orka exposes an **OpenAI-compatible API** at `/v1/chat/completions` and `/v1/models`, allowing any OpenAI-compatible client to use Orka as a provider. This includes tools like [Continue](https://continue.dev/), [Cursor](https://cursor.sh/), and others.

Orka acts as a **proxy** to whichever LLM provider is configured in your cluster (Anthropic, OpenAI, Azure OpenAI, etc.), with credentials managed securely via Kubernetes Secrets and Provider CRDs.

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

### Azure OpenAI provider example

If you use Azure OpenAI, configure a Provider with `type: azure-openai`:

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: azure-openai
  namespace: default
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
      "apiBase": "https://orka.example.com/v1",
      "apiKey": "YOUR_ORKA_TOKEN"
    }
  ]
}
```

### Environment

Set your Orka API token:

```bash
export ORKA_TOKEN=$(kubectl create token orka-client)
```

## Using with curl

### Non-streaming

```bash
curl -X POST https://orka.example.com/v1/chat/completions \
  -H "Authorization: Bearer $ORKA_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "anthropic/claude-sonnet-4-20250514",
    "messages": [{"role": "user", "content": "Hello!"}],
    "max_tokens": 1024
  }'
```

### Streaming

```bash
curl -X POST https://orka.example.com/v1/chat/completions \
  -H "Authorization: Bearer $ORKA_TOKEN" \
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
curl https://orka.example.com/v1/models \
  -H "Authorization: Bearer $ORKA_TOKEN"
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
│  Continue    │────▶│  Orka API Server       │────▶│  Anthropic API   │
│  (or any     │     │  /v1/chat/completions    │     │  OpenAI API      │
│   OAI client)│◀────│                          │◀────│  Azure OpenAI    │
└──────────────┘     │  Provider resolution:    │     └──────────────────┘
                     │  - Provider CRD lookup   │
                     │  - Secret-based API keys │
                     │  - Model routing         │
                     └─────────────────────────┘
```

Orka transparently proxies requests to the backend LLM provider. The client manages its own tool execution loop — Orka simply forwards the messages and tool definitions to the LLM and returns the response.
