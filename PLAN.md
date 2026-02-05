# Mercan - Kubernetes-Native Task Execution Platform

A Kubernetes-native task execution system inspired by OpenClaw, where a controller manages Jobs and Pods for incoming task requests.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         HTTP Clients                             │
│                    (CLI, curl, other apps)                       │
└───────────────────────────┬─────────────────────────────────────┘
                            │ REST API
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                     Mercan Controller                            │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │  API Server │  │ Reconciler  │  │   Task Queue Manager    │  │
│  │   (Fiber)   │  │  (ctrl-rt)  │  │                         │  │
│  └──────┬──────┘  └──────┬──────┘  └───────────┬─────────────┘  │
│         │                │                      │                │
│         └────────────────┼──────────────────────┘                │
│                          │                                       │
└──────────────────────────┼───────────────────────────────────────┘
                           │ Creates/Manages
                           ▼
┌─────────────────────────────────────────────────────────────────┐
│                    Kubernetes Cluster                            │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐           │
│  │  Task CRD    │  │    Jobs      │  │    Pods      │           │
│  │  (optional)  │  │              │  │  (workers)   │           │
│  └──────────────┘  └──────────────┘  └──────────────┘           │
└─────────────────────────────────────────────────────────────────┘
```

## Design Decisions

| Area | Decision | Rationale |
|------|----------|-----------|
| **Result Storage** | ConfigMap | Simple, no extra infrastructure. Limited to 1MB per result. Sufficient for text/JSON outputs. |
| **Session Storage** | ConfigMap | Conversation history stored in ConfigMap for multi-turn continuity. Tasks reference sessions via `sessionRef`. Limited to ~1MB (~50-100 messages). |
| **API Authentication** | Kubernetes ServiceAccount tokens | Native K8s auth with RBAC integration. Clients use bearer tokens. |
| **Namespace Scope** | Configurable per-deployment | Flag to choose cluster-scoped or namespace-scoped at deployment time. |
| **Task Queue Manager** | Priority queuing | Support task priorities (0-1000). Higher priority tasks are scheduled first. |
| **Secret Management** | Reference K8s Secrets in Task spec | Task spec includes `secretRef` field. Controller mounts secrets to worker pods. |
| **Observability** | Prometheus metrics + structured logs | Standard K8s observability stack. JSON logs with correlation IDs. |
| **AI Tools** | Built-in + extensible via CRDs | Ship with common tools: `web_search`, `code_exec`, `file_read`. Extend via Skill (prompt injection) and Tool (HTTP execution) CRDs. |
| **Failure Policy** | Configurable retry with backoff | `spec.retryPolicy` field with max retries and exponential backoff. Default: no retries. |
| **Result Delivery** | Polling + webhooks | Clients poll `GET /tasks/{id}`. Optional `webhookURL` in spec for push notification. |
| **Worker Runtime** | Standard containers (secure) | Non-root user, read-only root filesystem, dropped capabilities by default. |
| **Session Execution** | Serial execution per session | Tasks sharing a session run one-at-a-time to prevent race conditions. No parallel option—use separate sessions for parallel workloads. |

## Core Components

### 1. Controller (cmd/controller/)
- **API Server**: REST endpoints for task CRUD operations
- **Reconciler**: Watches Task resources, creates/manages Jobs
- **Status Manager**: Tracks task lifecycle and results
- **Session Manager**: Manages session ConfigMaps for conversation continuity

### 2. Task Custom Resource Definition (api/v1alpha1/)
Even with REST API as primary input, using CRDs provides:
- Kubernetes-native storage and lifecycle
- Built-in watch/reconciliation patterns
- Easy debugging via kubectl
- Finalizers for cleanup (result ConfigMaps, session locks)

### 3. Worker Images (workers/)
- **AI Worker**: Runs LLM agent tasks with tool execution
- **General Worker**: Runs arbitrary container commands

## Project Structure

```
mercan/
├── api/
│   └── v1alpha1/
│       ├── task_types.go         # Task CRD definition
│       ├── groupversion_info.go
│       └── zz_generated.deepcopy.go
├── cmd/
│   └── controller/
│       └── main.go               # Controller entrypoint (--watch-namespace flag)
├── internal/
│   ├── api/
│   │   ├── server.go             # REST API server
│   │   ├── handlers.go           # HTTP handlers
│   │   ├── auth.go               # ServiceAccount token validation
│   │   ├── middleware.go         # Logging, metrics, validation
│   │   └── pagination.go         # Pagination helpers
│   ├── controller/
│   │   ├── task_controller.go    # Task reconciler
│   │   ├── job_builder.go        # Job/Pod spec builder with security
│   │   ├── priority_queue.go     # Priority-based scheduling
│   │   ├── result_collector.go   # ConfigMap result management
│   │   ├── session_manager.go    # Session ConfigMap management
│   │   └── webhook.go            # Webhook notification delivery
│   ├── llm/
│   │   ├── provider.go           # LLM provider interface
│   │   ├── anthropic/
│   │   │   └── provider.go       # Anthropic implementation
│   │   └── openai/
│   │       └── provider.go       # OpenAI implementation
│   ├── tools/
│   │   ├── registry.go           # Tool registry
│   │   ├── web_search.go         # Built-in web search
│   │   ├── code_exec.go          # Built-in code execution
│   │   └── file_read.go          # Built-in file read
│   ├── metrics/
│   │   └── metrics.go            # Prometheus metrics definitions
│   └── worker/
│       ├── executor.go           # Task execution logic
│       └── ai_agent.go           # AI agent runtime
├── config/
│   ├── crd/
│   │   └── bases/                # Generated CRD manifests
│   ├── rbac/
│   │   ├── role.yaml             # Controller RBAC
│   │   └── client_role.yaml      # API client RBAC
│   └── manager/                  # Controller deployment
├── charts/
│   └── mercan/
│       ├── Chart.yaml
│       ├── values.yaml           # Configurable: namespaceScope, replicas, etc.
│       └── templates/
│           ├── deployment.yaml
│           ├── service.yaml
│           ├── serviceaccount.yaml
│           └── rbac.yaml
├── workers/
│   ├── ai/
│   │   ├── Dockerfile
│   │   └── main.go               # AI worker entrypoint
│   └── general/
│       ├── Dockerfile
│       └── main.go               # General worker entrypoint
├── Dockerfile                    # Controller image
├── Makefile
├── go.mod
└── go.sum
```

## Implementation Plan

### Phase 1: Project Setup & CRD Definition
1. Initialize Go module with kubebuilder
   ```bash
   kubebuilder init --domain mercan.ai --repo github.com/sozercan/mercan
   kubebuilder create api --group core --version v1alpha1 --kind Task
   ```
2. Define Task CRD with all fields:
   - `spec.type`: "ai" | "container"
   - `spec.image`: Container image for task
   - `spec.command`: Command to run
   - `spec.env`: Environment variables
   - `spec.timeout`: Task timeout
   - `spec.priority`: Queue priority (0-1000)
   - `spec.retryPolicy`: Retry configuration
   - `spec.webhookURL`: Completion notification URL
   - `spec.secretRef`: Reference to K8s Secret for credentials
   - `spec.sessionRef`: Reference to session ConfigMap for conversation continuity
   - `spec.ai`: AI-specific config (model, prompt, tools, skills)
   - `status.phase`: Pending/Running/Succeeded/Failed
   - `status.resultRef`: Reference to ConfigMap with result
   - `status.attempts`: Number of attempts made
   - `status.webhookDelivered`: Webhook delivery status
3. Generate CRD manifests and Go types
4. Create RBAC manifests for controller and API clients

### Phase 2: Controller Implementation
1. Implement Task reconciler:
   - Watch Task resources (configurable namespace scope via `--watch-namespace` flag)
   - Priority queue for task scheduling
   - Create Kubernetes Job for each Task
   - Apply secure pod security context to all workers
   - Monitor Job completion
   - Handle retries with exponential backoff
   - Update Task status with results
   - **Finalizer handling**: Add `mercan.ai/cleanup` finalizer to ensure:
     - Result ConfigMaps are deleted when Task is deleted
     - Session locks are released if Task is deleted mid-execution
     - Associated Jobs are cleaned up
2. Implement Job builder:
   - Build Job spec from Task spec
   - Configure resource limits
   - Mount secrets from `secretRef`
   - Set up result collection via ConfigMap
3. Implement webhook notifier:
   - Call `webhookURL` on task completion
   - Retry failed webhook deliveries
   - Update `status.webhookDelivered`
4. Implement session manager:
   - Create session ConfigMap if `sessionRef.create=true`
   - **Enforce serial execution**: Track active task per session, block new tasks until current completes
   - Load session transcript and inject into worker Pod
   - Append task messages to session after completion
   - Track token counts in ConfigMap annotations
   - Release session lock on task completion/failure

### Phase 2.5: Skills System
1. Add `skills` field to Task spec (`[]ConfigMapRef`)
2. Implement skill loader in controller:
   - Resolve ConfigMaps from `skills[].configMapRef`
   - Read and concatenate `skill.md` content
   - Inject into system prompt for AI tasks
3. Pass skill content to worker Pod via mounted ConfigMap or environment

### Phase 3: REST API Server
1. Implement ServiceAccount token authentication
   - Validate bearer tokens against Kubernetes API
   - Extract user info for audit logging
2. Implement endpoints:
   - `POST /api/v1/tasks` - Create task
   - `GET /api/v1/tasks` - List tasks (supports `?namespace=`, `?limit=`, `?continue=`)
   - `GET /api/v1/tasks/{id}` - Get task details
   - `DELETE /api/v1/tasks/{id}` - Cancel/delete task
   - `GET /api/v1/tasks/{id}/logs` - Stream task logs
   - `GET /api/v1/tasks/{id}/result` - Get task result from ConfigMap
   - `GET /api/v1/sessions` - List sessions (supports `?namespace=`, `?limit=`, `?continue=`)
   - `GET /api/v1/sessions/{id}` - Get session transcript
   - `DELETE /api/v1/sessions/{id}` - Delete/reset session
   - `GET /api/v1/tools` - List available tools (supports `?namespace=`, `?limit=`, `?continue=`)
   - `GET /api/v1/tools/{name}` - Get tool details
   - `GET /api/v1/agents` - List available agents (supports `?namespace=`, `?limit=`, `?continue=`)
   - `GET /api/v1/agents/{name}` - Get agent details
3. Implement pagination for list endpoints:
   - `?limit=N` - Maximum items to return (default: 100, max: 500)
   - `?continue=TOKEN` - Opaque continuation token for next page
   - Response includes `metadata.continue` if more results exist
   - Uses Kubernetes-style continuation tokens (base64-encoded resource version + name)
4. Add middleware:
   - Structured JSON logging with correlation IDs
   - Prometheus metrics (request count, latency)
   - Request validation

### Phase 4: Worker Images
1. **General Worker**:
   - Execute commands as non-root user
   - Capture stdout/stderr
   - Write results to ConfigMap (via K8s API or shared volume)
   - Handle timeouts gracefully
2. **AI Worker**:
   - Initialize LLM client (Anthropic/OpenAI) using mounted secrets
   - Execute agent loop with built-in tools:
     - `web_search`: Search the web
     - `code_exec`: Execute code in sandbox
     - `file_read`: Read files from workspace
   - Write results and conversation history to ConfigMap

### Phase 4.5: Tool CRD System
1. Define Tool CRD (`api/v1alpha1/tool_types.go`):
   - `spec.description`: Tool description for LLM
   - `spec.parameters`: JSON Schema for parameters
   - `spec.http`: HTTP execution config (url, method, headers, timeout, authSecretRef)
   - `status.available`: Health check result
2. Generate CRD manifests with kubebuilder
3. Implement Tool controller:
   - Optional periodic health checks for tool endpoints
   - Update `status.available` based on endpoint reachability
4. Update AI worker:
   - Discover Tool CRDs in namespace at startup
   - Build tool schema for LLM from Tool specs
   - Implement HTTP tool executor with auth injection
   - Handle timeouts and error responses

### Phase 5: Observability
1. Add Prometheus metrics:
   - Task counters (total, active, by phase)
   - Duration histograms
   - Queue depth gauges
   - API request metrics
2. Configure structured logging:
   - JSON format with correlation IDs
   - Log levels configurable via flag
3. Add `/healthz` and `/readyz` endpoints

### Phase 6: Testing & Deployment
1. Write unit tests for controller logic
2. Write integration tests using envtest
3. Create Helm chart for deployment
   - Configurable namespace scope
   - RBAC templates
   - ServiceAccount for API clients
4. Add example manifests and documentation

## Key Files to Create

| File | Purpose |
|------|---------|
| `api/v1alpha1/task_types.go` | Task CRD Go types |
| `api/v1alpha1/tool_types.go` | Tool CRD Go types |
| `internal/controller/task_controller.go` | Main reconciliation logic with finalizer handling |
| `internal/controller/tool_controller.go` | Tool health check reconciler |
| `internal/controller/skill_loader.go` | Load skills from ConfigMaps |
| `internal/controller/job_builder.go` | Build Job specs with security context |
| `internal/controller/priority_queue.go` | Priority-based task scheduling |
| `internal/controller/webhook.go` | Webhook notification delivery |
| `internal/controller/session_manager.go` | Session ConfigMap CRUD, serial execution lock, and context injection |
| `internal/api/server.go` | REST API server setup |
| `internal/api/handlers.go` | HTTP request handlers |
| `internal/api/auth.go` | ServiceAccount token validation |
| `internal/api/middleware.go` | Logging, metrics, validation middleware |
| `internal/api/pagination.go` | Pagination helpers (limit, continue token encoding/decoding) |
| `internal/llm/provider.go` | LLM provider interface |
| `internal/llm/anthropic/provider.go` | Anthropic implementation |
| `internal/llm/openai/provider.go` | OpenAI implementation |
| `internal/tools/web_search.go` | Built-in web search tool |
| `internal/tools/code_exec.go` | Built-in code execution tool |
| `internal/tools/file_read.go` | Built-in file read tool |
| `internal/worker/tool_executor.go` | HTTP tool execution for custom Tools |
| `workers/ai/main.go` | AI worker entrypoint |
| `workers/general/main.go` | General worker entrypoint |
| `Makefile` | Build, test, deploy commands |
| `charts/mercan/` | Helm chart for deployment |

## Task CRD Schema (Preview)

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: my-task
spec:
  type: container  # or "ai"
  image: python:3.11
  command: ["python", "-c", "print('Hello')"]
  env:
    - name: MY_VAR
      value: "my-value"
  timeout: 300s

  # Priority for queue ordering (0-1000, higher = more urgent)
  priority: 500

  # Retry policy for failed tasks
  retryPolicy:
    maxRetries: 3           # Default: 0 (no retries)
    backoffMultiplier: 2    # Exponential backoff multiplier
    initialDelay: 10s       # Initial delay before first retry

  # Webhook for completion notification
  webhookURL: "https://example.com/webhook"

  # Secret reference for credentials (API keys, etc.)
  secretRef:
    name: my-api-keys       # Name of K8s Secret
    namespace: default      # Optional, defaults to Task namespace

  # Session reference for conversation continuity
  sessionRef:
    name: user-alice-main   # Session identifier (ConfigMap: session-<name>)
    create: true            # Create session if it doesn't exist
    append: true            # Append task messages to session transcript
    maxMessages: 50         # Load only last N messages (default: 50)

  resources:
    limits:
      cpu: "1"
      memory: "512Mi"

  # AI-specific (when type: ai)
  ai:
    provider: anthropic     # or "openai"
    model: claude-sonnet-4-20250514
    prompt: "Analyze this data..."

    # Skills: prompt injections from ConfigMaps
    skills:
      - configMapRef:
          name: sql-safety        # ConfigMap with skill.md
      - configMapRef:
          name: careful-coder

    # Tools: built-in + custom Tool CRDs
    tools:
      - web_search              # Built-in
      - code_exec               # Built-in
      - file_read               # Built-in
      - jira-create             # Custom Tool CRD (same namespace)
      - slack-post              # Custom Tool CRD (same namespace)

status:
  phase: Succeeded          # Pending | Running | Succeeded | Failed
  startTime: "2026-02-04T10:00:00Z"
  completionTime: "2026-02-04T10:00:30Z"
  attempts: 1               # Number of attempts made
  jobName: my-task-job-abc123
  resultRef:
    configMapName: my-task-result  # ConfigMap containing result
    key: result
  webhookDelivered: true    # Whether webhook was successfully called
  conditions:
    - type: Complete
      status: "True"
      lastTransitionTime: "2026-02-04T10:00:30Z"
      reason: JobSucceeded
      message: "Task completed successfully"
```

