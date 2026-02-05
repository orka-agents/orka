# Agent Runtime Integration Plan

> **Status**: Draft (revised)
> **Author**: AI-assisted
> **Created**: 2026-02-04
> **Last Updated**: 2026-02-05
> **Review Incorporated**: v0.7 — addresses review findings from `AGENT_RUNTIME_PLAN_REVIEW.md`

## Table of Contents

1. [Executive Summary](#executive-summary)
2. [Review Response Summary](#review-response-summary)
3. [Motivation](#motivation)
4. [SDK Comparison](#sdk-comparison)
5. [Goals and Non-Goals](#goals-and-non-goals)
6. [Current State Analysis](#current-state-analysis)
7. [Proposed Architecture](#proposed-architecture)
8. [API Changes](#api-changes)
9. [Implementation Plan](#implementation-plan)
10. [Security Model](#security-model)
11. [Migration Strategy](#migration-strategy)
12. [Testing Strategy](#testing-strategy)
13. [Rollout Plan](#rollout-plan)
14. [Open Questions](#open-questions)
15. [Appendix](#appendix)

---

## Executive Summary

This document proposes integrating external agent CLIs as first-class worker runtimes in Mercan. Rather than building agentic capabilities from scratch, Mercan will leverage battle-tested agent runtimes (GitHub Copilot CLI, Claude Code CLI) that provide autonomous coding capabilities including file manipulation, shell execution, git operations, and MCP server support—all while preserving Mercan's Kubernetes-native architecture and enterprise controls.

### Key Outcomes

- **10x capability increase**: Full Read/Write/Edit/Bash/Git support without custom Tool CRDs
- **Zero breaking changes**: New `type: agent` task type alongside existing types
- **Pluggable runtimes**: Support multiple agent CLIs (Copilot CLI, Claude Code CLI)
- **Enterprise-ready**: Kubernetes RBAC, secrets management, and audit preserved
- **MCP ecosystem access**: Leverage growing library of MCP integrations

---

## Review Response Summary

The following changes were made to the plan based on the [comprehensive review](AGENT_RUNTIME_PLAN_REVIEW.md) (v0.6):

### Critical (applied)

| # | Review Finding | Resolution |
|---|----------------|------------|
| 1 | Sidecar container is unnecessary — existing workers write results via K8s API | **Dropped sidecar.** Agent workers write result ConfigMaps directly, matching `workers/ai/main.go` and `workers/general/main.go` patterns. |
| 2 | Missing env var plumbing — workers read `MERCAN_PROMPT` but job builder never sets it | **Added `addAgentEnvVars()` specification** with full env var mapping table in Controller Integration section. |
| 3 | `--dangerously-skip-permissions` defeats `AgentPermissions` | **Replaced with `--allowedTools` / `--disallowedTools` mapping.** Claude CLI supports fine-grained tool flags since 1.0. |
| 4 | REST API `CreateTaskRequest` missing `AgentRuntime` field | **Added `AgentRuntime *AgentRuntimeSpec` field** to `CreateTaskRequest` in `handlers.go`. |

### High (applied)

| # | Review Finding | Resolution |
|---|----------------|------------|
| 5 | `AgentSettings` redundantly duplicates existing `Agent.Spec.Model`/`SystemPrompt` | **Eliminated `AgentSettings`.** Existing `Agent.Spec` fields are reused with runtime-aware semantics. |
| 6 | `readOnlyRootFilesystem: false` is a security regression | **Kept `readOnlyRootFilesystem: true`** with writable `emptyDir` volumes for `/home/worker`, `/tmp`, and `/workspace`. |
| 7 | `AgentRuntimeConfig` CRD is over-engineered for a config file | **Demoted to controller flags + optional ConfigMap.** Image refs are `JobBuilder` fields set via `--copilot-worker-image` / `--claude-worker-image`. Promote to CRD later if needed. |
| 8 | Phase ordering: controller integration should come before workers | **Reordered.** Phase 2 is now controller integration (testable with stub worker images), workers follow. |
| 9 | Missing `gitSecretRef` for private repo auth | **Added `GitSecretRef` to `WorkspaceConfig`.** |

### Medium (applied)

| # | Review Finding | Resolution |
|---|----------------|------------|
| 10 | `AgentRuntimeConfig` naming collision between struct and CRD | **Renamed struct on `AgentSpec` to `AgentCLIRuntime`** to avoid confusion (CRD is deferred). |
| 11 | Maps in CRD specs produce poor `kubectl` UX | **Moot** — `AgentRuntimeConfig` CRD deferred. |
| 12 | Webhook payload should include `RuntimeType` | **Added `RuntimeType` optional field** to `WebhookPayload`. |
| 13 | No Makefile targets for multi-image builds | **Added targets** in Implementation Plan. |
| 14 | `Agent.Spec.Model.Provider` validation when `runtime` is set | **Added validation** that `Model.Provider` must be empty when `Runtime` is set. |
| 15 | 1MB ConfigMap limit for agent transcripts | **Agent result stored as final text only** in session. Full transcript logged, not stored in ConfigMap. |
| 16 | `BashAllowPatterns`/`BashDenyPatterns` may be unenforceable at CLI level | **Simplified to boolean `AllowBash`** for v1alpha1. Glob patterns deferred to beta. |
| 17 | Defer `AgentPermissions`, `MCPServerRef`, `PVCReference` to beta | **Deferred.** New type count reduced from ~13 to ~5. |

### Additional (from secondary analysis)

| # | Finding | Resolution |
|---|---------|------------|
| A1 | Worker-does-clone is simpler than init container for alpha | **Dropped init container.** Worker binary handles `git clone` as first step. |
| A2 | Session continuity across runtimes undefined | **Defined.** Sessions store user/assistant messages only (lowest common denominator). Agent-specific metadata lives in annotations. Cross-runtime continuation is supported. |
| A3 | No graceful shutdown for long-running agent tasks | **Added SIGTERM handling requirement** to worker specifications. |
| A4 | Image size impact undocumented | **Added sizing note** and shared base image recommendation. |

---

## Motivation

### Current Limitations

| Capability | Current State | Impact |
|------------|---------------|--------|
| File writing | Requires custom Tool CRD | High friction for basic tasks |
| Shell commands | Requires custom Tool CRD | Cannot run tests, builds natively |
| Git operations | Requires custom Tool CRD | No native PR/commit support |
| Code editing | Requires custom Tool CRD | Limited refactoring capability |
| Agent iterations | Hardcoded to 10 | Complex tasks may not complete |
| MCP servers | Not supported | Missing ecosystem integrations |

### Why External Agent SDKs?

1. **Battle-tested agent loop**: Production-grade execution with smart context management
2. **Comprehensive tooling**: 15+ built-in tools for coding tasks
3. **MCP support**: Extensible through Model Context Protocol
4. **Active development**: Regular updates from vendors
5. **Reduced maintenance**: Leverage community improvements

---

## SDK Comparison

### Available Options

| Feature | Copilot SDK (Go) | Claude Code CLI | Current Mercan |
|---------|------------------|-----------------|----------------|
| **Worker Language** | Go (shells out to CLI) | Go (shells out to CLI) | Go |
| **Agent Loop** | ✅ Built-in | ✅ Built-in | ⚠️ Custom (limited) |
| **Built-in Tools** | ✅ File/Git/Web | ✅ File/Git/Web | ⚠️ 3 tools |
| **MCP Support** | ✅ Yes | ✅ Yes | ❌ No |
| **BYOK** | ✅ Anthropic/OpenAI/Azure | ❌ Anthropic only | ✅ Any |
| **Maturity** | Technical Preview | GA | Production |
| **Container Deps** | Go + Node.js + Copilot CLI | Go + Node.js + Claude CLI | Go binary |
| **Subagents** | ✅ Yes | ✅ Yes | ❌ No |
| **Session Management** | ✅ Yes | ✅ Yes (--resume) | ✅ Yes |

### Recommendation

**Primary**: GitHub Copilot SDK (Go)
- Official Go SDK aligns with Mercan's tech stack
- BYOK allows using Anthropic models
- Note: Requires Copilot CLI binary (Node.js-based) - SDK communicates via JSON-RPC

**Secondary**: Claude Code CLI (direct integration)
- For users who prefer Claude Code's native experience
- Go worker shells out to `claude` CLI
- Requires Node.js runtime in container (for CLI)

> **Important**: Both runtimes require Node.js in the container image since the Go SDKs communicate with their respective CLIs via JSON-RPC rather than embedding the agent logic directly.

---

## Goals and Non-Goals

### Goals

1. **G1**: Add pluggable agent runtime support (`type: agent`)
2. **G2**: Support GitHub Copilot SDK as primary runtime (Go + CLI)
3. **G3**: Support Claude Code CLI as secondary runtime (Go + CLI)
4. **G4**: Preserve all existing Mercan features (sessions, webhooks, priority queue)
5. **G5**: Enable fine-grained permission control over agent capabilities
6. **G6**: Support MCP servers through Kubernetes-native configuration
7. **G7**: Maintain backward compatibility with existing `type: ai` and `type: container` tasks
8. **G8**: Provide workspace management (git clone, PVC mounting)

### Non-Goals

1. **NG1**: Deprecating the existing AI worker (it remains useful for API-only scenarios)
2. **NG2**: Interactive/streaming mode (batch execution only in v1)
3. **NG3**: Building our own agent loop from scratch
4. **NG4**: GUI/dashboard changes (API-first approach)

---

## Current State Analysis

### Existing Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                      Current Mercan                             │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Task CRD ──► Controller ──► Job Builder ──► Worker Pod         │
│                    │                              │              │
│                    ▼                              ▼              │
│              Session Manager              ┌──────────────┐      │
│              Priority Queue               │ AI Worker    │      │
│              Webhook Notifier             │ • LLM API    │      │
│                                           │ • 3 tools    │      │
│                                           │ • 10 iters   │      │
│                                           └──────────────┘      │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Files to Modify

| File | Change Type | Description |
|------|-------------|-------------|
| `api/v1alpha1/task_types.go` | Modify | Add `TaskTypeAgent`, `AgentRuntimeSpec` (minimal: workspace + basic settings) |
| `api/v1alpha1/agent_types.go` | Modify | Add `Runtime *AgentCLIRuntime` field to `AgentSpec` |
| `internal/controller/job_builder.go` | Modify | Add `case TaskTypeAgent`, `addAgentEnvVars()`, workspace volume handling |
| `internal/controller/task_controller.go` | Modify | Add `validateTaskAgentCompatibility()`, handle agent type in `handlePending()` |
| `internal/api/handlers.go` | Modify | Add `AgentRuntime *AgentRuntimeSpec` field to `CreateTaskRequest` |
| `internal/controller/webhook.go` | Modify | Add `RuntimeType` optional field to `WebhookPayload` |
| `workers/agent/copilot/main.go` | **New** | Copilot SDK worker (Go, uses CLI via JSON-RPC) |
| `workers/agent/copilot/Dockerfile` | **New** | Copilot SDK worker image (Go + Node.js + Copilot CLI) |
| `workers/agent/claude/main.go` | **New** | Claude Code CLI worker (Go, shells out to CLI) |
| `workers/agent/claude/Dockerfile` | **New** | Claude Code CLI worker image (Go + Node.js + Claude CLI) |
| `config/crd/bases/` | Generate | Updated CRD manifests (no new CRDs — just updated Task/Agent) |
| `Makefile` | Modify | Add `docker-build-copilot-worker`, `docker-build-claude-worker` targets |
| `config/samples/` | **New** | Example agent task and agent manifests |

**Files NOT created (deferred from original plan):**

| File | Reason |
|------|--------|
| `api/v1alpha1/agentruntimeconfig_types.go` | CRD deferred — use controller flags + optional ConfigMap instead |
| `internal/controller/runtimeconfig_controller.go` | No CRD = no controller needed |
| `config/crd/bases/agentruntimeconfig.yaml` | No CRD |
| `config/default/agentruntimeconfig.yaml` | Use controller flags (`--copilot-worker-image`, `--claude-worker-image`) |

---

## Proposed Architecture

### High-Level Design

```
┌──────────────────────────────────────────────────────────────────────────┐
│                         KUBERNETES CLUSTER                               │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │                        Control Plane                               │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐                         │  │
│  │  │Task CRD  │  │Agent CRD │  │Tool CRD  │                         │  │
│  │  │          │  │          │  │          │                         │  │
│  │  └────┬─────┘  └──────────┘  └──────────┘                         │  │
│  │       │                                                            │  │
│  │       ▼                                                            │  │
│  │  ┌─────────────────────────────────────────────────────────────┐   │  │
│  │  │                     Task Controller                         │   │  │
│  │  │  • Session Manager (unchanged)                              │   │  │
│  │  │  • Priority Queue (unchanged)                               │   │  │
│  │  │  • Webhook Notifier (minor: adds RuntimeType field)         │   │  │
│  │  │  • Job Builder (extended for agent runtimes)                │   │  │
│  │  │  • Agent worker images configurable via controller flags     │   │  │
│  │  └─────────────────────────────────────────────────────────────┘   │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │                        Data Plane                                  │  │
│  │                                                                    │  │
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐     │  │
│  │  │ type: container │  │ type: ai        │  │ type: agent     │     │  │
│  │  │ (unchanged)     │  │ (unchanged)     │  │ (NEW)           │     │  │
│  │  │                 │  │                 │  │                 │     │  │
│  │  │ General Worker  │  │ AI Worker       │  │ Agent Runtime   │     │  │
│  │  │ • Run commands  │  │ • LLM API       │  │ Worker          │     │  │
│  │  │                 │  │ • Limited tools │  │ • Copilot CLI   │     │  │
│  │  │                 │  │                 │  │ • Claude CLI    │     │  │
│  │  │                 │  │                 │  │ • Full tools    │     │  │
│  │  │                 │  │                 │  │ • Git clone     │     │  │
│  │  │                 │  │                 │  │ • Direct K8s    │     │  │
│  │  │                 │  │                 │  │   result write  │     │  │
│  │  └─────────────────┘  └─────────────────┘  └─────────────────┘     │  │
│  │                                                                    │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│                                                                          │
│  ┌────────────────────────────────────────────────────────────────────┐  │
│  │                        Storage                                     │  │
│  │  ┌──────────────┐  ┌──────────────┐                                │  │
│  │  │ ConfigMaps   │  │ Secrets      │                                │  │
│  │  │ • Results    │  │ • API Keys   │                                │  │
│  │  │ • Sessions   │  │ • Git creds  │                                │  │
│  │  └──────────────┘  └──────────────┘                                │  │
│  └────────────────────────────────────────────────────────────────────┘  │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

> **Change from v0.6**: Removed `AgentRuntimeConfig` CRD, PVC storage, and MCP config ConfigMaps from the diagram. Agent worker images are configured via controller flags. PVCs and MCP servers deferred to beta.

### Agent Runtime Worker Pod Structure

```
┌─────────────────────────────────────────────────────────────────────────┐
│                     Agent Runtime Worker Pod                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ┌─────────────────────────────────────────────────────────────────┐    │
│  │ Main Container: agent-runtime (Go binary)                       │    │
│  │                                                                 │    │
│  │  Startup:                                                       │    │
│  │  1. Clone git repository (if MERCAN_GIT_REPO set)               │    │
│  │  2. Invoke CLI runtime (Copilot SDK or Claude CLI)              │    │
│  │  3. Write result ConfigMap via K8s API (like existing workers)  │    │
│  │                                                                 │    │
│  │  Runtime: Copilot SDK (Go+CLI) or Claude Code CLI (Go+CLI)     │    │
│  │  • Receives prompt via MERCAN_PROMPT env var                    │    │
│  │  • Executes autonomous agent loop                               │    │
│  │  • Uses built-in tools (Read/Write/Edit/Bash/etc)               │    │
│  │  • Handles SIGTERM for graceful shutdown                        │    │
│  │                                                                 │    │
│  │  Volume Mounts:                                                 │    │
│  │  • /workspace   ← emptyDir (git clone target)                   │    │
│  │  • /tmp         ← emptyDir (temp files)                         │    │
│  │  • /home/worker ← emptyDir (writable home for CLI config/cache) │    │
│  │  • /secrets     ← Secret volumes (API keys, git creds)          │    │
│  │  • /session     ← ConfigMap (session transcript, optional)      │    │
│  └─────────────────────────────────────────────────────────────────┘    │
│                                                                         │
│  Security: readOnlyRootFilesystem=true, runAsUser=1000,                 │
│            capabilities drop ALL, seccomp RuntimeDefault                │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

> **Change from v0.6**: Removed init container (worker binary handles git clone) and sidecar (worker writes result ConfigMap directly via K8s API, matching `workers/ai/main.go` pattern). This reduces pod complexity from 3 containers to 1.

---

## API Changes

> **v0.7 Note**: Type count reduced from ~13 to ~5 per review recommendation. `AgentSettings` eliminated (reuses existing `Agent.Spec` fields). `AgentPermissions` simplified to boolean `AllowBash`. `MCPServerRef`, `PVCReference`, and `AgentRuntimeConfig` CRD deferred to beta.

### New Task Type: `agent`

```go
// api/v1alpha1/task_types.go

// TaskType defines the type of task
// +kubebuilder:validation:Enum=container;ai;agent
type TaskType string

const (
    TaskTypeContainer TaskType = "container"
    TaskTypeAI        TaskType = "ai"
    TaskTypeAgent     TaskType = "agent"  // NEW
)

// TaskSpec defines the desired state of Task
type TaskSpec struct {
    // ... existing fields ...

    // AgentRuntime contains agent runtime specific configuration (when type is "agent")
    // +optional
    AgentRuntime *AgentRuntimeSpec `json:"agentRuntime,omitempty"`
}
```

### AgentRuntimeSpec Definition (Simplified)

```go
// api/v1alpha1/task_types.go

// AgentRuntimeType defines the agent runtime to use
// +kubebuilder:validation:Enum=copilot;claude
type AgentRuntimeType string

const (
    AgentRuntimeCopilot AgentRuntimeType = "copilot"  // GitHub Copilot CLI
    AgentRuntimeClaude  AgentRuntimeType = "claude"   // Claude Code CLI
)

// AgentRuntimeSpec defines task-level overrides for agent runtime configuration.
// Runtime type and credentials come from the referenced Agent CRD.
// Model and system prompt overrides reuse existing Agent.Spec fields.
type AgentRuntimeSpec struct {
    // Workspace defines the working directory configuration
    // +optional
    Workspace *WorkspaceConfig `json:"workspace,omitempty"`

    // MaxTurns overrides the agent's max turns for this task
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=200
    // +optional
    MaxTurns *int32 `json:"maxTurns,omitempty"`

    // AllowedTools explicitly lists tools the agent can use for this task
    // Overrides Agent defaults. If empty, uses Agent defaults.
    // +optional
    AllowedTools []string `json:"allowedTools,omitempty"`

    // DisallowedTools explicitly blocks specific tools for this task
    // +optional
    DisallowedTools []string `json:"disallowedTools,omitempty"`

    // AllowBash enables the Bash tool for this task
    // Overrides Agent default. Defaults to false.
    // +kubebuilder:default=false
    // +optional
    AllowBash bool `json:"allowBash,omitempty"`
}

// WorkspaceConfig defines workspace setup
type WorkspaceConfig struct {
    // GitRepo is the repository URL to clone
    // +optional
    GitRepo string `json:"gitRepo,omitempty"`

    // Branch is the git branch to checkout (default: main)
    // +kubebuilder:default=main
    // +optional
    Branch string `json:"branch,omitempty"`

    // Ref is a specific commit, tag, or ref to checkout
    // +optional
    Ref string `json:"ref,omitempty"`

    // GitSecretRef references a Secret containing git credentials
    // The Secret should contain a GITHUB_TOKEN or GIT_PASSWORD key
    // +optional
    GitSecretRef *corev1.LocalObjectReference `json:"gitSecretRef,omitempty"`

    // SubPath is a subdirectory within the workspace to use
    // +optional
    SubPath string `json:"subPath,omitempty"`
}
```

**Eliminated types** (compared to v0.6):
- ~~`AgentSettings`~~ → Reuses existing `Agent.Spec.Model.Name` as `--model` flag, `Agent.Spec.SystemPrompt.Inline` as `--system-prompt` flag
- ~~`AgentPermissions`~~ → Simplified to `AllowBash` boolean. Glob-based `BashAllowPatterns`/`BashDenyPatterns` and `WriteAllowPatterns`/`WriteDenyPatterns` deferred to beta (unenforceable at CLI level for v1alpha1)
- ~~`PVCReference`~~ → Deferred to beta. Using `emptyDir` + git clone for v1alpha1
- ~~`MCPServerRef`~~ → Deferred to beta. Use env vars for MCP configuration initially

**Field semantics when `Agent.Runtime` is set:**

| Existing Agent Field | `type: ai` semantics | `type: agent` semantics |
|---|---|---|
| `Model.Name` | LLM model to call via API | `--model` flag for CLI |
| `SystemPrompt.Inline` | System prompt for LLM API | `--system-prompt` / `--append-system-prompt` for CLI |
| `SecretRef` | API key secret | CLI auth secret (`GITHUB_TOKEN` or `ANTHROPIC_API_KEY`) |
| `Tools[]` | Built-in/CRD tool names | CLI tool allow-list |
| `Model.Provider` | LLM provider selection | **Must be empty** (validated by controller) |

### Agent Extension

The `Agent` CRD is extended with a `runtime` field that determines whether the Agent is used for `type: ai` tasks (existing AI worker) or `type: agent` tasks (external CLI runtimes).

**Mutual Exclusivity Rule:**
- If `runtime` is **nil** → Agent is for `type: ai` tasks only
- If `runtime` is **set** → Agent is for `type: agent` tasks only

**Credential Semantics:**
The existing `Agent.spec.secretRef` field is used for credentials, but the expected secret keys depend on the runtime:

| Runtime | Task Type | Secret Key | Description |
|---------|-----------|------------|-------------|
| nil (default) | `type: ai` | `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` | LLM API credentials |
| `copilot` | `type: agent` | `GITHUB_TOKEN` | GitHub PAT from Copilot-licensed user |
| `claude` | `type: agent` | `ANTHROPIC_API_KEY` | Anthropic API key |

```go
// api/v1alpha1/agent_types.go

// AgentSpec defines the desired state of Agent
type AgentSpec struct {
    // ... existing fields (model, systemPrompt, tools, skills, resources, session, etc.) ...

    // SecretRef references a Secret containing credentials
    // - For type: ai tasks: LLM API keys (ANTHROPIC_API_KEY, OPENAI_API_KEY)
    // - For type: agent + copilot: GitHub token (GITHUB_TOKEN)
    // - For type: agent + claude: Anthropic API key (ANTHROPIC_API_KEY)
    // +optional
    SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

    // Runtime defines agent CLI runtime configuration
    // If set, this Agent can ONLY be used with type: agent tasks
    // If nil, this Agent can ONLY be used with type: ai tasks
    // When set, Model.Provider must be empty (validated by controller)
    // +optional
    Runtime *AgentCLIRuntime `json:"runtime,omitempty"`
}

// AgentCLIRuntime defines agent CLI runtime configuration for an Agent.
// Note: Renamed from AgentRuntimeConfig to avoid collision with the
// (now-deferred) cluster-scoped CRD of the same name.
type AgentCLIRuntime struct {
    // Type specifies which CLI runtime to use
    // +kubebuilder:validation:Enum=copilot;claude
    // +kubebuilder:validation:Required
    Type AgentRuntimeType `json:"type"`

    // DefaultMaxTurns is the default max turns for tasks using this agent
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=200
    // +kubebuilder:default=50
    // +optional
    DefaultMaxTurns *int32 `json:"defaultMaxTurns,omitempty"`

    // DefaultAllowedTools are the default tools for this agent
    // +optional
    DefaultAllowedTools []string `json:"defaultAllowedTools,omitempty"`

    // DefaultAllowBash is the default bash permission for this agent
    // +kubebuilder:default=false
    // +optional
    DefaultAllowBash bool `json:"defaultAllowBash,omitempty"`
}
```

> **v0.7 Note**: The struct on `AgentSpec` was renamed from `AgentRuntimeConfig` to `AgentCLIRuntime` to avoid naming collision with the cluster-scoped CRD (which is now deferred). `SystemPrompt`, `MCPServers`, and `DefaultPermissions` fields removed — system prompt reuses `Agent.Spec.SystemPrompt`, MCP servers deferred, permissions simplified to `AllowBash` boolean.

### Controller Flag Configuration (replaces AgentRuntimeConfig CRD)

Instead of a cluster-scoped CRD, agent worker images are configured via controller flags:

```go
// In JobBuilder, add configurable image maps
type JobBuilder struct {
    client.Client
    AIWorkerImage         string
    GeneralWorkerImage    string
    CopilotWorkerImage    string  // --copilot-worker-image flag
    ClaudeWorkerImage     string  // --claude-worker-image flag
}
```

Controller startup flags:
```
--copilot-worker-image=ghcr.io/sozercan/mercan/agent-worker-copilot:v1.0.0
--claude-worker-image=ghcr.io/sozercan/mercan/agent-worker-claude:v1.0.0
```

> **Rationale**: A cluster-scoped CRD requires a separate controller, cluster-scoped RBAC, CRD validation, status updates, and condition management — all for what amounts to "which image to use for copilot/claude workers." Controller flags achieve the same with zero additional complexity. Promote to CRD later if operators need runtime-configurable image management.

### REST API Changes

```go
// internal/api/handlers.go

// CreateTaskRequest is the request body for creating a task
type CreateTaskRequest struct {
    // ... existing fields ...
    AgentRuntime *corev1alpha1.AgentRuntimeSpec `json:"agentRuntime,omitempty"` // NEW
}
```

### Env Var Plumbing (Controller → Worker)

The job builder must set env vars that agent workers read. This was **missing from v0.6**.

```go
// internal/controller/job_builder.go

// addAgentEnvVars adds agent-runtime-specific environment variables
func (b *JobBuilder) addAgentEnvVars(envVars []corev1.EnvVar, task *corev1alpha1.Task, agent *corev1alpha1.Agent) []corev1.EnvVar {
    // Prompt (required)
    prompt := task.Spec.Prompt
    if prompt == "" && task.Spec.AI != nil {
        prompt = task.Spec.AI.Prompt
    }
    envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_PROMPT", Value: prompt})

    runtime := agent.Spec.Runtime

    // Max turns: Task override > Agent default > 50
    maxTurns := int32(50)
    if runtime.DefaultMaxTurns != nil {
        maxTurns = *runtime.DefaultMaxTurns
    }
    if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
        maxTurns = *task.Spec.AgentRuntime.MaxTurns
    }
    envVars = append(envVars, corev1.EnvVar{
        Name:  "MERCAN_MAX_TURNS",
        Value: fmt.Sprintf("%d", maxTurns),
    })

    // Allowed tools: Task override > Agent default
    allowedTools := runtime.DefaultAllowedTools
    if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
        allowedTools = task.Spec.AgentRuntime.AllowedTools
    }
    if len(allowedTools) > 0 {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "MERCAN_ALLOWED_TOOLS",
            Value: strings.Join(allowedTools, ","),
        })
    }

    // Disallowed tools
    if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "MERCAN_DISALLOWED_TOOLS",
            Value: strings.Join(task.Spec.AgentRuntime.DisallowedTools, ","),
        })
    }

    // Allow bash: Task override > Agent default > false
    allowBash := runtime.DefaultAllowBash
    if task.Spec.AgentRuntime != nil {
        allowBash = task.Spec.AgentRuntime.AllowBash
    }
    envVars = append(envVars, corev1.EnvVar{
        Name:  "MERCAN_ALLOW_BASH",
        Value: fmt.Sprintf("%t", allowBash),
    })

    // Model override from Agent.Spec.Model.Name
    if agent.Spec.Model != nil && agent.Spec.Model.Name != "" {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "MERCAN_MODEL",
            Value: agent.Spec.Model.Name,
        })
    }

    // System prompt from Agent.Spec.SystemPrompt.Inline
    if agent.Spec.SystemPrompt != nil && agent.Spec.SystemPrompt.Inline != "" {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "MERCAN_SYSTEM_PROMPT",
            Value: agent.Spec.SystemPrompt.Inline,
        })
    }

    // Workspace git config
    if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
        ws := task.Spec.AgentRuntime.Workspace
        if ws.GitRepo != "" {
            envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_GIT_REPO", Value: ws.GitRepo})
            envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_GIT_BRANCH", Value: ws.Branch})
        }
        if ws.Ref != "" {
            envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_GIT_REF", Value: ws.Ref})
        }
        if ws.SubPath != "" {
            envVars = append(envVars, corev1.EnvVar{Name: "MERCAN_WORKSPACE_SUBPATH", Value: ws.SubPath})
        }
    }

    // Timeout
    if task.Spec.Timeout != nil {
        envVars = append(envVars, corev1.EnvVar{
            Name:  "MERCAN_TIMEOUT_SECONDS",
            Value: fmt.Sprintf("%d", int64(task.Spec.Timeout.Duration.Seconds())),
        })
    }

    return envVars
}
```

**Full env var mapping:**

| Env Var | Source | Priority (highest wins) |
|---------|--------|------------------------|
| `MERCAN_PROMPT` | `task.Spec.Prompt` | Task only |
| `MERCAN_MAX_TURNS` | Agent.Runtime.DefaultMaxTurns, Task.AgentRuntime.MaxTurns | Task > Agent > 50 |
| `MERCAN_ALLOWED_TOOLS` | Agent.Runtime.DefaultAllowedTools, Task.AgentRuntime.AllowedTools | Task > Agent |
| `MERCAN_DISALLOWED_TOOLS` | Task.AgentRuntime.DisallowedTools | Task only |
| `MERCAN_ALLOW_BASH` | Agent.Runtime.DefaultAllowBash, Task.AgentRuntime.AllowBash | Task > Agent > false |
| `MERCAN_MODEL` | Agent.Spec.Model.Name | Agent only |
| `MERCAN_SYSTEM_PROMPT` | Agent.Spec.SystemPrompt.Inline | Agent only |
| `MERCAN_GIT_REPO` | Task.AgentRuntime.Workspace.GitRepo | Task only |
| `MERCAN_GIT_BRANCH` | Task.AgentRuntime.Workspace.Branch | Task only |
| `MERCAN_GIT_REF` | Task.AgentRuntime.Workspace.Ref | Task only |
| `MERCAN_WORKSPACE_SUBPATH` | Task.AgentRuntime.Workspace.SubPath | Task only |
| `MERCAN_TIMEOUT_SECONDS` | task.Spec.Timeout | Task only |
| `ANTHROPIC_API_KEY` / `GITHUB_TOKEN` | Agent.SecretRef (mounted as env) | Agent only |

---

## Implementation Plan

> **v0.7 Note**: Phases reordered per review recommendation. Controller integration now comes before worker binaries to unblock e2e testing with stub workers. Tests written alongside each phase, not at the end.

### Phase 1: API Types & Generation (Week 1)

| Task | Description | Files |
|------|-------------|-------|
| 1.1 | Add `TaskTypeAgent` constant | `api/v1alpha1/task_types.go` |
| 1.2 | Define `AgentRuntimeSpec`, `WorkspaceConfig`, `AgentRuntimeType` | `api/v1alpha1/task_types.go` |
| 1.3 | Extend `AgentSpec` with `Runtime *AgentCLIRuntime` | `api/v1alpha1/agent_types.go` |
| 1.4 | Generate CRD manifests | `make generate manifests` |
| 1.5 | Add `AgentRuntime` field to `CreateTaskRequest` | `internal/api/handlers.go` |
| 1.6 | Unit tests for new types | `api/v1alpha1/*_test.go` |

### Phase 2: Controller Integration (Week 2-3)

| Task | Description | Files |
|------|-------------|-------|
| 2.1 | Add `CopilotWorkerImage`/`ClaudeWorkerImage` fields to `JobBuilder` | `internal/controller/job_builder.go` |
| 2.2 | Add `case TaskTypeAgent` in `buildContainer()` | `internal/controller/job_builder.go` |
| 2.3 | Implement `addAgentEnvVars()` (see env var mapping above) | `internal/controller/job_builder.go` |
| 2.4 | Add workspace/home `emptyDir` volumes for agent pods | `internal/controller/job_builder.go` |
| 2.5 | Add git secret volume mounting | `internal/controller/job_builder.go` |
| 2.6 | Implement `validateTaskAgentCompatibility()` | `internal/controller/task_controller.go` |
| 2.7 | Add `RuntimeType` field to `WebhookPayload` | `internal/controller/webhook.go` |
| 2.8 | Add controller flags `--copilot-worker-image`, `--claude-worker-image` | `cmd/main.go` |
| 2.9 | Add Makefile targets for multi-image builds | `Makefile` |
| 2.10 | Unit tests for job builder agent path | `internal/controller/job_builder_test.go` |
| 2.11 | E2e test with stub worker image (`busybox` echo + result ConfigMap) | `test/e2e/agent_test.go` |

> **Key**: Phase 2 is testable end-to-end with a stub worker image (e.g., `busybox` that reads `MERCAN_PROMPT`, echoes it, and writes a result ConfigMap). This validates the full flow: Task CRD → controller → Job → Pod → result, without depending on any external SDK.

### Phase 3: Claude Code CLI Worker (Week 3-4)

| Task | Description | Files |
|------|-------------|-------|
| 3.1 | Create Go worker that shells out to Claude CLI | `workers/agent/claude/main.go` |
| 3.2 | Implement workspace git clone in worker startup | `workers/agent/claude/main.go` |
| 3.3 | Implement result ConfigMap write (matching existing pattern) | `workers/agent/claude/main.go` |
| 3.4 | Implement SIGTERM handler for graceful shutdown | `workers/agent/claude/main.go` |
| 3.5 | Map `MERCAN_ALLOWED_TOOLS` → `--allowedTools` (NOT `--dangerously-skip-permissions`) | `workers/agent/claude/main.go` |
| 3.6 | Create Dockerfile (shared base image approach) | `workers/agent/claude/Dockerfile` |
| 3.7 | Build and test worker image | Docker build + e2e |

> **Rationale**: Claude worker comes before Copilot because it's simpler (just `exec.Command`) and the Claude Code CLI is GA (not Technical Preview).

### Phase 4: Copilot SDK Worker (Week 4-5)

| Task | Description | Files |
|------|-------------|-------|
| 4.1 | Create Go worker using Copilot SDK | `workers/agent/copilot/main.go` |
| 4.2 | Implement workspace git clone in worker startup | `workers/agent/copilot/main.go` |
| 4.3 | Implement result ConfigMap write | `workers/agent/copilot/main.go` |
| 4.4 | Implement SIGTERM handler for graceful shutdown | `workers/agent/copilot/main.go` |
| 4.5 | Create Dockerfile (shared base image with Claude worker) | `workers/agent/copilot/Dockerfile` |
| 4.6 | Build and test worker image | Docker build + e2e |
| 4.7 | Pin Copilot SDK version, add upgrade test | `go.mod` |

### Phase 5: Session, Webhook & Polish (Week 5-6)

| Task | Description | Files |
|------|-------------|-------|
| 5.1 | Define session format for agent tasks (final text result only) | `internal/controller/session_manager.go` |
| 5.2 | Document cross-runtime session continuity semantics | Docs |
| 5.3 | Example manifests for all agent task patterns | `config/samples/` |
| 5.4 | User documentation | `docs/` |
| 5.5 | Update CLAUDE.md | `CLAUDE.md` |

### Phase 6: Integration Testing (Week 6-7)

| Task | Description | Files |
|------|-------------|-------|
| 6.1 | E2e tests with real Claude worker | `test/e2e/agent_claude_test.go` |
| 6.2 | E2e tests with real Copilot worker | `test/e2e/agent_copilot_test.go` |
| 6.3 | E2e test: workspace git clone + agent execution | `test/e2e/agent_workspace_test.go` |
| 6.4 | E2e test: session continuity across agent tasks | `test/e2e/agent_session_test.go` |

> **Note**: Unit tests are written alongside each phase (1.6, 2.10), not batched at the end.

### Image Size Considerations

| Image | Estimated Size | Notes |
|-------|---------------|-------|
| Controller (distroless) | ~20 MB | Unchanged |
| AI Worker (Go binary) | ~30 MB | Unchanged |
| Agent Worker (Copilot) | ~600 MB+ | Node.js 22 slim (~200MB) + Copilot CLI + Go binary |
| Agent Worker (Claude) | ~500 MB+ | Node.js 22 slim (~200MB) + Claude CLI + Go binary |

**Mitigation**: Use a shared base image for both CLI workers to improve Docker layer caching. Consider multi-arch builds (current Dockerfile supports `TARGETARCH`).

---

## Validation Rules

The controller enforces mutual exclusivity between task types and agent configurations:

### Task + Agent Compatibility

| Task Type | Agent.runtime | Valid? | Notes |
|-----------|---------------|--------|-------|
| `container` | (any) | ✅ | Agent not used for container tasks |
| `ai` | nil | ✅ | Agent provides LLM config for AI worker |
| `ai` | set | ❌ | **Reject**: Agent is for `type: agent` only |
| `agent` | nil | ❌ | **Reject**: Agent is for `type: ai` only |
| `agent` | set (copilot) | ✅ | Agent provides Copilot runtime config |
| `agent` | set (claude) | ✅ | Agent provides Claude runtime config |

### Controller Validation Logic

```go
// internal/controller/task_controller.go

func (r *TaskReconciler) validateTaskAgentCompatibility(task *v1alpha1.Task, agent *v1alpha1.Agent) error {
    if task.Spec.AgentRef == nil {
        return nil // No agent reference, nothing to validate
    }

    hasRuntime := agent.Spec.Runtime != nil

    switch task.Spec.Type {
    case v1alpha1.TaskTypeAI:
        if hasRuntime {
            return fmt.Errorf("Task type 'ai' cannot use Agent '%s' which has runtime configured (use type 'agent' instead)", agent.Name)
        }
    case v1alpha1.TaskTypeAgent:
        if !hasRuntime {
            return fmt.Errorf("Task type 'agent' requires Agent '%s' to have runtime configured", agent.Name)
        }
    }

    // When runtime is set, Model.Provider must be empty
    // (the CLI handles its own model selection; provider field is for type: ai only)
    if hasRuntime && agent.Spec.Model != nil && agent.Spec.Model.Provider != "" {
        return fmt.Errorf("Agent '%s' has runtime configured but Model.Provider is set; Model.Provider is only for type: ai tasks", agent.Name)
    }

    return nil
}
```

### Secret Key Validation

```go
// Validate secret has required keys for the runtime type
func (r *TaskReconciler) validateSecretForRuntime(secret *corev1.Secret, runtimeType v1alpha1.AgentRuntimeType) error {
    switch runtimeType {
    case v1alpha1.AgentRuntimeCopilot:
        if _, ok := secret.Data["GITHUB_TOKEN"]; !ok {
            return fmt.Errorf("Secret must contain GITHUB_TOKEN for Copilot runtime")
        }
    case v1alpha1.AgentRuntimeClaude:
        if _, ok := secret.Data["ANTHROPIC_API_KEY"]; !ok {
            return fmt.Errorf("Secret must contain ANTHROPIC_API_KEY for Claude runtime")
        }
    }
    return nil
}
```

### Validation Error Examples

```yaml
# ERROR: Agent has runtime but task is type: ai
apiVersion: core.mercan.ai/v1alpha1
kind: Task
spec:
  type: ai  # ❌ Wrong type
  agentRef:
    name: copilot-coder  # Has runtime.type: copilot
# Error: Task type 'ai' cannot use Agent 'copilot-coder' which has runtime configured

---
# ERROR: Agent has no runtime but task is type: agent
apiVersion: core.mercan.ai/v1alpha1
kind: Task
spec:
  type: agent  # ❌ Wrong type
  agentRef:
    name: claude-analyst  # No runtime field
# Error: Task type 'agent' requires Agent 'claude-analyst' to have runtime configured
```

---

## Security Model

> **v0.7 Note**: Simplified permission model. `readOnlyRootFilesystem` kept `true` (writable `emptyDir` volumes added instead). `AgentPermissions` reduced to boolean `AllowBash`. Glob-based bash/write filtering deferred to beta. `--dangerously-skip-permissions` replaced with `--allowedTools` mapping.

### Permission Hierarchy (Simplified)

```
┌─────────────────────────────────────────────────────────────────────────┐
│                    Permission Resolution Order                          │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  1. Agent.Spec.Runtime (agent-level defaults)                           │
│     │  • DefaultAllowedTools                                            │
│     │  • DefaultAllowBash                                               │
│     │  • DefaultMaxTurns                                                │
│     │                                                                   │
│     ▼                                                                   │
│  2. Task.Spec.AgentRuntime (task-level overrides)                       │
│     • AllowedTools (overrides Agent default)                            │
│     • DisallowedTools (additive)                                        │
│     • AllowBash (overrides Agent default)                               │
│     • MaxTurns (overrides Agent default)                                │
│                                                                         │
│  Rule: More specific (Task) overrides less specific (Agent)             │
│  Rule: DisallowedTools always takes precedence over AllowedTools        │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

> **Change from v0.6**: Removed the three-tier hierarchy (AgentRuntimeConfig → Agent → Task). Now two-tier (Agent → Task) since `AgentRuntimeConfig` CRD is deferred.

### Tool Permission Matrix

| Tool | Default | Can Override | Security Notes |
|------|---------|--------------|----------------|
| Read | ✅ Allowed | Yes | Low risk, read-only |
| Glob | ✅ Allowed | Yes | Low risk, file listing |
| Grep | ✅ Allowed | Yes | Low risk, content search |
| Write | ❌ Denied | Yes | High risk, creates files |
| Edit | ❌ Denied | Yes | High risk, modifies files |
| Bash | ❌ Denied | Yes (via `AllowBash`) | High risk, arbitrary commands |
| WebFetch | ✅ Allowed | Yes | Medium risk, network access |
| WebSearch | ✅ Allowed | Yes | Low risk, search only |

### CLI Permission Mapping

**Claude Code CLI** (replaces `--dangerously-skip-permissions`):
```bash
# v0.6 (WRONG — makes AgentPermissions decorative):
claude --print --dangerously-skip-permissions ...

# v0.7 (CORRECT — maps permissions to CLI flags):
claude --print \
  --allowedTools "Read,Glob,Grep,WebSearch" \
  --max-turns 30 \
  "$MERCAN_PROMPT"

# When AllowBash=true, add Bash to allowedTools:
claude --print \
  --allowedTools "Read,Glob,Grep,Bash,Write,Edit" \
  ...
```

**Copilot SDK**:
```go
// Permissions are passed via SDK config, not CLI flags
config := copilot.SessionConfig{
    AllowedTools: allowedTools,  // From MERCAN_ALLOWED_TOOLS
}
```

### Filesystem Security

> **v0.7 Note**: `readOnlyRootFilesystem` remains `true` (not `false` as in v0.6). Writable directories provided via `emptyDir` volumes.

```go
// job_builder.go — volumes for agent pods
volumes := []corev1.Volume{
    {Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
    {Name: "home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
    {Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
}

volumeMounts := []corev1.VolumeMount{
    {Name: "tmp", MountPath: "/tmp"},
    {Name: "home", MountPath: "/home/worker"},       // CLI config/cache
    {Name: "workspace", MountPath: "/workspace"},     // Git clone target
}

// Security context (unchanged from existing workers)
securityContext := &corev1.SecurityContext{
    AllowPrivilegeEscalation: ptr.To(false),
    ReadOnlyRootFilesystem:   ptr.To(true),   // KEPT TRUE
    RunAsNonRoot:             ptr.To(true),
    RunAsUser:                ptr.To(int64(1000)),
    Capabilities: &corev1.Capabilities{
        Drop: []corev1.Capability{"ALL"},
    },
}
```

Agent CLIs write to `~/.config/`, `~/.cache/`, `/tmp/`, and `/workspace/` — all covered by `emptyDir` mounts. Dockerfile sets `HOME=/home/worker`.

### Git Credentials for Private Repos

```go
// WorkspaceConfig includes GitSecretRef for private repo auth
type WorkspaceConfig struct {
    GitRepo      string                          `json:"gitRepo,omitempty"`
    Branch       string                          `json:"branch,omitempty"`
    Ref          string                          `json:"ref,omitempty"`
    GitSecretRef *corev1.LocalObjectReference    `json:"gitSecretRef,omitempty"`  // NEW
    SubPath      string                          `json:"subPath,omitempty"`
}
```

For Copilot runtime, `GITHUB_TOKEN` from `Agent.SecretRef` can serve double duty (CLI auth + git clone). For Claude runtime, a separate `GitSecretRef` is needed since `ANTHROPIC_API_KEY` can't authenticate git operations.

### Network Security

```yaml
# NetworkPolicy for agent runtime worker pods
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: agent-runtime-worker
spec:
  podSelector:
    matchLabels:
      mercan.ai/worker-type: agent
  policyTypes:
    - Egress
  egress:
    # Allow LLM APIs (Anthropic, OpenAI, Azure, etc.)
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
      ports:
        - protocol: TCP
          port: 443
    # Allow DNS
    - to:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
```

### Deferred Security Features (Beta)

The following were removed from v1alpha1 scope. They require enforcement mechanisms that may not be available in current CLI versions:

- **`BashAllowPatterns` / `BashDenyPatterns`** — Glob-based command filtering. Neither Copilot SDK nor Claude CLI supports glob-based bash command filtering natively. Runtime-level enforcement requires parsing the CLI's tool call stream in real-time, which is complex and fragile.
- **`WriteAllowPatterns` / `WriteDenyPatterns`** — File write path filtering. Same enforcement challenge as bash filtering.
- **`AllowFileWrite` / `AllowNetwork`** — Granular per-capability booleans. For v1alpha1, use `AllowedTools` / `DisallowedTools` lists which map directly to CLI flags.

---

## Migration Strategy

### Phase 1: Parallel Deployment (Non-breaking)

- New `type: agent` tasks run alongside existing types
- No changes to existing `type: ai` behavior
- Users opt-in to agent runtimes explicitly

### Phase 2: Gradual Adoption

```yaml
# Start with read-only exploration
spec:
  type: agent
  agentRuntime:
    workspace:
      gitRepo: https://github.com/org/repo.git
    allowedTools:
      - Read
      - Glob
      - Grep
      - WebSearch
    allowBash: false
```

### Phase 3: Full Capabilities

```yaml
# Enable full coding capabilities
spec:
  type: agent
  agentRuntime:
    workspace:
      gitRepo: https://github.com/org/repo.git
    allowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
    allowBash: true
    maxTurns: 100
```

---

## Testing Strategy

### Unit Tests

```go
// api/v1alpha1/task_types_test.go

func TestAgentRuntimeSpecValidation(t *testing.T) {
    tests := []struct {
        name    string
        spec    AgentRuntimeSpec
        wantErr bool
    }{
        {
            name: "valid minimal spec",
            spec: AgentRuntimeSpec{},
            wantErr: false,
        },
        {
            name: "valid with workspace",
            spec: AgentRuntimeSpec{
                Workspace: &WorkspaceConfig{
                    GitRepo: "https://github.com/org/repo.git",
                    Branch:  "main",
                },
            },
            wantErr: false,
        },
        {
            name: "invalid max turns",
            spec: AgentRuntimeSpec{
                MaxTurns: ptr(int32(500)), // exceeds 200
            },
            wantErr: true,
        },
    }
    // ... test implementation
}
```

### Integration Tests

```go
// test/e2e/agent_test.go

var _ = Describe("Agent Runtime Tasks", func() {
    Context("when creating an agent task with Copilot SDK", func() {
        It("should clone the repository and execute the prompt", func() {
            task := &corev1alpha1.Task{
                ObjectMeta: metav1.ObjectMeta{
                    Name:      "test-agent-task",
                    Namespace: "default",
                },
                Spec: corev1alpha1.TaskSpec{
                    Type:   corev1alpha1.TaskTypeAgent,
                    Prompt: "List all Go files in the repository",
                    AgentRef: &corev1alpha1.AgentReference{
                        Name: "test-copilot-agent",
                    },
                    AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
                        Workspace: &corev1alpha1.WorkspaceConfig{
                            GitRepo: "https://github.com/sozercan/mercan.git",
                        },
                        AllowedTools: []string{"Read", "Glob", "Grep"},
                    },
                },
            }

            Expect(k8sClient.Create(ctx, task)).To(Succeed())

            Eventually(func() corev1alpha1.TaskPhase {
                k8sClient.Get(ctx, client.ObjectKeyFromObject(task), task)
                return task.Status.Phase
            }, timeout, interval).Should(Equal(corev1alpha1.TaskPhaseSucceeded))
        })
    })
})
```

### Manual Testing Checklist

- [ ] Create agent task with Copilot SDK runtime
- [ ] Create agent task with Claude Code CLI runtime
- [ ] Create agent task with git workspace
- [ ] Create agent task with PVC workspace
- [ ] Verify tool restrictions are enforced
- [ ] Verify bash command filtering works
- [ ] Verify file write path filtering works
- [ ] Test MCP server integration
- [ ] Test session continuity across tasks
- [ ] Test webhook notifications
- [ ] Test priority queue ordering
- [ ] Test timeout handling
- [ ] Test retry policy
- [ ] Test BYOK with different providers

---

## Rollout Plan

### Stage 1: Alpha (Internal Testing)

- Deploy to development cluster
- Test with internal team only
- Collect feedback and fix issues
- **Duration**: 2 weeks

### Stage 2: Beta (Limited Users)

- Enable feature flag for select users
- Monitor resource usage and errors
- Iterate on API based on feedback
- **Duration**: 4 weeks

### Stage 3: General Availability

- Remove feature flag
- Publish documentation
- Announce in release notes
- **Duration**: Ongoing

### Rollback Plan

1. If critical issues found, disable agent runtime job creation
2. Existing tasks continue to completion
3. Users fall back to `type: ai` with custom tools
4. Fix issues and re-enable

---

## Open Questions

| # | Question | Status | Decision |
|---|----------|--------|----------|
| 1 | Should we support streaming output? | Deferred | Defer to v2 |
| 2 | How to handle very large results (>1MB)? | Open | Store final text only in session ConfigMap. Full agent transcript logged but not stored (1MB limit). Consider PVC for results in beta. |
| 3 | Should Copilot SDK be default for `type: ai`? | Resolved | No, keep explicit. `type: ai` uses existing AI worker, `type: agent` uses CLI runtimes. |
| 4 | How to handle MCP server crashes? | Deferred | MCP servers deferred to beta. |
| 5 | Support for custom runtime images? | Resolved | Yes, via controller flags `--copilot-worker-image` / `--claude-worker-image`. |
| 6 | Audit logging for agent actions? | Open | Log tool calls to pod stdout (captured by K8s logging). Structured audit log deferred to beta. |
| 7 | How to handle Copilot SDK technical preview changes? | Open | Pin SDK version in `go.mod`. Add integration tests that detect breaking changes. |
| 8 | Copilot CLI installation method? | Open | Verify correct npm package or binary distribution. |
| 9 | Session continuity across runtimes? | Resolved | Supported. Sessions store user/assistant messages as lowest common denominator (JSONL). A `type: ai` session can be continued by `type: agent` and vice versa. Agent-specific metadata lives in annotations. |
| 10 | PVC lifecycle management? | Deferred | PVCs deferred to beta. For v1alpha1, `emptyDir` only. When PVC support is added, PVCs will be user-managed (not controller-managed). |
| 11 | Node.js tooling writing to `node_modules/.cache/`? | Open | If CLI is installed globally, `.cache` writes may fall outside `emptyDir` mounts. Spike needed: run Claude CLI and Copilot CLI with `readOnlyRootFilesystem: true` and verify all write paths are covered. |

---

## Appendix

### A. Example Manifests

#### A.1 Secrets for Agent Runtimes

```yaml
# Secret for Copilot CLI (GitHub authentication)
apiVersion: v1
kind: Secret
metadata:
  name: copilot-credentials
  namespace: dev
type: Opaque
stringData:
  GITHUB_TOKEN: "ghp_xxxxxxxxxxxxxxxxxxxx"  # PAT from Copilot-licensed user
---
# Secret for Claude Code CLI (Anthropic API)
apiVersion: v1
kind: Secret
metadata:
  name: claude-credentials
  namespace: dev
type: Opaque
stringData:
  ANTHROPIC_API_KEY: "sk-ant-api03-xxxxxxxxxxxx"
```

#### A.2 Agent for Copilot CLI (type: agent tasks)

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: copilot-go-developer
  namespace: dev
spec:
  # Model.Name is reused as --model flag for the CLI
  model:
    name: claude-sonnet-4-20250514  # Re-used as --model for CLI (BYOK via Copilot)
    # provider: intentionally empty — validated by controller
  # SystemPrompt.Inline is reused as --system-prompt for the CLI
  systemPrompt:
    inline: |
      You are a Go developer working on this backend service.

      ## Guidelines
      - Follow existing code patterns
      - Write table-driven tests
      - Use meaningful variable names
      - Handle all errors explicitly

      ## Common Commands
      - `go test ./...` - Run all tests
      - `go build ./...` - Build all packages
      - `make lint` - Run linters
  # secretRef contains GITHUB_TOKEN for Copilot authentication
  secretRef:
    name: copilot-credentials
  # runtime field means this Agent is for type: agent tasks ONLY
  runtime:
    type: copilot
    defaultMaxTurns: 50
    defaultAllowedTools:
      - Read
      - Glob
      - Grep
    defaultAllowBash: false
  resources:
    limits:
      memory: 2Gi
      cpu: "2"
```

#### A.3 Agent for Claude Code CLI (type: agent tasks)

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: claude-code-reviewer
  namespace: dev
spec:
  # SystemPrompt reused as --append-system-prompt for Claude CLI
  systemPrompt:
    inline: |
      You are a code reviewer focused on security and performance.
      Be thorough but concise in your feedback.
  # secretRef contains ANTHROPIC_API_KEY for Claude CLI
  secretRef:
    name: claude-credentials
  # runtime field means this Agent is for type: agent tasks ONLY
  runtime:
    type: claude
    defaultMaxTurns: 30
    defaultAllowedTools:
      - Read
      - Glob
      - Grep
      - WebSearch
    defaultAllowBash: false
  resources:
    limits:
      memory: 2Gi
      cpu: "2"
```

#### A.4 Agent for AI Worker (type: ai tasks) - No Runtime

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Agent
metadata:
  name: claude-analyst
  namespace: dev
spec:
  # For type: ai tasks, need LLM provider config
  model:
    provider: anthropic
    name: claude-sonnet-4-20250514
  # secretRef contains ANTHROPIC_API_KEY for LLM API calls
  secretRef:
    name: claude-credentials
  # NO runtime field = this Agent is for type: ai tasks ONLY
  systemPrompt:
    inline: |
      You are a helpful assistant that analyzes data and answers questions.
```

#### A.5 Task using Copilot Agent

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: fix-bug-123
  namespace: dev
spec:
  type: agent  # Must match Agent's runtime config
  prompt: |
    Fix the null pointer exception in the authentication module.
    The error occurs in src/auth/handler.go line 45.
    Write a test to verify the fix.
  agentRef:
    name: copilot-go-developer  # Agent has runtime.type: copilot
  agentRuntime:
    # Runtime type comes from Agent, only task-level overrides here
    workspace:
      gitRepo: https://github.com/org/backend.git
      branch: main
      gitSecretRef:
        name: git-credentials  # For private repos
    maxTurns: 30
    allowedTools:
      - Read
      - Write
      - Edit
      - Glob
      - Grep
      - Bash
    allowBash: true
  timeout: 15m
  webhookURL: https://slack.example.com/webhook
```

#### A.6 Task using Claude Code Agent

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: review-pr-456
  namespace: dev
spec:
  type: agent  # Must match Agent's runtime config
  prompt: |
    Review the code changes in this PR for security issues,
    performance problems, and code quality.
  agentRef:
    name: claude-code-reviewer  # Agent has runtime.type: claude
  agentRuntime:
    workspace:
      gitRepo: https://github.com/org/frontend.git
      branch: feature/new-auth
    maxTurns: 20
  timeout: 10m
```

#### A.7 Task using AI Worker (type: ai)

```yaml
apiVersion: core.mercan.ai/v1alpha1
kind: Task
metadata:
  name: analyze-data
  namespace: dev
spec:
  type: ai  # Must match Agent without runtime
  prompt: "Summarize the key trends in Q4 sales data."
  agentRef:
    name: claude-analyst  # Agent has NO runtime field
  timeout: 5m
```

#### A.8 Controller Flags (replaces AgentRuntimeConfig CRD)

```bash
# In controller deployment, set worker images via flags:
/manager \
  --copilot-worker-image=ghcr.io/sozercan/mercan/agent-worker-copilot:v1.0.0 \
  --claude-worker-image=ghcr.io/sozercan/mercan/agent-worker-claude:v1.0.0

# Or via environment variables in the manager Deployment:
env:
  - name: COPILOT_WORKER_IMAGE
    value: ghcr.io/sozercan/mercan/agent-worker-copilot:v1.0.0
  - name: CLAUDE_WORKER_IMAGE
    value: ghcr.io/sozercan/mercan/agent-worker-claude:v1.0.0
```

> **v0.7 Note**: The cluster-scoped `AgentRuntimeConfig` CRD from v0.6 is deferred. Controller flags provide the same functionality (image configuration) without requiring a new CRD, controller, or cluster-scoped RBAC. Promote to CRD in beta if operators need dynamic runtime-configurable image management.

### B. Copilot SDK Worker (Go)

```go
// workers/agent/copilot/main.go

package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "os"
    "os/exec"
    "os/signal"
    "syscall"

    "github.com/github/copilot-sdk/go/copilot"
    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
)

func main() {
    // Handle SIGTERM for graceful shutdown (agent tasks may run 15+ minutes)
    ctx, cancel := context.WithCancel(context.Background())
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
    go func() {
        <-sigCh
        log.Println("Received shutdown signal, cancelling...")
        cancel()
    }()

    // Read configuration from environment
    prompt := os.Getenv("MERCAN_PROMPT")
    if prompt == "" {
        log.Fatal("MERCAN_PROMPT is required")
    }

    // Clone git repo if specified (worker-does-clone pattern)
    if gitRepo := os.Getenv("MERCAN_GIT_REPO"); gitRepo != "" {
        if err := cloneRepo(ctx, gitRepo); err != nil {
            writeErrorResult(ctx, fmt.Sprintf("git clone failed: %v", err))
            os.Exit(1)
        }
    }

    // Configure the session
    config := copilot.SessionConfig{
        WorkingDirectory: "/workspace",
        OnUserInputRequest: func(req copilot.UserInputRequest) (string, error) {
            return "", fmt.Errorf("interactive input not supported in batch mode")
        },
    }

    // Create session
    session, err := copilot.NewSession(ctx, config)
    if err != nil {
        writeErrorResult(ctx, fmt.Sprintf("failed to create session: %v", err))
        os.Exit(1)
    }
    defer session.Close()

    // Execute the prompt
    result, err := session.Query(ctx, prompt)
    if err != nil {
        writeErrorResult(ctx, fmt.Sprintf("query failed: %v", err))
        os.Exit(1)
    }

    // Write result ConfigMap via K8s API (matching existing worker pattern)
    if err := writeResult(ctx, result.Text); err != nil {
        log.Fatalf("Failed to write result: %v", err)
    }

    fmt.Println("Task completed successfully")
}

// cloneRepo clones a git repository into /workspace
func cloneRepo(ctx context.Context, repoURL string) error {
    branch := os.Getenv("MERCAN_GIT_BRANCH")
    if branch == "" {
        branch = "main"
    }
    cmd := exec.CommandContext(ctx, "git", "clone", "--branch", branch, "--depth", "1", repoURL, "/workspace")
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    return cmd.Run()
}

// writeResult writes the result to a ConfigMap via K8s API
// (same pattern as workers/ai/main.go and workers/general/main.go)
func writeResult(ctx context.Context, result string) error {
    namespace := os.Getenv("MERCAN_TASK_NAMESPACE")
    cmName := os.Getenv("MERCAN_RESULT_CONFIGMAP")

    config, err := rest.InClusterConfig()
    if err != nil {
        return err
    }
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        return err
    }

    cm := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      cmName,
            Namespace: namespace,
        },
        Data: map[string]string{
            "result": result,
        },
    }
    _, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
    return err
}

func writeErrorResult(ctx context.Context, errMsg string) {
    if err := writeResult(ctx, fmt.Sprintf("ERROR: %s", errMsg)); err != nil {
        log.Printf("Failed to write error result: %v", err)
    }
}
```

### C. Claude Code CLI Worker (Go)

```go
// workers/agent/claude/main.go

package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "os"
    "os/exec"
    "os/signal"
    "strconv"
    "strings"
    "syscall"
    "time"

    corev1 "k8s.io/api/core/v1"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/client-go/kubernetes"
    "k8s.io/client-go/rest"
)

func main() {
    // Handle SIGTERM for graceful shutdown
    ctx, cancel := context.WithCancel(context.Background())
    sigCh := make(chan os.Signal, 1)
    signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
    go func() {
        <-sigCh
        log.Println("Received shutdown signal, cancelling...")
        cancel()
    }()

    // Read configuration from environment
    prompt := os.Getenv("MERCAN_PROMPT")
    if prompt == "" {
        log.Fatal("MERCAN_PROMPT is required")
    }

    // Clone git repo if specified (worker-does-clone pattern)
    if gitRepo := os.Getenv("MERCAN_GIT_REPO"); gitRepo != "" {
        if err := cloneRepo(ctx, gitRepo); err != nil {
            writeErrorResult(ctx, fmt.Sprintf("git clone failed: %v", err))
            os.Exit(1)
        }
    }

    // Parse settings
    maxTurns := os.Getenv("MERCAN_MAX_TURNS")
    if maxTurns == "" {
        maxTurns = "50"
    }

    // Build command arguments
    args := []string{
        "--print",
        "--output-format", "json",
        "--max-turns", maxTurns,
    }

    // Map AllowedTools to --allowedTools flag
    // (replaces --dangerously-skip-permissions from v0.6)
    allowedTools := os.Getenv("MERCAN_ALLOWED_TOOLS")
    if allowedTools != "" {
        // Add Bash to allowed tools if MERCAN_ALLOW_BASH is true
        if os.Getenv("MERCAN_ALLOW_BASH") == "true" && !strings.Contains(allowedTools, "Bash") {
            allowedTools += ",Bash"
        }
        args = append(args, "--allowedTools", allowedTools)
    } else {
        // Default: read-only tools
        tools := "Read,Glob,Grep,WebSearch"
        if os.Getenv("MERCAN_ALLOW_BASH") == "true" {
            tools += ",Bash"
        }
        args = append(args, "--allowedTools", tools)
    }

    // Disallowed tools
    disallowedTools := os.Getenv("MERCAN_DISALLOWED_TOOLS")
    if disallowedTools != "" {
        args = append(args, "--disallowedTools", disallowedTools)
    }

    // System prompt from Agent.Spec.SystemPrompt.Inline
    systemPrompt := os.Getenv("MERCAN_SYSTEM_PROMPT")
    if systemPrompt != "" {
        args = append(args, "--append-system-prompt", systemPrompt)
    }

    // Model override from Agent.Spec.Model.Name
    model := os.Getenv("MERCAN_MODEL")
    if model != "" {
        args = append(args, "--model", model)
    }

    // Add the prompt as the final argument
    args = append(args, prompt)

    // Create command with timeout
    timeout, _ := strconv.Atoi(os.Getenv("MERCAN_TIMEOUT_SECONDS"))
    if timeout == 0 {
        timeout = 900 // 15 minutes default
    }
    ctx, timeoutCancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
    defer timeoutCancel()

    cmd := exec.CommandContext(ctx, "claude", args...)
    cmd.Dir = "/workspace"
    cmd.Env = os.Environ()

    // Capture output
    output, err := cmd.Output()
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            log.Printf("Claude CLI stderr: %s", string(exitErr.Stderr))
        }
        writeErrorResult(ctx, fmt.Sprintf("Claude CLI failed: %v", err))
        os.Exit(1)
    }

    // Parse JSON output and extract result
    resultText := parseClaudeOutput(output)

    // Write result ConfigMap via K8s API (matching existing worker pattern)
    if err := writeResult(ctx, resultText); err != nil {
        log.Fatalf("Failed to write result: %v", err)
    }

    fmt.Println("Task completed successfully")
}

// cloneRepo clones a git repository into /workspace
func cloneRepo(ctx context.Context, repoURL string) error {
    branch := os.Getenv("MERCAN_GIT_BRANCH")
    if branch == "" {
        branch = "main"
    }
    cmd := exec.CommandContext(ctx, "git", "clone", "--branch", branch, "--depth", "1", repoURL, "/workspace")
    cmd.Stdout = os.Stdout
    cmd.Stderr = os.Stderr
    return cmd.Run()
}

func parseClaudeOutput(output []byte) string {
    // Claude CLI with --output-format json outputs newline-delimited JSON
    lines := strings.Split(string(output), "\n")

    var lastAssistantMessage string

    for _, line := range lines {
        if line == "" {
            continue
        }
        var msg map[string]interface{}
        if err := json.Unmarshal([]byte(line), &msg); err != nil {
            continue
        }

        if msgType, ok := msg["type"].(string); ok {
            if msgType == "assistant" {
                if content, ok := msg["content"].(string); ok {
                    lastAssistantMessage = content
                }
            }
            if msgType == "result" {
                if content, ok := msg["result"].(string); ok {
                    lastAssistantMessage = content
                }
            }
        }
    }

    return lastAssistantMessage
}

// writeResult writes the result to a ConfigMap via K8s API
// (same pattern as workers/ai/main.go and workers/general/main.go)
func writeResult(ctx context.Context, result string) error {
    namespace := os.Getenv("MERCAN_TASK_NAMESPACE")
    cmName := os.Getenv("MERCAN_RESULT_CONFIGMAP")

    config, err := rest.InClusterConfig()
    if err != nil {
        return err
    }
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        return err
    }

    cm := &corev1.ConfigMap{
        ObjectMeta: metav1.ObjectMeta{
            Name:      cmName,
            Namespace: namespace,
        },
        Data: map[string]string{
            "result": result,
        },
    }
    _, err = clientset.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
    return err
}

func writeErrorResult(ctx context.Context, errMsg string) {
    if err := writeResult(ctx, fmt.Sprintf("ERROR: %s", errMsg)); err != nil {
        log.Printf("Failed to write error result: %v", err)
    }
}
```

### D. Worker Dockerfiles

> **v0.7 Note**: Dockerfiles updated to set `HOME=/home/worker` (supports `readOnlyRootFilesystem: true` with writable `emptyDir` at `/home/worker`). Consider extracting a shared base image for layer caching between Copilot and Claude workers.

#### D.1 Copilot SDK Worker

```dockerfile
# workers/agent/copilot/Dockerfile

FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -o /agent-worker ./workers/agent/copilot/main.go

FROM node:22-slim

# Install system dependencies
RUN apt-get update && apt-get install -y \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Copilot CLI (required by SDK)
# The SDK communicates with the CLI via JSON-RPC
# TODO: Verify correct npm package name (Open Question #8)
RUN npm install -g @githubnext/github-copilot-cli

# Create non-root user
RUN groupadd -g 1000 worker && \
    useradd -u 1000 -g worker -m -s /bin/bash worker

# Create directories (will be overlaid by emptyDir volumes)
RUN mkdir -p /workspace /secrets /session && \
    chown -R worker:worker /workspace /home/worker

COPY --from=builder /agent-worker /usr/local/bin/agent-worker

USER worker
WORKDIR /workspace

# HOME must point to writable emptyDir volume
ENV HOME=/home/worker

ENTRYPOINT ["/usr/local/bin/agent-worker"]
```

#### D.2 Claude Code CLI Worker

```dockerfile
# workers/agent/claude/Dockerfile

FROM golang:1.25 AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOARCH=${TARGETARCH} go build -o /agent-worker ./workers/agent/claude/main.go

FROM node:22-slim

# Install system dependencies
RUN apt-get update && apt-get install -y \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI globally
RUN npm install -g @anthropic-ai/claude-code

# Create non-root user
RUN groupadd -g 1000 worker && \
    useradd -u 1000 -g worker -m -s /bin/bash worker

# Create directories (will be overlaid by emptyDir volumes)
RUN mkdir -p /workspace /secrets /session && \
    chown -R worker:worker /workspace /home/worker

# Copy the Go binary from builder
COPY --from=builder /agent-worker /usr/local/bin/agent-worker

USER worker
WORKDIR /workspace

# HOME must point to writable emptyDir volume
ENV HOME=/home/worker
ENV PATH="/home/worker/.local/bin:${PATH}"

ENTRYPOINT ["/usr/local/bin/agent-worker"]
```

> **Image size note**: Each agent worker image is ~500-600 MB (Node.js 22 slim ~200MB + CLI packages ~100-200MB + Go binary), compared to ~30 MB for existing workers. Both images share the `node:22-slim` base, so Docker layer caching helps when pulling both. Multi-arch builds should use `TARGETARCH` (as shown above) to match the existing controller Dockerfile pattern.

---

## Revision History

| Version | Date | Author | Changes |
|---------|------|--------|---------|
| 0.1 | 2026-02-04 | AI-assisted | Initial draft (Claude Code focus) |
| 0.2 | 2026-02-04 | AI-assisted | Generalized for multiple agent SDKs |
| 0.3 | 2026-02-04 | AI-assisted | Changed Claude runtime to CLI-direct (Go worker shelling out to CLI) |
| 0.4 | 2026-02-05 | AI-assisted | Fixed Copilot SDK deps: requires CLI binary (Node.js), not Go-only |
| 0.5 | 2026-02-05 | AI-assisted | Added default AgentRuntimeConfig deployment requirement |
| 0.6 | 2026-02-05 | AI-assisted | Mutual exclusivity: Agent.runtime determines task type compatibility. Credentials flow through Agent.secretRef (GITHUB_TOKEN for Copilot, ANTHROPIC_API_KEY for Claude). Removed LLMProviderConfig. Added validation rules section. |
| 0.7 | 2026-02-05 | AI-assisted | **Review incorporation.** Dropped sidecar container (workers write results via K8s API). Dropped init container (worker-does-clone). Eliminated `AgentSettings` type (reuse existing Agent.Spec fields). Simplified `AgentPermissions` to boolean `AllowBash`. Deferred `AgentRuntimeConfig` CRD to beta (use controller flags). Deferred `PVCReference`, `MCPServerRef` to beta. Reduced new type count from ~13 to ~5. Fixed env var plumbing gap (`addAgentEnvVars`). Replaced `--dangerously-skip-permissions` with `--allowedTools`. Kept `readOnlyRootFilesystem: true` with emptyDir volumes. Added `gitSecretRef` for private repos. Renamed `AgentRuntimeConfig` struct to `AgentCLIRuntime`. Added `RuntimeType` to webhook payload. Reordered phases (controller before workers). Added SIGTERM handling. Added REST API `CreateTaskRequest` field. Added `Model.Provider` empty validation when runtime is set. Defined session continuity semantics across runtimes. |
