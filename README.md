# Mercan

Mercan is a Kubernetes-native task execution platform that supports container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes. It provides a declarative way to run workloads, manage conversational AI agents, and orchestrate multi-agent workflows — all with a built-in web dashboard and REST API.

## Features

- **Three Task Types**: Container tasks for arbitrary workloads, AI tasks for LLM-powered agents, and agent tasks for external CLI runtimes
- **Agent Runtimes**: Run tasks via Claude Code CLI or GitHub Copilot CLI with full autonomous coding capabilities
- **Custom Resources**: Task, Agent, Tool, and Provider CRDs for declarative configuration
- **Web Dashboard**: Built-in React UI with task management, agent configuration, session browsing, and interactive chat
- **Interactive Chat**: Agentic chat endpoint with SSE streaming, tool execution loop, and session persistence
- **CLI Tool**: `mercan login` for browser-based authentication with kubeconfig token extraction
- **Multi-Agent Coordination**: Coordinator agents can delegate work to specialist agents with depth and concurrency limits
- **Session Continuity**: Multi-turn conversations with context preserved in ConfigMaps (JSONL format)
- **Custom Tools**: Define HTTP-based tools with header or body auth injection
- **Skills**: Reusable prompt templates (ConfigMaps) injected into agent system prompts
- **Multiple LLM Providers**: Anthropic Claude, OpenAI, and Azure OpenAI via the Provider CRD
- **Built-in Tools**: Web search, sandboxed code execution (Python/JS/Bash), and file reading
- **Priority Queue**: Task scheduling with priorities (0-1000)
- **Webhooks**: Completion notifications via HTTP callbacks
- **REST API**: Full CRUD API with ServiceAccount token authentication and pagination
- **Scheduled Tasks**: Cron-based recurring task execution with concurrency policies and run history limits
- **Prometheus Metrics**: 19 metrics covering tasks, API requests, sessions, tools, and agents
- **Helm Chart**: Production-ready Helm chart with configurable RBAC, monitoring, and security contexts
- **Embedded UI**: The React dashboard is compiled into the controller binary — no separate frontend deployment needed

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                          Mercan Controller                           │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐    │
│  │   Task      │  │   Agent      │  │   Tool & Provider        │    │
│  │ Reconciler  │  │  Controller  │  │   Controllers            │    │
│  └──────┬──────┘  └──────────────┘  └──────────────────────────┘    │
│         │                                                            │
│  ┌──────┴──────┐  ┌──────────────┐  ┌──────────────────────────┐    │
│  │   Session   │  │   Priority   │  │   REST API + Chat        │    │
│  │   Manager   │  │    Queue     │  │   (Fiber framework)      │    │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘    │
│                                                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐   │
│  │  Prometheus  │  │   Embedded   │  │   Auth Middleware        │   │
│  │   Metrics    │  │   Web UI     │  │  (SA token validation)   │   │
│  └──────────────┘  └──────────────┘  └──────────────────────────┘   │
└──────────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
       ┌──────┴──────┐ ┌─────┴───────┐ ┌─────┴───────┐
       │   General   │ │     AI      │ │    Agent    │
       │   Worker    │ │   Worker    │ │   Workers   │
       │ (containers)│ │ (LLM agent) │ │(Claude CLI, │
       └─────────────┘ └─────────────┘ │ Copilot CLI)│
                                       └─────────────┘
```

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
    persistence: configmap  # configmap, pvc, or none
    ttl: 24h
    maxMessages: 50
  coordination:
    enabled: true
    allowedAgents:
      - name: coder-agent
    maxConcurrentChildren: 5
    maxDepth: 3
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

## Web Dashboard

Mercan includes a built-in React web dashboard that is embedded into the controller binary. No separate frontend deployment is needed.

**Dashboard pages:**

| Page | Description |
|------|-------------|
| Dashboard | Overview with task/session/agent/tool counts and recent tasks |
| Tasks | Create, monitor, and manage tasks with log streaming |
| Sessions | Browse session transcripts and manage conversation history |
| Agents | Create and configure AI agents and CLI runtime agents |
| Tools | View built-in and custom Tool CRD definitions |
| Chat | Interactive chat interface with SSE streaming and tool execution |

**Access the dashboard:**

```bash
# Port-forward the controller service
kubectl port-forward -n mercan-system svc/mercan-controller 8080:8080

# Open in browser
open http://localhost:8080
```

**Authentication:**

Use the `mercan` CLI tool to log in, or provide a ServiceAccount token via the login page:

```bash
# Build and use the CLI
make build-cli
./bin/mercan login --server http://localhost:8080

