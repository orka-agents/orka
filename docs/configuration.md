# Configuration

## Custom Resources

### Task

The core work unit. Supports container commands, AI agent prompts, or external agent CLI runtimes.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: my-task
spec:
  type: ai  # or "container" or "agent"
  agentRef:
    name: my-agent
  prompt: "Analyze the latest Kubernetes security best practices"
  sessionRef:
    name: my-session
    create: true
    append: true
    maxMessages: 50
  priority: 750
  timeout: 5m
  retryPolicy:
    maxRetries: 3
    backoffMultiplier: 2
    initialDelay: 10s
  webhookURL: "https://example.com/webhook"
  # Scheduled/recurring task fields (optional)
  schedule: "0 */6 * * *"      # Cron expression
  timeZone: "America/New_York" # IANA timezone
  concurrencyPolicy: Forbid    # Allow or Forbid concurrent runs
  suspend: false
  successfulRunsHistoryLimit: 3
  failedRunsHistoryLimit: 1
```

### Agent

Reusable agent configurations with model settings, tools, skills, and optional agent-to-agent coordination.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: researcher-agent
spec:
  providerRef:
    name: anthropic-prod
  model:
    temperature: 0.3
    maxTokens: 4096
  systemPrompt:
    inline: "You are a research specialist..."
  tools:
    - name: web-search
    - name: github-search
  skills:
    - configMapRef:
        name: skill-researcher
  session:
    persistence: sqlite    # sqlite or none
    ttl: 24h
    maxMessages: 50
  coordination:
    enabled: true
    allowedAgents:
      - name: coder-agent
    maxConcurrentChildren: 5
    maxDepth: 3
```

### Provider Fallback Chain

You can configure fallback providers that are automatically tried when the primary provider fails (e.g., due to auth errors, provider outages, or rate limiting). Fallbacks are configured on the Agent CRD's `spec.model.fallbacks` field.

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Agent
metadata:
  name: resilient-agent
spec:
  providerRef:
    name: my-openai
  model:
    name: gpt-4o
    fallbacks:
      - providerRef: my-anthropic
        model: claude-sonnet-4-20250514
      - providerRef: my-azure-openai
        model: gpt-4o
```

#### How fallbacks work

1. The primary provider is tried first with automatic retries (exponential backoff on 429/5xx errors).
2. If the primary provider fails with an auth error (401/403), network error, or exhausts all retries, the first fallback provider is tried.
3. Each fallback provider also gets automatic retries.
4. If all providers fail, the last error is returned.

#### Fallback fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `providerRef` | string | Yes | Name of a Provider CRD to fall back to |
| `model` | string | No | Model to use with this provider. If empty, uses the provider's `defaultModel` |

#### Notes

- Fallbacks are only supported on Agent-based tasks. Agent-less tasks get retries only.
- Each fallback provider must have its own Provider CRD with a valid secret reference.
- Rate-limited providers (429 responses) are temporarily cooled down and skipped in subsequent requests.
- Streaming requests are retried/failed over only on the initial connection — mid-stream failures are not retried.

### Agent (with Runtime)

Agent configuration for external CLI runtimes (Claude Code CLI or GitHub Copilot CLI).

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  secretRef:
    name: claude-credentials
  model:
    name: "claude-sonnet-4-20250514"
  systemPrompt:
    inline: "You are a senior software engineer."
  runtime:
    type: claude         # or "copilot"
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
```

Agent runtime tasks reference an Agent with `runtime` configured:

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: code-review
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Review the code in this repo for security issues"
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/example/repo.git"
      branch: main
      # gitSecretRef:
      #   name: git-credentials
      # subPath: "services/api"
    maxTurns: 100
    allowBash: true
    allowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
```

### Tool

Custom HTTP-based tool definitions for agents. Supports header-based or body-based auth injection.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Tool
metadata:
  name: tavily-search
spec:
  description: "Search the web for current information"
  parameters:
    type: object
    properties:
      query:
        type: string
        description: "Search query"
    required: ["query"]
  http:
    url: "https://api.tavily.com/search"
    method: POST
    timeout: 30s
    authSecretRef:
      name: tavily-secret
      key: api-key
    authInject: body     # "header" (Bearer token) or "body" (JSON key)
    authBodyKey: api_key # JSON key name when authInject=body
```

### Provider

LLM provider configuration with credentials. Supports Anthropic, OpenAI, and Azure OpenAI.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic-prod
spec:
  type: anthropic  # or "openai", "azure-openai"
  secretRef:
    name: anthropic-secret
    key: api-key
  baseURL: ""  # optional custom endpoint for proxies
  defaultModel: claude-sonnet-4-20250514
  rateLimit:
    requestsPerMinute: 60
    tokensPerMinute: 100000
  # Azure-specific (only for type: azure-openai)
  # azure:
  #   deploymentName: my-deployment
  #   apiVersion: "2024-02-15-preview"
