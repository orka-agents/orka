# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## CRITICAL: Security

**NEVER leak API keys, secrets, credentials, or sensitive data.** This includes:
- Never commit secrets to version control
- Never log or print API keys, tokens, or passwords
- Never include secrets in error messages or responses
- Always use Kubernetes Secrets or environment variables for sensitive data
- Never hardcode credentials in code or configuration files

## Project Overview

Mercan is a Kubernetes-native task execution platform. A controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes (Copilot, Claude Code). See @docs/architecture.md for full architecture details.

## Build & Development Commands

```bash
# Generate CRD manifests and Go types (run after editing *_types.go or markers)
make manifests
make generate

# Build (includes UI)
make build

# Run tests
make test

# Lint
make lint-fix

# UI development
cd ui && bun install && bun run dev   # Dev server on :5173
cd ui && bun run test                 # UI tests
cd ui && bun run test:coverage        # UI tests with coverage

# Docker images
make docker-build-all                 # Controller + all agent workers
make docker-push-all

# Deploy
make deploy IMG=<registry>/mercan:tag
```

## Architecture

### Core Components

- **Controller** (`cmd/controller/`): Main entrypoint with `--watch-namespace`, `--copilot-worker-image`, `--claude-worker-image`, `--store-backend`, and `--store-path` flags
- **API Server** (`internal/api/`): REST API using Fiber framework with ServiceAccount token auth
- **Chat Endpoint** (`internal/api/chat.go`): Agentic chat with SSE streaming, tool execution loop, session persistence
- **Task Reconciler** (`internal/controller/`): Watches Task CRDs, creates Jobs, manages lifecycle
- **Session Manager**: Manages conversation continuity via SQLite store with serial execution enforcement
- **Store** (`internal/store/`): Storage interfaces (`ResultStore`, `SessionStore`) with SQLite implementation (`internal/store/sqlite/`)
- **Workers** (`workers/`): AI worker (LLM agent with tools), general worker (container commands), and agent workers (`workers/agent/copilot/`, `workers/agent/claude/`) for external CLI runtimes; workers POST results to controller via HTTP
- **Web UI** (`ui/`): React SPA embedded into controller binary via `//go:embed`

### Custom Resources

- **Task** (`api/v1alpha1/task_types.go`): Core work unit — `container`, `ai`, or `agent` type with optional scheduling
- **Tool** (`api/v1alpha1/tool_types.go`): Custom HTTP-based tool definitions
- **Agent** (`api/v1alpha1/agent_types.go`): Reusable agent configurations with model, tools, skills, and optional `runtime` field for CLI runtimes
- **Provider** (`api/v1alpha1/provider_types.go`): LLM provider configuration (anthropic, openai, azure-openai)

### Task Types

- **`container`**: Runs arbitrary container commands
- **`ai`**: Runs AI agent tasks with built-in LLM integration (Anthropic, OpenAI)
- **`agent`**: Runs external agent CLI runtimes (Copilot CLI, Claude Code CLI) via dedicated worker images

### Key Patterns

- Results stored in SQLite via the `ResultStore` interface (no size limit)
- Sessions stored in SQLite with normalized schema (session metadata + messages) via the `SessionStore` interface
- Workers POST results to the controller's internal HTTP endpoint (`/internal/v1/results/:namespace/:taskName`)
- Session transcripts delivered to worker pods via an init container that fetches from the controller
- Skills are ConfigMaps with `skill.md` content injected into system prompts
- Tools execute via HTTP calls to internal services
- Priority queue (0-1000) for task scheduling
- Finalizers ensure cleanup of session locks
- LLM tool args for nested objects arrive as `map[string]any`, not strings — always type-switch
- Multi-agent coordination: coordinator agents delegate to specialists via `delegate_task`/`wait_for_tasks` tools; controller enforces depth, allowedAgents, concurrency limits
- Iterative coordination: `delegate_task` supports `prior_task`, `feedback`, and `pushBranch` params; workers apply prior diffs via `PrepareWorkspace()` and produce structured results via `FinalizeResult()`
- Auto-push: When `pushBranch` is set on workspace config, `FinalizeResult` commits and pushes changes to that branch automatically
- PR creation: `create_pull_request` coordination tool creates GitHub PRs from pushed branches using git credentials from task workspace config

### Multi-Agent Coordination

- **Coordination tools** (`internal/tools/delegate_task.go`, `internal/tools/wait_for_tasks.go`): LLM tools that create child Tasks and poll for results
- **Controller enforcement** (`internal/controller/task_controller.go`): Validates `maxDepth`, `allowedAgents`, `maxConcurrentChildren` in `handlePending`; populates `status.childTasks` in `handleRunning`
- **Job builder** (`internal/controller/job_builder.go`): Injects `MERCAN_COORDINATION_*` env vars and auto-adds coordination tools when `agent.Spec.Coordination.Enabled`
- **AI worker** (`workers/ai/main.go`): Registers coordination tools via `tools.RegisterCoordinationTools()` when `MERCAN_COORDINATION_ENABLED=true`; increases `maxIterations` to 50
- **RBAC** (`config/rbac/worker_role.yaml`): Workers have Task `create/get/list/watch` for coordination
- Child tasks use labels (`mercan.ai/parent-task`, `mercan.ai/delegated-agent`) and annotations (`mercan.ai/coordination-depth`) for tracking
- Owner references enable cascade deletion of child tasks
- **Iterative workflows**: `prior_task` param in `delegate_task` sets `PriorTaskRef` on child tasks; job builder injects `MERCAN_PRIOR_TASK`/`MERCAN_PRIOR_TASK_NAMESPACE` env vars; workers apply prior diffs before starting
- **Auto-push**: `PushBranch` field on `WorkspaceConfig` → `MERCAN_PUSH_BRANCH` env var → `FinalizeResult` commits and pushes to that branch
- **PR creation tool** (`internal/tools/create_pull_request.go`): Reads git secret from child task's workspace config, creates PR via GitHub REST API
- **Structured results** (`workers/common/result.go`): `StructuredResult` envelope with summary, diff, verdict, feedback, files, pushBranch; `wait_for_tasks` strips diffs from coordinator context
- See @docs/multi-agent-coordination.md for full details