## REST API Examples

```bash
# Get authentication token
TOKEN=$(kubectl create token mercan-client -n default)

# Create a container task with priority and webhook
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "my-task",
    "namespace": "default",
    "type": "container",
    "image": "python:3.11",
    "command": ["python", "-c", "print(\"Hello\")"],
    "priority": 500,
    "timeout": "300s",
    "webhookURL": "https://example.com/webhook",
    "retryPolicy": {
      "maxRetries": 2,
      "initialDelay": "10s"
    }
  }'

# Create an AI task with secret reference
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "ai-task",
    "namespace": "default",
    "type": "ai",
    "priority": 800,
    "secretRef": {
      "name": "llm-api-keys"
    },
    "ai": {
      "provider": "anthropic",
      "model": "claude-sonnet-4-20250514",
      "prompt": "Write a haiku about Kubernetes",
      "tools": [
        {"name": "web_search", "enabled": true}
      ]
    }
  }'

# Get task status
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/tasks/my-task

# Get task result (from ConfigMap)
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/tasks/my-task/result

# Stream logs
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/tasks/my-task/logs

# List tasks (with namespace filter and pagination)
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/tasks?namespace=default&limit=10"

# Get next page using continue token
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/tasks?namespace=default&limit=10&continue=eyJ2IjoiMTIzNCIsIm4iOiJ0YXNrLXh5eiJ9"

# List available tools
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/tools?namespace=default"

# List available agents
curl -H "Authorization: Bearer $TOKEN" \
  "http://localhost:8080/api/v1/agents?namespace=default"

# Example paginated response:
# {
#   "items": [...],
#   "metadata": {
#     "continue": "eyJ2IjoiMTIzNCIsIm4iOiJ0YXNrLXh5eiJ9",
#     "remainingItemCount": 42
#   }
# }

# Create an AI task with session for multi-turn conversation
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "chat-task",
    "namespace": "default",
    "type": "ai",
    "sessionRef": {
      "name": "user-session-main",
      "create": true,
      "append": true,
      "maxMessages": 50
    },
    "ai": {
      "provider": "anthropic",
      "model": "claude-sonnet-4-20250514",
      "prompt": "Remember this: my favorite color is blue"
    }
  }'
```

