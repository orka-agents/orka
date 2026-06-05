---
slug: /architecture
---

# Architecture

Orka is a Kubernetes-native task execution platform where a controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes.

## Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                          Orka Controller                             │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐     │
│  │     Task    │  │    Agent     │  │     Tool & Provider      │     │
│  │  Reconciler │  │  Controller  │  │       Controllers        │     │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘     │
│                                                                      │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐     │
│  │   Session   │  │   Priority   │  │     REST API + Chat      │     │
│  │   Manager   │  │    Queue     │  │    (Fiber framework)     │     │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘     │
│                                                                      │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────────────┐     │
│  │  Prometheus │  │   Embedded   │  │     Auth Middleware      │     │
│  │   Metrics   │  │    Web UI    │  │      (SA/OIDC auth)      │     │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘     │
└──────────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              │               │               │
       ┌──────┴──────┐ ┌──────┴──────┐ ┌──────┴──────┐
       │   General   │ │     AI      │ │    Agent    │
       │   Worker    │ │   Worker    │ │   Workers   │
       │ (containers)│ │ (LLM agent) │ │(Claude CLI, │
       └─────────────┘ └─────────────┘ │ Copilot CLI)│
                                       └─────────────┘
```

## Core Components

### Controller (`cmd/main.go`)

The controller is the central component that runs as a Kubernetes Deployment. It contains:

- **API Server**: Fiber-based REST and compatibility endpoints for:
  - task CRUD, results, logs, artifacts, plans, and children
  - sessions
  - memories and memory proposals
  - tools
  - agents
  - skills
  - repository security scanning
  - chat
  - OpenAI-compatible API
  - Anthropic-compatible API
  - internal worker APIs
- **Task Reconciler**: Watches Task resources, creates/manages Jobs, handles lifecycle
- **RepositoryScanReconciler**: Watches `RepositoryScan` resources and drives repository security scanning:
  - schedules manual and cron scans
  - creates AI tasks for threat-model generation and vulnerability discovery
  - persists scan runs, threat models, findings, and patch proposals in SQLite
  - reads scan artifacts from the artifact store
  - auto-creates validation and patch proposal tasks when configured
  - updates status with phase, last scan, commits, and finding counts
- **Session Manager**: Manages session persistence (via SQLite store) for conversation continuity with serial execution enforcement
- **Memory Store**: Persists durable memories, memory proposals, and transcript search data in SQLite for namespace-scoped agent context
- **Priority Queue**: Schedules tasks based on priority (0-1000)
- **Webhook Notifier**: Delivers completion notifications via HTTP callbacks
- **Embedded Web UI**: The React dashboard is compiled into the controller binary

### Custom Resource Definitions (`api/v1alpha1/`)

Orka uses six CRDs:

| CRD | Purpose |
|-----|---------|
| **Task** | Core work unit — container, AI, or agent type |
| **Agent** | Reusable agent configurations with model, tools, skills, and optional runtime |
| **Tool** | Custom HTTP-based tool definitions for agents |
| **Provider** | LLM provider configuration (Anthropic, OpenAI, Azure OpenAI) |
| **Skill** | Reusable prompt content injected into agent system prompts |
| **RepositoryScan** | Repository security scan configuration, scheduling, status, and finding counts |

### Worker Images (`workers/`)

| Worker | Description |
|--------|-------------|
| **General Worker** (`workers/general/`) | Runs arbitrary container commands |
| **AI Worker** (`workers/ai/`) | Runs LLM agent tasks with built-in core, coordination, GitHub, agent-management, planning, memory, transcript, chat, session, and task-management tools |
| **Copilot Agent Worker** (`workers/agent/copilot/`) | Runs tasks via GitHub Copilot CLI using the Go SDK |
| **Claude Agent Worker** (`workers/agent/claude/`) | Runs tasks via Claude Code CLI |
| **Codex Agent Worker** (`workers/agent/codex/`) | Runs tasks via OpenAI Codex CLI |

## Design Decisions

| Area | Decision | Rationale |
|------|----------|-----------|
| **Result Storage** | SQLite (embedded) | No size limit, zero external dependencies, pure Go via `modernc.org/sqlite`. |
| **Session Storage** | SQLite (embedded) | Normalized schema with efficient querying and pagination. No size limit. |
| **Plan Storage** | SQLite (embedded) | Persists autonomous coordination plan state across iterations. |
| **Memory Storage** | SQLite (embedded) | Persists durable memories and reviewable memory proposals for namespace-scoped recall. |
| **Artifact Storage** | SQLite stores artifact metadata and BLOB content, 10MB max per artifact. | Keeps worker outputs co-located with task/session state while bounding per-artifact size. |
| **Security Scan Storage** | SQLite stores repository scan runs, threat models, findings, and patch proposals. | Provides durable repository-security history without an external database. |
| **API Authentication** | Kubernetes ServiceAccount tokens plus optional OIDC JWT and generic context-token validation. | Native K8s auth by default; OIDC and `kontxt` TxTokens support external/request-scoped API clients. |
| **Task Queue** | Priority queuing (0-1000) | Higher priority tasks are scheduled first. |
| **Secret Management** | Reference K8s Secrets in specs | Controller mounts secrets to worker pods. |
| **Observability** | Prometheus metrics, structured logs, optional OpenTelemetry tracing. | Standard K8s metrics/logging with opt-in distributed tracing. |
| **AI Tools** | Built-in + extensible via CRDs | Ship with categorized built-in tools and can be extended via Tool CRDs. |
| **Failure Policy** | Configurable retry with backoff | `spec.retryPolicy` with max retries and exponential backoff. |
| **Session Execution** | Serial per session | Tasks sharing a session run one-at-a-time to prevent race conditions. |
| **Worker Security** | Hardened pods | Non-root, read-only rootfs, all capabilities dropped, seccomp RuntimeDefault. |

## Project Structure

```
orka/
├── api/v1alpha1/           # CRD type definitions (Task, Agent, Tool, Provider, Skill, RepositoryScan)
├── cmd/
│   ├── main.go                # Controller entrypoint
│   ├── cli/                   # CLI tool (login, chat, agent, task, status)
│   └── migrate/               # Database migration (ConfigMaps → SQLite)
├── internal/
│   ├── api/                # REST API server, handlers, auth, chat, compatibility APIs
│   ├── controller/         # Reconcilers, job builder, session manager, priority queue
│   ├── llm/                # LLM provider interface and implementations
│   │   ├── anthropic/      # Anthropic Claude provider
│   │   └── openai/         # OpenAI provider
│   ├── store/              # Storage interfaces and SQLite implementation
│   │   └── sqlite/         # SQLite backend for results, sessions, plans, artifacts, memory, security
│   ├── tools/              # Built-in tool implementations
│   ├── metrics/            # Prometheus metrics
│   ├── worker/             # Tool executor for custom Tool CRDs
│   ├── cli/                # CLI command implementations
│   └── uiembed/            # Go embed for UI static assets
├── workers/
│   ├── ai/                 # AI worker (LLM agent with tools)
│   ├── general/            # General worker (container commands)
│   └── agent/
│       ├── copilot/        # Copilot CLI agent worker
│       ├── claude/         # Claude Code CLI agent worker
│       └── codex/          # Codex CLI agent worker
├── ui/                     # React SPA (Vite + TanStack Router + shadcn/ui)
├── config/                 # Kustomize manifests (CRDs, RBAC, samples)
├── charts/orka/          # Helm chart
├── website/docs/           # Documentation
├── examples/               # Example workflows
└── test/                   # E2E tests
```

## Task Lifecycle

```
Task Created
      │
      ▼
