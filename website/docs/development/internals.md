---
slug: /internals
description: "How Orka's controller is laid out in the source tree, and the storage and model-provider internals behind it."
---

# Controller internals

This page is for people changing Orka itself. For how the pieces fit together, read
[Architecture](../concepts/architecture.md) first.

## Source layout

```
orka/
├── api/                    # Custom resource types
│   ├── v1alpha1/           # core.orka.ai: Task, Agent, Provider, Tool, RuntimePool, ...
│   └── gateway/            # gateway.orka.ai: GatewayClass, Gateway, GatewayBinding
├── cmd/
│   ├── main.go             # Controller entrypoint
│   ├── cli/                # The orka CLI
│   ├── orka-acp-runtime/   # Coding-agent runtime supervisor
│   ├── orka-provider-auth-proxy/
│   ├── orka-workspace-publisher/
│   ├── orka-scm-egress-proxy/
│   ├── orka-compat-router/ # Shared OpenAI/Anthropic endpoint router
│   ├── orka-gateway-*      # Gateway conformance tool and reference adapter
│   └── build/              # Release tooling and the Helm chart generator
├── internal/
│   ├── api/                # REST API, auth, chat, and compatibility endpoints
│   ├── controller/         # Reconcilers, job builder, ACP dispatcher, RuntimePool controller
│   ├── acp/, harness/      # orka.harness.v2 client, supervisor contract, and conformance
│   ├── store/              # Kubernetes-authoritative control store and SQLite payload store
│   ├── llm/                # Model provider clients, retries, fallbacks, and cooldown
│   ├── tools/, aitools/    # Built-in tools and effective tool resolution
│   ├── gateway/            # Generic gateway inbox, outbox, and bindings
│   ├── security/           # Repository security scanning
│   ├── connectors/         # Per-user linked accounts
│   ├── workspace/          # Execution workspace providers
│   ├── tracing/, metrics/  # OpenTelemetry and Prometheus
│   └── uiembed/            # Go embed for the dashboard
├── workers/
│   ├── ai/                 # Native AI worker
│   ├── general/            # Container worker
│   ├── acp/                # Coding-agent runtime images
│   └── publisher/          # Clean-room workspace publisher image
├── ui/                     # Dashboard (React, Vite, TanStack Router)
├── config/                 # Kustomize manifests: CRDs, RBAC, samples, overlays
├── manifest_staging/       # Generated installer manifest and Helm chart for the next release
├── website/                # This documentation site
├── docs/                   # Architecture decision records and protocol notes
├── examples/               # Runnable examples
└── test/                   # End-to-end tests
```

[Development](development.md) explains which of these are generated and how to regenerate them.

## Model provider interface

The AI worker uses a pluggable provider interface:

```go
type Provider interface {
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
    Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
    Name() string
}
```

Implementations exist for Anthropic Claude and OpenAI. Provider selection is configured via the Provider CRD, which stores credentials in Kubernetes Secrets.

## LLM provider internals

### Retry strategy

LLM calls use exponential backoff with jitter:
- **Default**: 3 retries
- **Backoff**: `baseDelay × 2^attempt`, capped at 30s, with ±10% random jitter
- **Retryable status codes**: 429, 500, 502, 503, 529
- **Non-retryable**: 401, 403 (trigger fallback instead), context canceled/deadline exceeded (never retried)
- **Stream retry**: Peeks at the initial stream event to detect errors before consuming the stream

### Provider cooldown

Failed providers are temporarily cooled down to prevent repeated failures:
- **Cooldown formula**: `1min × 5^(errorCount-1)`, capped at 1 hour
- Rate-limited providers (429) are tracked and skipped in subsequent requests
- Cooldown is per-provider and resets on successful requests

### OpenAI API auto-detection

The OpenAI provider automatically detects which API to use:
1. Tries the **Responses API** first
2. If the endpoint returns 404/405 or a known unsupported-API error code, switches to **Chat Completions API**
3. The API mode is stored as an `atomic.Int32` for thread-safe switching
4. Once detected, the mode persists for the provider's lifetime