## Tooling

- **Kubebuilder** - Controller scaffolding and code generation
- Initialize with: `kubebuilder init --domain mercan.ai --repo github.com/sozercan/mercan`
- Create API with: `kubebuilder create api --group core --version v1alpha1 --kind Task`

## Dependencies

- `sigs.k8s.io/controller-runtime` - Controller framework
- `k8s.io/client-go` - Kubernetes client
- `github.com/gofiber/fiber/v3` - HTTP router (high-performance, Express-inspired)
- `github.com/anthropics/anthropic-sdk-go` - Anthropic Claude API
- `github.com/sashabaranov/go-openai` - OpenAI API

## LLM Provider Architecture

The AI worker uses a pluggable provider interface:

```go
// internal/llm/provider.go
type Provider interface {
    Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error)
    Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error)
    Name() string
}

// Implementations:
// - internal/llm/anthropic/provider.go
// - internal/llm/openai/provider.go
```

Provider selection via Task spec:
```yaml
spec:
  ai:
    provider: anthropic  # or "openai"
    model: claude-sonnet-4-20250514
```

## Observability

### Prometheus Metrics

The controller exposes metrics on `:8081/metrics`:

| Metric | Type | Description |
|--------|------|-------------|
| `mercan_tasks_total` | Counter | Total tasks by type, phase, namespace |
| `mercan_tasks_active` | Gauge | Currently running tasks |
| `mercan_task_duration_seconds` | Histogram | Task execution duration |
| `mercan_task_queue_depth` | Gauge | Tasks waiting by priority level |
| `mercan_task_retries_total` | Counter | Total retry attempts |
| `mercan_webhook_deliveries_total` | Counter | Webhook delivery attempts by status |
| `mercan_api_requests_total` | Counter | API requests by endpoint, method, status |
| `mercan_api_request_duration_seconds` | Histogram | API request latency |
| `mercan_sessions_total` | Gauge | Total active sessions by namespace |
| `mercan_session_messages_total` | Counter | Messages appended to sessions |
| `mercan_session_queue_waiting` | Gauge | Tasks waiting for session lock by session |

### Structured Logging

JSON logs with correlation IDs for request tracing:

```json
{
  "level": "info",
  "ts": "2026-02-04T10:00:00Z",
  "logger": "task-controller",
  "msg": "Task reconciled",
  "task": "my-task",
  "namespace": "default",
  "phase": "Running",
  "correlationId": "abc-123-def"
}
```

## Security

### Worker Pod Security

All worker pods run with secure defaults:

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 1000
  runAsGroup: 1000
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop:
      - ALL
  seccompProfile:
    type: RuntimeDefault
```

### API Authentication

REST API endpoints require Kubernetes ServiceAccount tokens:

```bash
# Get token for service account
TOKEN=$(kubectl create token mercan-client -n default)

