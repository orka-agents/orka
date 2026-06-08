---
slug: /api-reference
---

# API Reference

The controller exposes a REST API for programmatic access. All `/api/v1/*` endpoints require authentication. By default Orka accepts Kubernetes ServiceAccount bearer tokens; when configured, external callers can use a valid OIDC JWT or generic context token instead.

## Authentication

Send Kubernetes ServiceAccount and OIDC credentials with the standard bearer token header:

```http
Authorization: Bearer <token>
```

Authentication modes:

- **Kubernetes ServiceAccount token** — default mode. Tokens are validated with the Kubernetes TokenReview API.
- **OIDC JWT** — enabled when the controller is configured with `--oidc-issuer` and `--oidc-audience` (or `ORKA_OIDC_ISSUER` / `ORKA_OIDC_AUDIENCE`). Tokens are validated against the issuer, audience, expiration, and RS256 signature. If `--oidc-jwks-url` is omitted, Orka discovers the JWKS URL from the issuer metadata.
- **Context token / `kontxt` TxToken** — enabled with `--context-token-profile=kontxt`, `--context-token-issuer`, and `--context-token-audience` (or the matching `ORKA_CONTEXT_TOKEN_*` env vars). The built-in profile validates RS256 TxTokens with `typ: txntoken+jwt`, issuer/audience/time claims, `kid`, and required `iat`, `txn`, `scope`, and `req_wl` claims. By default tokens are read from the raw `Txn-Token` header; `Authorization: Bearer` support is opt-in with `--context-token-headers=Txn-Token,Authorization:Bearer`.

```http
Txn-Token: <txntoken+jwt>
```

When a Task is created through OIDC or context-token authentication, Orka stamps the verified caller identity into immutable `spec.requestedBy` (`subject`, `issuer`, `username`, `email`, `groups`, and `roles` when present). Context-token Task creation also stamps immutable `spec.transaction` plus transaction labels/annotations for audit correlation. Clients cannot provide or override `requestedBy` or `transaction`; requests containing top-level or nested `spec.requestedBy`/`spec.transaction` are rejected with `400`. See [Kontxt TxToken integration](../concepts/kontxt.md) for scope/`tctx` authorization, TTS exchange, delegation, and audit behavior.

## Webhooks

GitHub webhooks use HMAC verification instead of bearer-token authentication.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/webhooks/github` | POST | Accept GitHub `issues` / `pull_request` `labeled` events and create agent Tasks for labels such as `agent:implement` |

The controller requires `ORKA_GITHUB_WEBHOOK_SECRET` and verifies the `X-Hub-Signature-256` header. See [GitHub Label Triggers](../guides/github-label-triggers.md) for configuration and label behavior.

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

### Task Execution Workspace Schema

`POST /api/v1/tasks` accepts the Task CRD shape. Agent Tasks may include `spec.execution.workspace` to request experimental workspace-backed execution through an upstream `agent-sandbox` installation. The controller validates the request, resolves/defaults the effective `SandboxTemplate` and workspace settings, and injects the resolved settings into the outer Kubernetes worker Job. The agent worker wrapper then claims and executes inside the sandbox workspace.

| Path | Type | Values/default | Notes |
|------|------|----------------|-------|
| `spec.execution.workspace.enabled` | boolean | default `false` | Enables workspace-backed execution for an agent Task. The controller rejects enabled requests unless agent sandbox support is enabled. |
| `spec.execution.workspace.templateRef.name` | string | controller default template, if configured | Workspace template name. Required when `enabled: true` and no controller default template is configured. |
| `spec.execution.workspace.templateRef.namespace` | string | Task namespace | Namespace containing the workspace template in Orka metadata. Current SDK-backed execution creates claims in the Task namespace and requires the template to be usable there. |
| `spec.execution.workspace.reusePolicy` | string | `none`; allowed `none`, `session` | `session` derives the reuse key from `spec.sessionRef.name` and requires that field to be set. Automatic cross-Job reattach is limited until Orka persists sandbox claim identity. |
| `spec.execution.workspace.cleanupPolicy` | string | controller default cleanup policy; allowed `delete`, `retain` | Cleanup behavior after the sandbox command exits. |

Workspace requests are only valid on `spec.type: agent` Tasks. See [Agent Sandbox Workspaces](../concepts/agent-sandbox.md) for configuration, validation rules, live smoke-test steps, and current limitations.

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


## Memory

Memory endpoints manage namespace-scoped durable memories and reviewable memory proposals. See [Memory](../concepts/memory.md) for the full lifecycle, worker behavior, and examples.

### Durable Memories

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memories` | GET | List durable memories |
| `/api/v1/memories` | POST | Create durable memory |
| `/api/v1/memories/:id` | GET | Get durable memory |
| `/api/v1/memories/:id` | PUT | Update durable memory |
| `/api/v1/memories/:id` | DELETE | Soft-delete durable memory |
| `/api/v1/memories/:id/disable` | POST | Disable memory for normal recall |
| `/api/v1/memories/:id/enable` | POST | Re-enable memory for normal recall |

