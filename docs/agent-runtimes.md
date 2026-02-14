# Agent Runtimes

Agent runtimes let Orka delegate task execution to external agent CLIs—such as Claude Code CLI and GitHub Copilot CLI—instead of running tasks through Orka's built-in AI worker. This gives your tasks access to full autonomous coding capabilities (file read/write/edit, bash execution, git operations) provided by battle-tested agent runtimes, while Orka handles scheduling, lifecycle management, secrets, sessions, and Kubernetes-native orchestration.

## Supported Runtimes

| Runtime | `runtime.type` | Secret Key | Status |
|---------|---------------|------------|--------|
| Claude Code CLI | `claude` | `ANTHROPIC_API_KEY` (direct) or `ANTHROPIC_FOUNDRY_API_KEY` (Azure AI Foundry) | GA |
| GitHub Copilot CLI | `copilot` | `GITHUB_TOKEN` | Technical Preview |

## Quick Start

### 1. Create a Secret

```bash
# For Claude Code CLI
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...

# For GitHub Copilot CLI
kubectl create secret generic copilot-token \
  --from-literal=GITHUB_TOKEN=ghp_...
```

### Azure AI Foundry

Claude Code CLI supports Azure AI Foundry as an alternative to direct Anthropic API access. To use Azure AI Foundry, include the Foundry-specific environment variables in the secret:

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

All secret keys are injected as environment variables into the worker pod via `envFrom`, so any Claude Code CLI environment variable can be passed through the secret.

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
  -H "Authorization: Bearer $(kubectl create token orka-client)"
```

## Agent Configuration

An Agent resource with a `runtime` field defines the CLI runtime to use for `type: agent` tasks. The `runtime` field is mutually exclusive with `providerRef` (which is for `type: ai` tasks).

### Full Reference

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: my-agent
spec:
  # runtime marks this Agent for type: agent tasks
  runtime:
    # type: which CLI runtime to use (required)
    # Valid values: "copilot", "claude"
    type: claude

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
  # Claude runtime expects: ANTHROPIC_API_KEY
  # Copilot runtime expects: GITHUB_TOKEN
  secretRef:
    name: claude-api-key

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

  # resources: compute resources for worker pods
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

## Workspace Management

Agent tasks can clone a git repository into the worker pod's `/workspace` directory.

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
  --from-literal=password=ghp_your_token
```

```yaml
agentRuntime:
  workspace:
    gitRepo: "https://github.com/example/private-repo.git"
    branch: "feature/fix-tests"
    gitSecretRef:
      name: git-credentials
```

> **Note**: For the Copilot runtime, `GITHUB_TOKEN` from the Agent's `secretRef` can authenticate both the CLI and git clone operations. For the Claude runtime, a separate `gitSecretRef` is needed since `ANTHROPIC_API_KEY` cannot authenticate git operations.

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

## Session Continuity

Sessions enable multi-turn conversations across tasks. Session data is stored in SQLite with a normalized schema.

### How Sessions Work

- Sessions store **user and assistant messages only** (the lowest common denominator across runtimes)
- **Cross-runtime continuation is supported**: a `type: ai` session can be continued by a `type: agent` task and vice versa
- Agent-specific metadata (token counts, message counts) is tracked in the session record
- Full agent transcripts are logged to pod stdout but **not stored** in the session (keep sessions focused)
- Sessions enforce **serial execution**: only one task can hold a session lock at a time
- Session transcripts are delivered to worker pods via an **init container** that fetches from the controller's internal API

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

All agent worker pods run with a hardened security context:

| Setting | Value |
|---------|-------|
| Run as non-root | uid 1000 |
| Read-only root filesystem | `true` |
| Capabilities | All dropped |
| Seccomp profile | RuntimeDefault |
| Privilege escalation | Disabled |

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

### Secrets

API keys are injected as environment variables from Kubernetes Secrets. They are never logged or stored in Task specs.

```bash
# Claude runtime
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-...

# Copilot runtime
kubectl create secret generic copilot-token \
  --from-literal=GITHUB_TOKEN=ghp_...
```

## Controller Configuration

The controller accepts flags to configure agent worker images:

| Flag | Default | Description |
|------|---------|-------------|
| `--copilot-worker-image` | `orka-agent-worker-copilot:latest` | Container image for Copilot agent workers |
| `--claude-worker-image` | `orka-agent-worker-claude:latest` | Container image for Claude agent workers |

Example:

```bash
orka-controller \
  --copilot-worker-image=ghcr.io/sozercan/orka/agent-worker-copilot:v1.0.0 \
  --claude-worker-image=ghcr.io/sozercan/orka/agent-worker-claude:v1.0.0
```

## Examples

Complete sample manifests are available in [`config/samples/`](../config/samples/):

| File | Description |
|------|-------------|
| `core_v1alpha1_agent_claude.yaml` | Agent configured for Claude Code CLI |
| `core_v1alpha1_task_agent.yaml` | Basic agent task with workspace |
| `core_v1alpha1_task_agent_copilot.yaml` | Agent task using Copilot runtime |
| `core_v1alpha1_task_agent_workspace.yaml` | Agent task with full workspace configuration |

### Complete Example Flow

```bash
# 1. Create the API key secret
kubectl create secret generic claude-api-key \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-your-key

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
  -H "Authorization: Bearer $(kubectl create token orka-client)"

# 6. Check worker pod logs for full transcript
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
    -H "Authorization: Bearer $(kubectl create token orka-client)"
  ```
- **Agent not found**: Verify the Agent exists and has `runtime` configured:
  ```bash
  kubectl get agent claude-agent -o yaml
  ```

### Task fails immediately

- **Type mismatch**: `type: agent` tasks require an Agent with `runtime` configured. `type: ai` tasks cannot use Agents with `runtime`.
- **Invalid runtime type**: `runtime.type` must be `copilot` or `claude`.
- **Worker image not available**: Check that the worker image is accessible from your cluster:
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
- Ensure the worker image includes the expected CLI binary