# Use token in requests
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/tasks
```

RBAC rules control which service accounts can create/view/delete tasks.

### Secret Handling

- API keys referenced via `secretRef` in Task spec
- Controller mounts secrets as read-only volumes in worker pods
- Secrets are never logged or included in status/results

## Session Management

Sessions provide conversation continuity across multiple Tasks. Each session is stored as a ConfigMap containing the conversation transcript.

**Serial Execution**: Tasks sharing a session execute one-at-a-time. When a task references a session that has an in-progress task, it remains in `Pending` state until the active task completes. This prevents race conditions where concurrent tasks might read stale state or overwrite each other's changes. If you need parallel execution, use separate sessions.

### Session ConfigMap Structure

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: session-user-alice-main
  namespace: default
  labels:
    mercan.ai/session: "true"
    mercan.ai/user: alice
  annotations:
    mercan.ai/created-at: "2026-02-04T10:00:00Z"
    mercan.ai/updated-at: "2026-02-04T10:05:00Z"
    mercan.ai/message-count: "47"
    mercan.ai/input-tokens: "12000"
    mercan.ai/output-tokens: "8500"
    mercan.ai/active-task: ""           # Set to task name when locked, empty when free
data:
  transcript.jsonl: |
    {"role":"user","content":"Analyze my sales data","ts":"2026-02-04T10:00:00Z"}
    {"role":"assistant","content":"I'll analyze the Q4 sales...","ts":"2026-02-04T10:00:05Z"}
    {"role":"tool_use","name":"web_search","input":{"query":"Q3 benchmarks"}}
    {"role":"tool_result","name":"web_search","content":"..."}
    {"role":"assistant","content":"Based on the data...","ts":"2026-02-04T10:00:10Z"}
```

### Session Lifecycle

1. **Creation**: When a Task references `sessionRef.name` with `create: true`, controller creates the ConfigMap if missing
2. **Loading**: Controller reads transcript, applies `maxMessages` limit, mounts to worker Pod
3. **Execution**: Worker loads context, executes task, writes result
4. **Append**: Controller appends new messages (user prompt + assistant response + tool calls) to transcript
5. **Cleanup**: Sessions can be deleted via API or manually via kubectl

### Controller Flow

```
Task with sessionRef
        │
        ▼
┌───────────────────────┐
│ Check session lock    │
│ (another task active?)│
└───────────┬───────────┘
            │
      ┌─────┴─────┐
      │           │
   locked      unlocked
      │           │
      ▼           ▼
┌───────────┐  ┌───────────────────────┐
│ Stay in   │  │ Acquire session lock  │
│ Pending   │  │ (set active task)     │
└───────────┘  └───────────┬───────────┘
                           │
                           ▼
                ┌───────────────────────┐
                │ Load Session ConfigMap │
                │ (session-<name>)       │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Apply maxMessages     │
                │ (tail last N)         │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Mount to Worker Pod   │
                │ (/session/transcript) │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Worker executes with  │
                │ conversation context  │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Append new messages   │
                │ Update annotations    │
                └───────────┬───────────┘
                            │
                            ▼
                ┌───────────────────────┐
                │ Release session lock  │
                │ (next task can start) │
                └───────────────────────┘
```

### Session API Examples

```bash
# Create a task with session (creates session if missing)
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "chat-1",
    "type": "ai",
    "sessionRef": {
      "name": "user-alice-main",
      "create": true,
      "append": true
    },
    "ai": {
      "provider": "anthropic",
      "prompt": "What is the weather today?"
    }
  }'

# Follow-up task in same session (has context)
curl -X POST http://localhost:8080/api/v1/tasks \
  -H "Authorization: Bearer $TOKEN" \
  -d '{
    "name": "chat-2",
    "type": "ai",
    "sessionRef": {
      "name": "user-alice-main"
    },
    "ai": {
      "provider": "anthropic",
      "prompt": "How about tomorrow?"
    }
  }'

# List sessions
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/sessions

# Get session transcript
curl -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/sessions/user-alice-main

# Delete/reset session
curl -X DELETE -H "Authorization: Bearer $TOKEN" \
  http://localhost:8080/api/v1/sessions/user-alice-main
```

### Limitations (v1)

| Limitation | Description | Workaround |
|------------|-------------|------------|
| **1MB size limit** | ConfigMaps limited to ~1MB total | Use `maxMessages` to limit loaded context; delete old sessions |
| **~50-100 messages max** | Depending on message size, fits 50-100 messages | Start new session for long conversations |
| **No automatic compaction** | Old messages not summarized automatically | Manually delete session to reset; future: add compaction support |
| **No cross-namespace sessions** | Sessions scoped to Task namespace | Create sessions in each namespace as needed |
| **No session sharing** | Each session belongs to one user/context | Use separate sessions per user |
| **No token-based truncation** | `maxMessages` is count-based, not token-based | Set conservative `maxMessages` value |
| **Append-only** | Cannot edit/delete individual messages | Delete entire session to reset |
| **No TTL/expiry** | Sessions persist until manually deleted | Future: add `sessionRef.ttl` for auto-cleanup |

### Future Enhancements

- **PVC storage**: For sessions >1MB, spill to PersistentVolumeClaim
- **Compaction**: Summarize old messages to save space
- **Token-based limits**: `maxTokens` instead of `maxMessages`
- **Session TTL**: Auto-expire sessions after inactivity
- **Chunked ConfigMaps**: Split large sessions across multiple ConfigMaps

## Skills & Tools System

Mercan supports extensible AI capabilities through a two-layer system:

1. **Skills** (ConfigMap-based): Prompt injections that teach the model how to behave
2. **Tools** (CRD-based): New capabilities with HTTP execution

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 1: Skills (ConfigMaps)                                   │
│  - Prompt injection only                                        │
│  - Zero execution overhead                                      │
│  - Teaching/guidance for built-in tools                         │
├─────────────────────────────────────────────────────────────────┤
│  Layer 2: Built-in Tools (in worker image)                      │
│  - web_search, file_read, code_exec                             │
│  - Fast, no extra infrastructure                                │
├─────────────────────────────────────────────────────────────────┤
│  Layer 3: Custom Tools (Tool CRD + HTTP)                        │
│  - Point at internal services                                   │
│  - Namespace-scoped, RBAC-controlled                            │
└─────────────────────────────────────────────────────────────────┘
```

### Layer 1: Skills (Prompt Injection)

Skills are ConfigMaps that inject instructions into the system prompt. They teach the model *how* to use tools or behave in certain contexts.

#### Skill ConfigMap Structure

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: sql-safety
  namespace: team-a
  labels:
    mercan.ai/skill: "true"
data:
  skill.md: |
    # SQL Safety Guidelines

    When working with databases:
    - Always use parameterized queries to prevent SQL injection
    - Never run DROP, DELETE, or TRUNCATE without explicit user confirmation
    - Explain the query before executing it
    - Limit SELECT queries to 1000 rows unless specified otherwise
    - Always wrap multiple statements in transactions
```

#### Referencing Skills in Tasks

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: db-analysis
spec:
  type: ai
  ai:
    provider: anthropic
    model: claude-sonnet-4-20250514
    prompt: "Analyze the users table and find inactive accounts"
    skills:
      - configMapRef:
          name: sql-safety
      - configMapRef:
          name: careful-coder
```

#### Controller Behavior

1. Resolve all `skills[].configMapRef` in Task namespace
2. Read `skill.md` (or all keys) from each ConfigMap
3. Concatenate skill content into system prompt before user prompt
4. Mount nothing extra - pure prompt injection

### Layer 2: Built-in Tools

Shipped with the AI worker image. No configuration required.

| Tool | Description |
|------|-------------|
| `web_search` | Search the web via configurable search API |
| `code_exec` | Execute code in sandboxed environment |
| `file_read` | Read files from task workspace |

### Layer 3: Custom Tools (Tool CRD)

For adding *new capabilities* the model can invoke. Tools execute via HTTP calls to internal services.

#### Tool CRD Definition

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Tool
metadata:
  name: jira-create
  namespace: team-a
spec:
  # Description shown to the LLM
  description: "Create a Jira ticket in the team's project"

  # JSON Schema for parameters (OpenAI function calling format)
  parameters:
    type: object
    properties:
      project:
        type: string
        description: "Project key (e.g., TEAM)"
      title:
        type: string
        description: "Ticket title/summary"
      description:
        type: string
        description: "Detailed description"
      priority:
        type: string
        enum: ["low", "medium", "high", "critical"]
        description: "Ticket priority"
      labels:
        type: array
        items:
          type: string
        description: "Labels to apply"
    required: ["project", "title"]

  # HTTP execution configuration
  http:
    url: "http://jira-bridge.tools.svc.cluster.local/api/create"
    method: POST
    headers:
      Content-Type: "application/json"
    timeout: 30s

    # Optional: inject auth from Secret
    authSecretRef:
      name: jira-credentials
      key: api-token
      # Injected as: Authorization: Bearer <token>
```

