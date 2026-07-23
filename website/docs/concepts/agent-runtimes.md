---
slug: /agent-runtimes
---

# Agent Runtimes

Agent runtimes cover two paths: built-in CLI runtimes such as Codex CLI, Claude Code CLI, GitHub Copilot CLI, and OpenCode CLI, and bring-your-own remote execution backends selected through `AgentRuntime` and `spec.runtime.runtimeRef`. Built-in CLI runtimes give tasks autonomous coding capabilities (file read/write/edit, bash execution, git operations), while `AgentRuntime` lets teams run workloads on a generic HTTP runtime, AgentKit Serve, Foundry, or another adapter. Orka keeps scheduling, lifecycle, sessions, approvals, tool governance, idempotency, events, lineage, and result storage.

## Supported built-in CLI runtimes

| Runtime | `runtime.type` | Secret Key | Status |
|---------|---------------|------------|--------|
| Codex CLI | `codex` | `OPENAI_API_KEY` or `CODEX_API_KEY` | GA |
| Claude Code CLI | `claude` | `ANTHROPIC_API_KEY` (direct) or `ANTHROPIC_FOUNDRY_API_KEY` (Azure AI Foundry) | GA |
| GitHub Copilot CLI | `copilot` | `GITHUB_TOKEN` | Technical Preview |
| OpenCode CLI | `opencode` | `OPENAI_BASE_URL` and `OPENAI_API_KEY` | Technical Preview |

## Live Coverage

PR-blocking live CI currently exercises these runtime scenarios against real model families:

- `codex` + GPT with a pinned git workspace and `priorTaskRef`
- `claude` + Claude with `sessionRef`
- `copilot` + Gemini with a pinned public repo checkout

This coverage is about Orka's runtime wiring and task/session/workspace behavior. The live backend used in CI is harness infrastructure, not the main product under test.

OpenCode uses a custom OpenAI-compatible provider, so it can target chat-completions endpoints such as vLLM, Ray Serve, or Ollama. Set `OPENAI_BASE_URL` to the endpoint base, optionally including a trailing `/chat/completions`, and set `OPENAI_API_KEY` to the credential expected by that endpoint. The adapter strips the trailing chat-completions path before configuring OpenCode.

For OpenCode Agents, `spec.model.name` is the endpoint-specific model ID. `spec.model.maxTokens` sets the OpenCode output limit, defaults to `8192`, and must not exceed the OpenCode 1.18.2 cap of `32000`. `spec.model.contextWindow` sets the context limit and defaults to `128000`. After defaults are applied, `contextWindow` must be greater than `maxTokens` so input context remains available. These OpenCode-specific constraints do not impose a global output maximum on other runtimes.

OpenCode 1.18.2 cannot path-filter its `grep` permission. Orka therefore forces OpenCode `grep` to `deny`, even when `Grep` is present in `defaultAllowedTools` or a task allowlist. The adapter still allows path-aware `read` while denying `*.env` and `*.env.*` (except `*.env.example`). The Agent creation UI starts OpenCode with `defaultAllowBash: false` and omits both `Grep` and `Bash` from its tool defaults; direct API clients can request Bash, but the adapter always keeps Grep denied.

OpenCode CLI session continuation is not wired initially. Each Orka turn starts a new OpenCode CLI session, while Orka still retains its own task, result, and lineage records. Read-only scheduled agent tasks do not support OpenCode initially because non-interactive OpenCode runs pre-approve file edits.

## Bring-your-own AgentRuntime

`AgentRuntime` is the Orka-facing interface for remote execution backends. The backend can be a generic self-hosted HTTP runtime, AgentKit Serve, Foundry, or a future adapter. Orka keeps task lifecycle, approvals, tool governance, idempotency, events, lineage, and results.

Remote backends must not receive production Orka Tool credentials. In brokered mode, they request tool calls and Orka authorizes, approves, executes or brokers, injects idempotency, and audits them.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: AgentRuntime
metadata:
  name: sample-http-runtime
