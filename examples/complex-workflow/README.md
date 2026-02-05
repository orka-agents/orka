# Complex Workflow Example

This example demonstrates advanced Mercan features inspired by [OpenClaw](https://github.com/openclaw/openclaw) workflow patterns, including:

- **Multi-agent coordination** - Coordinator agent that delegates to specialists
- **Custom tools** - Web search (Tavily) and GitHub API integration
- **Skills** - Reusable prompt templates injected into system prompts
- **Session continuity** - Multi-turn conversations with context preservation
- **Agent specialization** - Different agents for different tasks

## Architecture

```
                    ┌─────────────────────┐
                    │  Coordinator Agent  │
                    │  (orchestration)    │
                    └──────────┬──────────┘
                               │
           ┌───────────────────┼───────────────────┐
           │                   │                   │
           ▼                   ▼                   ▼
┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐
│ Researcher Agent│  │  Coder Agent    │  │ Reviewer Agent  │
│ (web search,    │  │  (code gen,     │  │ (code review,   │
│  GitHub API)    │  │   architecture) │  │  quality)       │
└─────────────────┘  └─────────────────┘  └─────────────────┘
```

## Components

### Tools (`tools.yaml`)

| Tool | Description |
|------|-------------|
| `tavily-search` | Web search for current information |
| `github-search-repos` | Search GitHub repositories |
| `github-get-repo` | Get repository details |
| `github-list-issues` | List repository issues |
| `github-get-readme` | Fetch repository README |

### Skills (`skills.yaml`)

| Skill | Purpose |
|-------|---------|
| `skill-researcher` | Research methodology and output format |
| `skill-code-review` | Code review checklist and guidelines |
| `skill-architect` | Software architecture principles |
| `skill-coordinator` | Task coordination methodology |

### Agents (`agents.yaml`)

| Agent | Role | Tools | Skills |
|-------|------|-------|--------|
| `researcher-agent` | Find and analyze information | tavily-search, github-* | skill-researcher |
| `coder-agent` | Write and implement code | github-search-repos, github-get-readme | skill-architect |
| `reviewer-agent` | Review code quality | github-list-issues | skill-code-review |
| `coordinator-agent` | Orchestrate multi-agent workflows | tavily-search, github-search-repos | skill-coordinator |

### Example Tasks (`tasks.yaml`)

1. **research-kubernetes-operators** - Simple research task
2. **implement-rate-limiter** - Code generation task
3. **review-auth-implementation** - Code review task
4. **design-caching-system** - Complex coordinated task
5. **research-followup** - Multi-turn conversation
6. **analyze-similar-projects** - GitHub-focused research
7. **research-with-notification** - Task with webhook

## Setup

### 1. Configure Secrets

Edit `secrets.yaml` with your API keys:

```bash
# Get your API keys
# - Anthropic: https://console.anthropic.com/
# - Tavily: https://tavily.com/
# - GitHub: https://github.com/settings/tokens

kubectl create secret generic anthropic-secret \
  --from-literal=ANTHROPIC_API_KEY=your-key

kubectl create secret generic tavily-secret \
  --from-literal=api-key=your-key

kubectl create secret generic github-secret \
  --from-literal=token=your-token
```

### 2. Deploy Base Resources

```bash
# Deploy tools, skills, and agents
kubectl apply -k examples/complex-workflow/

# Or apply individually
kubectl apply -f examples/complex-workflow/tools.yaml
kubectl apply -f examples/complex-workflow/skills.yaml
kubectl apply -f examples/complex-workflow/agents.yaml
```

### 3. Run Tasks

```bash
# Run a specific task
kubectl apply -f examples/complex-workflow/tasks.yaml

# Or create tasks via API
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-research-task",
    "type": "ai",
    "agentRef": {"name": "researcher-agent"},
    "prompt": "Research the latest Kubernetes features in 2026"
  }'
```

### 4. Monitor Progress

```bash
# Watch task status
kubectl get tasks -w

# Get task result
kubectl get configmap task-<name>-result -o jsonpath='{.data.result}'

# Stream task logs
curl http://localhost:8080/api/v1/tasks/<id>/logs
```

## Patterns Demonstrated

### 1. Agent Coordination

The `coordinator-agent` can break down complex tasks and delegate to specialists:

```yaml
spec:
  coordination:
    enabled: true
    allowedAgents:
      - name: researcher-agent
      - name: coder-agent
      - name: reviewer-agent
    maxConcurrentChildren: 3
    maxDepth: 2
```

### 2. Session Continuity

Tasks can share context through sessions:

```yaml
# First task creates session
sessionRef:
  name: research-session
  create: true

# Follow-up task continues conversation
sessionRef:
  name: research-session
  create: false
  append: true
```

### 3. Skill Injection

Skills are injected into the agent's system prompt:

```yaml
skills:
  - configMapRef:
      name: skill-researcher
      key: skill.md  # default
```

### 4. Tool Integration

Tools call external APIs with auth from secrets:

```yaml
http:
  url: "https://api.tavily.com/search"
  method: POST
  authSecretRef:
    name: tavily-secret
    key: api-key
  authInject: body
  authBodyKey: api_key
```

## Comparison with OpenClaw

| Feature | OpenClaw | Mercan |
|---------|----------|--------|
| Runtime | Node.js CLI/Gateway | Kubernetes-native |
| Agents | Workspaces + profiles | Agent CRDs |
| Tools | Plugin system | Tool CRDs |
| Skills | AgentSkills | ConfigMap skills |
| Sessions | SQLite/local | ConfigMaps |
| Coordination | Lobster workflows | Agent delegation |
| Channels | WhatsApp, Telegram, etc. | REST API |

## Tips

- Use `priority` to control task ordering (higher = more urgent)
- Set appropriate `timeout` for complex tasks
- Use `retryPolicy` for tasks that may fail transiently
- Check agent `status.activeTasks` to monitor load
- Use `webhookURL` for async notifications