#### Tool CRD Go Types

```go
// api/v1alpha1/tool_types.go

type Tool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   ToolSpec   `json:"spec,omitempty"`
    Status ToolStatus `json:"status,omitempty"`
}

type ToolSpec struct {
    // Description shown to the LLM when presenting available tools
    Description string `json:"description"`

    // JSON Schema for tool parameters (OpenAI function calling format)
    // +optional
    Parameters *apiextensionsv1.JSONSchemaProps `json:"parameters,omitempty"`

    // HTTP execution configuration
    HTTP HTTPExecution `json:"http"`
}

type HTTPExecution struct {
    // URL to call when tool is invoked
    URL string `json:"url"`

    // HTTP method (default: POST)
    // +kubebuilder:default=POST
    Method string `json:"method,omitempty"`

    // Additional headers to include
    // +optional
    Headers map[string]string `json:"headers,omitempty"`

    // Request timeout (default: 30s)
    // +kubebuilder:default="30s"
    Timeout metav1.Duration `json:"timeout,omitempty"`

    // Secret reference for authentication
    // Token is injected as Authorization: Bearer <token>
    // +optional
    AuthSecretRef *SecretKeySelector `json:"authSecretRef,omitempty"`
}

type SecretKeySelector struct {
    // Name of the Secret
    Name string `json:"name"`
    // Key within the Secret
    Key string `json:"key"`
}

type ToolStatus struct {
    // Whether the tool endpoint is reachable
    Available bool `json:"available"`

    // Last health check timestamp
    // +optional
    LastCheck *metav1.Time `json:"lastCheck,omitempty"`

    // Error message if unavailable
    // +optional
    Error string `json:"error,omitempty"`
}
```

#### Referencing Tools in Tasks

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: bug-triage
  namespace: team-a
spec:
  type: ai
  ai:
    provider: anthropic
    model: claude-sonnet-4-20250514
    prompt: "Analyze bug #123 and create a Jira ticket for the fix"
    tools:
      - web_search         # built-in
      - code_exec          # built-in
      - jira-create        # custom Tool CRD (same namespace)
      - slack-post         # custom Tool CRD (same namespace)
```

#### Worker Tool Execution Flow

```go
// internal/worker/tool_executor.go

func (w *Worker) ExecuteTool(ctx context.Context, name string, params map[string]any) (string, error) {
    // 1. Check built-ins first
    if builtin, ok := w.builtins[name]; ok {
        return builtin.Execute(ctx, params)
    }

    // 2. Look up Tool CRD in same namespace
    tool := &mercanv1.Tool{}
    err := w.client.Get(ctx, types.NamespacedName{
        Namespace: w.namespace,
        Name:      name,
    }, tool)
    if err != nil {
        return "", fmt.Errorf("unknown tool %q: %w", name, err)
    }

    // 3. Build HTTP request
    body, _ := json.Marshal(params)
    req, _ := http.NewRequestWithContext(ctx, tool.Spec.HTTP.Method, tool.Spec.HTTP.URL, bytes.NewReader(body))

    // Add configured headers
    for k, v := range tool.Spec.HTTP.Headers {
        req.Header.Set(k, v)
    }

    // Add auth if configured
    if ref := tool.Spec.HTTP.AuthSecretRef; ref != nil {
        token, err := w.getSecretKey(ctx, ref.Name, ref.Key)
        if err != nil {
            return "", fmt.Errorf("failed to get auth secret: %w", err)
        }
        req.Header.Set("Authorization", "Bearer "+token)
    }

    // 4. Execute with timeout
    client := &http.Client{Timeout: tool.Spec.HTTP.Timeout.Duration}
    resp, err := client.Do(req)
    if err != nil {
        return "", fmt.Errorf("tool request failed: %w", err)
    }
    defer resp.Body.Close()

    // 5. Return response
    result, _ := io.ReadAll(resp.Body)
    if resp.StatusCode >= 400 {
        return "", fmt.Errorf("tool returned %d: %s", resp.StatusCode, string(result))
    }
    return string(result), nil
}
```

### Tool Service Pattern

Platform teams deploy tool services that handle the actual integrations:

```
┌─────────────────────────────────────────────────────────────────┐
│                       tools namespace                            │
├─────────────────────────────────────────────────────────────────┤
│  ┌─────────────┐  ┌─────────────┐  ┌──────────────┐            │
│  │ jira-bridge │  │ slack-bridge│  │ github-bridge│            │
│  │  Deployment │  │  Deployment │  │  Deployment  │            │
│  └──────┬──────┘  └──────┬──────┘  └──────┬───────┘            │
│         │                │                │                     │
│         └────────────────┼────────────────┘                     │
│                          │                                      │
│                    ClusterIP Services                           │
│     jira-bridge.tools    slack-bridge.tools    github-bridge    │
└─────────────────────────────────────────────────────────────────┘
                                ▲
                                │ HTTP (in-cluster)
                                │
┌───────────────────────────────┴─────────────────────────────────┐
│                    Task Pod (any namespace)                      │
│                    AI Worker calls tools via HTTP                │
└─────────────────────────────────────────────────────────────────┘
```

#### Example Tool Service (jira-bridge)

```go
// Simple HTTP service that wraps Jira API

func main() {
    http.HandleFunc("/api/create", func(w http.ResponseWriter, r *http.Request) {
        var req struct {
            Project     string   `json:"project"`
            Title       string   `json:"title"`
            Description string   `json:"description"`
            Priority    string   `json:"priority"`
            Labels      []string `json:"labels"`
        }
        json.NewDecoder(r.Body).Decode(&req)

        // Call Jira API
        issue, err := jiraClient.CreateIssue(req.Project, req.Title, req.Description)
        if err != nil {
            http.Error(w, err.Error(), 500)
            return
        }

        json.NewEncoder(w).Encode(map[string]string{
            "key": issue.Key,
            "url": issue.Self,
        })
    })
    http.ListenAndServe(":8080", nil)
}
```

### Security & RBAC

#### Namespace Scoping

- Skills (ConfigMaps) must be in the same namespace as the Task
- Tools (CRDs) must be in the same namespace as the Task
- No cross-namespace tool access by default

#### RBAC for Tool CRDs

```yaml
# Allow team-a service accounts to read Tool CRDs
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: tool-reader
  namespace: team-a