spec:
  contractVersion: orka.harness.v1
  deployment:
    mode: external-endpoint
    endpoint: http://sample-http-runtime.default.svc.cluster.local:8080
  clientAuth:
    bearerTokenSecretRef:
      name: sample-http-runtime-token
      key: token
  capabilities:
    toolExecutionModes:
      - observed
    supportsCancel: true
    supportsRuntimeSessions: true
---
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: remote-agent
spec:
  runtime:
    runtimeRef:
      name: sample-http-runtime
```

## Quick Start

### 1. Create a Secret

```bash
# For Codex CLI
kubectl create secret generic codex-api-key \
  --from-literal=OPENAI_API_KEY=<openai-api-key>

# For Claude Code CLI
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=<anthropic-api-key>

# For GitHub Copilot CLI
kubectl create secret generic copilot-token \
  --from-literal=GITHUB_TOKEN=<github-token>

kubectl create secret generic opencode-credentials \
  --from-literal=OPENAI_BASE_URL=http://models.example/v1 \
  --from-literal=OPENAI_API_KEY=<endpoint-api-key>
```

### Azure AI Foundry for Claude Code CLI

Claude Code CLI supports Azure AI Foundry as an alternative to direct Anthropic API access. This is the Claude Code CLI credential path. A Foundry hosted-agent adapter is a separate remote execution backend behind `AgentRuntime`. To use Azure AI Foundry with Claude Code CLI, include the Foundry-specific environment variables in the secret:

```bash
kubectl create secret generic claude-credentials \
  --from-literal=CLAUDE_CODE_USE_FOUNDRY=1 \
  --from-literal=ANTHROPIC_FOUNDRY_API_KEY=your-key \
  --from-literal=ANTHROPIC_FOUNDRY_RESOURCE=your-resource-name \
  --from-literal=ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5
```

| Variable | Required | Description |
|----------|----------|-------------|
| `CLAUDE_CODE_USE_FOUNDRY` | Yes | Set to `1` to enable Azure AI Foundry |
| `ANTHROPIC_FOUNDRY_API_KEY` | Yes | Azure AI Foundry API key |
| `ANTHROPIC_FOUNDRY_RESOURCE` | Yes | Azure AI Foundry resource name |
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Yes | Deployment name (e.g. `claude-sonnet-4-5`) |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | No | Optional Haiku deployment name |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | No | Optional Opus deployment name |

All secret keys are injected as environment variables into the harness wrapper pod via `envFrom`, so any Claude Code CLI environment variable can be passed through the secret.

### 2. Create an Agent

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  model:
    name: "claude-sonnet-4-20250514"
  systemPrompt:
    inline: "You are a helpful coding assistant."
  secretRef:
    name: claude-api-key
  execution:
    runtimeClassName: gvisor
    nodeSelector:
      sandbox-runtime: gvisor
  runtime:
    type: claude
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
```

### 3. Create a Task

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: refactor-task
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Refactor the main.go file to use structured logging"
  timeout: "30m"
```

### 4. Get the Result

```bash
kubectl get task refactor-task
# Get the result via the REST API
curl http://localhost:8080/api/v1/tasks/refactor-task/result \
  -H "Authorization: Bearer <service-account-token>"
