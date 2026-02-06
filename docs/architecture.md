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

### Controller (`cmd/controller/`)

The controller is the central component that runs as a Kubernetes Deployment. It contains:

- **API Server**: REST endpoints for task CRUD operations, built on the Fiber framework
- **Task Reconciler**: Watches Task resources, creates/manages Jobs, handles lifecycle
- **Session Manager**: Manages session ConfigMaps for conversation continuity with serial execution enforcement
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
| **AI Worker** (`workers/ai/`) | Runs LLM agent tasks with built-in tools (web search, code exec, file read) |
| **Copilot Agent Worker** (`workers/agent/copilot/`) | Runs tasks via GitHub Copilot CLI using the Go SDK |
| **Claude Agent Worker** (`workers/agent/claude/`) | Runs tasks via Claude Code CLI |

## Design Decisions

| Area | Decision | Rationale |
|------|----------|-----------|
| **Result Storage** | ConfigMap | Simple, no extra infrastructure. Limited to 1MB per result. |
| **Session Storage** | ConfigMap | Conversation history in JSONL format. Limited to ~1MB (~50-100 messages). |
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
│   ├── controller/         # Controller entrypoint
│   └── cli/                # CLI tool (mercan login)
├── internal/
│   ├── api/                # REST API server, handlers, auth, chat endpoint
│   ├── controller/         # Reconcilers, job builder, session manager, priority queue
│   ├── llm/                # LLM provider interface and implementations
│   │   ├── anthropic/      # Anthropic Claude provider
│   │   └── openai/         # OpenAI provider
│   ├── tools/              # Built-in tool implementations
│   ├── metrics/            # Prometheus metrics
│   ├── worker/             # Tool executor for custom Tool CRDs
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
Result stored in ConfigMap
Session lock released
Webhook delivered (if configured)
```

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
│  - web_search, file_read, code_exec                             │
│  - Fast, no extra infrastructure                                │
├─────────────────────────────────────────────────────────────────┤
│  Layer 3: Custom Tools (Tool CRD + HTTP)                        │
│  - Point at internal services                                   │
│  - Namespace-scoped, RBAC-controlled                            │
│  - Header-based or body-based auth injection                    │
└─────────────────────────────────────────────────────────────────┘
```

## Session Management

Sessions provide conversation continuity across multiple Tasks. Each session is stored as a ConfigMap containing a JSONL transcript.

Key behaviors:
- **Serial execution**: Tasks sharing a session execute one-at-a-time via a lock mechanism
- **Token tracking**: Input/output token counts tracked in ConfigMap annotations
- **Cross-runtime**: Sessions store user/assistant messages only, enabling cross-runtime continuation (AI ↔ agent tasks)
- **1MB limit**: ConfigMap size constraint; use `maxMessages` to limit loaded context

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
| `github.com/sashabaranov/go-openai` | OpenAI API |
| `github.com/github/copilot-sdk/go` | GitHub Copilot SDK |