rules:
  - apiGroups: ["mercan.ai"]
    resources: ["tools"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: workers-tool-reader
  namespace: team-a
subjects:
  - kind: ServiceAccount
    name: mercan-worker
roleRef:
  kind: Role
  name: tool-reader
  apiGroup: rbac.authorization.k8s.io
```

#### Tool Auth Secrets

Secrets referenced by `authSecretRef` must be in the same namespace as the Tool:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: jira-credentials
  namespace: team-a
type: Opaque
stringData:
  api-token: "your-jira-api-token"
```

### Project Structure Updates

```
mercan/
├── api/
│   └── v1alpha1/
│       ├── task_types.go
│       ├── tool_types.go          # NEW: Tool CRD definition
│       └── groupversion_info.go
├── internal/
│   ├── controller/
│   │   ├── task_controller.go
│   │   ├── tool_controller.go     # NEW: Tool health check reconciler
│   │   └── skill_loader.go        # NEW: Load skills from ConfigMaps
│   └── worker/
│       ├── executor.go
│       ├── ai_agent.go
│       └── tool_executor.go       # NEW: HTTP tool execution
├── config/
│   └── crd/
│       └── bases/
│           ├── mercan.ai_tasks.yaml
│           └── mercan.ai_tools.yaml  # NEW: Tool CRD manifest
```

### Implementation Plan Additions

#### Phase 2.5: Skills System (after Controller, before REST API)
1. Add `skills` field to Task spec (`[]ConfigMapRef`)
2. Implement skill loader in controller:
   - Resolve ConfigMaps from `skills[].configMapRef`
   - Read and concatenate `skill.md` content
   - Inject into system prompt before task execution
3. Mount skill content to worker Pod (or pass via env/configmap)

#### Phase 4.5: Tool CRD (after Worker Images)
1. Define Tool CRD (`api/v1alpha1/tool_types.go`)
2. Generate CRD manifests
3. Implement Tool controller:
   - Optional: periodic health checks for tool endpoints
   - Update `status.available` based on health
4. Update AI worker:
   - Discover Tool CRDs in namespace at startup
   - Build tool schema for LLM from Tool specs
   - Implement HTTP tool executor
   - Handle auth secret injection

### Example: Complete Workflow

**1. Platform team deploys tool services:**
```bash
kubectl apply -f tools/jira-bridge/
kubectl apply -f tools/slack-bridge/
```

**2. Team creates a Skill:**
```yaml
# team-a/skills/ticket-guidelines.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: ticket-guidelines
  namespace: team-a
  labels:
    mercan.ai/skill: "true"
data:
  skill.md: |
    When creating tickets:
    - Always include reproduction steps for bugs
    - Set priority based on user impact
    - Add "ai-created" label for tracking
```

**3. Team creates a Tool:**
```yaml
# team-a/tools/jira-create.yaml
apiVersion: mercan.ai/v1alpha1
kind: Tool
metadata:
  name: jira-create
  namespace: team-a
spec:
  description: "Create a Jira ticket"
  parameters:
    type: object
    properties:
      project: {type: string}
      title: {type: string}
      description: {type: string}
      priority: {type: string, enum: [low, medium, high, critical]}
    required: [project, title]
  http:
    url: "http://jira-bridge.tools.svc.cluster.local/api/create"
    authSecretRef:
      name: team-a-jira-token
      key: token
```

**4. User creates a Task:**
```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: triage-bug
  namespace: team-a
spec:
  type: ai
  ai:
    provider: anthropic
    model: claude-sonnet-4-20250514
    prompt: "Analyze the error logs and create a Jira ticket for the root cause"
    skills:
      - configMapRef:
          name: ticket-guidelines
    tools:
      - file_read
      - jira-create
```

### Prometheus Metrics (Skills & Tools)

| Metric | Type | Description |
|--------|------|-------------|
| `mercan_skills_loaded_total` | Counter | Skills loaded by namespace, name |
| `mercan_tools_discovered` | Gauge | Tool CRDs discovered per namespace |
| `mercan_tool_calls_total` | Counter | Tool invocations by tool name, status |
| `mercan_tool_call_duration_seconds` | Histogram | Tool HTTP call latency |
| `mercan_tool_health_status` | Gauge | Tool availability (1=available, 0=unavailable) |

### Limitations (v1)

| Limitation | Description | Workaround |
|------------|-------------|------------|
| **Namespace-scoped only** | Tools/Skills must be in Task namespace | Copy resources to each namespace |
| **HTTP only** | No gRPC, WebSocket, or other protocols | Wrap non-HTTP services with HTTP adapter |
| **No tool versioning** | Tool CRDs are not versioned | Use names like `jira-create-v2` |
| **No registry** | No ClawHub-style marketplace | Use GitOps to sync Tool CRDs |
| **No hot reload** | Tools discovered at Task start | Restart tasks to pick up new tools |
| **No MCP** | Model Context Protocol not supported | May add in future version |

### Future Enhancements

- **ClusterTool CRD**: Cluster-scoped tools shared across namespaces
- **Tool versioning**: `spec.version` field with compatibility checks
- **MCP support**: Connect to MCP servers for tool discovery
- **Tool templates**: Helm-style templating for tool parameters
- **Async tools**: Support for long-running tool operations with callbacks

## Multi-Agent System

Mercan supports multiple agents with distinct personas, tool sets, and configurations. Agents provide reusable configurations that Tasks can reference.

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────┐
│                         Incoming Request                         │
└───────────────────────────┬─────────────────────────────────────┘
                            │
                            ▼
┌─────────────────────────────────────────────────────────────────┐
│                      Routing Layer                               │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────┐  │
│  │   Labels    │  │  AgentRef   │  │   Default Agent         │  │
│  │  Matching   │  │  (explicit) │  │   (fallback)            │  │
│  └──────┬──────┘  └──────┬──────┘  └───────────┬─────────────┘  │
│         └────────────────┼─────────────────────┘                │
└──────────────────────────┼──────────────────────────────────────┘
                           │
              ┌────────────┼────────────┐
              ▼            ▼            ▼
        Agent: home   Agent: work   Agent: ops
        (CRD)         (CRD)         (CRD)
              │            │            │
              ▼            ▼            ▼
          Task Pod     Task Pod     Task Pod
```

### Agent CRD

Agents define reusable configurations for AI task execution.

#### Agent CRD Schema

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Agent
metadata:
  name: code-reviewer
  namespace: team-backend
spec:
  # Model configuration
  model:
    provider: anthropic           # or "openai"
    name: claude-sonnet-4-20250514
    temperature: 0.3
    maxTokens: 4096

  # System prompt (inline or from ConfigMap)
  systemPrompt:
    inline: "You are a code reviewer..."
    # OR
    configMapRef:
      name: code-reviewer-prompt
      key: prompt.txt

  # Default tools available to this agent
  tools:
    - name: code_exec
      enabled: true
    - name: web_search
      enabled: false
    - name: github-pr            # Custom Tool CRD

  # Default skills for this agent
  skills:
    - configMapRef:
        name: secure-coding-guidelines
    - configMapRef:
        name: code-review-checklist

  # Resource limits for tasks using this agent
  resources:
    limits:
      cpu: "2"
      memory: "4Gi"
    requests:
      cpu: "500m"
      memory: "1Gi"

  # Default secret for LLM API keys
  secretRef:
    name: llm-api-keys

  # Session defaults
  session:
    persistence: configmap       # configmap | pvc | none
    ttl: 24h                     # Auto-expire sessions
    maxMessages: 100

  # Rate limiting (optional)
  rateLimit:
    requestsPerMinute: 60
    tokensPerMinute: 100000

status:
  # Number of active tasks using this agent
  activeTasks: 3

  # Last used timestamp
  lastUsed: "2026-02-04T10:00:00Z"

  # Conditions
  conditions:
    - type: Ready
      status: "True"
      reason: ConfigValid
      message: "Agent configuration is valid"
```

#### Agent CRD Go Types

```go
// api/v1alpha1/agent_types.go

type Agent struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   AgentSpec   `json:"spec,omitempty"`
    Status AgentStatus `json:"status,omitempty"`
}

type AgentSpec struct {
    // Model configuration
    Model ModelConfig `json:"model"`

    // System prompt configuration
    // +optional
    SystemPrompt *PromptSource `json:"systemPrompt,omitempty"`

    // Default tools available to this agent
    // +optional
    Tools []ToolReference `json:"tools,omitempty"`

    // Default skills for this agent
    // +optional
    Skills []SkillReference `json:"skills,omitempty"`

    // Resource limits for tasks
    // +optional
    Resources corev1.ResourceRequirements `json:"resources,omitempty"`

    // Secret containing LLM API keys
    // +optional
    SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

    // Session defaults
    // +optional
    Session *SessionConfig `json:"session,omitempty"`

    // Rate limiting configuration
    // +optional
    RateLimit *RateLimitConfig `json:"rateLimit,omitempty"`
}

type ModelConfig struct {
    // LLM provider (anthropic, openai)
    Provider string `json:"provider"`

    // Model name/identifier
    Name string `json:"name"`

    // Temperature for generation
    // +optional
    // +kubebuilder:default=0.7
    Temperature *float32 `json:"temperature,omitempty"`

    // Maximum tokens to generate
    // +optional
    MaxTokens *int32 `json:"maxTokens,omitempty"`
}

type PromptSource struct {
    // Inline system prompt
    // +optional
    Inline string `json:"inline,omitempty"`

    // Reference to ConfigMap containing prompt
    // +optional
    ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`
}

