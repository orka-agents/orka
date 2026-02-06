# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## CRITICAL: Security

**NEVER leak API keys, secrets, credentials, or sensitive data.** This includes:
- Never commit secrets to version control
- Never log or print API keys, tokens, or passwords
- Never include secrets in error messages or responses
- Always use Kubernetes Secrets or environment variables for sensitive data
- Never hardcode credentials in code or configuration files

## Project Overview

Mercan is a Kubernetes-native task execution platform. A controller manages Jobs and Pods for incoming task requests, supporting container tasks, AI agent tasks with LLM integration, and external agent CLI runtimes (Copilot, Claude Code).

## Build & Development Commands

```bash
# Initialize project (one-time setup)
kubebuilder init --domain mercan.ai --repo github.com/sozercan/mercan
kubebuilder create api --group core --version v1alpha1 --kind Task

# Generate CRD manifests and Go types
make generate
make manifests

# Build
make build

# Run tests
make test

# Deploy to cluster
make deploy

# Local development with kind
kind create cluster
make docker-build docker-push IMG=<registry>/mercan:tag
make deploy IMG=<registry>/mercan:tag
```

### Agent Worker Images

```bash
# Build and push agent worker images
make docker-build-copilot-worker COPILOT_WORKER_IMG=<registry>/mercan-agent-worker-copilot:tag
make docker-build-claude-worker CLAUDE_WORKER_IMG=<registry>/mercan-agent-worker-claude:tag
make docker-push-copilot-worker
make docker-push-claude-worker

# Build/push all images at once (manager + agent workers)
make docker-build-all
make docker-push-all
```

## Architecture

### Core Components

- **Controller** (`cmd/controller/`): Main entrypoint with `--watch-namespace`, `--copilot-worker-image`, and `--claude-worker-image` flags
- **API Server** (`internal/api/`): REST API using Fiber framework with ServiceAccount token auth
- **Task Reconciler** (`internal/controller/`): Watches Task CRDs, creates Jobs, manages lifecycle
- **Session Manager**: Manages conversation continuity via ConfigMaps with serial execution enforcement
- **Workers** (`workers/`): AI worker (LLM agent with tools), general worker (container commands), and agent workers (`workers/agent/copilot/`, `workers/agent/claude/`) for external CLI runtimes

### Custom Resources

- **Task** (`api/v1alpha1/task_types.go`): Core work unit - `container`, `ai`, or `agent` type
- **Tool** (`api/v1alpha1/tool_types.go`): Custom HTTP-based tool definitions
- **Agent** (`api/v1alpha1/agent_types.go`): Reusable agent configurations with model, tools, skills, and optional `runtime` field for CLI runtimes

### Task Types

- **`container`**: Runs arbitrary container commands
- **`ai`**: Runs AI agent tasks with built-in LLM integration (Anthropic, OpenAI)
- **`agent`**: Runs external agent CLI runtimes (Copilot CLI, Claude Code CLI) via dedicated worker images

### Agent Runtime (type: agent)

Tasks with `type: agent` reference an Agent CRD that has `spec.runtime` (`AgentCLIRuntime`) set:
- `runtime.type`: `copilot` or `claude` — selects the CLI runtime
- `runtime.defaultMaxTurns`: Default max agent loop iterations (1-1000, default 50)
- `runtime.defaultAllowedTools`: Default tools allowed for tasks
- `runtime.defaultAllowBash`: Whether bash is allowed by default

Task-level overrides via `spec.agentRuntime` (`AgentRuntimeSpec`):
- `workspace`: `WorkspaceConfig` with `gitRepo`, `branch`, `ref`, `gitSecretRef`, `subPath`
- `maxTurns`: Override max agent loop iterations
- `allowedTools` / `disallowedTools`: Override tool permissions
- `allowBash`: Override bash permission

### Key Patterns

- Results stored in ConfigMaps (1MB limit per result)
- Sessions stored in ConfigMaps with JSONL transcript format
- Skills are ConfigMaps with `skill.md` content injected into system prompts
- Tools execute via HTTP calls to internal services
- Priority queue (0-1000) for task scheduling
- Finalizers ensure cleanup of result ConfigMaps and session locks

## Dependencies

- `sigs.k8s.io/controller-runtime` - Controller framework
- `k8s.io/client-go` - Kubernetes client
- `github.com/gofiber/fiber/v3` - HTTP router
- `github.com/anthropics/anthropic-sdk-go` - Anthropic Claude API
- `github.com/sashabaranov/go-openai` - OpenAI API

## API Endpoints

```
POST   /api/v1/tasks           Create task
GET    /api/v1/tasks           List tasks (?namespace=, ?limit=, ?continue=)
GET    /api/v1/tasks/{id}      Get task details
DELETE /api/v1/tasks/{id}      Cancel/delete task
GET    /api/v1/tasks/{id}/logs Stream logs
GET    /api/v1/tasks/{id}/result  Get result from ConfigMap
GET    /api/v1/sessions        List sessions
GET    /api/v1/sessions/{id}   Get session transcript
DELETE /api/v1/sessions/{id}   Delete session
GET    /api/v1/tools           List tools
GET    /api/v1/agents          List agents
```

## Worker Security Context

All worker pods run with: non-root (uid 1000), read-only rootfs, all capabilities dropped, seccomp RuntimeDefault.