┌───────────┐    session locked?     ┌───────────┐
│  Pending  │──────────────────────▶│  Pending  │ (wait for lock)
│           │◀──────────────────────│           │
└─────┬─────┘    lock acquired      └───────────┘
      │
      ▼
┌───────────┐
│  Running  │ ── Job created, Pod running
└─────┬─────┘
      │
   ┌──┴──┐
   │     │
   ▼     ▼
┌───────┐ ┌────────┐
│ Succ. │ │ Failed │ ── retry? → back to Running
└───────┘ └────────┘
      │
      ▼
Result stored in SQLite (workers POST to controller via HTTP)
Session lock released
Webhook delivered (if configured)
```

## Multi-Agent Coordination

Coordinator agents can delegate subtasks to specialist agents at runtime. The LLM uses `delegate_task` and `wait_for_tasks` tools to create child Tasks and collect results. GitHub PR tools (`create_pull_request`, `check_pull_request_ci`, `review_pull_request`, `post_review_comment`, `merge_pull_request`, `auto_merge_pull_request`) enable end-to-end code review workflows. The controller enforces guardrails:

```
Coordinator Agent (depth 0)
├── delegate_task(agent: "specialist-a", prompt: "...")  → Child Task (depth 1)
├── delegate_task(agent: "specialist-b", prompt: "...")  → Child Task (depth 1)
│   └── delegate_task(agent: "sub-specialist", ...)      → Grandchild Task (depth 2)
└── wait_for_tasks(tasks: [...])  → aggregated results
```

**Controller enforcement** (in `handlePending`):
- **maxDepth**: Rejects child tasks exceeding the coordinator's depth limit
- **allowedAgents**: Rejects delegation to agents not in the coordinator's allow list
- **maxConcurrentChildren**: Requeues (not fails) child tasks when the active sibling count is at the limit

**ChildTaskStatus tracking** (in `handleRunning`): Coordinator tasks get `status.childTasks[]` populated with each child's name, agent, phase, and truncated result.

Child tasks use owner references for cascade deletion and labels (`orka.ai/parent-task`, `orka.ai/delegated-agent`) for querying.

See [multi-agent-coordination.md](../guides/multi-agent-coordination.md) for full details.

### Autonomous Mode

When an agent's coordination config has `autonomous: true`, the controller runs the coordinator in a loop instead of completing the task after a single Job. Each iteration:

1. The coordinator Job runs, delegates sub-tasks, and updates the plan via the `update_plan` tool
2. The controller saves plan state to `PlanStore` (SQLite) and checks termination conditions
3. If not complete, a new Job is created for the next iteration with the accumulated plan state

Termination occurs when the LLM signals goal completion, max iterations are reached, or the task is suspended.

## Repository Security Scanning

`RepositoryScan` resources define repository URLs, branches, scan cadence, agents, validation policy, and patch-generation policy. The `RepositoryScanReconciler` starts with a threat-model task, then fans out discovery tasks across security scopes after the threat model succeeds. It ingests task artifacts from the artifact store, upserts threat models and findings into SQLite, updates scan-run status, and can automatically start validation or patch-proposal tasks based on scan policy.

RepositoryScan status reports the current phase, last scan ID/task, last successful scan time, processed commits, and finding counts so API clients and the UI can display repository security posture without querying all findings.

## LLM Provider Architecture

The AI worker uses a pluggable provider interface:

```go
type Provider interface {
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
    Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
    Name() string
}
```

Implementations exist for Anthropic Claude and OpenAI. Provider selection is configured via the Provider CRD, which stores credentials in Kubernetes Secrets.

## Skills & Tools System

Orka supports extensible AI capabilities through a three-layer system:

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 1: Skills (Skill CRDs)                                   │
│  - Agent Skills standard content (`spec.content.inline`)        │
│  - Mounted at /workspace/.skills and injected into prompts      │
├─────────────────────────────────────────────────────────────────┤
│  Layer 2: Built-in Tools (in worker image)                      │
│  - Core, coordination, GitHub, agent management, planning,      │
│    memory, transcript, chat, session, and task management       │
│  - Fast, no extra infrastructure                                │
├─────────────────────────────────────────────────────────────────┤
│  Layer 3: Custom Tools (Tool CRD + HTTP)                        │
│  - Point at internal services                                   │
│  - Namespace-scoped, RBAC-controlled                            │
│  - Header-based or body-based auth injection                    │
└─────────────────────────────────────────────────────────────────┘
```