type SessionConfig struct {
    // Storage backend: configmap, pvc, none
    // +kubebuilder:default=configmap
    Persistence string `json:"persistence,omitempty"`

    // Session TTL (auto-expire)
    // +optional
    TTL *metav1.Duration `json:"ttl,omitempty"`

    // Maximum messages to load
    // +kubebuilder:default=50
    MaxMessages int32 `json:"maxMessages,omitempty"`
}

type RateLimitConfig struct {
    // Maximum requests per minute
    // +optional
    RequestsPerMinute *int32 `json:"requestsPerMinute,omitempty"`

    // Maximum tokens per minute
    // +optional
    TokensPerMinute *int64 `json:"tokensPerMinute,omitempty"`
}

type AgentStatus struct {
    // Number of active tasks using this agent
    ActiveTasks int32 `json:"activeTasks"`

    // Last time agent was used
    // +optional
    LastUsed *metav1.Time `json:"lastUsed,omitempty"`

    // Standard conditions
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}
```

### Referencing Agents in Tasks

Tasks can reference an Agent to inherit its configuration:

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: review-pr-123
  namespace: team-backend
spec:
  # Reference to Agent CRD
  agentRef:
    name: code-reviewer

  # Task-specific prompt (required)
  prompt: "Review PR #123 for security issues and performance"

  # Override agent defaults (optional)
  ai:
    # Add extra tools for this task only
    tools:
      - github-comments         # Additional tool

    # Add extra skills for this task only
    skills:
      - configMapRef:
          name: security-checklist
```

#### Configuration Merging

When a Task references an Agent, configurations are merged:

| Field | Behavior |
|-------|----------|
| `model` | Agent config used (Task cannot override) |
| `systemPrompt` | Agent prompt used as base |
| `tools` | Merged (Agent defaults + Task additions) |
| `skills` | Merged (Agent defaults + Task additions) |
| `resources` | Agent defaults, Task can override |
| `secretRef` | Agent default, Task can override |
| `session` | Agent defaults, Task can override |

### Routing

Route tasks to agents based on labels without explicit `agentRef`.

#### Label-Based Routing

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: my-task
  labels:
    mercan.ai/agent: "code-reviewer"    # Explicit agent selection
    mercan.ai/channel: "slack"          # Source channel
    mercan.ai/user: "alice"             # Requesting user
spec:
  prompt: "Review this code..."
  # No agentRef - controller uses labels to find agent
```

#### Routing Rules ConfigMap

Configure routing rules for automatic agent selection:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: mercan-routing-rules
  namespace: mercan-system
data:
  rules.yaml: |
    # Routing rules (evaluated in order, first match wins)
    rules:
      # Route by channel
      - match:
          labels:
            mercan.ai/channel: "slack"
            mercan.ai/workspace: "engineering"
        agent:
          name: engineering-assistant
          namespace: engineering

      # Route by user
      - match:
          labels:
            mercan.ai/user: "alice"
        agent:
          name: alice-personal
          namespace: users

      # Route by content type (set by API/client)
      - match:
          labels:
            mercan.ai/task-type: "code-review"
        agent:
          name: code-reviewer
          namespace: shared-agents

    # Default agent if no rules match
    default:
      name: general-assistant
      namespace: default
```

#### Controller Routing Logic

```go
// internal/controller/router.go

func (r *Router) ResolveAgent(ctx context.Context, task *mercanv1.Task) (*mercanv1.Agent, error) {
    // 1. Explicit agentRef takes precedence
    if task.Spec.AgentRef != nil {
        return r.getAgent(ctx, task.Spec.AgentRef.Name, task.Namespace)
    }

    // 2. Check routing rules
    rules, err := r.loadRoutingRules(ctx)
    if err != nil {
        return nil, err
    }

    for _, rule := range rules.Rules {
        if r.matchesLabels(task.Labels, rule.Match.Labels) {
            ns := rule.Agent.Namespace
            if ns == "" {
                ns = task.Namespace
            }
            return r.getAgent(ctx, rule.Agent.Name, ns)
        }
    }

    // 3. Fall back to default agent
    if rules.Default != nil {
        return r.getAgent(ctx, rules.Default.Name, rules.Default.Namespace)
    }

    // 4. No agent - use inline task config
    return nil, nil
}
```

### Agent-to-Agent Communication

Enable agents to delegate work to other specialized agents.

#### Coordination Model

```
┌─────────────────────────────────────────────────────────────────┐
│                    Coordinator Task                              │
│                    (Agent: coordinator)                          │
│                                                                  │
│  "Analyze codebase for security and performance"                │
└───────────────────────────┬─────────────────────────────────────┘
                            │ delegate_to_agent tool
              ┌─────────────┼─────────────────┐
              ▼             ▼                 ▼
        Child Task    Child Task        Child Task
        (security)    (performance)     (summary)
              │             │                 │
              ▼             ▼                 ▼
         Agent:        Agent:            Agent:
         security-     perf-             coordinator
         reviewer      analyzer          (self)
```

#### Enabling Coordination

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Agent
metadata:
  name: coordinator
spec:
  model:
    provider: anthropic
    name: claude-sonnet-4-20250514

  # Enable agent-to-agent delegation
  coordination:
    enabled: true
    # Agents this agent can delegate to
    allowedAgents:
      - name: security-reviewer
      - name: performance-analyzer
      - name: documentation-writer
    # Maximum concurrent child tasks
    maxConcurrentChildren: 5
    # Maximum delegation depth (prevent infinite loops)
    maxDepth: 3

  tools:
    - delegate_to_agent       # Built-in delegation tool