```

## Agent Configuration

An Agent resource with a `runtime` field defines either a built-in CLI runtime with `runtime.type` or a namespace-local `AgentRuntime` facade with `runtime.runtimeRef` for `type: agent` tasks. The `runtime` field is mutually exclusive with `providerRef` (which is for `type: ai` tasks).

### Full Reference

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  # runtime marks this Agent for type: agent tasks
  runtime:
    # type: which built-in CLI runtime to use (set exactly one of type or runtimeRef)
    # Valid values: "copilot", "claude", "codex", "opencode"
    type: claude
    # runtimeRef selects a namespace-local AgentRuntime facade instead of a built-in CLI runtime.
    # runtimeRef:
    #   name: sample-http-runtime

    # defaultMaxTurns: default max agent loop iterations per task
    # Range: 1-1000, Default: 50
    defaultMaxTurns: 50

    # defaultAllowedTools: tools available by default
    # Claude tools: Read, Write, Edit, Bash, Glob, Grep, WebFetch, WebSearch
    defaultAllowedTools:
      - Read
      - Glob
      - Grep

    # defaultAllowBash: allow bash commands by default
    # Default: true
    defaultAllowBash: true

  # secretRef: Secret containing API credentials
  # Codex runtime expects: OPENAI_API_KEY or CODEX_API_KEY
  # Claude runtime expects: ANTHROPIC_API_KEY
  # Copilot runtime expects: GITHUB_TOKEN
  # OpenCode runtime expects: OPENAI_BASE_URL and OPENAI_API_KEY
  secretRef:
    name: claude-api-key

  # execution: default runtime and placement settings for harness wrapper pods
  execution:
    runtimeClassName: gvisor
    nodeSelector:
      sandbox-runtime: gvisor
    tolerations:
      - key: sandbox-runtime
        operator: Equal
        value: gvisor
        effect: NoSchedule

  # systemPrompt: injected via --system-prompt flag
  systemPrompt:
    inline: "You are a coding assistant."
    # Or reference a ConfigMap:
    # configMapRef:
    #   name: my-prompt-configmap
    #   key: prompt.txt

  # model: LLM model configuration (passed as --model flag)
  model:
    name: "claude-sonnet-4-20250514"
    # OpenCode output limit (positive integer, default: 8192, maximum: 32000)
    # maxTokens: 8192
    # OpenCode context limit (positive integer, default: 128000; must exceed maxTokens)
    # contextWindow: 128000

  # resources: compute resources for harness wrapper pods
  resources:
    requests:
      memory: "256Mi"
      cpu: "100m"
    limits:
      memory: "512Mi"
```

## Task Configuration

Tasks with `type: agent` reference an Agent and can override its runtime defaults.

### Full Reference

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: my-agent-task
spec:
  # type: must be "agent" for CLI runtime tasks
  type: agent

  # agentRef: references an Agent with runtime configured (required)
  agentRef:
    name: claude-agent
    # namespace: defaults to task namespace
    # namespace: other-ns

  # execution: task-level runtime/placement overrides
  execution:
    runtimeClassName: kata-qemu
    nodeSelector:
      sandbox-runtime: kata

  # prompt: instruction sent to the agent CLI
  prompt: "Fix the failing tests in api/"

  # agentRuntime: task-level overrides (all optional)
  agentRuntime:
    # workspace: git clone configuration
    workspace:
      gitRepo: "https://github.com/example/my-project.git"
      branch: "main"
      # ref: specific commit SHA or tag (mutually exclusive with branch)
      # ref: "abc123"
      # gitSecretRef: Secret with git credentials for private repos
      # gitSecretRef:
      #   name: git-credentials
      # subPath: subdirectory within repo to use as workspace root
      # subPath: "services/api"
      # forkRepo: writable fork URL used as git remote for pushes
      # forkRepo: "https://github.com/my-org/my-project.git"
      # prBaseBranch: upstream branch to target when creating PRs
      # prBaseBranch: "main"
      # pushBranch: branch name to push changes to after task completion
      # pushBranch: "feature/my-change"

    # maxTurns: override Agent's defaultMaxTurns (range: 1-1000)
    maxTurns: 100

    # allowedTools: override Agent's defaultAllowedTools
    allowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep

    # disallowedTools: deny specific tools (takes precedence over allowedTools)
    disallowedTools:
      - WebFetch

    # allowBash: override Agent's defaultAllowBash
    allowBash: true

  # timeout: max duration before the task is terminated
  timeout: "30m"

  # priority: queue ordering (0-1000, higher = more urgent, default: 500)
  priority: 600

  # sessionRef: conversation continuity
  sessionRef:
    name: my-session
    create: true

  # secretRef: override Agent's secretRef for this task
  # secretRef:
  #   name: different-api-key
```

### Permission Resolution

Task-level settings override Agent-level defaults. `disallowedTools` always takes precedence over `allowedTools`.

```
Agent.spec.runtime (defaults)
  └─► Task.spec.agentRuntime (overrides)
        └─► disallowedTools (always wins)
