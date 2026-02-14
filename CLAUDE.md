# CLAUDE.md

This file provides guidance to Claude Code when working with code in this repository.

## CRITICAL: Security

**NEVER leak API keys, secrets, credentials, or sensitive data.** This includes:
- Never commit secrets to version control
- Never log or print API keys, tokens, or passwords
- Never include secrets in error messages or responses

## CRITICAL: No Binaries in Repo

**NEVER commit compiled binaries to the repository.** Build artifacts belong in `bin/` (which is gitignored) or CI release pipelines — not in version control.
- Always use Kubernetes Secrets or environment variables for sensitive data
- Never hardcode credentials in code or configuration files

## CRITICAL: Fix Pre-existing Issues

When you encounter pre-existing bugs, failing tests, or broken CI — **fix them**. Do not skip or ignore issues just because they existed before your change. Leave the codebase better than you found it.

## Project Overview

Orka is a Kubernetes-native task execution platform. A controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes (Copilot, Claude Code). See @docs/architecture.md for full architecture details.

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
make deploy IMG=<registry>/orka:tag
```

## Architecture

### Core Components

- **Controller** (`cmd/main.go`): Main entrypoint with `--watch-namespace`, `--copilot-worker-image`, `--claude-worker-image`, `--ai-worker-image`, `--store-backend`, `--store-path`, `--controller-url`, and `--enforce-namespace-isolation` flags
- **CLI** (`cmd/cli/`): Command-line tool with `login`, `chat`, `agent`, `task`, and `status` commands
- **Migrate** (`cmd/migrate/`): Database migration tool for moving data from ConfigMaps to SQLite
- **API Server** (`internal/api/`): REST API using Fiber framework with ServiceAccount token auth
- **Chat Endpoint** (`internal/api/chat.go`): Agentic chat with SSE streaming, tool execution loop, session persistence
- **Task Reconciler** (`internal/controller/`): Watches Task CRDs, creates Jobs, manages lifecycle
- **Session Manager**: Manages conversation continuity via SQLite store with serial execution enforcement
- **Store** (`internal/store/`): Storage interfaces (`ResultStore`, `SessionStore`, `PlanStore`) with SQLite implementation (`internal/store/sqlite/`)
- **Workers** (`workers/`): AI worker (LLM agent with tools), general worker (container commands), and agent workers (`workers/agent/copilot/`, `workers/agent/claude/`) for external CLI runtimes; workers POST results to controller via HTTP
- **Tracing** (`internal/tracing/`): Optional OpenTelemetry tracing with OTLP gRPC export, enabled via `--enable-tracing`
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
- PR management: `merge_pull_request` merges PRs after CI checks pass (instant); `auto_merge_pull_request` polls CI and auto-merges when green (blocking with timeout); `review_pull_request` fetches PR diffs for analysis; `post_review_comment` posts reviews with verdicts and line-level comments
- Self-healing coordination: `delegate_task` supports `auto_retry` and `max_retries` params; `wait_for_tasks` automatically re-creates failed child tasks with error context prepended to original prompt; retry config stored as annotations (`orka.ai/auto-retry`, `orka.ai/max-retries`, `orka.ai/retry-count`)
- Autonomous mode: When `coordination.autonomous: true`, controller loops Jobs instead of completing; plan state persisted in SQLite via `PlanStore`; worker fetches plan via HTTP GET; `update_plan` tool saves progress; termination via goal_complete flag, maxIterations, or Suspend

### Multi-Agent Coordination

- **Coordination tools** (`internal/tools/delegate_task.go`, `internal/tools/wait_for_tasks.go`, `internal/tools/update_plan.go`): LLM tools that create child Tasks, poll for results, and update autonomous plan state
- **PR workflow tools** (`internal/tools/create_pull_request.go`, `internal/tools/merge_pull_request.go`, `internal/tools/auto_merge_pull_request.go`, `internal/tools/review_pull_request.go`, `internal/tools/post_review_comment.go`): GitHub PR creation, merging (instant and polling), review fetching, and review posting
- **Agent management tools** (`internal/tools/create_agent.go`, `internal/tools/delete_agent.go`): Dynamic Agent CRD creation and deletion at runtime
- **Controller enforcement** (`internal/controller/task_controller.go`): Validates `maxDepth`, `allowedAgents`, `maxConcurrentChildren` in `handlePending`; populates `status.childTasks` in `handleRunning`
- **Job builder** (`internal/controller/job_builder.go`): Injects `ORKA_COORDINATION_*` env vars and auto-adds coordination tools when `agent.Spec.Coordination.Enabled`
- **AI worker** (`workers/ai/main.go`): Registers coordination tools via `tools.RegisterCoordinationTools()` when `ORKA_COORDINATION_ENABLED=true`; increases `maxIterations` to 50
- **RBAC** (`config/rbac/worker_role.yaml`): Workers have Task `create/get/list/watch` and Agent `get/create/update/delete` for coordination
- Child tasks use labels (`orka.ai/parent-task`, `orka.ai/delegated-agent`) and annotations (`orka.ai/coordination-depth`) for tracking
- Owner references enable cascade deletion of child tasks
- **Iterative workflows**: `prior_task` param in `delegate_task` sets `PriorTaskRef` on child tasks; job builder injects `ORKA_PRIOR_TASK`/`ORKA_PRIOR_TASK_NAMESPACE` env vars; workers apply prior diffs before starting
- **Auto-push**: `PushBranch` field on `WorkspaceConfig` → `ORKA_PUSH_BRANCH` env var → `FinalizeResult` commits and pushes to that branch
- **PR creation tool** (`internal/tools/create_pull_request.go`): Reads git secret from child task's workspace config, creates PR via GitHub REST API
- **PR merge tool** (`internal/tools/merge_pull_request.go`): Verifies CI checks pass, then merges PR via GitHub REST API
- **PR auto-merge tool** (`internal/tools/auto_merge_pull_request.go`): Polls CI checks every 30s and auto-merges when green; handles force-pushes, external closures, and transient API errors
- **PR review tool** (`internal/tools/review_pull_request.go`): Fetches PR diff, file changes, and metadata for code review
- **PR comment tool** (`internal/tools/post_review_comment.go`): Posts review with verdict (APPROVE/REQUEST_CHANGES/COMMENT) and line-level comments
- **Autonomous mode** (`api/v1alpha1/agent_types.go`): `CoordinationConfig.Autonomous` enables controller-level Job loop; `MaxIterations` caps iterations; `TaskStatus.Iteration` tracks current iteration; `PlanStore` persists plan state in SQLite
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
- `github.com/openai/openai-go/v3` — OpenAI API (official SDK)
- `github.com/github/copilot-sdk/go` — GitHub Copilot SDK
- `modernc.org/sqlite` — Embedded SQLite (pure Go, no CGO)

## API Endpoints

```
POST   /api/v1/tasks              Create task
GET    /api/v1/tasks              List tasks (?namespace=, ?limit=, ?continue=)
GET    /api/v1/tasks/{id}         Get task details
DELETE /api/v1/tasks/{id}         Cancel/delete task
GET    /api/v1/tasks/{id}/logs    Stream logs
GET    /api/v1/tasks/{id}/result  Get result from SQLite store
GET    /api/v1/tasks/{id}/plan    Get autonomous plan state
GET    /api/v1/tasks/{id}/children Get child tasks
GET    /api/v1/sessions           List sessions
GET    /api/v1/sessions/{id}      Get session transcript
DELETE /api/v1/sessions/{id}      Delete session
GET    /api/v1/tools              List tools
GET    /api/v1/tools/{name}       Get tool details
POST   /api/v1/agents             Create agent
GET    /api/v1/agents             List agents
GET    /api/v1/agents/{name}      Get agent details
PUT    /api/v1/agents/{name}      Update agent
DELETE /api/v1/agents/{name}      Delete agent
GET    /api/v1/auth/validate      Validate auth token
GET    /api/v1/secrets            List secret names (metadata only)
POST   /api/v1/chat               Chat with SSE streaming (if enabled)
GET    /api/v1/chat/config        Get chat configuration
DELETE /api/v1/chat/{sessionId}   Cancel chat session
GET    /healthz                   Health check
GET    /readyz                    Readiness check
POST   /v1/chat/completions      OpenAI-compatible chat completions (streaming & non-streaming)
GET    /v1/models                OpenAI-compatible model listing