Built-in tool categories:

- **Core**: `web_search`, `code_exec`, `file_read`, `web_fetch`, `file_write`
- **Coordination/task**: `delegate_task`, `wait_for_tasks`, `create_container_task`, `cancel_task`, `send_message`, `check_messages`
- **GitHub**: `create_pull_request`, `check_pull_request_ci`, `merge_pull_request`, `auto_merge_pull_request`, `review_pull_request`, `post_review_comment`, `list_issues`, `list_pull_requests`, `get_issue`, `comment_on_issue`
- **Agent management**: `create_agent`, `delete_agent`, plus chat-management `update_agent`, `list_agents`
- **Planning/memory/transcript**: `update_plan`, `recall_memory`, `remember`, `propose_memory`, `search_transcript`
- **Chat/session/task management**: `create_ai_task`, `create_agent_task`, `check_task_progress`, `fetch_task_output`, `wait_for_task`, `list_tools`, `list_tasks`, `create_tool`, `delete_tool`, `delete_session`

## Session Management

Sessions provide conversation continuity across multiple Tasks. Each session is stored in SQLite with a normalized schema (session metadata + individual messages).

Key behaviors:
- **Serial execution**: Tasks sharing a session execute one-at-a-time via a lock mechanism
- **Token tracking**: Input/output token counts tracked in the session record
- **Cross-runtime**: Sessions store user/assistant messages only, enabling cross-runtime continuation (AI ↔ agent tasks)
- **No size limit**: SQLite storage removes the old ConfigMap 1MB constraint
- **Init container delivery**: Session transcripts are delivered to worker pods via an init container that fetches from the controller's internal API