# Or manually create a token
kubectl create token mercan-client -n mercan-system
```

## Interactive Chat

The chat endpoint provides an agentic conversational interface with tool execution capabilities. The LLM can create and manage Kubernetes resources through 16 built-in orchestrator tools.

**Core tools (always available):**

| Tool | Description |
|------|-------------|
| `create_ai_task` | Create an AI/LLM-powered task |
| `create_container_task` | Create a container task for shell commands |
| `create_agent_task` | Create a task with CLI runtime (Copilot/Claude) |
| `check_task_progress` | Get task phase/status |
| `fetch_task_output` | Get completed task result |
| `wait_for_task` | Wait for task completion (max 60s per call) |
| `cancel_task` | Cancel/delete a task |
| `list_agents` | List Agent CRDs |
| `list_tools` | List Tool CRDs and built-in tools |
| `list_tasks` | List tasks with optional status filter |

**Management tools (loaded on demand):**

| Tool | Description |
|------|-------------|
| `create_agent` | Create an Agent CRD |
| `update_agent` | Update an Agent CRD |
| `delete_agent` | Delete an Agent CRD |
| `create_tool` | Create a Tool CRD with HTTP endpoint |
| `delete_tool` | Delete a Tool CRD |
| `delete_session` | Delete a session and transcript |

**Features:**
- SSE streaming with event types: `status`, `tool_call`, `tool_result`, `message`, `error`, `done`
- JSON response mode (`Accept: application/json`)
- Session persistence in ConfigMaps with automatic truncation
- Concurrency control with 429 rate limiting
- Repetition detection to prevent infinite tool loops
- Progress check injection every 5 iterations

## Getting Started

### Prerequisites

- Go 1.25+
- Bun (for UI build)
- Docker 17.03+
- kubectl v1.11.3+
- Access to a Kubernetes v1.11.3+ cluster

### Installation

**Using Helm:**

```bash
helm install mercan charts/mercan \
  --namespace mercan-system \
  --create-namespace
```

**Using kubectl:**

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=ghcr.io/sozercan/mercan:latest
```

### Quick Start

1. Create an LLM provider secret:

```bash
kubectl create secret generic anthropic-secret \
  --from-literal=api-key=your-api-key
```

2. Create a Provider:

```yaml
kubectl apply -f - <<EOF
apiVersion: core.mercan.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic
spec:
  type: anthropic
  secretRef:
    name: anthropic-secret
    key: api-key
  defaultModel: claude-sonnet-4-20250514
EOF
```

3. Create an Agent:

```yaml
kubectl apply -f - <<EOF
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: assistant
spec:
  providerRef:
    name: anthropic
  model:
    temperature: 0.7
  systemPrompt:
    inline: "You are a helpful assistant."
EOF
```

4. Run a Task:

```yaml
kubectl apply -f - <<EOF
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: hello-task
spec:
  type: ai
  agentRef:
    name: assistant
  prompt: "What is Kubernetes?"
EOF
```

5. Check the result:

```bash
kubectl get task hello-task
kubectl get configmap task-hello-task-result -o jsonpath='{.data.result}'
```

### Agent Runtimes Quick Start

Agent runtimes let you run tasks via Claude Code CLI or GitHub Copilot CLI with full autonomous coding capabilities.

1. Create credentials secret:

```bash
# For Claude Code CLI (direct API)
kubectl create secret generic claude-credentials \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-your-key

# For Claude Code CLI (Azure AI Foundry)
kubectl create secret generic claude-credentials \
  --from-literal=CLAUDE_CODE_USE_FOUNDRY=1 \
  --from-literal=ANTHROPIC_FOUNDRY_API_KEY=your-key \
  --from-literal=ANTHROPIC_FOUNDRY_RESOURCE=your-resource \
  --from-literal=ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5
```

2. Create an Agent with runtime:

```yaml
kubectl apply -f - <<EOF
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  secretRef:
    name: claude-credentials
  runtime:
    type: claude
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
EOF
```

3. Run an agent task:

```yaml
kubectl apply -f - <<EOF
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
EOF
```

4. Check the result:

```bash
kubectl get task code-review
kubectl get configmap task-code-review-result -o jsonpath='{.data.result}'
```

See [Agent Runtimes Documentation](docs/agent-runtimes.md) for full configuration reference.

## REST API

The controller exposes a REST API for programmatic access. All `/api/v1/*` endpoints require a ServiceAccount bearer token.

### Tasks

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tasks` | POST | Create a task |
| `/api/v1/tasks` | GET | List tasks (paginated) |
| `/api/v1/tasks/:id` | GET | Get task details |
| `/api/v1/tasks/:id` | DELETE | Cancel/delete task |
| `/api/v1/tasks/:id/logs` | GET | Stream task logs |
| `/api/v1/tasks/:id/result` | GET | Get task result from ConfigMap |

### Sessions

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/sessions` | GET | List sessions |
| `/api/v1/sessions/:id` | GET | Get session transcript |
| `/api/v1/sessions/:id` | DELETE | Delete session |

### Agents

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/agents` | POST | Create an agent |
| `/api/v1/agents` | GET | List agents |
| `/api/v1/agents/:name` | GET | Get agent details |
| `/api/v1/agents/:name` | PUT | Update an agent |
| `/api/v1/agents/:name` | DELETE | Delete an agent |

### Tools

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tools` | GET | List tools (built-in + CRDs) |
| `/api/v1/tools/:name` | GET | Get tool details |