```

### Runtime Isolation

Agent harness wrapper pods can opt into stronger sandboxing through Kubernetes `RuntimeClass` using the shared `spec.execution` field on both Agents and Tasks.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: isolated-review
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Review the repo for security issues"
  execution:
    runtimeClassName: gvisor
    nodeSelector:
      sandbox-runtime: gvisor
    tolerations:
      - key: sandbox-runtime
        operator: Equal
        value: gvisor
        effect: NoSchedule
```

- `Agent.spec.execution` sets defaults for all tasks using that Agent.
- `Task.spec.execution` overrides the Agent and replaces `nodeSelector`, `tolerations`, and `affinity` when set.
- `gvisor` is the recommended first isolation profile for Linux `kind` and other containerd-based development clusters.
- `kata-qemu` uses the same API, but is best suited to `minikube` or production clusters with virtualization-capable nodes.
- The controller itself can remain on the default runtime; only worker Jobs need the isolation runtime.

### Durable Agent Sandbox Workspaces (Experimental)

For durable or retained coding environments, agent Tasks can request `Task.spec.execution.workspace`. That request is separate from the git checkout settings in `Task.spec.agentRuntime.workspace` below. When the controller feature is enabled, Orka validates the request, passes the resolved sandbox settings to the harness wrapper turn, and the worker wrapper claims an upstream `agent-sandbox` workspace before running the configured agent runtime inside it. For `reusePolicy: session`, Orka derives a deterministic sandbox claim name so separate worker Jobs in the same namespaced session/template scope can reattach when the workspace is retained.

See [Agent Sandbox Workspaces](agent-sandbox.md) for the supported fields, controller flags, Helm values, and current limitations.

## Workspace Management

Agent tasks can clone a git repository into the harness wrapper pod's `/workspace` directory.

### Public Repositories

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/example/public-repo.git"
    branch: "main"
```

### Private Repositories

Create a Secret with git credentials, then reference it:

```bash
kubectl create secret generic git-credentials \
  --from-literal=username=oauth2 \
  --from-literal=password=<github-token>
```

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/example/private-repo.git"
    branch: "feature/fix-tests"
    gitSecretRef:
      name: git-credentials
```

> **Note**: For the Copilot runtime, `GITHUB_TOKEN` from the Agent's `secretRef` can authenticate both the CLI and git clone operations. For the Claude, Codex, and OpenCode runtimes, a separate `gitSecretRef` is usually needed because their API keys do not authenticate git operations.

> **Codex caveat**: The current Codex runtime implementation requires `defaultAllowBash: true` (or task-level `allowBash: true`). If bash is disabled, the wrapper fails fast instead of launching Codex, because the current Codex CLI does not expose a reliable shell-disable mode.

### SubPath

Restrict the agent's workspace to a subdirectory of the cloned repository:

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/example/monorepo.git"
    subPath: "services/api"
```

### Specific Commit or Tag

Use `ref` instead of `branch` to check out a specific commit SHA or tag:

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/example/repo.git"
    ref: "v1.2.3"
```

### Push Changes to a Branch

Use `pushBranch` to have the worker commit and push changes automatically at the end of the task.
For fork-based workflows, also set `forkRepo` and `prBaseBranch`.

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/upstream/repo.git"
    forkRepo: "https://github.com/my-user/repo.git"
    prBaseBranch: "main"
    pushBranch: "feature/my-change"
```

## Session Continuity

Sessions enable multi-turn conversations across tasks. Session data is stored in SQLite with a normalized schema.

### How Sessions Work

- Sessions store **user and assistant messages only** (the lowest common denominator across runtimes)
- **Cross-runtime continuation is supported**: a `type: ai` session can be continued by a `type: agent` task and vice versa
- Agent-specific metadata (token counts, message counts) is tracked in the session record
- Full agent transcripts are logged to pod stdout but **not stored** in the session (keep sessions focused)
- Sessions enforce **serial execution**: only one task can hold a session lock at a time
- Session transcripts are delivered to harness wrapper pods via an **init container** that fetches from the controller's internal API

### Using Sessions

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: follow-up-task
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Now add error handling to the code you wrote"
  sessionRef:
    name: my-session
    create: true      # Create the session if it doesn't exist
    append: true      # Append this task's messages to the transcript (default: true)
    maxMessages: 50   # Max messages to load from history (default: 50)
```