## Memory Model

Durable memory is stored in SQLite and scoped by namespace. AI workers load a bounded set of reviewed durable memories through the controller internal API and append them to the system prompt as background context. Memory context is best-effort: task execution should continue even if memory recall is unavailable.

Workers can also use memory tools for active recall and proposal creation:

- `recall_memory` queries durable memories by text, tags, task, agent, source, and limit.
- `search_transcript` searches prior session transcripts and returns compact snippets.
- `remember` creates a durable-memory proposal for review.
- `propose_memory` creates a memory-adjacent governance proposal.

Proposal review is intentionally separate from durable memory mutation. Accepting or rejecting a proposal records governance state but does not automatically create durable memory. See [memory.md](memory.md) for API examples and validation details.

## Security Model

- **Worker pods**: Non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault
- **Controller**: Non-root (uid 65532), read-only rootfs, seccomp RuntimeDefault
- **ServiceAccount TokenReview**: Default API authentication validates Kubernetes ServiceAccount bearer tokens via the TokenReview API.
- **Optional OIDC JWT validation**: External API endpoints can validate OIDC JWTs when issuer/audience settings are configured.
- **Optional context-token validation**: External API endpoints can validate generic context tokens, with built-in `kontxt` TxToken support via `Txn-Token` and profile-specific issuer/audience/JWKS settings. Orka can enforce operation scopes and signed `tctx` constraints, stamp immutable transaction metadata, and use kontxt TTS to narrow child/outbound tokens for delegated agents and downstream Tool calls.
- **Internal worker endpoints**: `/internal/v1` endpoints require ServiceAccount authentication for worker result, plan, message, artifact, memory, and transcript calls.
- **Secrets**: API keys referenced via `secretRef`, mounted as read-only volumes, never logged
- **`--watch-namespace`**: Optionally scopes the controller and API to a single namespace.
- **Namespace isolation**: `--enforce-namespace-isolation` restricts users to their ServiceAccount namespace.
- **Cross-namespace references**: Cross-namespace Agent and Provider references are rejected when namespace isolation is enforced.
- **Chat endpoint**: Blocks operations in `kube-system` and `kube-public` namespaces.

## Dependencies

| Package | Purpose |
|---------|---------|
| `sigs.k8s.io/controller-runtime` | Controller framework |
| `k8s.io/client-go` | Kubernetes client |
| `github.com/gofiber/fiber/v3` | HTTP router |
| `github.com/anthropics/anthropic-sdk-go` | Anthropic Claude API |
| `github.com/openai/openai-go/v3` | OpenAI API (official SDK) |
| `github.com/github/copilot-sdk/go` | GitHub Copilot SDK |
| `modernc.org/sqlite` | Embedded SQLite (pure Go, no CGO) |

## SQLite Store Internals

All persistent data uses SQLite via `modernc.org/sqlite` (pure Go, no CGO dependency).

