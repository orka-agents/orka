# API Reference

The controller exposes a REST API for programmatic access. All `/api/v1/*` endpoints require a ServiceAccount bearer token.

## Tasks

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tasks` | POST | Create a task |
| `/api/v1/tasks` | GET | List tasks (paginated) |
| `/api/v1/tasks/:id` | GET | Get task details |
| `/api/v1/tasks/:id` | DELETE | Cancel/delete task |
| `/api/v1/tasks/:id/logs` | GET | Stream task logs |
| `/api/v1/tasks/:id/result` | GET | Get task result |
| `/api/v1/tasks/:id/plan` | GET | Get task plan |
| `/api/v1/tasks/:id/children` | GET | Get child tasks |

### Get Task Plan

Retrieve the autonomous plan state for a task.

**Endpoint:** `GET /api/v1/tasks/{id}/plan`

**Response (200):**
```json
{
  "TaskName": "build-feature",
  "Namespace": "default",
  "Iteration": 3,
  "Summary": "Completed auth module, working on CRUD endpoints",
  "ProgressPct": 40,
  "GoalComplete": false,
  "PlanDocument": "# Plan\n- [x] Auth\n- [ ] CRUD\n...",
  "CreatedAt": "2024-01-15T10:00:00Z",
  "UpdatedAt": "2024-01-15T12:30:00Z"
}
```

**Errors:**
- `404` — No plan found for this task
- `501` — Plan store not configured

## Sessions

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/sessions` | GET | List sessions |
| `/api/v1/sessions/:id` | GET | Get session transcript |
| `/api/v1/sessions/:id` | DELETE | Delete session |

## Agents

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/agents` | POST | Create an agent |
| `/api/v1/agents` | GET | List agents |
| `/api/v1/agents/:name` | GET | Get agent details |
| `/api/v1/agents/:name` | PUT | Update an agent |
| `/api/v1/agents/:name` | DELETE | Delete an agent |

## Tools

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/tools` | GET | List tools (built-in + CRDs) |
| `/api/v1/tools/:name` | GET | Get tool details |

## Auth

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/auth/validate` | GET | Validate auth token |

## Secrets

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/secrets` | GET | List secret names (metadata only) |

## Chat

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/chat` | POST | Send message (SSE streaming or JSON) |
| `/api/v1/chat/config` | GET | Get chat configuration and available tools |
| `/api/v1/chat/:sessionId` | DELETE | Cancel a chat session |

See [Interactive Chat](chat.md) for full chat documentation.

## OpenAI-Compatible API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/v1/chat/completions` | POST | Chat completions (streaming & non-streaming) |
| `/v1/models` | GET | List available models |

See [OpenAI Compatibility](openai-compat.md) for details.

## Internal API (Worker Communication)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/internal/v1/results/:namespace/:taskName` | POST | Submit task result |
| `/internal/v1/sessions/:namespace/:name/transcript` | GET | Get session transcript |
| `/internal/v1/plans/:namespace/:taskName` | POST | Save plan state |
| `/internal/v1/plans/:namespace/:taskName` | GET | Get plan state |

### Save Plan State

Workers call this to persist autonomous plan state.

**Endpoint:** `POST /internal/v1/plans/{namespace}/{taskName}`

**Request Body:**
```json
{
  "summary": "Completed phase 1",
  "progress_pct": 25,
  "goal_complete": false,
  "plan_document": "# Plan\n..."
}
```

**Response:** `204 No Content`

### Get Plan State

Workers call this to load the current plan state at startup.

**Endpoint:** `GET /internal/v1/plans/{namespace}/{taskName}`

**Response (200):** Same as public plan endpoint.

**Errors:**
- `404` — No plan found

## Health

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Health check |
| `/readyz` | GET | Readiness check |

## Example Usage

```bash
# Create a task
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "type": "ai",
    "agentRef": {"name": "assistant"},
    "prompt": "Explain microservices architecture"
  }'

# Get task result
curl http://localhost:8080/api/v1/tasks/my-task/result \
  -H "Authorization: Bearer $(kubectl create token orka-client)"

# Chat with SSE streaming
curl -N http://localhost:8080/api/v1/chat \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -H "Content-Type: application/json" \
  -d '{
    "message": "Create an AI task that summarizes Kubernetes best practices",
    "sessionId": "my-session"
  }'
```

## Built-in Tools

These tools are available to AI worker agents:

| Tool | Description | Parameters |
|------|-------------|------------|
| `web_search` | Search the web via configurable API (Tavily, etc.) | `query` (required), `limit` (default 5) |
| `code_exec` | Execute code in a sandboxed environment | `language` (python/javascript/bash), `code`, `timeout` (max 60s) |
| `file_read` | Read files from the workspace | `path`, `offset`, `limit` (max 1MB) |

### Coordination Tools

These tools are registered when `ORKA_COORDINATION_ENABLED=true`:

| Tool | Description | Parameters |
|------|-------------|------------|
| `delegate_task` | Delegate a subtask to another agent | `agent`, `prompt` (required); `namespace`, `priority`, `auto_retry`, `max_retries` |
| `wait_for_tasks` | Wait for delegated tasks to complete | `tasks` (required), `timeout` (default 10m) |
| `create_pull_request` | Create a GitHub pull request | `task_name`, `head_branch`, `base_branch`, `title` (required); `body` |
| `merge_pull_request` | Merge a GitHub pull request | `task_name`, `pr_number` (required); `merge_method`, `commit_title`, `commit_message` |
| `review_pull_request` | Fetch PR diff for review | `task_name`, `pr_number` (required) |
| `post_review_comment` | Post a review on a PR | `task_name`, `pr_number`, `body`, `event` (required); `comments` |
| `create_agent` | Create an Agent CRD at runtime | `name`, `provider`, `model` (required); `systemPrompt`, `tools`, `coordination` |
| `delete_agent` | Delete an Agent CRD | `name` (required), `namespace` |