### Session Storage

Sessions are stored in the controller's SQLite database with a normalized schema:
- **Session record**: metadata (name, namespace, type, active task, token counts, timestamps)
- **Session messages**: individual transcript entries (role, content, timestamp)

## Security

All agent harness wrapper pods run with a hardened security context:

| Setting | Value |
|---------|-------|
| Run as non-root | uid 1000 |
| Read-only root filesystem | `true` |
| Capabilities | All dropped |
| Seccomp profile | RuntimeDefault |
| Privilege escalation | Disabled |

If `spec.execution.runtimeClassName` is set, the harness wrapper pod is also routed through the selected Kubernetes `RuntimeClass` while keeping the same pod and container security defaults.

Writable directories are provided via `emptyDir` volumes:

| Mount | Purpose |
|-------|---------|
| `/tmp` | Temporary files |
| `/home/worker` | CLI config and cache (`HOME`) |
| `/workspace` | Git clone target and working directory |

### Tool Permissions

| Tool | Default | Risk Level |
|------|---------|------------|
| Read | Allowed | Low |
| Glob | Allowed | Low |
| Grep | Allowed | Low |
| WebSearch | Allowed | Low |
| WebFetch | Allowed | Medium |
| Write | Allowed | High |
| Edit | Allowed | High |
| Bash | Allowed | High |

All tools are allowed by default for autonomous operation. To restrict high-risk tools, set `defaultAllowBash: false` on the Agent or `allowBash: false` on individual Tasks.

For Codex specifically, bash-disabled tasks are not currently supported. Use `defaultAllowBash: true` for Codex Agents until the upstream CLI exposes a reliable shell-disable mode.

For OpenCode 1.18.2, Grep is always denied because the CLI cannot constrain grep to approved workspace paths. This deny overrides Agent and Task allowlists. Read access retains the sensitive environment-file deny rules described above.

### Secrets

API keys are injected as environment variables from Kubernetes Secrets. They are never logged or stored in Task specs.

```bash
# Claude runtime
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=<anthropic-api-key>

# Codex runtime
kubectl create secret generic codex-api-key \
  --from-literal=OPENAI_API_KEY=<openai-api-key>

# Copilot runtime
kubectl create secret generic copilot-token \
  --from-literal=GITHUB_TOKEN=<github-token>

kubectl create secret generic opencode-credentials \
  --from-literal=OPENAI_BASE_URL=http://models.example/v1 \
  --from-literal=OPENAI_API_KEY=<endpoint-api-key>
```

## Controller Configuration

Agent Tasks run exclusively through the CLI harness wrapper. The controller needs:

- `ORKA_HARNESS_WRAPPER_ENDPOINT`, pointing at the wrapper HTTP endpoint.
- `ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE` or `ORKA_HARNESS_WRAPPER_BEARER_TOKEN` when the wrapper requires bearer auth.

The default kustomize deployment includes an `agent-harness-wrapper` Deployment and Service. Per-runtime agent-worker image flags are not supported.

## Optimizing Agent Performance

