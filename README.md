<div align="center">

<img src="website/static/img/orka-logo.png" alt="Orka" width="400" />

# Orka

**Kubernetes-native AI agent orchestration.**

[Getting Started](website/docs/getting-started.md) · [Architecture](website/docs/concepts/architecture.md) · [API Reference](website/docs/reference/api-reference.md) · [Documentation](#documentation)

</div>

---

Orka turns your Kubernetes cluster into an AI-powered task execution platform. Spin up swarms of AI agents that write code, review PRs, research topics, or run containers — each as an isolated Kubernetes Job with full scheduling, retries, and observability. A coordinator agent dynamically decomposes complex tasks, spawns specialist agents to work in parallel, and synthesizes their results — no manual orchestration graphs required.

One `helm install`, one LLM secret, and you're chatting with an orchestrator that handles the rest.

> [!IMPORTANT]
> **Orka is experimental and under active development.** APIs, CRDs, and behavior may change without notice between releases, and it is not yet recommended for production use. Feedback, bug reports, and feature ideas are very welcome — please [open an issue](https://github.com/orka-agents/orka/issues).

> [!NOTE]
> The organization and repositories are intended to be donated to a community-governed foundation at the appropriate time. Until then, the project is governed by Microsoft policy, and external contributors are required to sign the Microsoft Contributor License Agreement (CLA).

## Why Run AI Agents on Kubernetes?

**No API keys on developer machines** — LLM credentials live in Kubernetes Secrets, managed by your platform team. Developers connect via ServiceAccount tokens — no risk of leaked keys in dotfiles, shell history, or laptops.

**Centralized control** — One place to set model policies, rate limits, and allowed providers across every team. Swap models or providers without touching developer configs.

**Every agent action is auditable** — Tasks run as Kubernetes Jobs with full logs, Prometheus metrics, and result storage. Know exactly what every agent did, when, and at what cost.

**Isolated execution** — Each agent runs in its own Pod with a hardened security context: non-root, read-only rootfs, all capabilities dropped, seccomp enforced. Agents can't escape their sandbox.

**Scale with your cluster** — Priority scheduling, retry policies, concurrency limits, and cron-based execution — all handled by the Kubernetes control plane you already operate.

## What Can You Build?

**Parallel code review** — Spawn a swarm of review agents — security, performance, test coverage, accessibility, whatever you need. Each reviews independently and in parallel, then the coordinator synthesizes findings into a single report.

**Autonomous dev workflows** — A coordinator agent dynamically breaks down a feature request, delegates implementation to specialist agents (backend, frontend, tests), and opens a PR with the combined result — no predefined workflow graphs.

**Research with competing hypotheses** — Multiple agents investigate different theories in parallel, challenge each other's findings, and converge on the strongest explanation. The adversarial structure avoids the anchoring bias of sequential investigation.

**Scheduled operations** — Cron-based agents that run daily security scans, dependency audits, or report generation — all with retry policies and webhook notifications.

**Use your favorite AI client** — Connect Continue, Cursor, or any OpenAI-compatible client to Orka's API. Your cluster manages the LLM credentials — developers just code.

**CI/CD integration** — Trigger agent tasks from GitHub Actions, monitor progress via the REST API, and gate deployments on agent analysis.

## Features

- 🤖 **AI Agents** — Anthropic, OpenAI, or Azure OpenAI with tools, skills, and session persistence
- 🛠️ **Agent Runtimes** — Delegate repo-backed coding tasks to Codex CLI, Claude Code CLI, or GitHub Copilot CLI
- 🔁 **Autonomous Task Loops** — Coordinators can iterate on long-running goals until complete, canceled, or at an iteration limit
- 🔀 **Multi-Agent Coordination** — Coordinators delegate to specialists with depth and concurrency controls
- 💬 **Interactive Chat** — Agentic orchestrator with SSE streaming that creates and manages agents and tasks for you
- 🧠 **Durable Memory** — Namespace-scoped recall, transcript search, and reviewable memory proposals that can be applied
- 🛡️ **Repository Security Scanning** — Scheduled and incremental repository scans with threat models, validated findings, patch generation, and remediation PRs
- 🔎 **Repository Monitors** — Durable GitHub PR review queues with scheduled and webhook-triggered review runs
- 🧰 **Agent Sandbox Workspaces** — Experimental durable, reusable coding workspaces through `agent-sandbox`
- 🖥️ **Web Dashboard** — Built-in React UI embedded in the controller binary — zero extra deployments
- 📦 **Declarative CRDs** — Task, Agent, Tool, Provider, Skill, RepositoryScan, and RepositoryMonitor custom resources for GitOps workflows
- ⏰ **Scheduled Tasks** — Cron-based recurring execution with concurrency policies
- 🔌 **REST & OpenAI-Compatible API** — Full CRUD + `/openai/v1/chat/completions` endpoint for Continue, Cursor, and any OpenAI-compatible client
- 🔐 **Kubernetes, OIDC & Kontxt TxToken Auth** — ServiceAccount tokens by default, with optional OIDC and scoped `kontxt` transaction-token flows
- 🔮 **Anthropic-Compatible API** — `/anthropic/v1/messages` endpoint for Claude Code and other Anthropic-native clients
- 📊 **Observability** — Prometheus metrics, structured logging, health probes, and optional OpenTelemetry traces + GenAI OTLP metrics
- 🔒 **Hardened by Default** — Non-root containers, read-only rootfs, ServiceAccount token auth

## Quick Start

### Install

```bash
helm install orka charts/orka \
  --namespace orka-system \
  --create-namespace
```

### Upgrade

Helm installs files under `crds/` only on a fresh install; an ordinary
`helm upgrade` does not create or update them. Package the target chart once,
run its guarded CRD migration, and use that exact archive for the release
upgrade:

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

The helper requires `jq`, rejects `HELM_KUBE*` endpoint or credential overrides,
reads Helm release storage through the explicit kubectl context, requires the
latest release status to be `deployed` or `failed`, and server-preflights all nine
exact patch/create operations before mutation. It rechecks Helm state before and
after mutation, then verifies target generation, accepted names, and served API
discovery for every CRD. If
mutation or verification fails, partial changes and recovery artifacts are left
in place with a unique migration marker; automatic schema
rollback is intentionally avoided because it can invalidate custom resources or
alter field ownership. CRDs are cluster-scoped, shared by all Orka releases, and
retained on uninstall. Users without a source checkout should follow the
[validated portable workflow](charts/orka/README.md#validated-portable-workflow),
which uses a separately trusted migrator and treats the target archive as data
only.

#### Reinstall with retained CRDs

If the prior Helm release was uninstalled but any Orka CRDs remain, migrate them
**before** installing the replacement release. The missing-release mode is an
explicit opt-in and refuses a cluster with neither a release nor existing Orka
CRDs:

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

### Set Up a Provider

```bash
kubectl create secret generic anthropic-secret \
  --from-literal=api-key=your-api-key

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

### Start Chatting

Use the built-in dashboard, or connect any OpenAI-compatible client:

```bash
kubectl port-forward -n orka-system svc/orka-api 8080:8080

# Open the web dashboard
open http://localhost:8080
```

The built-in orchestrator creates agents, runs tasks, monitors progress, and returns results — all from natural language. See the [OpenAI Compatibility](website/docs/reference/openai-compat.md) and [Anthropic Compatibility](website/docs/reference/anthropic-compat.md) docs for proxy setup with your preferred client.

## Documentation

|                                                              |                                                       |
| ------------------------------------------------------------ | ----------------------------------------------------- |
| [Getting Started](website/docs/getting-started.md)                   | Installation, quick start, CLI setup                  |
| [Architecture](website/docs/concepts/architecture.md)                         | System design, components, and data flow              |
| [Configuration](website/docs/concepts/configuration.md)                       | CRD reference, Helm values, controller flags, metrics |
| [Observability](website/docs/guides/observability.md)                        | OpenTelemetry traces, GenAI metrics, and task trace guidance |
| [Agent Runtimes](website/docs/concepts/agent-runtimes.md)                     | Built-in CLI runtimes and bring-your-own remote AgentRuntime backends |
| [CLI Harness Wrapper](website/docs/guides/cli-harness-wrapper.md)                  | Harness protocol wrapper for Codex, Claude, and Copilot CLI runtimes |
| [Agent Sandbox](website/docs/concepts/agent-sandbox.md)                       | Experimental upstream `agent-sandbox` workspace execution for agent runtimes |
| [Interactive Chat](website/docs/guides/chat.md)                             | Chat endpoint, tools, and SSE streaming               |
| [Multi-Agent Coordination](website/docs/guides/multi-agent-coordination.md) | Coordinator agents and task delegation                |
| [Autonomous Tasks](website/docs/guides/autonomous-tasks.md)                 | Long-running coordinator loops with persisted plan state |
| [Memory](website/docs/concepts/memory.md)                                   | Durable memory, proposals, transcript search, and validation |
| [API Reference](website/docs/reference/api-reference.md)                       | REST API endpoints and usage examples                 |
| [OpenAI Compatibility](website/docs/reference/openai-compat.md)                | OpenAI-compatible chat completions API                |
| [Anthropic Compatibility](website/docs/reference/anthropic-compat.md)          | Anthropic-compatible Messages API                     |
| [Web Dashboard](website/docs/guides/ui.md)                                  | Frontend architecture and pages                       |
| [Security](website/docs/concepts/security.md)                                 | Security model and hardening                          |
| [Kontxt Quickstart](website/docs/guides/kontxt-quickstart.md)               | Use OIDC identity to call Orka without long-lived tokens |
| [Kontxt TxToken Integration](website/docs/concepts/kontxt.md)                 | TxToken verification, authorization, TTS, and audit guidance |
| [Repository Security Scanning](website/docs/guides/repository-security-scanning.md) | Repository scan workflow, threat models, findings, and remediation |
| [Repository Monitors](website/docs/guides/repository-monitors.md) | Durable GitHub pull request monitor runs, review tasks, and dashboard state |
| [GitHub Label Triggers](website/docs/guides/github-label-triggers.md) | Trigger Orka agent tasks from GitHub labels such as `agent:implement` and `agent:review` |
| [Development](website/docs/development/development.md)                           | Building, testing, and contributing                   |
| [Testing](website/docs/development/testing.md)                                   | Test structure, patterns, and commands                |
