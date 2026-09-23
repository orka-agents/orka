---
description: "Install Orka on a Kubernetes cluster and run your first agent task."
---

# Getting started

Orka runs AI agents on Kubernetes. You describe work as a **Task**, and Orka runs it in
a Pod, keeps a durable record of what happened, and gives you the result over a REST API,
a CLI, or a built-in web dashboard.

The point is that the API keys stay in the cluster. Developers get a ServiceAccount token,
not an LLM key, and the platform team decides which models and providers are allowed.

## Mental model

Three custom resources cover most of what you will do:

| Resource | What it is |
| --- | --- |
| **Provider** | An LLM backend plus the Secret holding its API key. Anthropic, OpenAI, Azure OpenAI, or any OpenAI- or Anthropic-compatible endpoint. |
| **Agent** | A reusable configuration: which Provider and model to use, a system prompt, which tools it may call. |
| **Task** | One unit of work. This is the thing you create to make something happen. |

A Task points at an Agent; an Agent points at a Provider.

There are three kinds of Task, and the difference matters because they run in different places:

- **`type: ai`** — Orka's own AI worker. It runs in a per-Task Kubernetes Job, calls the
  model, and can use built-in tools like web search and code execution.
- **`type: agent`** — a real coding-agent CLI (Codex, Claude Code, GitHub Copilot CLI, or
  OpenCode) running inside Orka. Orka drives the CLI over the
  [Agent Client Protocol](https://agentclientprotocol.com), an open protocol these CLIs
  speak, and keeps a warm pool of them (a **RuntimePool**) so Tasks start fast.
- **`type: container`** — an arbitrary container command. No model involved. Useful for
  build and test steps that an agent needs done. See
  [Container tasks](guides/container-tasks.md) for the filesystem rules, which trip
  most people up the first time.

[Architecture](concepts/architecture.md) has the full component picture. When a term
is new to you, the [Glossary](reference/glossary.md) has it.

## Install

Follow [Install Orka](operations/installation.md). It is one Helm command, and it ends
with a test container task so you know the cluster side works. Come back here when that
task reports `Succeeded`.

To run your first AI task you also need either an API key for an LLM provider or a
model server your cluster can reach.

Building Orka yourself instead? See [Build from source](development/build-from-source.md),
then continue here.

## Connect to the API

The REST API authenticates with Kubernetes ServiceAccount tokens. The Helm install
created an `orka-client` ServiceAccount with the right permissions.

Forward the API port and leave this running:

```bash
kubectl -n orka-system port-forward svc/orka 8080:8080
```

In a second terminal, create a client token. Every command below runs in this terminal:

```bash
export ORKA_TOKEN="$(kubectl -n orka-system create token orka-client)"
```

The token lasts one hour. If the API starts answering `401`, run that command again,
or ask for a longer one with `--duration=8h`.

:::warning[Namespace matters]
Almost every command on this page needs `-n orka-system`. Orka watches exactly one
namespace, and resources created elsewhere are silently ignored — no error, they just
never run. `kubectl create token orka-client` fails the same way without it.
:::

If you installed with Kustomize rather than Helm, first
[create the client ServiceAccount and its RBAC roles](operations/troubleshooting.md#i-get-403-from-the-api).

## Your first task

### 1. Create a Provider

Store the API key in a Secret, then create a Provider that points at it. Pick the block
for your provider.

**Anthropic**

```bash
kubectl -n orka-system create secret generic anthropic-secret \
  --from-literal=api-key=your-api-key

kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: anthropic
  namespace: orka-system
spec:
  type: anthropic
  secretRef:
    name: anthropic-secret
    key: api-key
  defaultModel: claude-opus-5
EOF
```

**OpenAI, or any OpenAI-compatible endpoint**

```bash
kubectl -n orka-system create secret generic openai-secret \
  --from-literal=api-key=your-api-key

kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: openai
  namespace: orka-system
spec:
  type: openai
  secretRef:
    name: openai-secret
    key: api-key
  defaultModel: gpt-6-astra
  # For a compatible endpoint such as a local model server or a gateway:
  # baseURL: http://my-gateway.models.svc:8080/v1
EOF
```

**Azure OpenAI**

```bash
kubectl -n orka-system create secret generic azure-openai-secret \
  --from-literal=api-key=your-api-key

kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Provider
metadata:
  name: azure-openai
  namespace: orka-system
spec:
  type: azure-openai
  secretRef:
    name: azure-openai-secret
    key: api-key
  baseURL: https://<resource>.openai.azure.com
  azure:
    deploymentName: <your-deployment>
EOF
```

The rest of this page uses the Provider named `anthropic`. If you created a different
one, use its name in `providerRef` below. An Anthropic-compatible endpoint works the same
way with `type: anthropic` and a `baseURL`.

:::tip[No API key yet?]
A GitHub Copilot subscription is enough. Install the Vekil gateway with your Copilot
login as described in [Provider proxy](operations/provider-proxy.md), then point a
`type: openai` Provider's `baseURL` at it, `http://vekil.vekil-system.svc:1337/v1`, with
any placeholder value in the Secret, since the Provider requires one. The same
`baseURL` trick works for any OpenAI-compatible server in your cluster, such as
[Ollama](https://ollama.com/).
:::

### 2. Create an Agent

```bash
kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: assistant
  namespace: orka-system
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

```bash
kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: hello-task
  namespace: orka-system
spec:
  type: ai
  agentRef:
    name: assistant
  prompt: "What is Kubernetes?"
EOF
```

### 4. Read the result

```bash
kubectl -n orka-system get task hello-task
```

The `PHASE` column moves from `Pending` to `Running` to `Succeeded`, usually within a
minute. Then fetch the answer:

```bash
curl -H "Authorization: Bearer ${ORKA_TOKEN}" \
  http://localhost:8080/api/v1/tasks/hello-task/result
```

Files a Task writes to its artifact directory are available the same way at
`/api/v1/tasks/hello-task/artifacts`. This Task only produced text, so that list is empty.

If the Task never leaves `Pending`, see
[Troubleshooting](operations/troubleshooting.md#my-task-stays-pending).

## Running a coding agent

A `type: agent` Task runs a real coding-agent CLI against a git repository. Orka clones the
repo, hands the agent a working copy, and records everything it does.

### 1. Connect a model gateway

Coding agents do not use the Provider from the previous section. They reach models
through a gateway you run, so provider credentials never enter the agent process. Follow
[Provider proxy](operations/provider-proxy.md) once to install a gateway and connect it.
Then confirm the proxy is ready:

```bash
kubectl -n orka-system get deploy -l app.kubernetes.io/component=provider-auth-proxy
```

### 2. Create an Agent with a runtime

The `model.name` must be a model your gateway lists.

This uses the Codex runtime with the model the [gateway examples](operations/provider-proxy.md)
expose. For Claude Code use `type: claude` with a Claude model your gateway lists.

```bash
kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Agent
metadata:
  name: codex-agent
  namespace: orka-system
spec:
  model:
    name: gpt-6-astra
  runtime:
    type: codex
EOF
```

That is the whole Agent. The defaults give it its runtime's full tool set, a shell,
and 50 turns per Task. For `type: opencode`, use `provider/model` names such as
`openai/gpt-6-astra` and set `model.contextWindow` and `model.maxTokens`.
[Agent runtimes](concepts/agent-runtimes.md) has every option, including how to
restrict tools.

### 3. Run it

This Task reads the Orka repository itself, so it works without any credentials:

```bash
kubectl apply -f - <<'EOF'
apiVersion: core.orka.ai/v1alpha1
kind: Task
metadata:
  name: code-review
  namespace: orka-system
spec:
  type: agent
  agentRef:
    name: codex-agent
  prompt: "Review this repo for security issues. Do not modify files."
  workspace:
    intent: read
    gitRepo: "https://github.com/orka-agents/orka.git"
    branch: main
    # For a private repo, add a Secret with a read token. Only Orka's
    # clean-room publisher sees it; the agent process never does.
    # readCredentialRef:
    #   name: repository-read
  agentRuntime:
    maxTurns: 20
EOF
```

### 4. Watch it

```bash
kubectl -n orka-system get task code-review
kubectl -n orka-system get runtimepools
```

The first coding-agent Task starts a RuntimePool for that runtime, which pulls the agent
image and takes a few minutes. Later Tasks reuse the warm pool and start in seconds. Read
the result the same way as before, at `/api/v1/tasks/code-review/result`.

## The dashboard

With the [API port forwarded](#connect-to-the-api), open
[http://localhost:8080](http://localhost:8080) and sign in with your client token.
The dashboard is included in Orka. See [Web dashboard](guides/ui.md).

## The CLI

The CLI is optional. Build it from the root of an Orka source checkout with the
[Go toolchain](development/development.md#prerequisites) installed:

```bash
make build-cli
./bin/orka login                                  # reads your kubeconfig, opens a browser
./bin/orka login --server https://orka.example.com
./bin/orka login --token '<token>'
```

It can pull a token from a bearer token, a token file, exec-based auth (GKE, AWS IAM), or
an OIDC provider. With it, checking a Task and downloading its artifacts looks like this:

```bash
./bin/orka --server http://localhost:8080 --token "$ORKA_TOKEN" -n orka-system \
  task status code-review
./bin/orka --server http://localhost:8080 --token "$ORKA_TOKEN" -n orka-system \
  task artifacts '<task-name>'
./bin/orka --server http://localhost:8080 --token "$ORKA_TOKEN" -n orka-system \
  task download '<task-name>' '<artifact-name>'
```

`task status` keeps to the rows you need. A Task with a write-intent workspace also
shows its delivery state and publication branch, and a failed Task shows its reason;
`--verbose` adds the runtime details.

```console
$ orka task status code-review
FIELD  VALUE
Task   code-review
Phase  Succeeded
```

Full command list: [CLI reference](reference/cli.md).

## Next steps

**Learn the pieces**

- [Glossary](reference/glossary.md) — the terms these docs use, defined once
- [Architecture](concepts/architecture.md) — how a Task becomes a Pod
- [Configuration](reference/configuration.md) — Helm values and controller flags
- [Security](concepts/security.md) — hardening, auth, and tenancy, including
  [stronger isolation](concepts/security.md#execution-workloads) with gVisor or Kata

**Do something with it**

- [Interactive chat](guides/chat.md) — talk to an orchestrator that creates Tasks for you
- [Container tasks](guides/container-tasks.md) — build and test steps that actually work
- [Multi-agent coordination](reference/multi-agent-coordination.md) — one agent delegating to several
- [Repository monitors](guides/repository-monitors.md) — automatic PR review queues
- [Scheduled tasks](guides/scheduled-tasks.md) — cron-driven agents

**Connect your own tools**

- [OpenAI-compatible API](reference/openai-compat.md) — Continue, Cursor, and similar
- [Anthropic-compatible API](reference/anthropic-compat.md) — Claude Code and similar
- [REST API](reference/api-reference.md)

**Work on Orka**

- [Build from source](development/build-from-source.md)
- [Development](development/development.md)

**When it breaks**

- [Troubleshooting](operations/troubleshooting.md)
- [Operations runbook](operations/runbook.md)
