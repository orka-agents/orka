# Architecture

Mercan is a Kubernetes-native task execution platform where a controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes.

## Overview

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

## Core Components

### Controller (`cmd/main.go`)

The controller is the central component that runs as a Kubernetes Deployment. It contains:

- **API Server**: REST endpoints for task CRUD operations, built on the Fiber framework
- **Task Reconciler**: Watches Task resources, creates/manages Jobs, handles lifecycle
- **Session Manager**: Manages session persistence (via SQLite store) for conversation continuity with serial execution enforcement
- **Priority Queue**: Schedules tasks based on priority (0-1000)
- **Webhook Notifier**: Delivers completion notifications via HTTP callbacks
- **Embedded Web UI**: The React dashboard is compiled into the controller binary

### Custom Resource Definitions (`api/v1alpha1/`)

Mercan uses four CRDs:

| CRD | Purpose |
|-----|---------|
| **Task** | Core work unit — container, AI, or agent type |
| **Agent** | Reusable agent configurations with model, tools, skills, and optional runtime |
| **Tool** | Custom HTTP-based tool definitions for agents |
| **Provider** | LLM provider configuration (Anthropic, OpenAI, Azure OpenAI) |

### Worker Images (`workers/`)

| Worker | Description |
|--------|-------------|
| **General Worker** (`workers/general/`) | Runs arbitrary container commands |
| **AI Worker** (`workers/ai/`) | Runs LLM agent tasks with built-in tools (web search, code exec, file read) and coordination tools (delegate_task, wait_for_tasks, create_pull_request, merge_pull_request, review_pull_request, post_review_comment, create_agent, delete_agent) |
| **Copilot Agent Worker** (`workers/agent/copilot/`) | Runs tasks via GitHub Copilot CLI using the Go SDK |
| **Claude Agent Worker** (`workers/agent/claude/`) | Runs tasks via Claude Code CLI |

## Design Decisions

| Area | Decision | Rationale |
|------|----------|-----------|
| **Result Storage** | SQLite (embedded) | No size limit, zero external dependencies, pure Go via `modernc.org/sqlite`. |
| **Session Storage** | SQLite (embedded) | Normalized schema with efficient querying and pagination. No size limit. |
| **Plan Storage** | SQLite (embedded) | Persists autonomous coordination plan state across iterations. |
| **API Authentication** | Kubernetes ServiceAccount tokens | Native K8s auth with RBAC integration. |
| **Task Queue** | Priority queuing (0-1000) | Higher priority tasks are scheduled first. |
| **Secret Management** | Reference K8s Secrets in specs | Controller mounts secrets to worker pods. |
| **Observability** | Prometheus metrics + structured logs | Standard K8s observability stack. |
| **AI Tools** | Built-in + extensible via CRDs | Ship with `web_search`, `code_exec`, `file_read`. Extend via Tool CRDs. |
| **Failure Policy** | Configurable retry with backoff | `spec.retryPolicy` with max retries and exponential backoff. |
| **Session Execution** | Serial per session | Tasks sharing a session run one-at-a-time to prevent race conditions. |
| **Worker Security** | Hardened pods | Non-root, read-only rootfs, all capabilities dropped, seccomp RuntimeDefault. |

## Project Structure

```
mercan/
├── api/v1alpha1/           # CRD type definitions (Task, Agent, Tool, Provider)
├── cmd/
│   ├── main.go                # Controller entrypoint
│   ├── cli/                   # CLI tool (login, chat, agent, task, status)
│   └── migrate/               # Database migration (ConfigMaps → SQLite)
├── internal/
│   ├── api/                # REST API server, handlers, auth, chat endpoint
│   ├── controller/         # Reconcilers, job builder, session manager, priority queue
│   ├── llm/                # LLM provider interface and implementations
│   │   ├── anthropic/      # Anthropic Claude provider
│   │   └── openai/         # OpenAI provider
│   ├── store/              # Storage interfaces and SQLite implementation
│   │   └── sqlite/         # SQLite backend (ResultStore + SessionStore + PlanStore)
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
│       └── claude/         # Claude Code CLI agent worker
├── ui/                     # React SPA (Vite + TanStack Router + shadcn/ui)
├── config/                 # Kustomize manifests (CRDs, RBAC, samples)
├── charts/mercan/          # Helm chart
├── docs/                   # Documentation
├── examples/               # Example workflows
└── test/                   # E2E tests
```

