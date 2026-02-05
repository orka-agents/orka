# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Mercan is a Kubernetes-native task execution platform. A controller manages Jobs and Pods for incoming task requests, supporting both container tasks and AI agent tasks with LLM integration.

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

## Architecture

### Core Components

- **Controller** (`cmd/controller/`): Main entrypoint with `--watch-namespace` flag for namespace scoping
- **API Server** (`internal/api/`): REST API using Fiber framework with ServiceAccount token auth
- **Task Reconciler** (`internal/controller/`): Watches Task CRDs, creates Jobs, manages lifecycle
- **Session Manager**: Manages conversation continuity via ConfigMaps with serial execution enforcement
- **Workers** (`workers/`): AI worker (LLM agent with tools) and general worker (container commands)

### Custom Resources

- **Task** (`api/v1alpha1/task_types.go`): Core work unit - container or AI type
- **Tool** (`api/v1alpha1/tool_types.go`): Custom HTTP-based tool definitions
- **Agent** (`api/v1alpha1/agent_types.go`): Reusable agent configurations with model, tools, skills

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
