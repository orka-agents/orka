# Getting Started

## Prerequisites

- Docker 17.03+
- kubectl (version compatible with your cluster)
- Access to a Kubernetes cluster
- An LLM API key (Anthropic, OpenAI, or Azure OpenAI)

For development, you also need:
- Go 1.25.3+
- Bun (for UI build)

## Installation

### Using Helm

```bash
helm install orka charts/orka \
  --namespace orka-system \
  --create-namespace
```

### Using kubectl

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=ghcr.io/sozercan/orka:latest
```

## Quick Start

### 1. Create an LLM Provider

```bash
# Create an API key secret
kubectl create secret generic anthropic-secret \
  --from-literal=api-key=your-api-key

# Create a Provider
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic
spec:
  type: anthropic
  secretRef:
    name: anthropic-secret
    key: api-key
  defaultModel: claude-sonnet-4-20250514
EOF
```

### 2. Create an Agent

```yaml
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: assistant
spec:
  providerRef:
    name: anthropic
  model:
    temperature: 0.7
  systemPrompt:
    inline: "You are a helpful assistant."
EOF
```

### 3. Run a Task

```yaml
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: hello-task
spec:
  type: ai
  agentRef:
    name: assistant
  prompt: "What is Kubernetes?"
EOF
```

### 4. Check the Result

```bash
kubectl get task hello-task

# Get the result via the REST API
curl http://localhost:8080/api/v1/tasks/hello-task/result \
  -H "Authorization: Bearer $(kubectl create token orka-client)"
```

### 5. Retrieve Artifacts

After a task completes, you can list and download generated artifacts:

```bash
# API: list and download artifacts
curl http://localhost:8080/api/v1/tasks/hello-task/artifacts \
  -H "Authorization: Bearer $(kubectl create token orka-client)"
curl -L http://localhost:8080/api/v1/tasks/hello-task/artifacts/output.json \
  -H "Authorization: Bearer $(kubectl create token orka-client)" \
  -o output.json

# CLI
orka task artifacts <task-name>
orka task download <task-name> [filename] -o <path>
```

## Agent Runtimes Quick Start

Agent runtimes let you run tasks via Codex CLI, Claude Code CLI, or GitHub Copilot CLI with full autonomous coding capabilities.

### 1. Create Credentials

```bash
# For Claude Code CLI (direct API)
kubectl create secret generic claude-credentials \
  --from-literal=ANTHROPIC_API_KEY=sk-ant-your-key

# For Claude Code CLI (Azure AI Foundry)
kubectl create secret generic claude-credentials \
  --from-literal=CLAUDE_CODE_USE_FOUNDRY=1 \
  --from-literal=ANTHROPIC_FOUNDRY_API_KEY=your-key \
  --from-literal=ANTHROPIC_FOUNDRY_RESOURCE=your-resource \
  --from-literal=ANTHROPIC_DEFAULT_SONNET_MODEL=claude-sonnet-4-5

# For Codex CLI
kubectl create secret generic codex-api-key \
  --from-literal=OPENAI_API_KEY=sk-proj-your-key
```

### 2. Create an Agent with Runtime

```yaml
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: claude-agent
spec:
  secretRef:
    name: claude-credentials
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
EOF
```

For Codex Agents, keep `defaultAllowBash: true` for now. The current Codex runtime implementation fails fast when bash is disabled because the upstream Codex CLI does not yet expose a reliable shell-disable mode.

### 3. Run an Agent Task

```yaml
kubectl apply -f - <<EOF
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: code-review
spec:
  type: agent
  agentRef:
    name: claude-agent
  prompt: "Review the code in this repo for security issues"
  agentRuntime:
    workspace:
      gitRepo: "https://github.com/example/repo.git"
      branch: main
EOF
```

### 4. Check the Result

```bash
kubectl get task code-review

curl http://localhost:8080/api/v1/tasks/code-review/result \
  -H "Authorization: Bearer $(kubectl create token orka-client)"
```

See [Agent Runtimes](agent-runtimes.md) for full configuration reference.

## Optional Runtime Isolation

If your cluster exposes Kubernetes `RuntimeClass` objects such as `gvisor` or `kata-qemu`, you can route worker Jobs through them with `spec.execution`.

```yaml
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: isolated-hello
spec:
  type: ai
  agentRef:
    name: assistant
  prompt: "Summarize the repo"
  execution:
    runtimeClassName: gvisor
    nodeSelector:
      sandbox-runtime: gvisor
```

Use `Agent.spec.execution` for defaults, then override it per task when needed. See [Configuration](configuration.md#execution), [Agent Runtimes](agent-runtimes.md#runtime-isolation), and [Security](security.md#runtime-isolation) for details.

## Accessing the Dashboard

```bash
# Port-forward the controller service
kubectl port-forward -n orka-system svc/orka-controller 8080:8080

# Open in browser
open http://localhost:8080
```

## CLI Tool

The `orka` CLI provides browser-based authentication for the web dashboard.

```bash
# Build the CLI
make build-cli

# Login (extracts token from kubeconfig and opens browser)
./bin/orka login

# Login with custom server
./bin/orka login --server https://orka.example.com

# Login with explicit token
./bin/orka login --token <token>

# Specify kubeconfig
./bin/orka login --kubeconfig ~/.kube/my-config
```

The CLI supports token extraction from bearer tokens, token files, exec-based auth (GKE, AWS IAM), and OIDC auth providers.

## Next Steps

- [Agent Runtimes](agent-runtimes.md) — Codex CLI, Claude Code CLI, and Copilot CLI configuration
- [Interactive Chat](chat.md) — Chat endpoint with tool execution
- [Multi-Agent Coordination](multi-agent-coordination.md) — Coordinator agents and delegation
- [OpenAI Compatibility](openai-compat.md) — Use any OpenAI-compatible client via `/openai/v1/`
- [Anthropic Compatibility](anthropic-compat.md) — Use Anthropic clients (Claude Code, etc.) via `/anthropic/v1/`
- [API Reference](api-reference.md) — REST API endpoints
- [Configuration](configuration.md) — Helm values, controller flags, and metrics
