# Getting Started

Orka is a Kubernetes-native platform for running AI agents and tool-using workflows as
durable, observable Kubernetes Jobs. You describe work as a **Task**; the controller
schedules it, runs it in a hardened worker pod, stores the result, and (optionally) notifies
you — with sessions, retries, priorities, and multi-agent delegation handled for you.

## Mental Model

Three custom resources cover most use cases:

- **Provider** — an LLM backend (Anthropic, OpenAI, or Azure OpenAI) plus its API-key Secret.
- **Agent** — a reusable configuration: which Provider/model, system prompt, tools, skills,
  and (optionally) an external CLI runtime or coordination settings.
- **Task** — one unit of work. `type: ai` runs through Orka's built-in AI worker, `type: agent`
  runs an external coding CLI (Claude Code, Codex, Copilot), and `type: container` runs an
  arbitrary container command.

A Task references an Agent, an Agent references a Provider. Results are retrieved over the
REST API, the CLI, or the embedded dashboard. See [Architecture](concepts/architecture.md)
for the full component picture.

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

### Upgrading with Helm

Helm installs chart files under `crds/` only during a fresh install. It does not
create or update those CRDs during `helm upgrade`, including when an older Orka
release was installed from a chart that did not contain CRDs.

Package the exact target chart once, migrate its nine CRDs first, and use the
same explicit context and archive for the release upgrade:

```bash
TARGET_CONTEXT="replace-with-context"

(
  set -euo pipefail

  WORK_DIR="$(mktemp -d)"
  KEEP_WORK_DIR=false
  cleanup_work_dir() {
    status=$?
    trap - EXIT
    if [[ "$KEEP_WORK_DIR" == true ]]; then
      echo "Target chart and work files preserved at $WORK_DIR" >&2
    else
      rm -rf "$WORK_DIR"
    fi
    exit "$status"
  }
  trap cleanup_work_dir EXIT

  helm status orka \
    --namespace orka-system \
    --kube-context "$TARGET_CONTEXT"

  helm package charts/orka --destination "$WORK_DIR"
  TARGET_CHARTS=("$WORK_DIR"/orka-*.tgz)
  test "${#TARGET_CHARTS[@]}" -eq 1
  TARGET_CHART="${TARGET_CHARTS[0]}"
  test -f "$TARGET_CHART"

  KEEP_WORK_DIR=true
  scripts/helm-chart.sh upgrade-crds \
    --chart "$TARGET_CHART" \
    --kube-context "$TARGET_CONTEXT" \
    --release orka \
    --namespace orka-system

  helm upgrade orka "$TARGET_CHART" \
    --namespace orka-system \
    --kube-context "$TARGET_CONTEXT" \
    --wait
  KEEP_WORK_DIR=false
)
```

The `jq`-based helper rejects `HELM_KUBE*` endpoint or credential overrides,
reads Helm release storage through the explicit kubectl context, requires a
`deployed` or `failed` latest release, server-preflights all nine exact
patch/create operations before mutation, and verifies every live
target spec. On failure partial changes
are retained with recovery artifacts and a unique migration marker; automatic
schema rollback is intentionally avoided because it can invalidate custom
resources or alter field ownership. For a remote chart, pull one exact
`--version` and follow the [validated portable workflow](https://github.com/orka-agents/orka/blob/main/charts/orka/README.md#validated-portable-workflow),
which uses a separately trusted migrator and treats the target archive as data
only.
Orka CRDs are cluster-scoped, shared by every release, and retained on uninstall;
deleting a CRD manually also deletes all custom resources of that kind.

#### Reinstalling when CRDs were retained

When the old Helm release is gone but Orka CRDs remain, run the migration before
the replacement install. Add `--allow-missing-release` to the helper invocation,
then install the same archive with `--skip-crds`:

```bash
TARGET_CONTEXT="replace-with-context"

(
  set -euo pipefail

  TARGET_CONTEXT="${TARGET_CONTEXT:?set TARGET_CONTEXT}"

  WORK_DIR="$(mktemp -d)"
  KEEP_WORK_DIR=false
  cleanup_work_dir() {
    status=$?
    trap - EXIT
    if [[ "$KEEP_WORK_DIR" == true ]]; then
      echo "Target chart and work files preserved at $WORK_DIR" >&2
    else
      rm -rf "$WORK_DIR"
    fi
    exit "$status"
  }
  trap cleanup_work_dir EXIT

  helm package charts/orka --destination "$WORK_DIR"
  TARGET_CHARTS=("$WORK_DIR"/orka-*.tgz)
  test "${#TARGET_CHARTS[@]}" -eq 1
  TARGET_CHART="${TARGET_CHARTS[0]}"
  test -f "$TARGET_CHART"

  KEEP_WORK_DIR=true
  scripts/helm-chart.sh upgrade-crds \
    --chart "$TARGET_CHART" \
    --kube-context "$TARGET_CONTEXT" \
    --release orka \
    --namespace orka-system \
    --allow-missing-release

  helm install orka "$TARGET_CHART" \
    --skip-crds \
    --namespace orka-system \
    --create-namespace \
    --kube-context "$TARGET_CONTEXT" \
    --wait
  KEEP_WORK_DIR=false
)
```

The opt-in mode requires at least one existing Orka CRD and still performs the
same exact-set validation, server preflight, confirmation, and verification.

### Using kubectl

```bash
# Install CRDs
make install

# Deploy controller
make deploy IMG=ghcr.io/orka-agents/orka:latest
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

See [Agent Runtimes](concepts/agent-runtimes.md) for full configuration reference.

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

Use `Agent.spec.execution` for defaults, then override it per task when needed. See [Configuration](concepts/configuration.md#execution), [Agent Runtimes](concepts/agent-runtimes.md#runtime-isolation), and [Security](concepts/security.md#runtime-isolation) for details.

## Accessing the Dashboard

```bash
# Port-forward the controller service
kubectl port-forward -n orka-system svc/orka-api 8080:8080

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

**Core concepts**

- [Architecture](concepts/architecture.md) — Controller, workers, CRDs, and task lifecycle
- [Configuration](concepts/configuration.md) — Helm values, controller flags, and metrics
- [Memory](concepts/memory.md) — Namespace-scoped durable memory and reviewable proposals
- [Kontxt TxToken Integration](concepts/kontxt.md) — Request-scoped transaction-token auth
- [Agent Sandbox Workspaces](concepts/agent-sandbox.md) / [Substrate](concepts/substrate.md) — Durable, reusable execution workspaces
- [Security](concepts/security.md) — Pod hardening, authentication, and multi-tenancy

**Guides & reference**

- [Agent Runtimes](concepts/agent-runtimes.md) — Codex CLI, Claude Code CLI, and Copilot CLI configuration
- [Interactive Chat](guides/chat.md) — Chat endpoint with tool execution
- [Multi-Agent Coordination](guides/multi-agent-coordination.md) — Coordinator agents and delegation
- [OpenAI Compatibility](reference/openai-compat.md) — Use any OpenAI-compatible client via `/openai/v1/`
- [Anthropic Compatibility](reference/anthropic-compat.md) — Use Anthropic clients (Claude Code, etc.) via `/anthropic/v1/`
- [API Reference](reference/api-reference.md) — REST API endpoints