```

#### Delegation Tool Schema

```json
{
  "name": "delegate_to_agent",
  "description": "Delegate a subtask to another specialized agent and wait for the result",
  "parameters": {
    "type": "object",
    "properties": {
      "agent": {
        "type": "string",
        "description": "Name of the agent to delegate to (must be in allowedAgents)"
      },
      "prompt": {
        "type": "string",
        "description": "The task/prompt for the delegated agent"
      },
      "waitForResult": {
        "type": "boolean",
        "default": true,
        "description": "Whether to wait for the result before continuing"
      },
      "timeout": {
        "type": "string",
        "default": "5m",
        "description": "Timeout for the delegated task"
      }
    },
    "required": ["agent", "prompt"]
  }
}
```

#### Child Task Creation

When `delegate_to_agent` is called, the controller creates a child Task:

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: coordinator-task-child-1
  namespace: team-a
  labels:
    mercan.ai/parent-task: coordinator-task
    mercan.ai/delegation-depth: "1"
  ownerReferences:
    - apiVersion: mercan.ai/v1alpha1
      kind: Task
      name: coordinator-task
      controller: true
spec:
  agentRef:
    name: security-reviewer
  prompt: "Check for SQL injection vulnerabilities in the auth module"
  # Inherit session from parent for context
  sessionRef:
    name: coordinator-task-session
    append: false               # Don't pollute parent session
```

#### Coordination Status

Parent Task tracks child tasks in status:

```yaml
status:
  phase: Running
  childTasks:
    - name: coordinator-task-child-1
      agent: security-reviewer
      phase: Succeeded
      result: "Found 2 potential SQL injection points..."
    - name: coordinator-task-child-2
      agent: performance-analyzer
      phase: Running
  aggregatedResults:
    completed: 1
    running: 1
    failed: 0
```

### Project Structure Updates

```
mercan/
├── api/
│   └── v1alpha1/
│       ├── task_types.go
│       ├── tool_types.go
│       ├── agent_types.go          # NEW: Agent CRD definition
│       └── groupversion_info.go
├── internal/
│   ├── controller/
│   │   ├── task_controller.go
│   │   ├── tool_controller.go
│   │   ├── agent_controller.go     # NEW: Agent reconciler
│   │   ├── router.go               # NEW: Label-based routing
│   │   └── delegation.go           # NEW: Agent-to-agent delegation
│   └── worker/
│       ├── executor.go
│       ├── ai_agent.go
│       ├── tool_executor.go
│       └── delegation_tool.go      # NEW: delegate_to_agent implementation
├── config/
│   └── crd/
│       └── bases/
│           ├── mercan.ai_tasks.yaml
│           ├── mercan.ai_tools.yaml
│           └── mercan.ai_agents.yaml   # NEW: Agent CRD manifest
```

### Implementation Phases

#### Phase 7: Agent CRD (after Testing & Deployment)

1. Define Agent CRD (`api/v1alpha1/agent_types.go`)
2. Generate CRD manifests with kubebuilder
3. Implement Agent controller:
   - Validate agent configuration
   - Track active tasks per agent
   - Update status conditions
4. Update Task controller:
   - Resolve `agentRef` to Agent CRD
   - Merge agent config with task config
   - Inject agent settings into worker Pod

#### Phase 8: Routing System

1. Define routing rules ConfigMap schema
2. Implement router in controller:
   - Load rules from ConfigMap
   - Match task labels against rules
   - Resolve to Agent CRD
3. Add routing metrics:
   - Routes matched by rule
   - Fallback to default count
4. Document routing configuration

#### Phase 9: Agent Coordination (Optional)

1. Add `coordination` field to Agent spec
2. Implement `delegate_to_agent` built-in tool:
   - Validate target agent is allowed
   - Check delegation depth limit
   - Create child Task with ownerReference
   - Wait for result (or return immediately)
3. Update Task status with child task tracking
4. Add circuit breaker for runaway delegation
5. Implement result aggregation

### Example: Multi-Agent Workflow

**1. Define specialized agents:**

```yaml
# agents/security-reviewer.yaml
apiVersion: mercan.ai/v1alpha1
kind: Agent
metadata:
  name: security-reviewer
  namespace: shared-agents
spec:
  model:
    provider: anthropic
    name: claude-sonnet-4-20250514
    temperature: 0.2
  systemPrompt:
    inline: |
      You are a security expert. Focus on:
      - OWASP Top 10 vulnerabilities
      - Authentication/authorization issues
      - Input validation problems
      - Secrets exposure
  tools:
    - code_exec
    - file_read
---
# agents/coordinator.yaml
apiVersion: mercan.ai/v1alpha1
kind: Agent
metadata:
  name: coordinator
  namespace: shared-agents
spec:
  model:
    provider: anthropic
    name: claude-sonnet-4-20250514
  systemPrompt:
    inline: |
      You coordinate complex analysis tasks by delegating to specialists.
      Synthesize results into actionable recommendations.
  coordination:
    enabled: true
    allowedAgents:
      - name: security-reviewer
      - name: performance-analyzer
    maxDepth: 2
  tools:
    - delegate_to_agent
    - file_read
```

**2. Submit coordinated task:**

```yaml
apiVersion: mercan.ai/v1alpha1
kind: Task
metadata:
  name: full-code-review
  namespace: team-backend
spec:
  agentRef:
    name: coordinator
    namespace: shared-agents
  prompt: |
    Perform a comprehensive review of the auth module:
    1. Delegate security analysis to the security specialist
    2. Delegate performance analysis to the performance specialist
    3. Synthesize findings into a prioritized action plan
```

**3. Coordinator creates child tasks automatically:**

```
full-code-review (coordinator)
├── full-code-review-child-1 (security-reviewer) → "Found 2 vulnerabilities..."
├── full-code-review-child-2 (performance-analyzer) → "N+1 query detected..."
└── Result: "Priority actions: 1. Fix SQL injection in login..."
```

### Prometheus Metrics (Multi-Agent)

| Metric | Type | Description |
|--------|------|-------------|
| `mercan_agents_total` | Gauge | Total agents by namespace |
| `mercan_agent_tasks_active` | Gauge | Active tasks per agent |
| `mercan_agent_tasks_total` | Counter | Total tasks per agent |
| `mercan_routing_matches_total` | Counter | Routing rule matches by rule name |
| `mercan_routing_default_total` | Counter | Tasks routed to default agent |
| `mercan_delegation_total` | Counter | Delegation calls by parent/child agent |
| `mercan_delegation_depth` | Histogram | Delegation depth distribution |

### Limitations (v1)

| Limitation | Description | Workaround |
|------------|-------------|------------|
| **Namespace-scoped agents** | Agents must be in Task namespace or explicitly referenced | Use `agentRef.namespace` for cross-namespace |
| **No agent versioning** | Agent CRDs are not versioned | Use names like `code-reviewer-v2` |
| **Sync delegation only** | `delegate_to_agent` blocks until child completes | Set reasonable timeouts |
| **No partial results** | Parent waits for all children | Use `waitForResult: false` for fire-and-forget |
| **Single coordinator** | No peer-to-peer agent communication | Chain through coordinator |

### Future Enhancements

- **ClusterAgent CRD**: Cluster-scoped agents shared across namespaces
- **Agent versioning**: `spec.version` with rolling updates
- **Async delegation**: Callback-based child task completion
- **Agent discovery**: List available agents for coordination
- **Cost tracking**: Token/cost attribution per agent
- **Agent templates**: Helm-style templating for agent configs
- **Priority inheritance**: Child tasks inherit parent priority

## Verification Plan

1. **Unit Tests**: Run `make test`
2. **Local Testing**:
   - Use `kind` to create local cluster
   - Deploy controller with `make deploy`
   - Submit test tasks via API
3. **Integration Tests**:
   - Test task lifecycle (create → run → complete)
   - Test error handling (timeout, failure)
   - Test AI agent execution
   - Test session continuity (multi-turn conversations)
   - Test session limits (maxMessages truncation)