## Auto-Generated Files — Do NOT Edit

- `config/crd/bases/*.yaml` — regenerate with `make manifests`
- `config/rbac/role.yaml` — regenerate with `make manifests`
- `**/zz_generated.*.go` — regenerate with `make generate`
- `PROJECT` — managed by kubebuilder CLI
- `ui/src/routeTree.gen.ts` — managed by TanStack Router

Do NOT delete `// +kubebuilder:scaffold:*` comments — the CLI injects code at these markers.

## Code Style & Conventions

### Go

- Use table-driven tests with descriptive subtests
- Use `sigs.k8s.io/controller-runtime/pkg/client/fake` for K8s client mocking in tests
- Use `httptest.NewServer` for HTTP mocking
- Structured logging: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- Idempotent reconciliation — safe to run multiple times
- Re-fetch before updates: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update`
- Owner references for automatic garbage collection (`SetControllerReference`)
- RBAC markers above reconciler methods, then run `make manifests`

### TypeScript (UI)

- React 19 + TanStack Router (file-based routes in `ui/src/routes/`)
- Zustand stores for auth and UI state (`ui/src/stores/`)
- TanStack Query hooks per resource (`ui/src/hooks/use-*.ts`)
- Zod schemas matching Go API types (`ui/src/schemas/`)
- shadcn/ui components (new-york style) with Tailwind CSS 4
- Mock `zustand/middleware` with `vi.mock('zustand/middleware', () => ({ persist: (fn: unknown) => fn }))` in tests

## Dependencies

- `sigs.k8s.io/controller-runtime` — Controller framework
- `k8s.io/client-go` — Kubernetes client
- `github.com/gofiber/fiber/v3` — HTTP router
- `github.com/anthropics/anthropic-sdk-go` — Anthropic Claude API
- `github.com/sashabaranov/go-openai` — OpenAI API
- `github.com/github/copilot-sdk/go` — GitHub Copilot SDK
- `modernc.org/sqlite` — Embedded SQLite (pure Go, no CGO)

## API Endpoints

```
POST   /api/v1/tasks           Create task
GET    /api/v1/tasks           List tasks (?namespace=, ?limit=, ?continue=)
GET    /api/v1/tasks/{id}      Get task details
DELETE /api/v1/tasks/{id}      Cancel/delete task
GET    /api/v1/tasks/{id}/logs Stream logs
GET    /api/v1/tasks/{id}/result  Get result from SQLite store
GET    /api/v1/sessions        List sessions
GET    /api/v1/sessions/{id}   Get session transcript
DELETE /api/v1/sessions/{id}   Delete session
GET    /api/v1/tools           List tools
GET    /api/v1/tools/{name}    Get tool details
POST   /api/v1/agents          Create agent
GET    /api/v1/agents          List agents
GET    /api/v1/agents/{name}   Get agent details
PUT    /api/v1/agents/{name}   Update agent
DELETE /api/v1/agents/{name}   Delete agent
GET    /api/v1/secrets         List secret names (metadata only)
POST   /api/v1/chat            Chat with SSE streaming (if enabled)
GET    /api/v1/chat/config     Get chat configuration
DELETE /api/v1/chat/{sessionId} Cancel chat session
GET    /healthz                Health check
GET    /readyz                 Readiness check
POST   /v1/chat/completions   OpenAI-compatible chat completions (streaming & non-streaming)
GET    /v1/models             OpenAI-compatible model listing
```

## Verification

After making changes, always verify:

```bash
# After editing *_types.go or markers
make manifests generate

# After editing any *.go files
make lint-fix
make test

# After editing UI code
cd ui && bun run lint && bun run test
```

Prefer running single tests over the whole suite for faster feedback:
```bash
go test ./internal/api/ -run TestHandlerName -v
cd ui && bun run test -- src/components/tasks/task-list.test.tsx
```

## Worker Security Context

All worker pods run with: non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault.

## Documentation

See @docs/ for detailed documentation:
- @docs/architecture.md — System design and components
- @docs/agent-runtimes.md — Claude Code CLI and Copilot CLI runtime configuration
- @docs/chat.md — Chat endpoint, tools, SSE streaming
- @docs/multi-agent-coordination.md — Coordinator agents, delegation tools, controller enforcement
- @docs/ui.md — Web dashboard architecture
- @docs/testing.md — Test structure and patterns