## Task Lifecycle

```
Task Created
      │
      ▼
┌───────────┐    session locked?     ┌───────────┐
│  Pending   │──────────────────────▶│  Pending   │ (wait for lock)
│            │◀──────────────────────│            │
└─────┬──────┘    lock acquired      └───────────┘
      │
      ▼
┌───────────┐
│  Running   │ ── Job created, Pod running
└─────┬──────┘
      │
   ┌──┴──┐
   │     │
   ▼     ▼
┌──────┐ ┌──────┐
│Succ. │ │Failed│ ── retry? → back to Running
└──────┘ └──────┘
      │
      ▼
Result stored in SQLite (workers POST to controller via HTTP)
Session lock released
Webhook delivered (if configured)
```

## Multi-Agent Coordination

Coordinator agents can delegate subtasks to specialist agents at runtime. The LLM uses `delegate_task` and `wait_for_tasks` tools to create child Tasks and collect results. GitHub PR tools (`create_pull_request`, `merge_pull_request`, `review_pull_request`, `post_review_comment`) enable end-to-end code review workflows. The controller enforces guardrails:

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

Child tasks use owner references for cascade deletion and labels (`mercan.ai/parent-task`, `mercan.ai/delegated-agent`) for querying.

See [multi-agent-coordination.md](multi-agent-coordination.md) for full details.

### Autonomous Mode

When an agent's coordination config has `autonomous: true`, the controller runs the coordinator in a loop instead of completing the task after a single Job. Each iteration:

1. The coordinator Job runs, delegates sub-tasks, and updates the plan via the `update_plan` tool
2. The controller saves plan state to `PlanStore` (SQLite) and checks termination conditions
3. If not complete, a new Job is created for the next iteration with the accumulated plan state

Termination occurs when the LLM signals goal completion, max iterations are reached, or the task is suspended.

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

Mercan supports extensible AI capabilities through a three-layer system:

```
┌─────────────────────────────────────────────────────────────────┐
│  Layer 1: Skills (ConfigMaps)                                   │
│  - Prompt injection only, zero execution overhead               │
│  - Teaching/guidance for built-in tools                         │
├─────────────────────────────────────────────────────────────────┤
│  Layer 2: Built-in Tools (in worker image)                      │
│  - web_search, file_read, code_exec, delegate_task, wait_for_tasks│
│  - create_pull_request, merge_pull_request, review_pull_request,  │
│    post_review_comment, create_agent, delete_agent                 │
│  - Fast, no extra infrastructure                                │
├─────────────────────────────────────────────────────────────────┤
│  Layer 3: Custom Tools (Tool CRD + HTTP)                        │
│  - Point at internal services                                   │
│  - Namespace-scoped, RBAC-controlled                            │
│  - Header-based or body-based auth injection                    │
└─────────────────────────────────────────────────────────────────┘
```

## Session Management

Sessions provide conversation continuity across multiple Tasks. Each session is stored in SQLite with a normalized schema (session metadata + individual messages).

Key behaviors:
- **Serial execution**: Tasks sharing a session execute one-at-a-time via a lock mechanism
- **Token tracking**: Input/output token counts tracked in the session record
- **Cross-runtime**: Sessions store user/assistant messages only, enabling cross-runtime continuation (AI ↔ agent tasks)
- **No size limit**: SQLite storage removes the old ConfigMap 1MB constraint
- **Init container delivery**: Session transcripts are delivered to worker pods via an init container that fetches from the controller's internal API

## Security Model

- **Worker pods**: Non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault
- **Controller**: Non-root (uid 65532), read-only rootfs, seccomp RuntimeDefault
- **API auth**: ServiceAccount token validation via Kubernetes TokenReview API
- **Secrets**: API keys referenced via `secretRef`, mounted as read-only volumes, never logged
- **Chat endpoint**: Blocks operations in `kube-system` and `kube-public` namespaces
- **Namespace scoping**: Configurable via `--watch-namespace` flag

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