Research on LLM agent context ([arxiv 2602.11988](https://arxiv.org/abs/2602.11988)) shows that verbose instructions increase token costs without proportional quality gains. Apply these guidelines to Agent and Task configuration:

- **Minimal tool allowlists**: Only grant tools the task actually needs. A read-only analysis task should use `allowedTools: [Read, Glob, Grep]` — don't include `Write`, `Edit`, or `Bash` if the task doesn't require them. Fewer tools means less prompt overhead and a smaller attack surface.
- **Appropriate `maxTurns`**: Set lower values (10–30) for focused tasks like single-file edits, and higher values (50–100+) for complex multi-file refactors. Overly generous limits waste compute on unnecessary iterations if the agent gets stuck.
- **Concise system prompts**: Include only tooling commands, hard requirements, and non-discoverable gotchas. Codebase overviews and redundant documentation increase reasoning tokens by 14–22% without improving outcomes. See [Context Engineering Best Practices](configuration.md#context-engineering-best-practices) for detailed guidance.

## Examples

Complete sample manifests are available in [`config/samples/`](https://github.com/orka-agents/orka/tree/main/config/samples):

| File | Description |
|------|-------------|
| `core_v1alpha1_agent_codex.yaml` | Agent configured for Codex CLI |
| `core_v1alpha1_agent_claude.yaml` | Agent configured for Claude Code CLI |
| `core_v1alpha1_agent_opencode.yaml` | Agent configured for OpenCode CLI and an OpenAI-compatible endpoint |
| `core_v1alpha1_task_agent.yaml` | Basic agent task with workspace |
| `core_v1alpha1_task_agent_copilot.yaml` | Agent task using Copilot runtime |
| `core_v1alpha1_task_agent_workspace.yaml` | Agent task with full workspace configuration |

### Complete Example Flow

```bash
# 1. Create the API key secret
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=<anthropic-api-key>

# 2. Create the Agent
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  model:
    name: "claude-sonnet-4-20250514"
  systemPrompt:
    inline: "You are a senior software engineer."
  secretRef:
    name: claude-api-key
  runtime:
    type: claude
    defaultMaxTurns: 50
    defaultAllowBash: true
    defaultAllowedTools:
      - Read
      - Write
      - Edit
      - Bash
      - Glob
      - Grep
EOF

# 3. Create a Task that clones a repo and works on it
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: fix-tests
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Fix the failing CI tests in the api/ directory"
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/example/my-project.git"
      branch: "main"
    maxTurns: 100
  timeout: "30m"
  priority: 600
EOF

# 4. Watch the task progress
kubectl get task fix-tests -w

# 5. Get the result
curl http://localhost:8080/api/v1/tasks/fix-tests/result \
  -H "Authorization: Bearer <service-account-token>"

# 6. Check harness wrapper pod logs for full transcript
kubectl logs -l job-name=$(kubectl get task fix-tests -o jsonpath='{.status.jobName}')
```

## Troubleshooting

### Task stuck in Pending

- **Missing Secret**: Verify the Secret exists and contains the expected key (`ANTHROPIC_API_KEY` or `GITHUB_TOKEN`).
  ```bash
  kubectl get secret claude-api-key -o jsonpath='{.data}' | keys
  ```
- **Session locked**: Another task may hold the session lock. Check via the REST API:
  ```bash
  curl http://localhost:8080/api/v1/sessions/<name> \
    -H "Authorization: Bearer <service-account-token>"
  ```
- **Agent not found**: Verify the Agent exists and has `runtime` configured:
  ```bash
  kubectl get agent claude-agent -o yaml
  ```

### Task fails immediately

- **Type mismatch**: `type: agent` tasks require an Agent with `runtime` configured. `type: ai` tasks cannot use Agents with `runtime`.
- **Invalid runtime type**: `runtime.type` must be `copilot`, `claude`, `codex`, or `opencode`; for remote execution backends, use `runtime.runtimeRef` pointing at a Ready namespace-local `AgentRuntime`.
- **Worker image not available**: Check that the harness wrapper image is accessible from your cluster:
  ```bash
  kubectl describe pod -l orka.ai/worker-type=agent
  ```

### Result too large

Task results are stored in SQLite, which has no practical size limit. If the agent produces a very large output:

- The final text result is stored; full transcripts are logged to pod stdout only
- Check pod logs for the complete output:
  ```bash
  kubectl logs <pod-name>
  ```

### Pod in CrashLoopBackOff

- Check pod logs for CLI errors:
  ```bash
  kubectl logs <pod-name> --previous
  ```
- Verify the API key is valid and has sufficient permissions
- Ensure the harness wrapper image includes the expected CLI binary
