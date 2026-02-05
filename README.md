# Mercan

Mercan is a Kubernetes-native task execution platform that supports both container tasks and AI agent tasks with LLM integration. It provides a declarative way to run workloads, manage conversational AI agents, and orchestrate multi-agent workflows.

## Features

- **Two Task Types**: Container tasks for arbitrary workloads, AI tasks for LLM-powered agents
- **Custom Resources**: Task, Agent, Tool, and Provider CRDs for declarative configuration
- **Multi-Agent Coordination**: Coordinator agents can delegate work to specialist agents
- **Session Continuity**: Multi-turn conversations with context preserved in ConfigMaps
- **Custom Tools**: Define HTTP-based tools that agents can invoke
- **Skills**: Reusable prompt templates injected into agent system prompts
- **Multiple LLM Providers**: Anthropic Claude, OpenAI, and Azure OpenAI support
- **Built-in Tools**: Web search, code execution, and file reading
- **Priority Queue**: Task scheduling with priorities (0-1000)
- **Webhooks**: Completion notifications via HTTP callbacks
- **REST API**: Full API with ServiceAccount token authentication

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                        Mercan Controller                         │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │   Task      │  │   Agent     │  │   Tool & Provider       │  │
│  │ Reconciler  │  │ Controller  │  │   Controllers           │  │
│  └──────┬──────┘  └─────────────┘  └─────────────────────────┘  │
│         │                                                        │
│  ┌──────┴──────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │   Session   │  │  Priority   │  │      REST API           │  │
│  │   Manager   │  │   Queue     │  │   (Fiber framework)     │  │
│  └─────────────┘  └─────────────┘  └─────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┴───────────────┐
              │                               │
       ┌──────┴──────┐                 ┌──────┴──────┐
       │   General   │                 │     AI      │
       │   Worker    │                 │   Worker    │
       │ (containers)│                 │ (LLM agent) │
       └─────────────┘                 └─────────────┘
```

## Custom Resources

### Task

The core work unit. Supports container commands or AI agent prompts.

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: my-task
spec:
  type: ai  # or "container"
  agentRef:
    name: my-agent
  prompt: "Analyze the latest Kubernetes security best practices"
  sessionRef:
    name: my-session
    create: true
  priority: 750
  timeout: 5m
```

### Agent

Reusable agent configurations with model settings, tools, and skills.

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
  coordination:
    enabled: true
    allowedAgents:
      - name: coder-agent
```

### Tool

Custom HTTP-based tool definitions for agents.

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
    authSecretRef:
      name: tavily-secret
      key: api-key
    authInject: body
    authBodyKey: api_key
```

### Provider

LLM provider configuration with credentials.

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
  defaultModel: claude-sonnet-4-20250514
  rateLimit:
    requestsPerMinute: 60
```

## Getting Started

### Prerequisites

- Go 1.24+
- Docker 17.03+
- kubectl 1.11.3+
- Access to a Kubernetes 1.11.3+ cluster

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

## REST API

The controller exposes a REST API for programmatic access.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tasks` | POST | Create a task |
| `/api/v1/tasks` | GET | List tasks |
| `/api/v1/tasks/{id}` | GET | Get task details |
| `/api/v1/tasks/{id}` | DELETE | Cancel/delete task |
| `/api/v1/tasks/{id}/logs` | GET | Stream task logs |
| `/api/v1/tasks/{id}/result` | GET | Get task result |
| `/api/v1/sessions` | GET | List sessions |
| `/api/v1/sessions/{id}` | GET | Get session transcript |
| `/api/v1/sessions/{id}` | DELETE | Delete session |
| `/api/v1/tools` | GET | List tools |
| `/api/v1/agents` | GET | List agents |
| `/healthz` | GET | Health check |
| `/readyz` | GET | Readiness check |

### Example API Usage

```bash
# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $(kubectl create token mercan-controller)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "type": "ai",
    "agentRef": {"name": "assistant"},
    "prompt": "Explain microservices architecture"
  }'

# Get task result
curl http://localhost:8080/api/v1/tasks/my-task/result \
  -H "Authorization: Bearer $(kubectl create token mercan-controller)"
```

## Built-in Tools

| Tool | Description |
|------|-------------|
| `web-search` | Search the web using Tavily API |
| `code-exec` | Execute code in a sandboxed environment |
| `file-read` | Read files from the filesystem |

## Development

```bash
# Generate CRD manifests and Go types
make generate
make manifests

# Build
make build

# Run tests
make test

# Local development with kind
kind create cluster
make docker-build docker-push IMG=<registry>/mercan:tag
make deploy IMG=<registry>/mercan:tag
```

## Examples

See the [examples](examples/) directory for complete examples:

- [Complex Workflow](examples/complex-workflow/) - Multi-agent coordination with custom tools and skills
- [Tavily Integration](examples/tavily/) - Web search tool integration

## Security

- All worker pods run with: non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault
- ServiceAccount token authentication for API access
- Secrets for LLM API keys and tool authentication
- Namespace-scoped watching with `--watch-namespace` flag

## License

Copyright 2026.

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) for details.
