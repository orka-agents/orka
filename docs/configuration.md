# Configuration

## Custom Resources

### Task

The core work unit. Supports container commands, AI agent prompts, or external agent CLI runtimes.

```yaml
apiVersion: core.orka.ai/v1alpha1
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
apiVersion: core.orka.ai/v1alpha1
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
    - name: skill-researcher
  session:
    persistence: sqlite    # sqlite or none
    ttl: 24h
    maxMessages: 50
  coordination:
    enabled: true
    autonomous: true          # Enable autonomous loop mode
    maxIterations: 20         # Max loop iterations (0 = unlimited)
    allowedAgents:
      - name: coder-agent
    maxConcurrentChildren: 5
    maxDepth: 3
```

**Coordination fields:**

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `enabled` | bool | `false` | Enable agent-to-agent coordination tools |
| `autonomous` | bool | `false` | Enables autonomous loop mode. When true, the controller re-creates Jobs in a loop instead of marking the task as Succeeded |
| `maxIterations` | int32 | `0` | Limits the number of autonomous loop iterations. Only used when `autonomous` is true. `0` means unlimited |
| `allowedAgents` | list | `[]` | List of agent names this agent is allowed to delegate to |
| `maxConcurrentChildren` | int32 | `0` | Maximum number of concurrent child tasks. `0` means unlimited |
| `maxDepth` | int32 | `0` | Maximum delegation depth. `0` means unlimited |

**Auto-injected coordination tools** (when `enabled: true`):

`delegate_task`, `wait_for_tasks`, `cancel_task`, `send_message`, `check_messages`, `create_pull_request`, `merge_pull_request`, `auto_merge_pull_request`, `review_pull_request`, `post_review_comment`, `create_agent`, `delete_agent`, `update_plan`

### Provider Fallback Chain

You can configure fallback providers that are automatically tried when the primary provider fails (e.g., due to auth errors, provider outages, or rate limiting). Fallbacks are configured on the Agent CRD's `spec.model.fallbacks` field.

```yaml
apiVersion: orka.ai/v1alpha1
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
apiVersion: core.orka.ai/v1alpha1
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
apiVersion: core.orka.ai/v1alpha1
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

### Skill

Reusable skill definitions (Agent Skills standard) that are referenced by Agents and AI Tasks.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Skill
metadata:
  name: skill-researcher
  labels:
    orka.ai/category: "research"
spec:
  displayName: "Research Methodology"
  description: "Structured research workflow and source validation guidance"
  version: "1.0.0"
  tags: ["research", "analysis"]
  content:
    inline: |
      # Research Skill
      Use primary sources and cite references.
    files:
      templates/checklist.md: |
        - [ ] Validate source credibility
        - [ ] Cross-check key claims
status:
  phase: Ready
  contentHash: sha256:...
```

### Tool

Custom HTTP-based tool definitions for agents. Supports header-based or body-based auth injection.

```yaml
apiVersion: core.orka.ai/v1alpha1
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
apiVersion: core.orka.ai/v1alpha1
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
| `controller.image.repository` | `ghcr.io/sozercan/orka` | Controller image |
| `controller.watchNamespace` | `""` | Namespace scope (empty = cluster-wide) |
| `controller.apiPort` | `8080` | REST API port |
| `controller.metricsPort` | `8081` | Metrics endpoint port |
| `controller.healthPort` | `8082` | Health probe port |
| `controller.logLevel` | `info` | Log level (debug/info/warn/error) |
| `workers.ai.image.repository` | `ghcr.io/sozercan/orka/ai-worker` | AI worker image |
| `workers.general.image.repository` | `ghcr.io/sozercan/orka/general-worker` | General worker image |
| `service.type` | `ClusterIP` | Service type |
| `crds.install` | `true` | Install CRDs |
| `crds.keep` | `true` | Keep CRDs on uninstall |
| `monitoring.enabled` | `false` | Enable Prometheus ServiceMonitor |
| `client.create` | `true` | Create client ServiceAccount for API access |
| `client.name` | `orka-client` | Client ServiceAccount name |

See [charts/orka/values.yaml](../charts/orka/values.yaml) for the full list.

## Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-port` | `8080` | REST API server port |
| `--watch-namespace` | `""` | Namespace to watch (empty = all) |
| `--enforce-namespace-isolation` | `false` | Restrict users to their ServiceAccount's namespace |
| `--max-tasks-per-namespace` | `0` | Max active tasks per namespace (0 = unlimited) |
| `--controller-url` | `""` | Base URL workers use to reach the controller API (e.g., `http://orka-api.orka-system.svc:8080`). Required for worker result callbacks and session transcript fetching |
| `--ai-worker-image` | `ghcr.io/sozercan/orka/ai-worker:latest` | AI worker container image |
| `--copilot-worker-image` | `ghcr.io/sozercan/orka/agent-worker-copilot:latest` | Copilot agent worker image |
| `--claude-worker-image` | `ghcr.io/sozercan/orka/agent-worker-claude:latest` | Claude agent worker image |
| `--store-backend` | `sqlite` | Storage backend (sqlite) |
| `--store-path` | `/data/orka.db` | Path to SQLite database file |
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
| `--enable-tracing` | `false` | Enable OpenTelemetry distributed tracing (requires `OTEL_EXPORTER_OTLP_ENDPOINT`) |