### Chat

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/chat` | POST | Send message (SSE streaming or JSON) |
| `/api/v1/chat/config` | GET | Get chat configuration and available tools |
| `/api/v1/chat/:sessionId` | DELETE | Cancel a chat session |

### Other

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/secrets` | GET | List secret names (metadata only) |
| `/healthz` | GET | Health check |
| `/readyz` | GET | Readiness check |

### Example API Usage

```bash
# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $(kubectl create token mercan-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "type": "ai",
    "agentRef": {"name": "assistant"},
    "prompt": "Explain microservices architecture"
  }'

# Get task result
curl http://localhost:8080/api/v1/tasks/my-task/result \
  -H "Authorization: Bearer $(kubectl create token mercan-client)"

# Chat with SSE streaming
curl -N http://localhost:8080/api/v1/chat \
  -H "Authorization: Bearer $(kubectl create token mercan-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Create an AI task that summarizes Kubernetes best practices",
    "sessionId": "my-session"
  }'
```

## Built-in Tools

| Tool | Description | Parameters |
|------|-------------|------------|
| `web_search` | Search the web via configurable API (Tavily, etc.) | `query` (required), `limit` (default 5) |
| `code_exec` | Execute code in a sandboxed environment | `language` (python/javascript/bash), `code`, `timeout` (max 60s) |
| `file_read` | Read files from the workspace | `path`, `offset`, `limit` (max 1MB) |

## CLI

The `mercan` CLI provides browser-based authentication for the web dashboard.

```bash
# Build the CLI
make build-cli

# Login (extracts token from kubeconfig and opens browser)
./bin/mercan login

# Login with custom server
./bin/mercan login --server https://mercan.example.com

# Login with explicit token
./bin/mercan login --token <token>

# Specify kubeconfig
./bin/mercan login --kubeconfig ~/.kube/my-config
```

The CLI supports token extraction from bearer tokens, token files, exec-based auth (GKE, AWS IAM), and OIDC auth providers.

## Prometheus Metrics

Mercan exposes comprehensive Prometheus metrics:

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

Enable monitoring with the Helm chart:

```yaml
monitoring:
  enabled: true
  interval: 30s
```

## Development

```bash
# Generate CRD manifests and Go types
make generate
make manifests

# Build (includes UI)
make build

# Build CLI only
make build-cli

# Run locally
make run

# Run tests
make test

# Lint
make lint
make lint-fix

# UI development
make ui-install    # Install UI dependencies (bun)
make ui-dev        # Run UI dev server
make ui-build      # Build UI and copy to embed directory
make ui-lint       # Lint UI code
make ui-test       # Run UI unit tests
make ui-test-coverage  # Run UI tests with coverage

# Build docker images
make docker-build                  # Controller image
make docker-build-claude-worker    # Claude agent worker
make docker-build-copilot-worker   # Copilot agent worker
make docker-build-all              # All images

# Push docker images
make docker-push
make docker-push-claude-worker
make docker-push-copilot-worker
make docker-push-all

# Generate installer YAML
make build-installer IMG=ghcr.io/sozercan/mercan:latest

# Local development with kind
kind create cluster
make docker-build docker-push IMG=<registry>/mercan:tag
make deploy IMG=<registry>/mercan:tag

# E2E tests (uses isolated Kind cluster)
make test-e2e
```

## Helm Chart Configuration

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

See [charts/mercan/values.yaml](charts/mercan/values.yaml) for the full list of configuration options.

## Documentation

| Document | Description |
|----------|-------------|
| [Architecture](docs/architecture.md) | System design, components, and design decisions |
| [Agent Runtimes](docs/agent-runtimes.md) | Claude Code CLI and Copilot CLI runtime configuration |
| [Interactive Chat](docs/chat.md) | Chat endpoint, tools, SSE streaming, and session management |
| [Web Dashboard](docs/ui.md) | Frontend architecture, development, and pages |
| [Testing](docs/testing.md) | Test structure, patterns, and commands |
| [Competitive Analysis](docs/competitive-analysis.md) | Comparison with Gastown and Multiclaude |

## Examples

See the [examples](examples/) directory for complete examples:

- [Complex Workflow](examples/complex-workflow/) - Multi-agent coordination with custom tools and skills
- [Tavily Integration](examples/tavily/) - Web search tool integration
- [Sample Manifests](config/samples/) - Example CRDs for all resource types

## Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--api-port` | `8080` | REST API server port |
| `--watch-namespace` | `""` | Namespace to watch (empty = all) |
| `--copilot-worker-image` | `mercan-agent-worker-copilot:latest` | Copilot agent worker image |
| `--claude-worker-image` | `mercan-agent-worker-claude:latest` | Claude agent worker image |
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

## Security

- All worker pods run with: non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault
- Controller runs as non-root (uid 65532) with read-only rootfs and seccomp RuntimeDefault
- ServiceAccount token authentication for all API access
- Kubernetes Secrets for LLM API keys and tool authentication (never logged or stored in specs)
- Namespace-scoped watching with `--watch-namespace` flag
- Chat endpoint blocks operations in `kube-system` and `kube-public` namespaces
- Embedded UI served over the same port as the API (no separate attack surface)

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