Internal endpoints (worker communication):
POST   /internal/v1/results/{namespace}/{taskName}              Submit task result
GET    /internal/v1/sessions/{namespace}/{name}/transcript      Get session transcript
POST   /internal/v1/plans/{namespace}/{taskName}                Save autonomous plan state
GET    /internal/v1/plans/{namespace}/{taskName}                Get autonomous plan state
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

# After editing tracing code
go test ./internal/tracing/ -v
go test ./internal/llm/ -run TestTracing -v
```

## Worker Security Context

All worker pods run with: non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault.

## Documentation

See @docs/ for detailed documentation:
- @docs/architecture.md — System design and components
- @docs/agent-runtimes.md — Claude Code CLI and Copilot CLI runtime configuration
- @docs/api-reference.md — REST API endpoint reference
- @docs/chat.md — Chat endpoint, tools, SSE streaming
- @docs/cicd-integration.md — CI/CD integration patterns
- @docs/configuration.md — CRD configuration reference
- @docs/development.md — Build, test, and development setup
- @docs/getting-started.md — Installation and quick start
- @docs/multi-agent-coordination.md — Coordinator agents, delegation tools, controller enforcement
- @docs/autonomous-tasks.md — Autonomous task execution and planning loops
- @docs/openai-compat.md — OpenAI-compatible API proxy
- @docs/security.md — Security model and hardening
- @docs/testing.md — Test structure and patterns
- @docs/ui.md — Web dashboard architecture