Common list query parameters: `namespace`, `query`/`q`, `sessionName`, `agentName`, `taskName`, `parentTask`, `source`, `tags`, `ids`, `includeDisabled`, `includeDeleted`, and `limit`.

### Memory Proposals

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/memory-proposals` | GET | List memory proposals |
| `/api/v1/memory-proposals` | POST | Create a memory proposal |
| `/api/v1/memory-proposals/:id` | GET | Get a memory proposal |
| `/api/v1/memory-proposals/:id/review` | POST | Record a review decision without applying it |
| `/api/v1/memory-proposals/:id/apply` | POST | Apply an accepted `memory` proposal into durable memory |
| `/api/v1/memory-proposals/:id/archive` | POST | Archive a proposal without applying it |

Common list query parameters: `namespace`, `taskName`, `agentName`, `type`, `status`, `query`/`q`, and `limit`. Review and archive return `204 No Content`. Apply accepts optional `appliedBy` and returns the linked durable memory JSON; repeated apply requests return the same memory.

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

## Security

Repository security endpoints manage `RepositoryScan` configurations and their generated threat models, scan runs, findings, patch proposals, and remediation pull requests. Like other `/api/v1/*` endpoints, they require ServiceAccount bearer token authentication.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/security/repositories` | POST | Create a repository scan |
| `/api/v1/security/repositories` | GET | List repository scans |
| `/api/v1/security/repositories/:name` | GET | Get repository scan details |
| `/api/v1/security/repositories/:name` | PUT | Update repository scan spec |
| `/api/v1/security/repositories/:name` | DELETE | Delete repository scan |
| `/api/v1/security/repositories/:name/threat-model` | GET | Get latest threat model |
| `/api/v1/security/repositories/:name/threat-model` | PUT | Update threat model |
| `/api/v1/security/repositories/:name/scans` | GET | List scan runs |
| `/api/v1/security/repositories/:name/scans` | POST | Trigger manual scan |
| `/api/v1/security/repositories/:name/slices` | GET | List deterministic review slices |
| `/api/v1/security/repositories/:name/slices/:sliceID` | GET | Get review slice details |
| `/api/v1/security/repositories/:name/dropped-findings` | GET | List v2 dropped-finding diagnostics |
| `/api/v1/security/repositories/:name/findings` | GET | List findings |
| `/api/v1/security/findings/:id` | GET | Get finding details |
| `/api/v1/security/findings/:id/dismiss` | POST | Dismiss finding |
| `/api/v1/security/findings/:id/reopen` | POST | Reopen finding |
| `/api/v1/security/findings/:id/validate` | POST | Trigger validation |
| `/api/v1/security/findings/:id/patch` | POST | Generate patch proposal |
| `/api/v1/security/findings/:id/patches` | GET | List patch proposals |
| `/api/v1/security/findings/:id/pull-request` | POST | Create remediation PR |

Common query parameters:

- `namespace` — Kubernetes namespace to operate in.
- `limit` — page size for list endpoints that support pagination.
- `continue` — Kubernetes continue token for `GET /api/v1/security/repositories`.
- `cursor` — store cursor for `GET /api/v1/security/repositories/:name/scans`, `GET /api/v1/security/repositories/:name/slices`, `GET /api/v1/security/repositories/:name/dropped-findings`, and `GET /api/v1/security/repositories/:name/findings`.
- `severity`, `validationStatus`, `state`, `sliceID`, `category` — filters for `GET /api/v1/security/repositories/:name/findings`.
- `status` — filter for `GET /api/v1/security/repositories/:name/slices`.
- `scanRunID`, `sliceID` — filters for `GET /api/v1/security/repositories/:name/dropped-findings`.
- `recommended=true` — filters findings to recommended remediation candidates.

### Create Repository Scan

**Endpoint:** `POST /api/v1/security/repositories`

**Request Body:**
```json
{
  "name": "example-repo",
  "namespace": "default",
  "spec": {
    "provider": "github",
    "repoURL": "https://github.com/example/app",
    "branch": "main",
    "schedule": "0 2 * * *",
    "validationMode": "light",
    "analysisAgentRef": {"name": "security-reviewer"}
  }
}
```

**Response (201):** The created `RepositoryScan` resource.

Required fields are `name`, `spec.repoURL`, and `spec.analysisAgentRef.name`. The API defaults or infers provider, owner, repository, branch, and validation mode where possible.

### Security Findings Workflow

A typical remediation workflow is:

1. List findings with `GET /api/v1/security/repositories/:name/findings?namespace=default&recommended=true`.
2. Inspect evidence with `GET /api/v1/security/findings/:id`.
3. Optionally validate with `POST /api/v1/security/findings/:id/validate`.
4. Generate a patch with `POST /api/v1/security/findings/:id/patch`.
5. Review patch proposals with `GET /api/v1/security/findings/:id/patches`. A proposal is successful only after patch summary and diff verification passes.
6. Create a remediation pull request with `POST /api/v1/security/findings/:id/pull-request`.

Review slice and dropped-output inspection:

1. List slices with `GET /api/v1/security/repositories/:name/slices?namespace=default`.
2. Inspect one slice with `GET /api/v1/security/repositories/:name/slices/:sliceID?namespace=default`.
3. List rejected v2 model output with `GET /api/v1/security/repositories/:name/dropped-findings?namespace=default&scanRunID=scan_...`.

## Repository Monitors

Repository monitor endpoints manage `RepositoryMonitor` configurations and their durable monitor runs, PR queue items, review state, and audit events. The current implementation supports GitHub pull request monitoring and read-only review task creation.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/v1/monitors/repositories` | POST | Create a repository monitor |
| `/api/v1/monitors/repositories` | GET | List repository monitors |
| `/api/v1/monitors/repositories/:name` | GET | Get repository monitor details |
| `/api/v1/monitors/repositories/:name` | PUT | Update repository monitor spec |
| `/api/v1/monitors/repositories/:name` | DELETE | Delete repository monitor |
| `/api/v1/monitors/repositories/:name/runs` | POST | Trigger a manual monitor run |
| `/api/v1/monitors/repositories/:name/runs` | GET | List monitor runs |
| `/api/v1/monitors/repositories/:name/items` | GET | List current monitor items |
| `/api/v1/monitors/events` | GET | List monitor audit events |

Common query parameters:

- `namespace` - Kubernetes namespace to operate in.
- `limit` - page size for list endpoints.
- `continue` or `cursor` - pagination cursor for store-backed list endpoints.
- `kind`, `state`, `verdict`, `repairState`, and `automergeState` - filters for `GET /api/v1/monitors/repositories/:name/items`.
- `name`, `runID`, `itemKind`, `itemNumber`, and `eventType` - filters for `GET /api/v1/monitors/events`; `name` is required.

Context-token authorization scopes are `orka:monitors:read` for list/get endpoints, `orka:monitors:write` for create/update/delete, and `orka:monitors:operate` for manual run creation.

### Create Repository Monitor

**Endpoint:** `POST /api/v1/monitors/repositories`

**Request Body:**
```json
{
  "name": "example-app",
  "namespace": "default",
  "spec": {
    "provider": "github",
    "repoURL": "https://github.com/example/app",
    "branch": "main",
    "gitSecretRef": {"name": "repo-monitor-github"},
    "schedule": "*/30 * * * *",
    "targets": {
      "pullRequests": {
        "enabled": true,
        "includeDrafts": false,
        "maxPerRun": 10
      }
    },
    "agents": {
      "reviewer": {"name": "repo-reviewer"}
    },
    "review": {
      "event": "COMMENT",
      "staleReviewTTL": "24h"
    },
    "policy": {
      "protectedLabels": ["security-sensitive"],
      "pauseLabels": ["orka:pause"]
    },
    "validation": {
      "mode": "changed",
      "commands": ["make test"]
    }
  }
}
```

**Response (201):** The created `RepositoryMonitor` resource.

Required fields are `name`, `spec.repoURL`, and `spec.agents.reviewer.name` when pull request monitoring is enabled. The API defaults or infers provider, owner, repository, branch, pull request enablement, pull request `maxPerRun`, `review.event`, and validation mode where possible.

Only GitHub pull request monitoring is supported in this slice. Requests that enable issue or commit targets, disable pull request monitoring, use a non-GitHub provider, set `review.requireGreenCI`, or reference a non-Claude reviewer runtime are rejected with `400`.

### Trigger Manual Monitor Run

**Endpoint:** `POST /api/v1/monitors/repositories/{name}/runs`

**Request Body:**
```json
{
  "targetKind": "pull_request",
  "targetNumber": 123,
  "targetSHA": "abc123"
}
```

The request body can be omitted to run a full pull request inventory pass. `targetKind` must be empty or `pull_request`; `targetNumber` and `targetSHA` narrow the run to one PR or exact head. The API returns `409` when the monitor already has a queued or running run.

See [Repository Monitors](../guides/repository-monitors.md) for the full workflow and CRD example.

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

See [Interactive Chat](../guides/chat.md) for full chat documentation.

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

The `/anthropic/v1/messages` endpoint injects built-in tools and runs server-side tool execution by default. Set `X-Orka-Tools: disabled` header to use as a transparent proxy instead. See [Anthropic Compatibility](anthropic-compat.md) for details.

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

These tools are injected into AI worker agents when the Agent has `coordination.enabled: true`. They are not returned by `GET /api/v1/tools`.

The following tools are **auto-injected** when coordination is enabled:

| Tool | Description | Parameters |
|------|-------------|------------|
| `delegate_task` | Delegate a subtask to another agent | `agent`, `prompt` (required); `namespace`, `priority`, `auto_retry`, `max_retries` |
| `wait_for_tasks` | Wait for delegated tasks to complete | `tasks` (required), `timeout` (default 10m) |
| `create_container_task` | Create a child container task | `name`, `image`, `command`/`args`, env/workspace fields |
| `cancel_task` | Cancel a running child task | `task_name` (required); `namespace`, `reason` |
| `send_message` | Send a message to a sibling task | `to_task` (required, or `*` to broadcast), `content` (required) |
| `check_messages` | Check for messages from sibling tasks | `mark_read` (boolean, default true) |
| `recall_memory` | Recall durable namespace-scoped memories | `query`, `tags`, `task_name`, `agent_name`, `source`, `limit`, `include_disabled` |
| `remember` | Submit a durable memory proposal for review | `content` (required); `title`, `description`, `tags`, `agent_name` |
| `propose_memory` | Submit a memory-adjacent governance proposal | `title` (required); `type`, `skill_name`, `description`, `content`, `patch`, `agent_name` |
| `search_transcript` | Search prior session transcripts | `query` (required); `session_name`, `exclude_session_name`, `roles`, `limit`, `max_snippet_length` |
| `create_pull_request` | Create a GitHub pull request | `task_name`, `head_branch`, `base_branch`, `title` (required); `body` |
| `check_pull_request_ci` | Check GitHub CI status without merging | `pr_number` (required); `task_name`, `repo_url`, `wait_timeout`, `poll_interval` |
| `merge_pull_request` | Merge a GitHub pull request | `task_name`, `pr_number` (required); `merge_method`, `commit_title`, `commit_message` |
| `auto_merge_pull_request` | Poll CI checks and merge a PR when all pass | `task_name`, `pr_number` (required); `merge_method`, `commit_title`, `commit_message`, `timeout` |
| `review_pull_request` | Fetch PR diff for review | `task_name`, `pr_number` (required) |
| `post_review_comment` | Post a review on a PR | `task_name`, `pr_number`, `body`, `event` (required); `comments` |
| `create_agent` | Create an Agent CRD at runtime | `name`, `provider`, `model` (required); `systemPrompt`, `tools`, `coordination` |
| `delete_agent` | Delete an Agent CRD | `name` (required), `namespace` |
| `update_plan` | Update the autonomous execution plan | `summary`, `plan_document` (required); `progress_pct`, `goal_complete` |

The following 4 tools require explicit `spec.tools[]` entries on the Agent CRD:

| Tool | Description | Parameters |
|------|-------------|------------|
| `list_issues` | List open GitHub issues in a repository | `task_name`, `repo_url`; `unassigned_only` (default true), `per_page`, `page` |
| `list_pull_requests` | List open pull requests in a repository | `task_name`, `repo_url`; `per_page`, `page` |
| `get_issue` | Fetch full details of a GitHub issue | `issue_number` (required); `task_name`, `repo_url` |
| `comment_on_issue` | Post a comment on a GitHub issue | `issue_number`, `body` (required); `task_name`, `repo_url` |