```

## Helm Chart

Key configuration values for the Helm chart:

| Parameter | Default | Description |
|-----------|---------|-------------|
| `controller.replicas` | `1` | Controller replicas |
| `controller.image.repository` | `ghcr.io/sozercan/mercan` | Controller image |
| `controller.watchNamespace` | `""` | Namespace scope (empty = cluster-wide) |
| `controller.apiPort` | `8080` | REST API port |
| `controller.metricsPort` | `8081` | Metrics endpoint port |
| `controller.healthPort` | `8082` | Health probe port |
| `controller.logLevel` | `info` | Log level (debug/info/warn/error) |
| `workers.ai.image.repository` | `ghcr.io/sozercan/mercan-ai-worker` | AI worker image |
| `workers.general.image.repository` | `ghcr.io/sozercan/mercan-general-worker` | General worker image |
| `service.type` | `ClusterIP` | Service type |
| `crds.install` | `true` | Install CRDs |
| `crds.keep` | `true` | Keep CRDs on uninstall |
| `monitoring.enabled` | `false` | Enable Prometheus ServiceMonitor |
| `client.create` | `true` | Create client ServiceAccount for API access |
| `client.name` | `mercan-client` | Client ServiceAccount name |

See [charts/mercan/values.yaml](../charts/mercan/values.yaml) for the full list.

## Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-port` | `8080` | REST API server port |
| `--watch-namespace` | `""` | Namespace to watch (empty = all) |
| `--copilot-worker-image` | `mercan-agent-worker-copilot:latest` | Copilot agent worker image |
| `--claude-worker-image` | `mercan-agent-worker-claude:latest` | Claude agent worker image |
| `--store-backend` | `sqlite` | Storage backend (sqlite) |
| `--store-path` | `/data/mercan.db` | Path to SQLite database file |
| `--chat-enabled` | `true` | Enable the chat endpoint |
| `--chat-provider` | `""` | Default Provider CRD name for chat |
| `--chat-model` | `""` | Default model for chat |
| `--chat-max-iterations` | `20` | Max tool execution loops per chat request |
| `--chat-max-duration` | `5m` | Max wall-clock time per chat request |
| `--chat-tool-timeout` | `60s` | Max time for single tool execution |
| `--chat-max-concurrent` | `10` | Max concurrent chat sessions |
| `--chat-max-tasks-per-turn` | `5` | Max tasks created per chat turn |
| `--chat-max-session-size` | `512000` | Soft limit for session size before truncation (bytes) |
| `--leader-elect` | `false` | Enable leader election |
| `--metrics-bind-address` | `0` | Metrics endpoint address |
| `--health-probe-bind-address` | `:8081` | Health probe address |
| `--metrics-secure` | `true` | Serve metrics via HTTPS |
| `--enable-http2` | `false` | Enable HTTP/2 for metrics and webhook servers |

## Prometheus Metrics

Mercan exposes 19 Prometheus metrics. Enable monitoring with the Helm chart:

```yaml
monitoring:
  enabled: true
  interval: 30s
```

| Metric | Type | Description |
|--------|------|-------------|
| `mercan_tasks_total` | Counter | Total tasks created (by type, phase, namespace) |
| `mercan_tasks_active` | Gauge | Currently running tasks |
| `mercan_task_duration_seconds` | Histogram | Task execution duration |
| `mercan_task_queue_depth` | Gauge | Tasks waiting by priority |
| `mercan_task_retries_total` | Counter | Retry attempts |
| `mercan_webhook_deliveries_total` | Counter | Webhook deliveries (success/failure) |
| `mercan_api_requests_total` | Counter | API requests (by endpoint, method, status) |
| `mercan_api_request_duration_seconds` | Histogram | API latency |
| `mercan_sessions_total` | Gauge | Active sessions |
| `mercan_session_messages_total` | Counter | Messages appended to sessions |
| `mercan_session_queue_waiting` | Gauge | Tasks waiting for session lock |
| `mercan_tools_discovered` | Gauge | Tool CRDs discovered |
| `mercan_tool_calls_total` | Counter | Tool invocations (by tool, status) |
| `mercan_tool_call_duration_seconds` | Histogram | Tool HTTP call latency |
| `mercan_tool_health_status` | Gauge | Tool availability (1/0) |
| `mercan_agents_total` | Gauge | Agent count |
| `mercan_agent_tasks_active` | Gauge | Active tasks per agent |
| `mercan_agent_tasks_total` | Counter | Total tasks per agent |
| `mercan_skills_loaded_total` | Counter | Skills loaded |