### Schema

| Table | Primary Key | Purpose |
|-------|-------------|---------|
| `results` | `(namespace, task_name)` | Task output data (BLOB) |
| `sessions` | `(namespace, name)` | Session metadata, `active_task` field for locking, token counters |
| `session_messages` | `id` (FK → sessions) | Individual messages with role, content, tool_calls (JSON) |
| `messages` | `id` + `namespace` + `parent_task` | Inter-agent messages, broadcast via `to_task='*'` |
| `plan_states` | `(namespace, task_name)` | Autonomous loop state: iteration, progress %, goal_complete flag |
| `memories` | `id` | Durable namespace-scoped memories with provenance, tags, disabled/deleted flags, and recall counters |
| `memory_proposals` | `id` | Reviewable memory/skill/policy/workflow proposals with status, reviewer, and review notes |
| `artifacts` | `(namespace, task_name, filename)` | Artifact metadata and BLOB content, 10MB max per artifact |
| `security_scan_runs` | `id` | Repository scan run lifecycle, mode, commits, timestamps, summary, and errors |
| `security_threat_models` | `(namespace, repository_scan, version)` | Versioned repository threat models generated or edited for scans |
| `security_findings` | `id` | Deduplicated findings with severity, confidence, validation, evidence, and PR linkage |
| `security_patch_proposals` | `id` | Patch proposal tasks, branches, artifacts, status, and PR linkage for findings |

### Configuration

- **WAL mode** with single-writer enforcement: `SetMaxOpenConns(1)`, `SetMaxIdleConns(1)`
- **Per-connection pragmas** (set on every new connection, not persistent):
  - `busy_timeout=5000` — wait up to 5s for locks
  - `synchronous=NORMAL` — balance between safety and performance
  - `foreign_keys=ON` — enforce referential integrity
- **Namespace scoping**: All queries filter by `namespace` — data isolation is enforced at the SQL level

### Session Locking

Sessions use optimistic locking via an `active_task` column. `AcquireLock` atomically sets `active_task` only if it's currently empty. Tasks that fail to acquire the lock requeue every 5 seconds. The lock is released on task completion or deletion (via finalizer cleanup). There is no timeout — if the lock holder crashes, the lock persists until the task is deleted.

### Message Broadcast Scoping

Inter-agent broadcast messages (`to_task='*'`) are scoped by `parent_task`:

```sql
WHERE (to_task = ? OR (to_task = '*' AND parent_task = ?))
```

This ensures only sibling tasks (same parent coordinator) receive broadcasts. Senders don't receive their own broadcasts.

## LLM Provider Internals

### Retry Strategy

LLM calls use exponential backoff with jitter:
- **Default**: 3 retries
- **Backoff**: `baseDelay × 2^attempt`, capped at 30s, with ±10% random jitter
- **Retryable status codes**: 429, 500, 502, 503, 529
- **Non-retryable**: 401, 403 (trigger fallback instead), context canceled/deadline exceeded (never retried)
- **Stream retry**: Peeks at the initial stream event to detect errors before consuming the stream

### Provider Cooldown

Failed providers are temporarily cooled down to prevent repeated failures:
- **Cooldown formula**: `1min × 5^(errorCount-1)`, capped at 1 hour
- Rate-limited providers (429) are tracked and skipped in subsequent requests
- Cooldown is per-provider and resets on successful requests

### OpenAI API Auto-Detection

The OpenAI provider automatically detects which API to use:
1. Tries the **Responses API** first
2. If the endpoint returns 404/405 or a known unsupported-API error code, switches to **Chat Completions API**
3. The API mode is stored as an `atomic.Int32` for thread-safe switching
4. Once detected, the mode persists for the provider's lifetime

Copilot-compatible Responses API 403s are handled as a scoped fallback to Chat Completions. Generic 403s still surface as provider errors instead of being treated as unsupported API signals.

### Anthropic Quirks

- The Anthropic SDK appends `v1/messages` to the base URL — strip trailing `/v1` from custom `baseURL` to avoid doubled paths
- System messages are converted to `tool_result` blocks, not user messages
- Tool input JSON parsing errors are silently ignored (`_ = json.Unmarshal`)