## Prometheus Metrics

Orka exposes 19 Prometheus metrics. Enable monitoring with the Helm chart:

```yaml
monitoring:
  enabled: true
  interval: 30s
```

| Metric | Type | Description |
|--------|------|-------------|
| `orka_tasks_total` | Counter | Total tasks created (by type, phase, namespace) |
| `orka_tasks_active` | Gauge | Currently running tasks |
| `orka_task_duration_seconds` | Histogram | Task execution duration |
| `orka_task_queue_depth` | Gauge | Tasks waiting by priority |
| `orka_task_retries_total` | Counter | Retry attempts |
| `orka_webhook_deliveries_total` | Counter | Webhook deliveries (success/failure) |
| `orka_api_requests_total` | Counter | API requests (by endpoint, method, status) |
| `orka_api_request_duration_seconds` | Histogram | API latency |
| `orka_sessions_total` | Gauge | Active sessions |
| `orka_session_messages_total` | Counter | Messages appended to sessions |
| `orka_session_queue_waiting` | Gauge | Tasks waiting for session lock |
| `orka_tools_discovered` | Gauge | Tool CRDs discovered |
| `orka_tool_calls_total` | Counter | Tool invocations (by tool, status) |
| `orka_tool_call_duration_seconds` | Histogram | Tool HTTP call latency |
| `orka_tool_health_status` | Gauge | Tool availability (1/0) |
| `orka_agents_total` | Gauge | Agent count |
| `orka_agent_tasks_active` | Gauge | Active tasks per agent |
| `orka_agent_tasks_total` | Counter | Total tasks per agent |
| `orka_skills_loaded_total` | Counter | Skills loaded |

## OpenTelemetry Tracing

Orka supports opt-in OpenTelemetry distributed tracing for debugging and performance analysis. Tracing is disabled by default (zero overhead).

### Enabling Tracing

Add the `--enable-tracing` flag to the controller:

```yaml
args:
  - --enable-tracing
env:
  - name: OTEL_EXPORTER_OTLP_ENDPOINT
    value: "jaeger-collector.observability.svc:4317"
```

| Flag / Environment Variable | Default | Description |
|------------------------------|---------|-------------|
| `--enable-tracing` | `false` | Enable OpenTelemetry tracing |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | OTLP gRPC collector endpoint |

### Instrumented Components

| Tracer | Span | Attributes |
|--------|------|------------|
| `orka.api` | `GET /api/v1/tasks` | `http.method`, `http.route`, `http.status_code`, `http.request_id` |
| `orka.chat` | `chat.request` | `session.id`, `chat.provider`, `chat.model` |
| `orka.chat` | `chat.tool_loop.iteration` | `chat.iteration` |
| `orka.llm` | `llm.complete` | `llm.provider`, `llm.model`, `llm.input_tokens`, `llm.output_tokens` |
| `orka.tools` | `tool.execute` | `tool.name`, `tool.success` |
| `orka.controller` | `task.reconcile` | `task.name`, `task.namespace`, `task.type` |

### Example: Jaeger Setup

```bash
# Deploy Jaeger all-in-one (development only)
kubectl create namespace observability
kubectl apply -n observability -f https://raw.githubusercontent.com/jaegertracing/jaeger-operator/main/examples/simplest.yaml

# Configure the controller
kubectl set env deployment/orka-controller \
  OTEL_EXPORTER_OTLP_ENDPOINT=jaeger-collector.observability.svc:4317
```
