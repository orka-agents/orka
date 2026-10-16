---
description: "Run your first AI task and your first coding agent on Orka, then explore the dashboard, the CLI, and chat."
---

# Getting started

This guide takes you from a fresh install to a working AI Task, a coding agent reviewing a
real repository, and the tools you will use every day.

New to Orka? [What is Orka?](overview.md) explains the three resources you will create here —
a **Provider** (a model and its API key), an **Agent** (a reusable configuration), and a
**Task** (one unit of work) — and the three types of Task.

## Before you begin

You need:

- Orka installed. Follow [Install Orka](operations/installation.md); it is one Helm command
  and ends with a test Task. Building Orka yourself? See
  [Build from source](development/build-from-source.md), then continue here.
- An API key for a model provider, or a model server your cluster can reach.

## Connect to the API

The REST API authenticates with Kubernetes ServiceAccount tokens. The Helm install created an
`orka-client` ServiceAccount with the right permissions.

Forward the API port and leave this running:

```bash
kubectl -n orka-system port-forward svc/orka 8080:8080
```

In a second terminal, create a client token. Every command below runs in this terminal:

```bash
export ORKA_TOKEN="$(kubectl -n orka-system create token orka-client)"
```

The token lasts one hour. If the API starts answering `401`, run that command again, or ask
for a longer one with `--duration=8h`.

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
  tools:
    - name: web_search
EOF
```

`web_search` is one of Orka's [built-in tools](reference/built-in-tools.md). The Agent can
call it whenever the model decides it needs to.

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

## Run a coding agent {#running-a-coding-agent}

A `type: agent` Task runs a real coding-agent CLI against a git repository. Orka clones the
repository, hands the agent a working copy, and records everything it does.

### 1. Connect a model gateway

Coding agents do not use the Provider from the previous section. They reach models through
a gateway you run, so provider credentials never enter the agent process. Follow
[Provider proxy](operations/provider-proxy.md) once to install a gateway and connect it.
Then confirm the proxy is ready:

```bash
kubectl -n orka-system get deploy -l app.kubernetes.io/component=provider-auth-proxy
```

### 2. Create an Agent with a runtime

This uses the Codex runtime with the model the [gateway examples](operations/provider-proxy.md)
expose. The `model.name` must be a model your gateway lists. For Claude Code, use
`type: claude` with a Claude model.

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

To have an agent change code and open a pull request, set `workspace.intent: write` and add
publication credentials; see [Write and publication Tasks](concepts/agent-runtimes.md#write-and-publication-task).

## Ask the orchestrator

Chat describes the work in plain language and lets an orchestrator create and run the Tasks.
Send one message and watch it stream its progress:

```bash
curl -N -H "Authorization: Bearer ${ORKA_TOKEN}" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{"message": "Run a container task that prints the date, then tell me what it printed.", "namespace": "orka-system"}' \
  http://localhost:8080/api/v1/chat
```

[Interactive chat](guides/chat.md) covers sessions, the orchestrator's tools, and its limits.

## Use the dashboard {#the-dashboard}

The dashboard is built into the controller but off by default. Turn it on with the Helm value
`controller.dashboard.enabled=true` (see [Web dashboard](guides/ui.md#turn-it-on)), then,
with the [API port forwarded](#connect-to-the-api), open
[http://localhost:8080](http://localhost:8080) and sign in with your client token.

Want to look around first? The [dashboard demo](pathname:///demo/)
runs in your browser on sample data.

## Use the CLI {#the-cli}

The `orka` CLI is optional. Download it from
[GitHub Releases](https://github.com/orka-agents/orka/releases), choosing the version that
matches your installed chart, and follow [CLI installation](reference/cli.md#installation) to
verify and install it. No Go toolchain is needed. Contributors can
[build it from source](reference/cli.md#building-from-source).

Point the CLI at the forwarded API once, and every command after that uses it:

```bash
orka config set-server http://localhost:8080
orka config set-namespace orka-system
kubectl -n orka-system create token orka-client | orka config set-token --file -
orka version
```

Then check on the Tasks from this guide:

```console
$ orka task list
$ orka task result hello-task
$ orka task status code-review
FIELD  VALUE
Task   code-review
Phase  Succeeded
```

`task status` keeps to the rows you need. A Task with a write-intent workspace also shows its
delivery state and publication branch, and a failed Task shows its reason; `--verbose` adds
the runtime details. With the dashboard turned on, `orka dashboard` opens it already signed in.
The [CLI reference](reference/cli.md) covers authentication options, output formats, and
every workflow.

## Next steps

**Do more**

- [Agents, skills, and tools](guides/agents-skills-and-tools.md) — shape what an agent knows and can do
- [Multi-agent coordination](guides/multi-agent-coordination.md) — one agent delegating to several
- [Repository monitors](guides/repository-monitors.md) — review every pull request automatically
- [Issue-to-PR automation](guides/issue-to-pr-automation.md) — from a labeled issue to a pull request
- [Scheduled tasks](guides/scheduled-tasks.md) — run any Task on a cron schedule
- [Container tasks](guides/container-tasks.md) — build and test steps that work the first time

**Connect your tools**

- [OpenAI-compatible API](reference/openai-compat.md) — Codex, OpenCode and more
- [Anthropic-compatible API](reference/anthropic-compat.md) — Claude Code and similar
- [Gateways](guides/gateways.md) — Microsoft Teams, Telegram, and A2A

**Understand it**

- [Architecture](concepts/architecture.md) — how a Task becomes a Pod
- [Security](concepts/security.md) — credential boundaries and hardening, including
  [stronger isolation](concepts/security.md#execution-workloads) with gVisor or Kata
- [Glossary](reference/glossary.md) — every term, defined once

**When it breaks**

- [Troubleshooting](operations/troubleshooting.md) — error messages, causes, and fixes
- [Operations runbook](operations/runbook.md) — what to do when something looks stuck
