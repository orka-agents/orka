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
| `/api/v1/tasks/:id/artifacts` | GET | List task artifacts |
| `/api/v1/tasks/:id/artifacts/:filename` | GET | Download a task artifact |
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

## Skills

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/skills` | POST | Create a skill |
| `/api/v1/skills` | GET | List skills |
| `/api/v1/skills/:name` | GET | Get skill details |
| `/api/v1/skills/:name/content` | GET | Get raw `spec.content.inline` markdown |
| `/api/v1/skills/:name` | PUT | Update a skill |
| `/api/v1/skills/:name` | DELETE | Delete a skill |

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
| `/openai/v1/chat/completions` | POST | Chat completions (streaming & non-streaming) |
| `/openai/v1/models` | GET | List available models |

See [OpenAI Compatibility](openai-compat.md) for details.

## Anthropic-Compatible API

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/anthropic/v1/messages` | POST | Create a message (streaming & non-streaming) |
| `/anthropic/v1/models` | GET | List available models |

See [Anthropic Compatibility](anthropic-compat.md) for details.

## Internal API (Worker Communication)

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/internal/v1/results/:namespace/:taskName` | POST | Submit task result |
| `/internal/v1/artifacts/:namespace/:taskName/:filename` | POST | Upload task artifact |
| `/internal/v1/sessions/:namespace/:name/transcript` | GET | Get session transcript |
| `/internal/v1/plans/:namespace/:taskName` | POST | Save plan state |
| `/internal/v1/plans/:namespace/:taskName` | GET | Get plan state |
| `/internal/v1/messages/:namespace` | POST | Send inter-agent message |
| `/internal/v1/messages/:namespace/:taskName` | GET | Get messages for a task |

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

### Send Message

Workers call this to send messages to sibling tasks (same parent coordinator).

**Endpoint:** `POST /internal/v1/messages/{namespace}`

**Request Body:**
```json
{
  "fromTask": "worker-a",
  "toTask": "worker-b",
  "parentTask": "coordinator",
  "content": "Found a bug in the auth module"
}
```

Use `"toTask": "*"` to broadcast to all siblings.

**Response:** `204 No Content`

### Get Messages

Workers call this to check for unread messages.

**Endpoint:** `GET /internal/v1/messages/{namespace}/{taskName}?parentTask={parentTask}&markRead={true|false}`

**Query Parameters:**
- `parentTask` (required) — Parent coordinator task name (scopes messages to siblings)
- `markRead` (optional, default: `true`) — Whether to mark returned messages as read

**Response (200):**
```json
[
  {
    "id": 1,
    "namespace": "default",
    "fromTask": "worker-b",
    "toTask": "worker-a",
    "parentTask": "coordinator",
    "content": "Found a bug in the auth module",
    "read": false,
    "createdAt": "2026-01-15T10:30:00Z"
  }
]
```

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

# List task artifacts
curl http://localhost:8080/api/v1/tasks/my-task/artifacts \
  -H "Authorization: Bearer $(kubectl create token orka-client)"

# Download an artifact
curl -L http://localhost:8080/api/v1/tasks/my-task/artifacts/output.json \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -o output.json

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
| `web_fetch` | Fetch and extract URL content | `url` (required), `max_chars` (default 50000), `raw` |
| `file_write` | Write or append files in workspace paths | `path` (required), `content` (required), `mode` (`write`/`append`), `create_dirs` |

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