Copilot-compatible Responses API 403s are handled as a scoped fallback to Chat Completions. Generic 403s still surface as provider errors instead of being treated as unsupported API signals.

### Anthropic quirks

- The Anthropic SDK appends `v1/messages` to the base URL — strip trailing `/v1` from custom `baseURL` to avoid doubled paths
- `system` messages in the conversation are sent as user text messages; only `tool` messages become user `tool_result` blocks
- Tool input JSON parsing errors are silently ignored (`_ = json.Unmarshal`)

## SQLite store internals

SQLite via `modernc.org/sqlite` (pure Go, no CGO dependency) stores Orka
payload/read-model data. ACP control authority is deliberately excluded:
controller epochs and Session mutation ownership use Kubernetes Leases, while
attempt, Session, branch, publication, and external-effect transitions use CRD
status `resourceVersion` CAS.

### Schema

Startup creates the complete current layout in one transaction for an empty
database. Production startup opens the store through `OpenLockedStore`, which holds the
exclusive controller lock, validates an existing database against the exact current
layout, and upgrades an exactly recognized older layout in place after taking a verified
backup beside the database. Unknown, partially modified, and newer layouts are refused
without changes. Reopening a current layout validates it without rewriting records or
changing their IDs and ordering. The lower-level `NewDB` constructor and in-memory stores
validate only and never migrate. The
[database support policy](../operations/upgrading.md#supported-database-layout) lists the
recognized layouts.

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
| `session_turns` / outbox tables | stable IDs | ACP transcript/SessionTurn payloads and deferred terminal projections committed behind Kubernetes epoch/Session fences |
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

### Session locking

Sessions use optimistic locking via an `active_task` column. `AcquireLock` atomically sets `active_task` only if it's currently empty. Tasks that fail to acquire the lock requeue every 5 seconds. The lock is released on task completion or deletion (via finalizer cleanup). There is no timeout — if the lock holder crashes, the lock persists until the task is deleted.

### Message broadcast scoping

Inter-agent broadcast messages (`to_task='*'`) are scoped by `parent_task`:

```sql
WHERE (to_task = ? OR (to_task = '*' AND parent_task = ?))
```

This ensures only sibling tasks (same parent coordinator) receive broadcasts. Senders don't receive their own broadcasts.

## Internal worker API

Workers authenticate these routes with their ServiceAccount token.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/internal/v1/results/:namespace/:taskName` | POST | Submit task result |
| `/internal/v1/artifacts/:namespace/:taskName/:filename` | POST | Upload task artifact |
| `/internal/v1/sessions/:namespace/:name/transcript` | GET | Get session transcript |
| `/internal/v1/plans/:namespace/:taskName` | POST | Save plan state |
| `/internal/v1/plans/:namespace/:taskName` | GET | Get plan state |
| `/internal/v1/messages/:namespace` | POST | Send inter-agent message |
| `/internal/v1/messages/:namespace/:taskName` | GET | Get messages for a task |

### Save plan state

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

### Get plan state

Workers call this to load the current plan state at startup.

**Endpoint:** `GET /internal/v1/plans/{namespace}/{taskName}`

**Response (200):** Same as public plan endpoint.

**Errors:**
- `404` — No plan found

### Send message

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

### Get messages

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
    "namespace": "orka-system",
    "fromTask": "worker-b",
    "toTask": "worker-a",
    "parentTask": "coordinator",
    "content": "Found a bug in the auth module",
    "read": false,
    "createdAt": "2026-01-15T10:30:00Z"
  }
]
```

## Dependencies

| Package | Purpose |
|---------|---------|
| `sigs.k8s.io/controller-runtime` | Controller framework |
| `k8s.io/client-go` | Kubernetes client |
| `github.com/gofiber/fiber/v3` | HTTP router |
| `github.com/anthropics/anthropic-sdk-go` | Anthropic Claude API |
| `github.com/openai/openai-go/v3` | OpenAI API (official SDK) |
| `github.com/github/copilot-sdk/go` | GitHub Copilot integration used by the built-in Copilot ACP RuntimePool profile |
| `modernc.org/sqlite` | Embedded SQLite (pure Go, no CGO) |

